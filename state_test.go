package ghalet

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/procgroup"
)

func TestStateSizeLimitPreservesReadableRecord(t *testing.T) {
	c := testConfig(t)
	state := testState(t, c)
	store := stateStore{c.Runner.WorkRoot}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"error", "members"} {
		t.Run(field, func(t *testing.T) {
			large := *state
			if field == "error" {
				large.Error = strings.Repeat("x", maxStateBytes)
			} else {
				large.Members = make([]procgroup.ProcessIdentity, 8000)
				for i := range large.Members {
					large.Members[i] = procgroup.ProcessIdentity{PID: i + 2, PGID: 1234, UID: state.UID, Started: "linux:00000000-0000-0000-0000-000000000000:123456789"}
				}
			}
			if err := store.save(&large); err == nil || !strings.Contains(err.Error(), "byte limit") {
				t.Fatal("oversized state write was not rejected", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatal("oversized save replaced the last readable record", err)
			}
			records, err := store.load()
			if err != nil || len(records) != 1 || records[0].Err != nil || records[0].State.RunID != state.RunID {
				t.Fatal("oversized save broke recovery of the previous record", err)
			}
		})
	}
}

func TestStateSizeBoundary(t *testing.T) {
	c := testConfig(t)
	state := testState(t, c)
	store := stateStore{c.Runner.WorkRoot}
	state.UpdatedAt = time.Date(2000, 1, 1, 0, 0, 0, 123456789, time.UTC)
	state.Error = "x"
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	state.Error = strings.Repeat("x", maxStateBytes-len(data))
	if err := store.save(state); err != nil {
		t.Fatal("state at the write limit was rejected", err)
	}
	path := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
	data, err = os.ReadFile(path)
	if err != nil || len(data) > maxStateBytes || len(data) < maxStateBytes-10 {
		t.Fatal("test did not reach the state size boundary", len(data), err)
	}
	// Normalize subsecond timestamp length differences to test the exact read limit.
	state.Error += strings.Repeat("x", maxStateBytes-len(data))
	data, err = json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if len(data) != maxStateBytes {
		t.Fatal("incorrect exact-boundary fixture size", len(data))
	}
	for _, extra := range []string{"", "\n", "x"} {
		if err := os.WriteFile(path, append(append([]byte(nil), data...), extra...), 0600); err != nil {
			t.Fatal(err)
		}
		records, err := store.load()
		if err != nil || len(records) != 1 || (records[0].Err != nil) != (extra != "") {
			t.Fatal("state read size limit was inconsistent", len(extra), records, err)
		}
	}
}

func TestStateAtomicRoundTripAndLastFileCleanup(t *testing.T) {
	c := testConfig(t)
	state := testState(t, c)
	store := stateStore{c.Runner.WorkRoot}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	state.Phase = "registered"
	state.RunnerID = 42
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	records, err := store.load()
	if err != nil || len(records) != 1 || records[0].Err != nil || records[0].State.RunnerID != 42 {
		t.Fatalf("records %+v, error %v", records, err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
	if err != nil || !json.Valid(data) || strings.Contains(string(data), "jit") {
		t.Fatal("invalid or secret-bearing state")
	}
	if bytes.Contains(data, []byte(`"scopeRef"`)) {
		t.Fatal("new state persisted a legacy scope reference")
	}
	info, _ := os.Stat(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("state permissions are not private")
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.RunnerDir, "artifact"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	homeSentinel := filepath.Join(testDirectory(t), "home-sentinel")
	os.WriteFile(homeSentinel, []byte("keep"), 0600)
	os.Symlink(filepath.Dir(homeSentinel), filepath.Join(state.RunnerDir, "outside"))
	if err := store.removeFiles(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(state.RunnerDir)); !os.IsNotExist(err) {
		t.Fatal("execution directory was not removed")
	}
	if _, err := os.Stat(homeSentinel); err != nil {
		t.Fatal("cleanup followed a symlink outside its execution")
	}
}

func TestStateSaveSyncsOnlyExecutionDirectory(t *testing.T) {
	c := testConfig(t)
	ancestor := filepath.Join(testDirectory(t), "restricted")
	c.Runner.WorkRoot = filepath.Join(ancestor, "runs")
	state := testState(t, c)
	if err := fshelper.EnsureDirectory(c.Runner.WorkRoot, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ancestor, 0711); err != nil {
			t.Error(err)
		}
	})
	store := stateStore{c.Runner.WorkRoot}
	if err := store.save(state); err != nil {
		t.Fatal("state save tried to sync an unchanged storage ancestor", err)
	}
	dir := filepath.Dir(state.RunnerDir)
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0755); err != nil {
			t.Error(err)
		}
	})
	if err := store.save(state); !errors.Is(err, os.ErrPermission) {
		t.Fatal("state save did not require syncing its execution directory", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal("test did not reach the post-rename durability boundary", err)
	}
}

