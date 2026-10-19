package ghalet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runnerOutput struct {
	mu      sync.Mutex
	file    *os.File
	buffer  []byte
	secrets *secrets
	ready   func()
}

func (w *runnerOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer = append(w.buffer, data...)
	for {
		line, rest, ok := strings.Cut(string(w.buffer), "\n")
		if !ok {
			break
		}
		if strings.Contains(line, "Listening for Jobs") && w.ready != nil {
			w.ready()
			w.ready = nil
		}
		if _, err := io.WriteString(w.file, w.secrets.redact(line)+"\n"); err != nil {
			return 0, err
		}
		w.buffer = []byte(rest)
	}
	return len(data), nil
}

func (w *runnerOutput) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, writeErr := io.WriteString(w.file, w.secrets.redact(string(w.buffer)))
	return errors.Join(writeErr, w.file.Close())
}

func copyRedacted(input io.Reader, dest string, secret *secrets) error {
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(input)
	for {
		line, readErr := reader.ReadString('\n')
		if _, err := io.WriteString(output, secret.redact(line)); err != nil {
			output.Close()
			return err
		}
		if readErr != nil {
			if readErr != io.EOF {
				output.Close()
				return readErr
			}
			break
		}
	}
	return errors.Join(output.Sync(), output.Close())
}

type archivedRun struct {
	RunID      string    `json:"runId"`
	TargetID   string    `json:"targetId"`
	Scope      string    `json:"scope"`
	ArchivedAt time.Time `json:"archivedAt"`
}

func archiveLogs(c *Config, state *RunState, secret *secrets) error {
	if !c.Cleanup.PreserveLogs {
		return nil
	}
	base := filepath.Join(c.Cleanup.LogRoot, "runs")
	if err := safeDirectory(base, 0700); err != nil {
		return err
	}
	final := filepath.Join(base, state.RunID)
	if info, err := os.Lstat(final); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe archive destination")
		}
		data, err := os.ReadFile(filepath.Join(final, "metadata.json"))
		var archived archivedRun
		if err == nil && json.Unmarshal(data, &archived) == nil && archived.RunID == state.RunID && archived.TargetID == state.TargetID {
			return syncLogArchive(final)
		}
		return errors.New("existing log archive is incomplete or belongs to another execution")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(base, ".archive-"+state.RunID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if info, err := os.Lstat(state.RunnerDir); errors.Is(err, os.ErrNotExist) {
		// Preparation can fail before a runner directory exists.
	} else if err != nil {
		return err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runner directory is not a real directory")
	} else {
		root, err := os.OpenRoot(state.RunnerDir)
		if err != nil {
			return err
		}
		defer root.Close()
		err = filepath.WalkDir(state.RunnerDir, func(source string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(state.RunnerDir, source)
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if relative != "." && relative != "_diag" && !strings.HasPrefix(relative, "_diag"+string(filepath.Separator)) {
					return filepath.SkipDir
				}
				return nil
			}
			if relative == "_diag" && entry.Type()&os.ModeSymlink != 0 {
				return errors.New("runner diagnostic directory is a symlink; original logs retained")
			}
			if relative != "output.log" && !(strings.HasPrefix(relative, "_diag"+string(filepath.Separator)) && strings.HasSuffix(relative, ".log")) {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to archive symlink %s", source)
			}
			info, err := root.Stat(relative)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("invalid runner log %s", source)
			}
			dest := filepath.Join(stage, relative)
			if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				return err
			}
			input, err := root.Open(relative)
			if err != nil {
				return err
			}
			copyErr := copyRedacted(input, dest, secret)
			return errors.Join(copyErr, input.Close())
		})
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(archivedRun{RunID: state.RunID, TargetID: state.TargetID, Scope: state.ScopeRef, ArchivedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := atomicFile(filepath.Join(stage, "metadata.json"), data, 0600); err != nil {
		return err
	}
	if err := syncLogArchive(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	return syncLogArchive(final)
}

func syncLogArchive(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	var entries []string
	if err := filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return fmt.Errorf("unsafe log archive entry %s", name)
		}
		relative, err := filepath.Rel(path, name)
		if err != nil {
			return err
		}
		entries = append(entries, relative)
		return nil
	}); err != nil {
		return err
	}
	// Persist files and child directories before their parent entries.
	for i := len(entries) - 1; i >= 0; i-- {
		file, err := root.Open(entries[i])
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return syncDirectoryChain(filepath.Dir(path))
}

func expireLogs(ctx context.Context, c *Config, dryRun bool) ([]OperationResult, error) {
	base := filepath.Join(c.Cleanup.LogRoot, "runs")
	if err := existingDirectory(base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var results []OperationResult
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || len(entry.Name()) != 32 {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
		var archive archivedRun
		if err != nil || json.Unmarshal(data, &archive) != nil || archive.RunID != entry.Name() || archive.ArchivedAt.IsZero() {
			results = append(results, OperationResult{Path: dir, Error: "invalid log archive metadata; retained"})
			continue
		}
		if time.Since(archive.ArchivedAt) < time.Duration(c.Cleanup.LogRetention) {
			continue
		}
		result := OperationResult{Path: dir, Action: "expire archived logs"}
		if !dryRun {
			root, openErr := os.OpenRoot(base)
			if openErr != nil {
				result.Error = openErr.Error()
			} else {
				if err := root.RemoveAll(entry.Name()); err != nil {
					result.Error = err.Error()
				}
				root.Close()
			}
		}
		results = append(results, result)
	}
	return results, nil
}
