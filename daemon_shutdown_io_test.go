package ghalet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Songmu/ghalet/procgroup"
	"github.com/actions/scaleset"
)

type blockedLookupAPI struct {
	scaleAPI
	entered chan struct{}
	release chan struct{}
}

func (b *blockedLookupAPI) GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error) {
	close(b.entered)
	<-b.release
	return b.scaleAPI.GetRunnerByName(ctx, name)
}

func TestPreparationFailureStopsAcquisitionBeforeSlowRollback(t *testing.T) {
	d, api := testDaemon(t)
	seedDistribution(t, d.config, "#!/bin/sh\nexit 0\n")
	api.jitErr = scaleset.UnauthorizedError
	blocked := &blockedLookupAPI{scaleAPI: api, entered: make(chan struct{}), release: make(chan struct{})}
	releaseOnCleanup(t, blocked.release)
	target := d.targets["build"]
	target.Client, target.Assigned = blocked, 1
	d.mu.Lock()
	d.reconcile()
	d.mu.Unlock()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("failed preparation did not reach rollback")
	}
	if status := responsiveStatus(t, d); status.Targets[0].Fault == "" {
		t.Fatal("slow rollback hid an already observed target failure", status)
	}
	session := &fakeSession{}
	if err := (&targetScaler{daemon: d, target: target, session: session}).Scale(t.Context(), availableMessage(1, 2)); err != nil || len(session.acquired) != 0 {
		t.Fatal("failed target accepted work while rollback was blocked", err)
	}
	close(blocked.release)
	settleRuns(d)
	if target.Failures != 1 {
		t.Fatal("preparation failure was published more than once", target.Failures)
	}
}

func TestGCRefreshesSnapshotAfterSameIdentityStop(t *testing.T) {
	d, run := startTerminationRunner(t, "#!/bin/sh\nsleep 30 &\nprintf 'Listening for Jobs\\n'\nwait\n")
	table, err := procgroup.Snapshot(t.Context(), []procgroup.ProcessIdentity{run.State.Process})
	if err != nil {
		t.Fatal(err)
	}
	api := d.targets["build"].Client.(*fakeScaleAPI)
	api.mu.Lock()
	api.removeErr = errTestAPI
	api.mu.Unlock()
	d.mu.Lock()
	identity := run.State.Process
	done := d.runIO(run, func(work *activeRun) error { return d.stop(work, true) })
	generation := run.generation
	d.mu.Unlock()
	<-done
	if !run.State.ProcessDone || run.State.Process != identity || run.State.Error == "" {
		t.Fatal("test did not establish a same-identity stopped execution with pending cleanup")
	}
	api.mu.Lock()
	api.removeErr = nil
	api.mu.Unlock()
	result := d.gcRun(t.Context(), run, false, table, identity, generation)
	if result.Error != "" || d.runs[run.State.RunID] != nil {
		t.Fatal("GC reused the live snapshot after the stop completed", result)
	}
}

func TestShutdownWaitsForWholeGCPass(t *testing.T) {
	d, _ := testDaemon(t)
	dir := testDirectory(t)
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	script := "#!/bin/sh\nprintf entered > \"$GHALET_TEST_PS_ENTERED\"\nwhile [ ! -e \"$GHALET_TEST_PS_RELEASE\" ]; do sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHALET_TEST_PS_ENTERED", entered)
	t.Setenv("GHALET_TEST_PS_RELEASE", release)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	var stopped chan error
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Error(err)
		}
		if stopped != nil {
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Error("daemon did not finish shutdown after GC")
			}
		}
	})
	collected := make(chan controlResponse, 1)
	go func() { collected <- d.gc(d.ctx, false) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("GC did not enter process inspection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stopped = make(chan error, 1)
	go func() {
		defer close(stopped)
		stopped <- d.serve(ctx)
	}()
	select {
	case err := <-stopped:
		t.Fatal("shutdown bypassed the in-flight GC pass", err)
	case response := <-collected:
		t.Fatal("shutdown interrupted the in-flight GC pass", response)
	case <-time.After(300 * time.Millisecond):
	}
	responsiveStatus(t, d)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if response := <-collected; response.Error != "" {
		t.Fatal("GC did not report its synchronous result", response)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after GC")
	}
	if response := d.gc(t.Context(), false); response.Error == "" {
		t.Fatal("stopped daemon accepted a new GC pass")
	}
}
