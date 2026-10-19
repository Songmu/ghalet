package ghalet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxStateBytes = 1024 * 1024

type ProcessIdentity struct {
	PID     int    `json:"pid"`
	PGID    int    `json:"pgid"`
	UID     int    `json:"uid"`
	Started string `json:"started"`
}

type RunState struct {
	Schema       int               `json:"schema"`
	RunID        string            `json:"runId"`
	TargetID     string            `json:"targetId"`
	ScopeRef     string            `json:"scopeRef"`
	ScopeURL     string            `json:"scopeUrl"`
	GroupName    string            `json:"groupName"`
	GroupID      int               `json:"groupId"`
	ScaleSetName string            `json:"scaleSetName"`
	ScaleSetID   int               `json:"scaleSetId"`
	RunnerName   string            `json:"runnerName"`
	RunnerID     int               `json:"runnerId,omitempty"`
	User         string            `json:"user"`
	UID          int               `json:"uid"`
	Home         string            `json:"home"`
	RunnerDir    string            `json:"runnerDir"`
	Workspace    string            `json:"workspace"`
	Phase        string            `json:"phase"`
	Process      ProcessIdentity   `json:"process"`
	Members      []ProcessIdentity `json:"members,omitempty"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	StartedAt    time.Time         `json:"startedAt,omitempty"`
	Ready        bool              `json:"ready"`
	Busy         bool              `json:"busy"`
	ProcessDone  bool              `json:"processDone"`
	Error        string            `json:"error,omitempty"`
}

type stateStore struct{ root string }

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func safeDirectory(path string, mode os.FileMode) error {
	return managedDirectory(path, mode, true)
}

func existingDirectory(path string) error {
	return managedDirectory(path, 0, false)
}

func managedDirectory(path string, mode os.FileMode, create bool) error {
	if !filepath.IsAbs(path) || path == "/" {
		return fmt.Errorf("unsafe directory %s", path)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if create && errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if err := os.Chmod(current, mode); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed path must not contain symlinks or non-directories: %s", current)
		}
	}
	return nil
}

func syncDirectoryChain(path string) error {
	for dir := path; ; dir = filepath.Dir(dir) {
		file, err := os.Open(dir)
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

func (s stateStore) directory(state *RunState) (string, error) {
	if !safeID.MatchString(state.TargetID) || len(state.RunID) != 32 {
		return "", errors.New("invalid state targetId or runId")
	}
	if _, err := hex.DecodeString(state.RunID); err != nil {
		return "", errors.New("invalid runId")
	}
	dir := filepath.Join(s.root, state.TargetID, state.RunID)
	if state.RunnerDir != filepath.Join(dir, "runner") || state.Workspace != filepath.Join(dir, "work") {
		return "", errors.New("state paths do not match the managed execution directory")
	}
	return dir, nil
}

func (s stateStore) save(state *RunState) error {
	dir, err := s.directory(state)
	if err != nil {
		return err
	}
	if err := safeDirectory(dir, 0711); err != nil {
		return err
	}
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxStateBytes {
		return fmt.Errorf("state exceeds %d byte limit", maxStateBytes)
	}
	file, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, "state.json")); err != nil {
		return err
	}
	return syncDirectoryChain(dir)
}

type recoveredState struct {
	State *RunState
	Path  string
	Err   error
}

func (s stateStore) load() ([]recoveredState, error) {
	targets, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var result []recoveredState
	for _, target := range targets {
		if target.Name() == ".lock" {
			continue
		}
		path := filepath.Join(s.root, target.Name())
		if !target.IsDir() || target.Type()&os.ModeSymlink != 0 || !safeID.MatchString(target.Name()) {
			result = append(result, recoveredState{Path: path, Err: errors.New("unrecognized workRoot entry; inspect manually")})
			continue
		}
		runs, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			path := filepath.Join(path, run.Name())
			record := recoveredState{Path: path}
			if !run.IsDir() || run.Type()&os.ModeSymlink != 0 {
				record.Err = errors.New("execution entry is not a real directory")
				result = append(result, record)
				continue
			}
			statePath := filepath.Join(path, "state.json")
			info, err := os.Lstat(statePath)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				record.Err = fmt.Errorf("missing or unsafe state file in %s", path)
				result = append(result, record)
				continue
			}
			file, err := os.Open(statePath)
			if err != nil {
				record.Err = err
			} else {
				var state RunState
				input := &io.LimitedReader{R: file, N: maxStateBytes + 1}
				decoder := json.NewDecoder(input)
				decoder.DisallowUnknownFields()
				err = decoder.Decode(&state)
				var extra any
				if err == nil && decoder.Decode(&extra) != io.EOF {
					err = errors.New("state contains trailing data")
				}
				if input.N == 0 {
					err = fmt.Errorf("state exceeds %d byte limit", maxStateBytes)
				}
				file.Close()
				if err == nil {
					var dir string
					dir, err = s.directory(&state)
					if err == nil && (dir != path || state.Schema != 1 || state.UID <= 0 ||
						state.RunnerName == "" || state.Phase == "" || !filepath.IsAbs(state.Home)) {
						err = errors.New("invalid state schema, identity, phase or location")
					}
				}
				if err == nil {
					record.State = &state
				}
				record.Err = err
			}
			result = append(result, record)
		}
	}
	return result, nil
}

func (s stateStore) removeFiles(state *RunState) error {
	dir, err := s.directory(state)
	if err != nil {
		return err
	}
	if err := safeDirectory(dir, 0711); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "state.json" {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	// The recovery record is deliberately the final file removed.
	if err := root.Remove("state.json"); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil {
		if saveErr := s.save(state); saveErr != nil {
			return errors.Join(err, fmt.Errorf("restore cleanup record: %w", saveErr))
		}
		return err
	}
	return nil
}