func TestMalformedAndUnrecordedStateIsRetained(t *testing.T) {
	c := testConfig(t)
	state := testState(t, c)
	store := stateStore{c.Runner.WorkRoot}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
	for _, data := range []string{"{", "{}", `{"schema":999}`, `{"schema":1,"unknown":"secret"}`} {
		os.WriteFile(path, []byte(data), 0600)
		records, err := store.load()
		if err != nil || len(records) != 1 || records[0].Err == nil {
			t.Fatalf("malformed state accepted: %s", data)
		}
	}
	os.Remove(path)
	records, err := store.load()
	if err != nil || len(records) != 1 || records[0].Err == nil {
		t.Fatal("unrecorded directory was ignored")
	}
}

func TestStateRejectsEscapingPathsAndSymlinks(t *testing.T) {
	c := testConfig(t)
	store := stateStore{c.Runner.WorkRoot}
	state := testState(t, c)
	state.Workspace = "/"
	if err := store.save(state); err == nil {
		t.Fatal("escaped workspace accepted")
	}
	state = testState(t, c)
	external := testDirectory(t)
	if err := fshelper.EnsureDirectory(c.Runner.WorkRoot, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(c.Runner.WorkRoot, "build")); err != nil {
		t.Fatal(err)
	}
	if err := store.save(state); err == nil {
		t.Fatal("symlink target directory accepted")
	}
}

func TestWorkRootLockIsOSBacked(t *testing.T) {
	root := filepath.Join(testDirectory(t), "runs")
	first, err := lockWorkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockWorkRoot(root); !errors.Is(err, syscall.EWOULDBLOCK) {
		if second != nil {
			second.Close()
		}
		t.Fatal("second workRoot lock did not report contention:", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, ".lock"), 0644); err != nil {
		t.Fatal(err)
	}
	third, err := lockWorkRoot(root)
	if err != nil {
		t.Fatal("stale lock file prevented reacquisition:", err)
	}
	third.Close()
}

func TestLaunchedProcessRequiresCompleteIdentity(t *testing.T) {
	valid := procgroup.ProcessIdentity{PID: 1234, PGID: 1234, UID: 501, Started: "kernel-start"}
	for _, tc := range []struct {
		name     string
		identity procgroup.ProcessIdentity
		wantErr  bool
	}{
		{"valid", valid, false},
		{"empty start", procgroup.ProcessIdentity{PID: 1234, PGID: 1234, UID: 501}, true},
		{"wrong group", procgroup.ProcessIdentity{PID: 1234, PGID: 5678, UID: 501, Started: "kernel-start"}, true},
		{"wrong user", procgroup.ProcessIdentity{PID: 1234, PGID: 1234, UID: 502, Started: "kernel-start"}, true},
		{"wrong PID", procgroup.ProcessIdentity{PID: 5678, PGID: 1234, UID: 501, Started: "kernel-start"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, err := procgroup.LaunchedIdentity(map[int]procgroup.Entry{1234: {ProcessIdentity: tc.identity}}, valid.PID, valid.UID)
			if (err != nil) != tc.wantErr || (!tc.wantErr && identity != valid) {
				t.Fatal("unexpected launch identity validation result", identity, err)
			}
		})
	}
	if _, err := procgroup.LaunchedIdentity(nil, valid.PID, valid.UID); err == nil {
		t.Fatal("absent launch identity was accepted")
	}
}

