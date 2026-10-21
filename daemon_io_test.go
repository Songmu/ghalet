package ghalet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type blockedAcquisition struct {
	fakeSession
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	err     error
}

func releaseOnCleanup(t *testing.T, release chan struct{}) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
}

func (b *blockedAcquisition) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	b.calls.Add(1)
	close(b.entered)
	select {
	case <-b.release:
		return ids, b.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func responsiveStatus(t *testing.T, d *daemon) Status {
	t.Helper()
	result := make(chan controlResponse, 1)
	go func() { result <- d.handle(t.Context(), controlRequest{Command: "status"}) }()
	select {
	case response := <-result:
		return *response.Status
	case <-time.After(time.Second):
		t.Fatal("slow execution I/O blocked status")
		return Status{}
	}
}

func TestBlockedAcquisitionReservesBeforeIOAndNeverReplays(t *testing.T) {
	for _, acquireErr := range []error{nil, errTestAPI} {
		t.Run(map[bool]string{true: "ambiguous", false: "accepted"}[acquireErr != nil], func(t *testing.T) {
			d, _ := testDaemon(t)
			first := d.targets["build"]
			second := *first
			second.Config.ID = "second"
			d.targets["second"] = &second
			d.order = append(d.order, "second")
			session := &blockedAcquisition{entered: make(chan struct{}), release: make(chan struct{}), err: acquireErr}
			releaseOnCleanup(t, session.release)
			scaler := &targetScaler{daemon: d, target: first, session: session}
			message := availableMessage(7, 1)
			result := make(chan error, 1)
			go func() { result <- scaler.Scale(t.Context(), message) }()
			<-session.entered
			if status := responsiveStatus(t, d); status.Targets[0].Pending != 1 {
				t.Fatal("in-flight acquisition did not reserve shared capacity", status)
			}
			other := &fakeSession{}
			if err := (&targetScaler{daemon: d, target: &second, session: other}).Scale(t.Context(), availableMessage(8, 5)); err != nil || len(other.acquired) != 1 {
				t.Fatal("blocked acquisition stalled or oversubscribed another target", err)
			}
			d.mu.Lock()
			d.beginDrain(first)
			first.Forced = true
			drained := d.drained(first.Config.ID)
			d.mu.Unlock()
			if drained {
				t.Fatal("forced drain bypassed an acquisition still in flight")
			}
			close(session.release)
			if err := <-result; !errors.Is(err, acquireErr) {
				t.Fatal(err)
			}
			if err := scaler.Scale(t.Context(), message); err != nil || session.calls.Load() != 1 {
				t.Fatal("redelivery repeated accepted or ambiguous acquisition", err)
			}
		})
	}
}

type blockedRunnerAPI struct {
	scaleAPI
	jitEntered    chan struct{}
	jitRelease    chan struct{}
	removeEntered chan struct{}
	removeRelease chan struct{}
}

func (b *blockedRunnerAPI) GenerateJitRunnerConfig(ctx context.Context, settings *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if b.jitEntered != nil {
		close(b.jitEntered)
		// Deliberately model a server accepting the request despite cancellation.
		<-b.jitRelease
	}
	return b.scaleAPI.GenerateJitRunnerConfig(ctx, settings, id)
}

func (b *blockedRunnerAPI) RemoveRunner(ctx context.Context, id int64) error {
	if b.removeEntered != nil {
		close(b.removeEntered)
		<-b.removeRelease
	}
	return b.scaleAPI.RemoveRunner(ctx, id)
}

func TestBlockedJITAllowsOtherTargetAndCanceledPreparationCannotLaunch(t *testing.T) {
	d, api := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 30\n")
	blocked := &blockedRunnerAPI{scaleAPI: api, jitEntered: make(chan struct{}), jitRelease: make(chan struct{})}
	releaseOnCleanup(t, blocked.jitRelease)
	first := d.targets["build"]
	first.Client, first.Assigned = blocked, 1
	reconcileAndWaitOther := func() {
		d.mu.Lock()
		d.reconcile()
		d.mu.Unlock()
	}
	reconcileAndWaitOther()
	<-blocked.jitEntered
	if status := responsiveStatus(t, d); status.Occupied != 1 || len(status.Runs) != 1 || status.Runs[0].Phase != "preparing" {
		t.Fatal("preparation did not expose a coherent reservation", status)
	}
	secondAPI := newFakeAPI()
	second := &targetRuntime{Config: first.Config, Client: secondAPI, Set: secondAPI.set, Assigned: 1}
	second.Config.ID = "second"
	d.mu.Lock()
	d.targets["second"] = second
	d.order = append(d.order, "second")
	d.mu.Unlock()
	// Unlike pumpUntil, this intentionally leaves the first target blocked.
	deadline := time.Now().Add(5 * time.Second)
	for {
		reconcileAndWaitOther()
		status := responsiveStatus(t, d)
		if len(status.Runs) == 2 && (status.Runs[0].Process.PID > 0 || status.Runs[1].Process.PID > 0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("one target's JIT request blocked another target's launch", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.mu.Lock()
	var canceled *activeRun
	for _, run := range d.runs {
		if run.State.TargetID == first.Config.ID {
			canceled = run
			run.Cancel()
		}
	}
	d.mu.Unlock()
	close(blocked.jitRelease)
	settleRuns(d)
	if canceled.State.Process.PID != 0 {
		t.Fatal("canceled preparation launched after JIT returned")
	}
	if _, err := os.Stat(filepath.Join(canceled.State.RunnerDir, "output.log")); !os.IsNotExist(err) {
		t.Fatal("canceled preparation opened runner output", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.nextID != 101 || api.removeCalls != 1 || len(api.runners) != 0 {
		t.Fatal("preparation was duplicated or canceled registration was not cleaned")
	}
}

func TestBlockedCleanupKeepsGCResultsSynchronousAndDrainOrdered(t *testing.T) {
	d, api := testDaemon(t)
	state := testState(t, d.config)
	state.RunnerID, state.ProcessDone, state.Phase = 101, true, "cleaning"
	api.runners[101] = &scaleset.RunnerReference{ID: 101, Name: state.RunnerName, RunnerScaleSetID: state.ScaleSetID}
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}

	d.runs[state.RunID] = &activeRun{State: state}
	blocked := &blockedRunnerAPI{scaleAPI: api, removeEntered: make(chan struct{}), removeRelease: make(chan struct{})}
	releaseOnCleanup(t, blocked.removeRelease)
	d.targets["build"].Client = blocked
	result := make(chan controlResponse, 1)
	go func() { result <- d.gc(t.Context(), false) }()
	<-blocked.removeEntered
	status := responsiveStatus(t, d)
	if len(status.Runs) != 1 || status.Occupied != 0 {
		t.Fatal("cleanup snapshot hid execution or retained stopped capacity", status)
	}
	d.mu.Lock()
	for range 3 {
		d.reconcile()
	}
	drained := d.drained("")
	d.mu.Unlock()
	if drained {
		t.Fatal("drain completed while cleanup was still running")
	}
	select {
	case <-result:
		t.Fatal("GC returned before cleanup completed")
	default:
	}
	close(blocked.removeRelease)
	response := <-result
	if response.Error != "" || len(response.Results) != 1 || len(d.runs) != 0 || api.removeCalls != 1 {
		t.Fatal("GC hid results or overlapped cleanup", response)
	}
}

func TestBlockedProcessInspectionDoesNotHoldSharedLock(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	d.runs[state.RunID] = &activeRun{State: state}
	dir := testDirectory(t)
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	t.Setenv("GHALET_TEST_PS_ENTERED", entered)
	t.Setenv("GHALET_TEST_PS_RELEASE", release)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	script := "#!/bin/sh\nprintf entered > \"$GHALET_TEST_PS_ENTERED\"\nwhile [ ! -e \"$GHALET_TEST_PS_RELEASE\" ]; do sleep 0.02; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Error(err)
		}
		settleRuns(d)
	})
	d.mu.Lock()
	d.reconcile()
	d.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process inspection did not enter test gate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status := responsiveStatus(t, d); status.Occupied != 1 {
		t.Fatal("blocked inspection lost its capacity reservation", status)
	}
	session := &fakeSession{}
	if err := (&targetScaler{daemon: d, target: d.targets["build"], session: session}).Scale(t.Context(), availableMessage(1, 1)); err != nil || len(session.acquired) != 1 {
		t.Fatal("blocked inspection prevented acquisition", err)
	}
}

func TestProcessInspectionSnapshotIsSharedAcrossExecutions(t *testing.T) {
	d, _ := testDaemon(t)
	for range 2 {
		state := testState(t, d.config)
		d.runs[state.RunID] = &activeRun{State: state, Uncertain: true}
	}

	dir := testDirectory(t)
	marker := filepath.Join(dir, "scans")
	t.Setenv("GHALET_TEST_PS_SCANS", marker)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nprintf 'scan\\n' >> \"$GHALET_TEST_PS_SCANS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	reconcileAndWait(d)
	data, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(data), "scan\n") != 1 {
		t.Fatal("monitored executions did not share one process snapshot", string(data), err)
	}
}

func TestGCProcessSnapshotIsSharedAcrossExecutions(t *testing.T) {
	d, _ := testDaemon(t)
	for range 2 {
		state := testState(t, d.config)
		state.ProcessDone, state.Phase = true, "cleaning"
		if err := d.store.save(state); err != nil {
			t.Fatal(err)
		}
		d.runs[state.RunID] = &activeRun{State: state}
	}
	dir := testDirectory(t)
	marker := filepath.Join(dir, "scans")
	t.Setenv("GHALET_TEST_PS_SCANS", marker)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nprintf 'scan\\n' >> \"$GHALET_TEST_PS_SCANS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	response := d.gc(t.Context(), false)
	data, err := os.ReadFile(marker)
	if response.Error != "" || len(d.runs) != 0 || err != nil || strings.Count(string(data), "scan\n") != 1 {
		t.Fatal("GC executions did not share one process snapshot", response, string(data), err)
	}
}

func TestForcedDrainDuringLaunchNeverReleasesRunnerGate(t *testing.T) {
	d, api := testDaemon(t)
	cleanupProcesses(t, d)
	dir := testDirectory(t)
	entered, release, executed := filepath.Join(dir, "entered"), filepath.Join(dir, "release"), filepath.Join(dir, "executed")
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	seedDistribution(t, d.config, fmt.Sprintf("#!/bin/sh\nprintf executed > %q\nprintf 'Listening for Jobs\\n'\nsleep 30\n", executed))
	script := fmt.Sprintf("#!/bin/sh\nprintf entered > %q\nwhile [ ! -e %q ]; do sleep 0.02; done\nexec %q \"$@\"\n", entered, release, realPS)
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Error(err)
		}
		settleRuns(d)
	})
	target := d.targets["build"]
	target.Assigned = 1
	d.mu.Lock()
	d.reconcile()
	d.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launch did not reach its gated-child inspection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	responsiveStatus(t, d)
	d.mu.Lock()
	d.beginDrain(target)
	target.DrainStarted = time.Now().Add(-time.Hour)
	d.enforceDrainDeadline(target)
	d.mu.Unlock()
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	settleRuns(d)
	if _, err := os.Stat(executed); !os.IsNotExist(err) {
		t.Fatal("canceled launch released the runner gate", err)
	}
	if len(d.runs) != 0 || api.removeCalls != 1 || !target.Forced {
		t.Fatal("canceled gated child was not reaped and cleaned")
	}
}

