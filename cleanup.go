package ghalet

import (
	"bytes"
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

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/logreduct"
)

const maxLogLineBytes = 64 * 1024
const omittedLogLine = "[ghalet: log line over 64 KiB omitted]\n"

var archiveDirectoryMu sync.Mutex

type runnerOutput struct {
	mu       sync.Mutex
	file     *os.File
	buffer   []byte
	dropping bool
	secrets  *logreduct.Redactor
	ready    func()
}

func (w *runnerOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if !w.dropping {
			if len(part) > maxLogLineBytes-len(w.buffer) {
				w.buffer = w.buffer[:0]
				w.dropping = true
				if _, err := io.WriteString(w.file, w.secrets.Redact(omittedLogLine)); err != nil {
					return 0, err
				}
			} else {
				w.buffer = append(w.buffer, part...)
			}
		}
		if end < 0 {
			break
		}
		if !w.dropping {
			line := string(w.buffer)
			if strings.Contains(line, "Listening for Jobs") && w.ready != nil {
				w.ready()
				w.ready = nil
			}
			if _, err := io.WriteString(w.file, w.secrets.Redact(line+"\n")); err != nil {
				return 0, err
			}
		}
		w.buffer, w.dropping = w.buffer[:0], false
		data = data[end+1:]
	}
	return size, nil
}

func (w *runnerOutput) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, writeErr := io.WriteString(w.file, w.secrets.Redact(string(w.buffer)))
	return errors.Join(writeErr, w.file.Sync(), w.file.Close())
}

func copyRedacted(input io.Reader, dest string, secret *logreduct.Redactor) error {
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := &runnerOutput{file: output, secrets: secret}
	_, copyErr := io.Copy(writer, input)
	return errors.Join(copyErr, writer.close())
}

type archivedRun struct {
	RunID      string    `json:"runId"`
	TargetID   string    `json:"targetId"`
	Scope      string    `json:"scope"`
	ArchivedAt time.Time `json:"archivedAt"`
}

func archiveLogs(c *Config, state *RunState, secret *logreduct.Redactor) error {
	if !c.Logs.Preserve {
		return nil
	}
	base := filepath.Join(c.Logs.Root, "runs")
	archiveDirectoryMu.Lock()
	err := fshelper.EnsureDirectory(base, 0700)
	archiveDirectoryMu.Unlock()
	if err != nil {
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
			if err := syncLogArchive(final); err != nil {
				return err
			}
			return fshelper.SyncDirectory(base)
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
	data, err := json.Marshal(archivedRun{RunID: state.RunID, TargetID: state.TargetID, Scope: state.ScopeURL, ArchivedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := fshelper.WriteFileAtomic(filepath.Join(stage, "metadata.json"), data, 0600); err != nil {
		return err
	}
	if err := syncLogArchive(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	return fshelper.SyncDirectory(base)
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
		if entry.IsDir() {
			entries = append(entries, relative)
		}
		return nil
	}); err != nil {
		return err
	}
	// Log contents are already synced when closed; persist directory entries
	// from the leaves up before publishing the archive.
	for i := len(entries) - 1; i >= 0; i-- {
		file, err := root.Open(entries[i])
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}

func expireLogs(ctx context.Context, c *Config, dryRun bool) ([]OperationResult, error) {
	base := filepath.Join(c.Logs.Root, "runs")
	if err := fshelper.CheckDirectory(base); err != nil {
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
		if time.Since(archive.ArchivedAt) < time.Duration(c.Logs.Retention) {
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