func TestProcessIdentitySafety(t *testing.T) {
	state := &RunState{UID: 501, RunnerDir: "/managed/runner", Phase: "busy",
		Process: procgroup.ProcessIdentity{PID: 1234, PGID: 1234, UID: 501, Started: "original-start"}}
	for _, pid := range []int{-1234, -1, 1} {
		malformed := *state
		malformed.Process.PID, malformed.Process.PGID = pid, pid
		if _, _, err := procgroup.Inspect(malformed.processGroup(), nil); err == nil {
			t.Fatal("invalid recorded PID was accepted as an absent group", pid)
		}
	}
	leader := procgroup.Entry{ProcessIdentity: state.Process, Command: "/managed/runner/bin/Runner.Listener run"}
	alive, members, err := procgroup.Inspect(state.processGroup(), map[int]procgroup.Entry{1234: leader})
	if err != nil || !alive || len(members) != 1 {
		t.Fatal("valid identity rejected", err)
	}

	reused := leader
	reused.Started = "different-start"
	if _, _, err := procgroup.Inspect(state.processGroup(), map[int]procgroup.Entry{1234: reused}); err == nil {
		t.Fatal("reused PID accepted")
	}
	child := procgroup.Entry{ProcessIdentity: procgroup.ProcessIdentity{PID: 5678, PGID: 1234, UID: 501, Started: "child-start"}, Command: "sleep 30"}
	if _, _, err := procgroup.Inspect(state.processGroup(), map[int]procgroup.Entry{5678: child}); err == nil {
		t.Fatal("unverified descendants accepted after leader loss")
	}
	state.Members = []procgroup.ProcessIdentity{child.ProcessIdentity}
	if alive, _, err := procgroup.Inspect(state.processGroup(), map[int]procgroup.Entry{5678: child}); err != nil || !alive {
		t.Fatal("previously observed descendants could not be rediscovered")
	}
	state.ProcessDone = true
	if _, _, err := procgroup.Inspect(state.processGroup(), map[int]procgroup.Entry{5678: child}); err == nil {
		t.Fatal("live processes accepted for a completed record")
	}
	state.ProcessDone = false
	state.Process = procgroup.ProcessIdentity{}
	state.Phase = "starting"
	if _, _, err := procgroup.Inspect(state.processGroup(), nil); err == nil {
		t.Fatal("unrecorded launch boundary treated as safely stopped")
	}
}

func TestRecoveryRetainsMalformedProcessIDs(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.Phase = "busy"
	state.Process = procgroup.ProcessIdentity{PID: -1234, PGID: -1234, UID: state.UID, Started: "invalid"}
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := d.runs[state.RunID]
	reconcileAndWait(d)
	if run == nil || !run.Uncertain || run.State.ProcessDone || d.occupied() != 1 || d.drained("") {
		t.Fatal("malformed process identity was treated as safely stopped")
	}
	if response := d.gc(t.Context(), false); response.Error == "" {
		t.Fatal("gc accepted a malformed process identity")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state.RunnerDir), "state.json")); err != nil {
		t.Fatal("malformed recovery state was removed", err)
	}
}

