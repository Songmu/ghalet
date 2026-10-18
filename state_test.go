package ghalet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	info, _ := os.Stat(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("state permissions are not private")
	}
	if err := safeDirectory(state.RunnerDir, 0700); err != nil {
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
	if err := safeDirectory(c.Runner.WorkRoot, 0711); err != nil {
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
	if second, err := lockWorkRoot(root); err == nil {
		second.close()
		t.Fatal("second workRoot lock succeeded")
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	third, err := lockWorkRoot(root)
	if err != nil {
		t.Fatal("stale lock file prevented reacquisition:", err)
	}
	third.close()
}

func TestProcessIdentitySafety(t *testing.T) {
	state := &RunState{UID: 501, RunnerDir: "/managed/runner", Phase: "busy",
		Process: ProcessIdentity{PID: 1234, PGID: 1234, UID: 501, Started: "original-start"}}
	leader := processEntry{ProcessIdentity: state.Process, Command: "/managed/runner/bin/Runner.Listener run"}
	alive, members, err := inspectGroup(state, map[int]processEntry{1234: leader})
	if err != nil || !alive || len(members) != 1 {
		t.Fatal("valid identity rejected", err)
	}
	reused := leader
	reused.Started = "different-start"
	if _, _, err := inspectGroup(state, map[int]processEntry{1234: reused}); err == nil {
		t.Fatal("reused PID accepted")
	}
	child := processEntry{ProcessIdentity: ProcessIdentity{PID: 5678, PGID: 1234, UID: 501, Started: "child-start"}, Command: "sleep 30"}
	if _, _, err := inspectGroup(state, map[int]processEntry{5678: child}); err == nil {
		t.Fatal("unverified descendants accepted after leader loss")
	}
	state.Members = []ProcessIdentity{child.ProcessIdentity}
	if alive, _, err := inspectGroup(state, map[int]processEntry{5678: child}); err != nil || !alive {
		t.Fatal("previously observed descendants could not be rediscovered")
	}
	state.ProcessDone = true
	if _, _, err := inspectGroup(state, map[int]processEntry{5678: child}); err == nil {
		t.Fatal("live processes accepted for a completed record")
	}
	state.ProcessDone = false
	state.Process = ProcessIdentity{}
	state.Phase = "starting"
	if _, _, err := inspectGroup(state, nil); err == nil {
		t.Fatal("unrecorded launch boundary treated as safely stopped")
	}
}