func TestPendingRetirementDoesNotShrinkBelowWarmPool(t *testing.T) {
	for _, yield := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm pool", true: "yield capacity"}[yield], func(t *testing.T) {
			d, api := testDaemon(t)
			target := d.targets["build"]
			target.Config.Scaling.MinRunners = 1
			for id := 101; id <= 102; id++ {
				state := testState(t, d.config)
				state.RunnerID, state.Ready, state.Phase = id, true, "registered"
				d.runs[state.RunID] = &activeRun{State: state}
				api.runners[id] = &scaleset.RunnerReference{ID: id, Name: state.RunnerName, RunnerScaleSetID: state.ScaleSetID}
				if err := d.store.save(state); err != nil {
					t.Fatal(err)
				}
			}
			blocked := &blockedRunnerAPI{scaleAPI: api, removeEntered: make(chan struct{}), removeRelease: make(chan struct{})}
			releaseOnCleanup(t, blocked.removeRelease)
			target.Client = blocked
			retire := func() {
				if yield {
					d.yieldIdle("second")
				} else {
					d.reconcileIdle(target, false)
				}
			}
			d.mu.Lock()
			retire()
			d.mu.Unlock()
			<-blocked.removeEntered
			d.mu.Lock()
			for range 3 {
				retire()
			}
			retiring := d.retiring(target.Config.ID)
			d.mu.Unlock()
			if retiring != 1 {
				t.Fatal("in-flight retirement caused extra warm runners to be retired", retiring)
			}
			close(blocked.removeRelease)
			settleRuns(d)
			if d.active(target.Config.ID) != 1 || api.removeCalls != 1 {
				t.Fatal("pending retirement shrank below the configured warm pool")
			}
		})
	}
}