func TestStateIdentityThroughTrustedBaseAlias(t *testing.T) {
	c := testConfig(t)
	base := testDirectory(t)
	link := filepath.Join(testDirectory(t), "state-layout")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATE_DIRECTORY", link)
	c, err := settings.ParseConfig(c.Path, []byte(readFixture(t, "config/base.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	state := testState(t, c)
	store := stateStore{c.Runner.WorkRoot}
	if !strings.HasPrefix(state.RunnerDir, base+string(filepath.Separator)) {
		t.Fatal("new state retained a storage alias", state.RunnerDir)
	}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	lock, err := lockWorkRoot(c.Runner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	t.Setenv("STATE_DIRECTORY", base)
	reloaded, err := settings.LoadConfig(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockWorkRoot(reloaded.Runner.WorkRoot); err == nil {
		second.Close()
		t.Fatal("canonical base bypassed the alias's work lock")
	}
	records, err := (stateStore{reloaded.Runner.WorkRoot}).load()
	if err != nil || len(records) != 1 || records[0].Err != nil ||
		records[0].State.RunnerDir != state.RunnerDir || records[0].State.Workspace != state.Workspace {
		t.Fatal("canonical base changed recovery identity", records, err)
	}
	if err := store.removeFiles(records[0].State); err != nil {
		t.Fatal("canonical recovered state could not be cleaned", err)
	}
}

func TestStateCleanupRejectsReplacedDeletionRoots(t *testing.T) {
	for _, location := range []string{"workRoot", "target", "run"} {
		t.Run(location, func(t *testing.T) {
			c := testConfig(t)
			state := testState(t, c)
			store := stateStore{c.Runner.WorkRoot}
			if err := store.save(state); err != nil {
				t.Fatal(err)
			}
			path := c.Runner.WorkRoot
			switch location {
			case "target":
				path = filepath.Dir(filepath.Dir(state.RunnerDir))
			case "run":
				path = filepath.Dir(state.RunnerDir)
			}
			outside := filepath.Join(testDirectory(t), "moved")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "keep")
			if err := os.WriteFile(sentinel, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := store.removeFiles(state); err == nil {
				t.Fatal("cleanup followed a replaced deletion root")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
				t.Fatal("cleanup modified an external file", err)
			}
		})
	}
}

func TestStateRecoveryAfterTrustedBaseMigration(t *testing.T) {
	for _, movedDirectory := range []string{"base", "managed workRoot"} {
		t.Run(movedDirectory, func(t *testing.T) {
			c := testConfig(t)
			base := testDirectory(t)
			t.Setenv("STATE_DIRECTORY", base)
			c, err := settings.LoadConfig(c.Path)
			if err != nil {
				t.Fatal(err)
			}
			state := testState(t, c)
			state.ProcessDone, state.Phase = true, "cleaning"
			store := stateStore{c.Runner.WorkRoot}
			if err := store.save(state); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(testDirectory(t), "migrated")
			source := base
			if movedDirectory == "managed workRoot" {
				source = c.Runner.WorkRoot
				movedBase := testDirectory(t)
				moved = filepath.Join(movedBase, "runs")
				t.Setenv("STATE_DIRECTORY", movedBase)
			}
			if err := os.Rename(source, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, source); err != nil {
				t.Fatal(err)
			}
			reloaded, err := settings.LoadConfig(c.Path)
			if err != nil {
				t.Fatal(err)
			}
			newStore := stateStore{reloaded.Runner.WorkRoot}
			records, err := newStore.load()
			if err != nil || len(records) != 1 {
				t.Fatal("migrated record was not discovered", records, err)
			}
			if movedDirectory == "managed workRoot" {
				if records[0].Err == nil {
					t.Fatal("recovery resolved a managed workRoot instead of its trusted base")
				}
				return
			}
			recovered := records[0].State
			if records[0].Err != nil || recovered.RunID != state.RunID || recovered.UID != state.UID ||
				recovered.RunnerName != state.RunnerName || recovered.ProcessDone != state.ProcessDone {
				t.Fatal("base migration lost recovery identity", records, err)
			}
			dir := filepath.Join(reloaded.Runner.WorkRoot, state.TargetID, state.RunID)
			if recovered.RunnerDir != filepath.Join(dir, "runner") || recovered.Workspace != filepath.Join(dir, "work") {
				t.Fatal("recovered paths are not canonical", recovered)
			}
			if err := newStore.save(recovered); err != nil {
				t.Fatal("canonical record could not be persisted", err)
			}
			if err := newStore.removeFiles(recovered); err != nil {
				t.Fatal("migrated stopped execution could not be cleaned", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("cleanup retained the migrated execution", err)
			}
		})
	}
}
