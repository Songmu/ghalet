// Package runnerlog collects, redacts, archives, and expires runner logs.
package runnerlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/logreduct"
)

var archiveDirectoryMu sync.Mutex

// Run identifies the execution whose allowed logs will be archived.
type Run struct {
	RunID     string
	TargetID  string
	Scope     string
	RunnerDir string
}

// Metadata identifies a published archive and its retention age.
type Metadata struct {
	RunID      string    `json:"runId"`
	TargetID   string    `json:"targetId"`
	Scope      string    `json:"scope"`
	ArchivedAt time.Time `json:"archivedAt"`
}

// Archive durably publishes allowed logs, or verifies an already published
// archive on retry without reopening its previously synced log files.
func Archive(logRoot string, run Run, secret *logreduct.Redactor) error {
	base := filepath.Join(logRoot, "runs")
	archiveDirectoryMu.Lock()
	err := fshelper.EnsureDirectory(base, 0700)
	archiveDirectoryMu.Unlock()
	if err != nil {
		return err
	}
	final := filepath.Join(base, run.RunID)
	if info, err := os.Lstat(final); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe archive destination")
		}
		data, err := os.ReadFile(filepath.Join(final, "metadata.json"))
		var archived Metadata
		if err == nil && json.Unmarshal(data, &archived) == nil && archived.RunID == run.RunID && archived.TargetID == run.TargetID {
			if err := syncLogArchive(final); err != nil {
				return err
			}
			return fshelper.SyncDirectory(base)
		}
		return errors.New("existing log archive is incomplete or belongs to another execution")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(base, ".archive-"+run.RunID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if info, err := os.Lstat(run.RunnerDir); errors.Is(err, os.ErrNotExist) {
		// Preparation can fail before a runner directory exists.
	} else if err != nil {
		return err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runner directory is not a real directory")
	} else {
		root, err := os.OpenRoot(run.RunnerDir)
		if err != nil {
			return err
		}
		defer root.Close()
		err = filepath.WalkDir(run.RunnerDir, func(source string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(run.RunnerDir, source)
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
	data, err := json.Marshal(Metadata{RunID: run.RunID, TargetID: run.TargetID, Scope: run.Scope, ArchivedAt: time.Now().UTC()})
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

// Expiration reports an expired archive or a retained invalid archive.
type Expiration struct {
	Path   string
	Action string
	Error  string
}

// Expire removes only completed archives older than retention. Results collected
// before cancellation or an error are returned to the caller.
func Expire(ctx context.Context, logRoot string, retention time.Duration, dryRun bool) ([]Expiration, error) {
	base := filepath.Join(logRoot, "runs")
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
	var results []Expiration
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || len(entry.Name()) != 32 {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
		var archive Metadata
		if err != nil || json.Unmarshal(data, &archive) != nil || archive.RunID != entry.Name() || archive.ArchivedAt.IsZero() {
			results = append(results, Expiration{Path: dir, Error: "invalid log archive metadata; retained"})
			continue
		}
		if time.Since(archive.ArchivedAt) < retention {
			continue
		}
		result := Expiration{Path: dir, Action: "expire archived logs"}
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
