package ghalet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Songmu/ghalet/fshelper"
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
				if err := stopGroup(ctx, run.State, func() error { return d.store.save(run.State) }); err != nil {
					t.Errorf("test process cleanup: %v", err)
				}
				cancel()
			}
			if run.WaitDone != nil {
				select {
				case <-run.WaitDone:
				case <-time.After(10 * time.Second):
					t.Error("test runner output collection did not finish")
				}
			}
		}
		d.mu.Unlock()
	})
}

func TestCanRetireIdle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*activeRun)
		want   bool
	}{
		{"idle", func(*activeRun) {}, true},
		{"not ready", func(r *activeRun) { r.State.Ready = false }, false},
		{"busy", func(r *activeRun) { r.State.Busy = true }, false},
		{"process done", func(r *activeRun) { r.State.ProcessDone = true }, false},
		{"uncertain", func(r *activeRun) { r.Uncertain = true }, false},
		{"preparing", func(r *activeRun) { r.Preparing = true }, false},
		{"inspection failed", func(r *activeRun) { r.InspectFailures = 1 }, false},
		{"exiting", func(r *activeRun) { r.State.Phase = "exiting" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &activeRun{State: &RunState{Ready: true, Phase: "idle"}}
			tc.change(run)
			if got := run.canRetireIdle(); got != tc.want {
				t.Fatalf("canRetireIdle() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileInspectsOnlyMonitoredRuns(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preparing   bool
		processDone bool
		uncertain   bool
		wantInspect bool
	}{
		{name: "empty"},
		{name: "preparing", preparing: true},
		{name: "completed", processDone: true},
		{name: "live", wantInspect: true},
		{name: "uncertain", uncertain: true, wantInspect: true},
		{name: "uncertain completed", processDone: true, uncertain: true, wantInspect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := testDaemon(t)
			if tc.name != "empty" {
				state := testState(t, d.config)
				state.ProcessDone = tc.processDone
				d.runs[state.RunID] = &activeRun{
					State: state, Preparing: tc.preparing, Uncertain: tc.uncertain, CleanupTried: true,
				}
			}
			dir := testDirectory(t)
			marker := filepath.Join(dir, "inspected")
			t.Setenv("GHALET_TEST_PS_MARKER", marker)
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nprintf inspected > \"$GHALET_TEST_PS_MARKER\"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			d.nextRetention = time.Now().Add(-time.Hour)
			before := d.nextRetention
			d.reconcile()
			_, err := os.Stat(marker)
			if tc.wantInspect {
				if err != nil || !d.nextRetention.Equal(before) {
					t.Fatal("failed process inspection did not suspend reconciliation", err)
				}
			} else if !os.IsNotExist(err) || d.nextRetention.Equal(before) {
				t.Fatal("reconciliation inspected processes or skipped maintenance without monitored runs", err)
			}
		})
	}
}

func TestReconcileSchedulesFirstRunWithoutProcessInspection(t *testing.T) {
	d, api := testDaemon(t)
	seedDistribution(t, d.config, "#!/bin/sh\nexit 0\n")
	api.jitErr = errTestAPI // Stop preparation before the independent launch-time inspection.
	dir := testDirectory(t)
	marker := filepath.Join(dir, "inspected")
	t.Setenv("GHALET_TEST_PS_MARKER", marker)
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nprintf inspected > \"$GHALET_TEST_PS_MARKER\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	d.targets["build"].Assigned = 1
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reconcile()
	if len(d.runs) != 1 || d.active("build") != 1 {
		t.Fatal("idle process inspection failure prevented the first reservation")
	}
	for _, run := range d.runs {
		if !run.Preparing {
			t.Fatal("first run was not scheduled for preparation")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("scheduling the first run inspected the process table", err)
	}
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
		if _, err := os.Stat(filepath.Join(d.config.Logs.Root, "runs", state.RunID, "output.log")); err != nil {
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

func TestShutdownWaitsForUncertainLiveRunnerAfterDrainDeadline(t *testing.T) {
	d, _ := testDaemon(t)
	var done chan error
	t.Cleanup(func() {
		if done != nil {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("shutdown did not observe test process cleanup")
			}
		}
	})
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 30\n")
	d.config.Runner.DrainTimeout = Duration(50 * time.Millisecond)
	d.targets["build"].Assigned = 1
	var run *activeRun
	pumpUntil(t, d, func() bool {
		for _, candidate := range d.runs {
			if candidate.State.Ready && !candidate.Preparing {
				run = candidate
				return true
			}
		}
		return false
	})
	d.mu.Lock()
	run.Uncertain, run.State.Error = true, "identity requires manual inspection"
	run.State.Busy, run.State.Phase = true, "busy"
	d.targets["build"].Assigned = 0
	d.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done = make(chan error, 1)
	go func() {
		defer close(done)
		done <- d.serve(ctx)
	}()
	select {
	case err := <-done:
		t.Fatal("shutdown abandoned an uncertain live runner", err)
	case <-time.After(time.Second):
	}
	response := d.handle(t.Context(), controlRequest{Command: "doctor"})
	if response.Error == "" || len(response.Status.Targets) != 1 || !response.Status.Targets[0].Draining ||
		!response.Status.Targets[0].Forced {
		t.Fatal("shutdown lost diagnostics/control access or did not reach its drain deadline", response)
	}
	table, err := processTable(t.Context(), []ProcessIdentity{run.State.Process})
	if err != nil {
		t.Fatal(err)
	}
	if alive, _, err := inspectGroup(run.State, table); err != nil || !alive {
		t.Fatal("shutdown stopped the uncertain runner", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(run.State.RunnerDir), "state.json")); err != nil {
		t.Fatal("shutdown removed the recovery record", err)
	}
	d.mu.Lock()
	err = stopGroup(t.Context(), run.State, func() error { return d.store.save(run.State) })
	d.mu.Unlock()
	if err != nil {
		t.Fatal("test could not stop its verified runner", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unresolved diagnostics") {
			t.Fatal("shutdown hid the retained uncertain execution", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not observe the uncertain group's disappearance")
	}
	if !run.Uncertain || !run.GroupStopped || !run.Exited || !run.outputCollected() {
		t.Fatal("shutdown skipped group, exit or output confirmation, or cleared uncertainty")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(run.State.RunnerDir), "state.json")); err != nil {
		t.Fatal("group disappearance authorized deletion of uncertain state", err)
	}
}

func TestDrainedRequiresUncertainGroupAndObserverConfirmation(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ProcessDone = true
	run := &activeRun{State: state, Uncertain: true, Stopping: true, WaitDone: make(chan struct{})}
	d.runs[state.RunID] = run
	if d.drained("") {
		t.Fatal("persisted stopped state bypassed uncertain group verification")
	}
	d.reconcile()
	if !run.GroupStopped || d.drained("") {
		t.Fatal("group absence bypassed exit/output observers")
	}
	if d.occupied() != 1 {
		t.Fatal("group absence released capacity before exit/output observers")
	}
	close(run.WaitDone)
	if d.drained("") {
		t.Fatal("collected output bypassed local exit observation")
	}
	d.processEvent(runEvent{id: state.RunID, kind: "exit"})
	if !d.drained("") || !run.Uncertain || len(d.statusLocked().Problems) == 0 {
		t.Fatal("confirmed stopped execution blocked shutdown or lost its diagnostics")
	}
	if d.occupied() != 0 || d.active("build") != 0 {
		t.Fatal("confirmed stopped execution retained capacity")
	}
	run.WaitDone = nil
	run.Exited = false
	if !d.drained("") {
		t.Fatal("recovered execution required a nonexistent local exit observer")
	}
	state.Process = ProcessIdentity{PID: 2, PGID: 3, UID: state.UID}
	d.reconcile()
	if run.GroupStopped || d.drained("") {
		t.Fatal("failed process inspection retained a stale absence confirmation")
	}
}

func TestRunnerExitStopsObservedDescendantsBeforeCollectingOutput(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	release := filepath.Join(testDirectory(t), "release")
	if err := syscall.Mkfifo(release, 0600); err != nil {
		t.Fatal(err)
	}
	seedDistribution(t, d.config, "#!/bin/sh\nsleep 30 &\nprintf 'Listening for Jobs\\n'\nread -r release < '"+release+"'\nprintf 'final-secret-tail'\nexit 0\n")
	d.targets["build"].Assigned = 1
	var run *activeRun
	pumpUntil(t, d, func() bool {
		for _, candidate := range d.runs {
			if candidate.State.Ready && !candidate.Preparing && len(candidate.State.Members) >= 2 {
				run = candidate
				return true
			}
		}
		return false
	})
	d.mu.Lock()
	run.State.Busy, run.State.Phase = true, "busy"
	d.targets["build"].Assigned = 0
	if err := d.save(run); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	d.secrets.Add("final-secret")
	gate, err := os.OpenFile(release, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := gate.WriteString("exit\n")
	if err := errors.Join(writeErr, gate.Close()); err != nil {
		t.Fatal(err)
	}
	pumpUntil(t, d, func() bool { return d.runs[run.State.RunID] == nil })
	if !run.Exited || !run.outputCollected() || d.occupied() != 0 || d.targets["build"].Forced {
		t.Fatal("runner exit retained descendants/output or required forced drain")
	}
	data, err := os.ReadFile(filepath.Join(d.config.Logs.Root, "runs", run.State.RunID, "output.log"))
	if err != nil || string(data) != "Listening for Jobs\n***-tail" {
		t.Fatalf("final partial output was not flushed and redacted: %q (%v)", data, err)
	}
}

func TestExitClassificationWaitsForCollectedReadiness(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ready       bool
		exitErr     error
		stopping    bool
		wantFailure bool
	}{
		{name: "late readiness", ready: true},
		{name: "no readiness", wantFailure: true},
		{name: "failed after readiness", ready: true, exitErr: errTestAPI, wantFailure: true},
		{name: "intentional stop", stopping: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := testDaemon(t)
			target := d.targets["build"]
			target.Failures = maxAttempts - 1
			state := testState(t, d.config)
			state.Phase = "registered"
			if err := d.store.save(state); err != nil {
				t.Fatal(err)
			}
			run := &activeRun{State: state, WaitDone: make(chan struct{}), Stopping: tc.stopping}
			d.runs[state.RunID] = run
			d.processEvent(runEvent{id: state.RunID, kind: "exit", err: tc.exitErr})
			if !run.Exited || run.ExitResult == nil || target.Failures != maxAttempts-1 || target.Fault != "" {
				t.Fatal("direct exit was not observed or was classified before output collection")
			}
			run.OutputReady = tc.ready
			close(run.WaitDone)
			d.reconcile()
			if d.runs[state.RunID] != nil || run.ExitResult != nil {
				t.Fatal("collected exit did not finish classification and cleanup")
			}
			wantFailures := 0
			if tc.wantFailure {
				wantFailures = maxAttempts
			} else if tc.stopping {
				wantFailures = maxAttempts - 1
			}
			if target.Failures != wantFailures || (target.Fault != "") != tc.wantFailure {
				t.Fatal("incorrect readiness-dependent exit classification", target.Failures, target.Fault)
			}
			d.processEvent(runEvent{id: state.RunID, kind: "ready"})
			if target.Failures != wantFailures {
				t.Fatal("late ready event changed an already classified result")
			}
		})
	}
}

func TestCompletedTargetDrainDoesNotExpire(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	target := d.targets["build"]
	d.beginDrain(target)
	target.DrainStarted = time.Now().Add(-time.Hour)
	d.reconcile()
	if target.Forced || len(d.statusLocked().Problems) != 0 {
		t.Fatal("a completed drain became a forced-drain failure")
	}
	if response := d.drain(t.Context(), "build"); response.Error != "" {
		t.Fatal(response.Error)
	}
	if err := d.deleteRetiredScaleSet(t.Context(), target); err != nil || api.deleteCalls != 1 {
		t.Fatal("completed drain prevented later scale set deletion", err)
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
	state.Phase = "launching"
	d.store.save(state)
	brokenID, _ := newID()
	broken := filepath.Join(d.config.Runner.WorkRoot, "build", brokenID)
	fshelper.EnsureDirectory(broken, 0700)
	os.WriteFile(filepath.Join(broken, "state.json"), []byte("{"), 0600)
	invalid := testState(t, d.config)
	invalid.Schema, invalid.ProcessDone = 2, true
	if err := d.store.save(invalid); err != nil {
		t.Fatal(err)
	}
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.occupied() != 3 || !d.runs[state.RunID].Uncertain || len(d.statusLocked().Problems) < 3 {
		t.Fatal("unknown state did not reserve capacity and produce diagnostics")
	}
	session := &fakeSession{}
	scaler := &targetScaler{daemon: d, target: d.targets["build"], session: session}
	if err := scaler.Scale(t.Context(), availableMessage(1, 5)); err != nil || len(session.acquired) != 0 {
		t.Fatal("malformed records did not retain acquisition capacity", err)
	}
	response := d.gc(t.Context(), false)
	if response.Error == "" {
		t.Fatal("gc silently removed uncertain state")
	}
	if _, err := os.Stat(broken); err != nil {
		t.Fatal("corrupt recovery record was deleted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(invalid.RunnerDir), "state.json")); err != nil {
		t.Fatal("invalid record's stopped marker authorized deletion")
	}
}

func TestRecoveredStoppedRunnerLookupFailureReleasesCapacity(t *testing.T) {
	d, api := testDaemon(t)
	state := testState(t, d.config)
	state.ProcessDone, state.Phase = true, "cleaning"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	api.getErr = scaleset.UnauthorizedError
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.reconcile()
	run := d.runs[state.RunID]
	if run == nil || !run.CleanupTried || run.State.Error == "" || run.Uncertain ||
		d.occupied() != 0 || d.active("build") != 0 || !d.drained("") {
		t.Fatal("remote cleanup failure retained execution capacity or lost diagnostics")
	}
	if api.removeCalls != 0 {
		t.Fatal("lookup failure authorized runner deletion")
	}
	if response := d.gc(t.Context(), false); response.Error == "" || d.runs[state.RunID] == nil || d.occupied() != 0 {
		t.Fatal("gc hid remote cleanup failure or retained its capacity", response)
	}
}

func TestStoppedStatePersistenceFailureDoesNotRestoreCapacityReservation(t *testing.T) {
	for _, operation := range []string{"reconcile", "gc", "stop"} {
		t.Run(operation, func(t *testing.T) {
			d, _ := testDaemon(t)
			state := testState(t, d.config)
			state.Phase = "registered"
			// Force the absence observation itself to need a state update.
			state.Members = []ProcessIdentity{{PID: 2}}
			run := &activeRun{State: state}
			d.runs[state.RunID] = run
			statePath := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
			if err := os.MkdirAll(statePath, 0700); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "reconcile":
				d.reconcileRun(run, map[int]processEntry{})
			case "gc":
				if response := d.gc(t.Context(), false); response.Error == "" {
					t.Fatal("gc hid a state persistence failure")
				}
			case "stop":
				if err := d.stop(run, false); err == nil {
					t.Fatal("stop hid a state persistence failure")
				}
			}
			if !run.Uncertain || !run.GroupStopped || run.State.Error == "" ||
				d.occupied() != 0 || d.active("build") != 0 || !d.drained("") {
				t.Fatal("persistence failure restored a verified stopped reservation or lost diagnostics")
			}
			if err := d.cleanup(run); err == nil {
				t.Fatal("persistence failure authorized cleanup")
			}
		})
	}
}

func TestStoppedRecoveryAllowsUnrelatedRunnerLaunch(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 30\n")
	state := testState(t, d.config)
	state.ProcessDone, state.Phase = true, "cleaning"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	delete(d.targets, "build")
	otherAPI := newFakeAPI()
	other := &targetRuntime{Config: d.config.Targets[0], Client: otherAPI, Set: otherAPI.set, Assigned: 1}
	other.Config.ID = "other"
	d.targets["other"], d.order = other, []string{"other"}
	d.config.Capacity.MaxRunners = 1
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	pumpUntil(t, d, func() bool {
		for _, run := range d.runs {
			if run.State.TargetID == "other" && run.State.Ready && !run.Preparing {
				return true
			}
		}
		return false
	})
	if d.occupied() != 1 || d.active("other") != 1 || d.active("build") != 0 || d.runs[state.RunID] == nil {
		t.Fatal("unrelated launch lost retained state or miscounted shared capacity")
	}
}

func TestRecoveryStoppedDiagnosticsDoNotReserveCapacity(t *testing.T) {
	for _, stopped := range []string{"persisted stopped", "native absence"} {
		for _, mismatch := range []string{"removed target", "authentication", "registration", "execution user"} {
			t.Run(stopped+"/"+mismatch, func(t *testing.T) {
				d, api := testDaemon(t)
				state := testState(t, d.config)
				if stopped == "persisted stopped" {
					state.ProcessDone, state.Phase = true, "cleaning"
				} else {
					command := exec.Command("/bin/sh", "-c", "read -r release")
					input, release, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					defer input.Close()
					defer release.Close()
					command.Stdin = input
					command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
					if err := command.Start(); err != nil {
						t.Fatal(err)
					}
					start, err := kernelProcessStart(command.Process.Pid)
					killErr := command.Process.Kill()
					command.Wait()
					if err != nil || killErr != nil {
						t.Fatal("cannot establish exited process identity", err, killErr)
					}
					state.Process = ProcessIdentity{PID: command.Process.Pid, PGID: command.Process.Pid, UID: state.UID, Started: start}
					state.Phase = "busy"
				}
				if err := d.store.save(state); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				other := *d.targets["build"]
				other.Config.ID = "other"
				d.targets["other"] = &other
				d.order = append(d.order, "other")
				switch mismatch {
				case "removed target":
					delete(d.targets, "build")
					d.order = []string{"other"}
				case "authentication":
					d.targets["build"].Fault = "authentication failed"
					d.targets["build"].Set = nil
				case "registration":
					d.targets["build"].Config.ScaleSetName = "replacement"
				case "execution user":
					d.user.Home += "/changed"
				}
				if err := d.recover(t.Context()); err != nil {
					t.Fatal(err)
				}
				run := d.runs[state.RunID]
				if run == nil || !run.Uncertain || !run.GroupStopped || !run.State.ProcessDone {
					t.Fatal("stopped process and cleanup uncertainty were not distinguished")
				}
				d.reconcileRun(run, map[int]processEntry{})
				status := d.statusLocked()
				if status.Occupied != 0 || d.active("build") != 0 || len(status.Problems) == 0 || !d.canStart(&other) {
					t.Fatal("stopped diagnostic blocked unrelated capacity or lost diagnostics", status)
				}
				session := &fakeSession{}
				scaler := &targetScaler{daemon: d, target: &other, session: session}
				if err := scaler.Scale(t.Context(), availableMessage(1, 5)); err != nil || len(session.acquired) != d.config.Capacity.MaxRunners {
					t.Fatal("stopped diagnostic reduced acquisition capacity", err, session.acquired)
				}
				other.Pending = 0
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if response := d.drain(ctx, ""); response.Error == "" || ctx.Err() != nil || !d.drained("") {
					t.Fatal("stopped diagnostic blocked drain completion or hid incomplete cleanup", response)
				}
				for _, dryRun := range []bool{true, false} {
					if response := d.gc(t.Context(), dryRun); response.Error == "" {
						t.Fatal("gc authorized cleanup of an unmatched execution")
					}
				}
				after, err := os.ReadFile(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
				if err != nil || string(before) != string(after) || api.removeCalls != 0 {
					t.Fatal("unmatched recovery mutated records or removed a registration", err)
				}
			})
		}
	}
}

func TestRecoveryPotentiallyLiveDiagnosticsKeepCapacity(t *testing.T) {
	for _, identity := range []string{"live", "reused PID", "stopped marker with reused PID"} {
		t.Run(identity, func(t *testing.T) {
			d, api := testDaemon(t)
			state := testState(t, d.config)
			if err := os.MkdirAll(filepath.Join(state.RunnerDir, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state.RunnerDir, "bin", "Runner.Listener")
			if err := os.WriteFile(path, []byte("#!/bin/sh\nread -r release\n"), 0700); err != nil {
				t.Fatal(err)
			}
			input, release, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer release.Close()
			command := exec.Command(path)
			command.Stdin = input
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { command.Process.Kill(); command.Wait() })
			start, err := kernelProcessStart(command.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			state.Process = ProcessIdentity{PID: command.Process.Pid, PGID: command.Process.Pid, UID: state.UID, Started: start}
			state.Phase = "busy"
			state.ScopeURL = "https://github.com/old/repository"
			if identity != "live" {
				state.Process.Started += "-old"
			}
			state.ProcessDone = identity == "stopped marker with reused PID"
			if err := d.store.save(state); err != nil {
				t.Fatal(err)
			}
			if err := d.recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			run := d.runs[state.RunID]
			if run == nil || !run.Uncertain || run.GroupStopped || d.occupied() != 1 || d.active("build") != 1 || d.drained("") {
				t.Fatal("potentially live execution did not retain capacity and block drain")
			}
			scaler := &targetScaler{daemon: d, target: d.targets["build"], session: &fakeSession{}}
			if err := scaler.Scale(t.Context(), availableMessage(1, 5)); err != nil || d.targets["build"].Pending != d.config.Capacity.MaxRunners-1 {
				t.Fatal("scaler ignored a potentially live reservation", err)
			}
			if response := d.gc(t.Context(), false); response.Error == "" || api.removeCalls != 0 {
				t.Fatal("gc authorized cleanup of a potentially live execution")
			}
			if err := command.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatal("recovery or gc signaled the uncertain process", err)
			}
		})
	}
}

func TestRecoveryAcceptsLegacyScopeRefWithoutUsingIt(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ScopeRef = "removed-scope"
	state.ProcessDone, state.Phase = true, "cleaning"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	d.offline = true
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := d.runs[state.RunID]
	if run == nil || run.Uncertain || len(d.problems) != 0 {
		t.Fatal("legacy scope name prevented recovery of the matching registration")
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
	for _, missingCompletion := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit stop", true: "missed completion"}[missingCompletion], func(t *testing.T) {
			testRecoveredRunner(t, missingCompletion)
		})
	}
}

func testRecoveredRunner(t *testing.T, missingCompletion bool) {
	t.Helper()
	d, api := testDaemon(t)
	state := testState(t, d.config)
	d.store.save(state)
	fshelper.EnsureDirectory(filepath.Join(state.RunnerDir, "bin"), 0700)
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
	if missingCompletion {
		run.State.Busy, run.State.Phase = true, "busy"
		if err := d.save(run); err != nil {
			t.Fatal(err)
		}
		if err := stopGroup(t.Context(), run.State, func() error { return d.store.save(run.State) }); err != nil {
			t.Fatal(err)
		}
		d.reconcile()
		if !run.Exited || run.ExitError != "" || d.runs[state.RunID] != nil || d.occupied() != 0 {
			t.Fatal("missing completion retained a recovered busy execution or invented an exit result")
		}
	} else {
		d.mu.Lock()
		err := d.stop(run, true)
		d.mu.Unlock()
		if err != nil || d.occupied() != 0 {
			t.Fatal("reparented runner could not be stopped and cleaned", err)
		}
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
	table, err := processTable(t.Context(), []ProcessIdentity{{PID: command.Process.Pid, PGID: command.Process.Pid}})
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

func TestReservationRequiresSyncedRunParent(t *testing.T) {
	d, _ := testDaemon(t)
	parent := filepath.Join(d.config.Runner.WorkRoot, "build")
	if err := ensureExecutionDirectory(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0755); err != nil {
			t.Error(err)
		}
	})
	if err := d.reserve(d.targets["build"]); !errors.Is(err, os.ErrPermission) {
		t.Fatal("reservation skipped syncing the newly created run entry", err)
	}
	if len(d.runs) != 0 {
		t.Fatal("failed run-parent sync started preparation")
	}
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed reservation retained an unsynced run directory", entries, err)
	}
}
