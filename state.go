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
	"time"

	"github.com/Songmu/ghalet/fshelper"
)

const maxStateBytes = 1024 * 1024

type ProcessIdentity struct {
	PID     int    `json:"pid"`
	PGID    int    `json:"pgid"`
	UID     int    `json:"uid"`
	Started string `json:"started"`
}

type RunState struct {
	Schema   int    `json:"schema"`
	RunID    string `json:"runId"`
	TargetID string `json:"targetId"`
	// Retained for existing state files, not used to identify registrations.
	ScopeRef     string            `json:"scopeRef,omitempty"`
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

func ensureExecutionDirectory(path string) error {
	if err := fshelper.EnsureDirectory(path, 0755); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	// Upgrade only the legacy managed mode, not custom ancestor permissions.
	if info.Mode() == os.ModeDir|0711 {
		return os.Chmod(path, 0755)
	}
	return nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
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

func (s stateStore) ensureDirectory(state *RunState) (string, error) {
	dir, err := s.directory(state)
	if err != nil {
		return "", err
	}
	for _, path := range []string{s.root, filepath.Dir(dir), dir} {
		if err := ensureExecutionDirectory(path); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (s stateStore) save(state *RunState) error {
	dir, err := s.ensureDirectory(state)
	if err != nil {
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
	return fshelper.SyncDirectory(dir)
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
			} else {
				record.State, record.Err = s.loadRun(path)
			}
			result = append(result, record)
		}
	}
	return result, nil
}

func (s stateStore) loadRun(path string) (*RunState, error) {
	statePath := filepath.Join(path, "state.json")
	info, err := os.Lstat(statePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("missing or unsafe state file in %s", path)
	}
	file, err := os.Open(statePath)
	if err != nil {
		return nil, err
	}
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
	if err != nil {
		return nil, err
	}
	dir, err := s.directory(&state)
	if err != nil {
		return nil, err
	}
	if dir != path || state.Schema != 1 || state.UID <= 0 ||
		state.RunnerName == "" || state.Phase == "" || !filepath.IsAbs(state.Home) {
		return nil, errors.New("invalid state schema, identity, phase or location")
	}
	return &state, nil
}

func (s stateStore) removeFiles(state *RunState) error {
	dir, err := s.ensureDirectory(state)
	if err != nil {
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
