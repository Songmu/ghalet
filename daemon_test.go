package ghalet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func pumpUntil(t *testing.T, d *daemon, predicate func() bool) {
	t.Helper()
	check := func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for {
			select {
			case event := <-d.events:
				d.processEvent(event)
			default:
				d.reconcile()
				return predicate()
			}
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.mu.Lock()
	status, _ := json.MarshalIndent(d.statusLocked(), "", "  ")
	d.mu.Unlock()
	t.Fatalf("condition not satisfied before timeout: %s", status)
}

func cleanupProcesses(t *testing.T, d *daemon) {
	t.Helper()
	t.Cleanup(func() {
		d.mu.Lock()
		for _, run := range d.runs {
			if run.Cancel != nil {
				run.Cancel()
			}
			if run.State.Process.PID != 0 && !run.State.ProcessDone {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := stopGroup(ctx, run.State); err != nil {
					t.Errorf("test process cleanup: %v", err)
				}
				cancel()
			}
		}
		d.mu.Unlock()
	})
}

func TestEphemeralJobsNeverReuseExecutionDirectories(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 0.4\nexit 0\n")
	ids := map[string]bool{}
	for job := 0; job < 2; job++ {
		d.targets["build"].Assigned = 1
		var state *RunState
		pumpUntil(t, d, func() bool {
			for _, run := range d.runs {
				if run.State.Ready {
					state = run.State
					return true
				}
			}
			return false
		})
		if ids[state.RunID] {
			t.Fatal("execution was reused")
		}
		ids[state.RunID] = true
		d.mu.Lock()
		d.targets["build"].Assigned = 0
		state.Busy, state.Phase = true, "busy"
		d.save(d.runs[state.RunID])
		d.mu.Unlock()
		pumpUntil(t, d, func() bool { return len(d.runs) == 0 })
		if _, err := os.Stat(filepath.Dir(state.RunnerDir)); !os.IsNotExist(err) {
			t.Fatal("completed execution was not cleaned")
		}
		if _, err := os.Stat(filepath.Join(d.config.Cleanup.LogRoot, "runs", state.RunID, "output.log")); err != nil {
			t.Fatal("ephemeral runner logs missing", err)
		}
	}
	if d.occupied() != 0 {
		t.Fatal("minRunners=0 left occupied slots")
	}
}

func TestSharedCapacityYieldsIdleButNeverBusyRunners(t *testing.T) {
	d, api := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 30\n")
	d.targets["build"].Config.Scaling.MinRunners = 2
	otherConfig := d.targets["build"].Config
	otherConfig.ID, otherConfig.ScaleSetName = "other", "other"
	otherConfig.Scaling.MinRunners = 0
	otherAPI := newFakeAPI()
	otherAPI.set.ID = 20
	otherAPI.set.Name = "other"
	d.targets["other"] = &targetRuntime{Config: otherConfig, Client: otherAPI, Set: otherAPI.set}
	d.order = append(d.order, "other")
	pumpUntil(t, d, func() bool {
		ready := 0
		for _, run := range d.runs {
			if run.State.Ready {
				ready++
			}
		}
		return ready == 2
	})
	var busyID string
	d.mu.Lock()
	for _, run := range d.runs {
		run.State.Busy, run.State.Phase = true, "busy"
		busyID = run.State.RunID
		break
	}
	d.targets["build"].Assigned = 1
	d.targets["other"].Assigned = 1
	d.mu.Unlock()
	pumpUntil(t, d, func() bool {
		if d.occupied() > 2 {
			t.Fatal("global capacity exceeded")
		}
		for _, run := range d.runs {
			if run.State.TargetID == "other" && run.State.Ready {
				return true
			}
		}
		return false
	})
	if d.runs[busyID] == nil || !d.runs[busyID].State.Busy || d.active("build") != 1 || d.active("other") != 1 {
		t.Fatal("capacity yield stopped a busy runner or failed to transfer exactly one slot")
	}
	if api.removeCalls != 1 {
		t.Fatal("capacity transfer did not arbitrate deletion at GitHub")
	}
}

func TestAssignmentRacePreventsIdleReclamation(t *testing.T) {
	d, api := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 30\n")
	d.targets["build"].Config.Scaling.MinRunners = 1
	pumpUntil(t, d, func() bool {
		for _, run := range d.runs {
			return run.State.Ready
		}
		return false
	})
	api.removeErr = scaleset.JobStillRunningError
	for _, run := range d.runs {
		if err := d.retireIdle(run); !errors.Is(err, scaleset.JobStillRunningError) {
			t.Fatal("assignment race was not surfaced", err)
		}
		if !run.State.Busy || run.State.ProcessDone || d.occupied() != 1 {
			t.Fatal("assigned runner was stopped or capacity prematurely released")
		}
	}
}

func TestDrainStartsPreviouslyAcceptedBacklogWithinCapacity(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	d.config.Capacity.MaxRunners = 1
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 0.4\n")
	target := d.targets["build"]
	target.Assigned = 2
	d.beginDrain(target)
	seen := map[string]bool{}
	for remaining := 1; remaining >= 0; remaining-- {
		var run *activeRun
		pumpUntil(t, d, func() bool {
			if d.occupied() > 1 {
				t.Fatal("drain exceeded shared capacity")
			}
			for _, candidate := range d.runs {
				if candidate.State.Ready && !seen[candidate.State.RunID] {
					run = candidate
					return true
				}
			}
			return false
		})
		seen[run.State.RunID] = true
		d.mu.Lock()
		run.State.Busy, run.State.Phase = true, "busy"
		target.Assigned = remaining
		d.mu.Unlock()
		pumpUntil(t, d, func() bool { return d.runs[run.State.RunID] == nil })
	}
	if !d.drained("build") || len(seen) != 2 {
		t.Fatal("drain skipped previously accepted unstarted jobs")
	}
}

func TestStartupTimeoutIsMeasuredAfterProcessStart(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nsleep 30\n")
	d.targets["build"].Assigned = 1
	var state *RunState
	pumpUntil(t, d, func() bool {
		for _, run := range d.runs {
			if run.State.Process.PID > 0 && !run.Preparing {
				state = run.State
				return true
			}
		}
		return false
	})
	if state.StartedAt.IsZero() || state.Ready {
		t.Fatal("startup clock or readiness is invalid")
	}
	d.mu.Lock()
	state.StartedAt = time.Now().Add(-3 * time.Minute)
	d.reconcile()
	if d.runs[state.RunID] != nil || d.targets["build"].Failures != 1 {
		status := d.statusLocked()
		d.mu.Unlock()
		t.Fatalf("startup timeout did not stop/reap and back off: status=%+v", status)
	}
	d.mu.Unlock()
}

func TestDrainTimeoutForceStopsButIsNotSuccess(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 30\n")
	target := d.targets["build"]
	target.Assigned = 1
	pumpUntil(t, d, func() bool {
		for _, run := range d.runs {
			if run.State.Ready {
				run.State.Busy, run.State.Phase = true, "busy"
				return true
			}
		}
		return false
	})
	d.mu.Lock()
	d.beginDrain(target)
	target.DrainStarted = time.Now().Add(-time.Hour)
	d.reconcile()
	d.mu.Unlock()
	if !target.Forced || !d.drained("build") || d.occupied() != 0 || len(d.statusLocked().Problems) == 0 {
		t.Fatal("forced drain was not distinguished from normal completion")
	}
	response := d.drain(t.Context(), "build")
	if response.Error == "" {
		t.Fatal("forced drain returned success")
	}
}

func TestFinitePreparationFailuresFaultOnlyAffectedTarget(t *testing.T) {
	d, _ := testDaemon(t)
	other := *d.targets["build"]
	other.Config.ID = "other"
	d.targets["other"] = &other
	d.order = append(d.order, "other")
	for range maxAttempts {
		d.startFailure(d.targets["build"], errTestAPI)
	}
	if d.canStart(d.targets["build"]) || !d.canStart(d.targets["other"]) {
		t.Fatal("target failure was hidden or affected an unrelated target")
	}
	state := testState(t, d.config)
	state.ProcessDone = true
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	// A real state write failure must not turn into an unrecorded launch.
	os.WriteFile(d.config.Runner.WorkRoot+"/build", []byte("not a directory"), 0600)
	if err := d.save(run); err == nil || !run.Uncertain || d.occupied() != 1 {
		t.Fatal("state failure did not retain a conservative reservation")
	}
}

func TestRecoveryKeepsUnmatchedAndCorruptStateConservative(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ScopeURL = "https://github.com/old/repository"
	d.store.save(state)
	brokenID, _ := newID()
	broken := filepath.Join(d.config.Runner.WorkRoot, "build", brokenID)
	safeDirectory(broken, 0700)
	os.WriteFile(filepath.Join(broken, "state.json"), []byte("{"), 0600)
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.occupied() != 2 || !d.runs[state.RunID].Uncertain || len(d.statusLocked().Problems) < 2 {
		t.Fatal("unknown state did not reserve capacity and produce diagnostics")
	}
	response := d.gc(t.Context(), false)
	if response.Error == "" {
		t.Fatal("gc silently removed uncertain state")
	}
	if _, err := os.Stat(broken); err != nil {
		t.Fatal("corrupt recovery record was deleted")
	}
}

func TestRecoveredStoppedCleanupIsNotCountedTwice(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ProcessDone, state.Phase = true, "cleaning"
	d.store.save(state)
	d.offline = true
	before, _ := os.ReadFile(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
	if string(before) != string(after) {
		t.Fatal("offline recovery mutated state before a dry-run")
	}
	if d.occupied() != 0 {
		t.Fatal("file-only cleanup consumed a recovered capacity slot")
	}
	if response := d.gc(t.Context(), true); response.Error != "" {
		t.Fatal(response.Error)
	}
	if _, err := os.Stat(filepath.Dir(state.RunnerDir)); err != nil {
		t.Fatal("dry-run removed the execution")
	}
	if response := d.gc(t.Context(), false); response.Error != "" || len(d.runs) != 0 {
		t.Fatal("cleanup did not resume", response.Error)
	}
}

func TestExecutorRediscoversReparentedRunner(t *testing.T) {
	d, api := testDaemon(t)
	state := testState(t, d.config)
	d.store.save(state)
	safeDirectory(filepath.Join(state.RunnerDir, "bin"), 0700)
	os.WriteFile(filepath.Join(state.RunnerDir, "bin", "Runner.Listener"), []byte("#!/bin/sh\nsleep 30\n"), 0700)
	dataPath := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
	command := exec.Command(os.Args[0], "-test.run=^TestReparentedProcessHelper$")
	command.Env = append(os.Environ(), "GHALET_PROCESS_HELPER="+dataPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
	api.runners[123] = &scaleset.RunnerReference{ID: 123, Name: state.RunnerName, RunnerScaleSetID: 10}
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	cleanupProcesses(t, d)
	run := d.runs[state.RunID]
	if run == nil || run.Uncertain || run.State.ProcessDone || d.occupied() != 1 || run.WaitDone != nil {
		t.Fatalf("reparented runner not safely adopted: %+v", run)
	}
	d.mu.Lock()
	err := d.stop(run, true)
	d.mu.Unlock()
	if err != nil || d.occupied() != 0 {
		t.Fatal("reparented runner could not be stopped and cleaned", err)
	}
}

func TestReparentedProcessHelper(t *testing.T) {
	path := os.Getenv("GHALET_PROCESS_HELPER")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state RunState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	account, err := resolveUser("")
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(state.RunnerDir, "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(state.RunnerDir, "bin", "Runner.Listener"), "run")
	command.Dir, command.SysProcAttr, command.Stdout, command.Stderr = state.RunnerDir, processAttributes(account), output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	table, err := processTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Process = table[command.Process.Pid].ProcessIdentity
	state.Members = []ProcessIdentity{state.Process}
	state.RunnerID, state.Phase, state.Ready = 123, "idle", true
	state.StartedAt = time.Now()
	store := stateStore{strings.TrimSuffix(filepath.Dir(filepath.Dir(filepath.Dir(path))), "/")}
	if err := store.save(&state); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
