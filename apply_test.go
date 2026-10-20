package ghalet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/actions/scaleset"
	"github.com/goccy/go-yaml"
)

func nextConfig(d *daemon) *Config {
	next := *d.config
	next.Targets = append([]Target{}, next.Targets...)
	return &next
}

func writeApplyConfig(t *testing.T, c *Config) {
	t.Helper()
	data, err := yaml.Marshal(map[string]any{
		"capacity": c.Capacity, "targets": c.Targets,
		"runner": map[string]any{
			"version": c.Runner.Version, "user": c.Runner.User,
			"startupTimeout": time.Duration(c.Runner.StartupTimeout).String(),
			"drainTimeout":   time.Duration(c.Runner.DrainTimeout).String(),
			"workRoot":       c.Runner.WorkRoot, "distributionRoot": c.Runner.DistributionRoot,
			"cacheRoot": c.Runner.CacheRoot, "env": c.Runner.Env,
		},
		"cleanup": map[string]any{
			"preserveLogs": c.Cleanup.PreserveLogs, "logRoot": c.Cleanup.LogRoot,
			"logRetention": time.Duration(c.Cleanup.LogRetention).String(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRemovesAllTargetsAndDeletesUnusedScaleSet(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets = []Target{}
	writeApplyConfig(t, next)
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	result, err := Apply(t.Context(), next.Path)
	if err != nil || result.Status == nil || len(result.Status.Targets) != 0 {
		t.Fatal("apply did not publish the empty configuration", result, err)
	}
	if api.deleteCalls != 1 || api.group == nil || len(d.targets) != 0 || len(d.retired) != 0 || d.applying {
		t.Fatal("apply did not delete exactly the unused scale set")
	}
	result, err = Apply(t.Context(), next.Path)
	if err != nil || api.deleteCalls != 1 {
		t.Fatal("repeated apply was not idempotent", result, err)
	}
}

func TestApplyReusesScaleSetAcrossTargetRename(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets[0].ID = "renamed"
	next.Capacity.MaxRunners = 3
	next.Runner.Version = "2.339.0"
	next.Runner.Env = map[string]string{"BUILD_MODE": "new"}
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error != "" || len(response.Status.Targets) != 1 ||
		response.Status.Targets[0].ID != "renamed" || response.Status.Targets[0].Draining ||
		response.Status.Capacity != 3 {
		t.Fatal("renamed target was not applied", response)
	}
	if api.deleteCalls != 0 || api.createCalls != 0 || len(d.retired) != 0 ||
		d.config.Runner.Version != next.Runner.Version || !reflect.DeepEqual(d.config.Runner.Env, next.Runner.Env) {
		t.Fatal("apply replaced a referenced scale set or lost global settings")
	}
}

func TestApplyReplacesRegistrationAndUsesOldCredentialsForDeletion(t *testing.T) {
	for _, change := range []string{"scope", "group", "scale set"} {
		t.Run(change, func(t *testing.T) {
			d, oldAPI := testDaemon(t)
			useFakeManagement(d, oldAPI)
			oldConfig := d.targets["build"].Config
			newAPI := newFakeAPI()
			newAPI.set.ID = 20
			next := nextConfig(d)
			next.Targets[0].Credential.TokenEnv = "NEW_TOKEN"
			switch change {
			case "scope":
				next.Targets[0].URL = "https://github.com/owner/other"
			case "group":
				oldConfig.URL = "https://github.com/owner"
				d.targets["build"].Config = oldConfig
				next.Targets[0].URL = oldConfig.URL
				next.Targets[0].RunnerGroup = "new-group"
			case "scale set":
				next.Targets[0].ScaleSetName = "other"
			}
			var received Target
			d.clientFactory = func(_ context.Context, target Target) (scaleAPI, error) {
				received = target
				return newAPI, nil
			}
			writeApplyConfig(t, next)
			response := d.apply(t.Context())
			if response.Error != "" || received.Credential.TokenEnv != "NEW_TOKEN" ||
				oldAPI.deleteCalls != 1 || newAPI.deleteCalls != 0 || d.targets["build"].Set.ID != 20 {
				t.Fatal("apply failed to separate old and new registrations/clients", response)
			}
		})
	}
}

func TestApplyKeepsScaleSetReferencedByActualID(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets[0].ScaleSetName = "alias"
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error != "" || api.deleteCalls != 0 || len(d.retired) != 0 {
		t.Fatal("apply deleted a registration still referenced by its actual ID", response)
	}
}

func TestApplyRejectsInvalidOrRestartOnlyChangesBeforeDrain(t *testing.T) {
	for _, field := range []string{"syntax", "user", "workRoot", "cacheRoot", "distributionRoot", "logRoot"} {
		t.Run(field, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			original := d.config
			next := nextConfig(d)
			switch field {
			case "user":
				next.Runner.User = "another-user"
			case "workRoot":
				next.Runner.WorkRoot += "-new"
			case "cacheRoot":
				next.Runner.CacheRoot += "-new"
			case "distributionRoot":
				next.Runner.DistributionRoot += "-new"
			case "logRoot":
				next.Cleanup.LogRoot += "-new"
			}
			writeApplyConfig(t, next)
			if field == "syntax" {
				if err := os.WriteFile(next.Path, []byte("targets: [\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			response := d.apply(t.Context())
			if response.Error == "" || d.config != original || d.targets["build"].Draining ||
				api.deleteCalls != 0 || d.applying {
				t.Fatal("invalid apply mutated the running configuration", response)
			}
		})
	}
}

func TestApplyRetriesFailedDeletionWithoutRollingBackConfig(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	api.deleteErr = errors.New("deletion unavailable")
	next := nextConfig(d)
	next.Targets = []Target{}
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error == "" || len(d.config.Targets) != 0 || len(d.targets) != 0 || len(d.retired) != 1 ||
		len(response.Status.Problems) == 0 {
		t.Fatal("failed deletion rolled back configuration or lost diagnostics", response)
	}
	api.deleteErr = nil
	response = d.apply(t.Context())
	if response.Error != "" || len(d.retired) != 0 || api.deleteCalls != 2 || len(response.Status.Problems) != 0 {
		t.Fatal("apply failed to retry retired registration cleanup", response)
	}
}

func TestApplyRetainsUnsafeScaleSets(t *testing.T) {
	for _, reason := range []string{"forced", "fault", "close failure", "identity changed", "statistics", "busy", "local record", "cleanup"} {
		t.Run(reason, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			target := d.targets["build"]
			switch reason {
			case "forced":
				target.Forced = true
			case "fault":
				target.Fault = "authentication unavailable"
			case "close failure":
				target.CloseError = "session did not close"
			case "identity changed":
				copy := *api.set
				copy.ID++
				api.set = &copy
			case "statistics":
				api.set.Statistics = nil
			case "busy":
				api.set.Statistics.TotalBusyRunners = 1
			case "local record":
				d.problems = append(d.problems, Problem{Reason: "corrupt record"})
			case "cleanup":
				state := testState(t, d.config)
				state.ProcessDone, state.Error = true, "cleanup failed"
				d.runs[state.RunID] = &activeRun{State: state, CleanupTried: true}
			}
			next := nextConfig(d)
			next.Targets = []Target{}
			writeApplyConfig(t, next)
			response := d.apply(t.Context())
			if response.Error == "" || api.deleteCalls != 0 || len(d.config.Targets) != 0 || len(d.retired) != 1 {
				t.Fatal("unsafe deletion succeeded or blocked desired configuration", response)
			}
		})
	}
}

func TestApplyPreservesRetainedExecutionContext(t *testing.T) {
	d, oldAPI := testDaemon(t)
	useFakeManagement(d, oldAPI)
	state := testState(t, d.config)
	state.ProcessDone, state.Error = true, "cleanup failed"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, CleanupTried: true}
	d.runs[state.RunID] = run
	oldConfig, oldTarget := d.config, d.targets["build"]
	newAPI := newFakeAPI()
	newAPI.set.ID = 20
	d.clientFactory = func(context.Context, Target) (scaleAPI, error) { return newAPI, nil }
	next := nextConfig(d)
	next.Targets[0].ScaleSetName = "new"
	next.Cleanup.PreserveLogs = false
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error == "" || run.Target != oldTarget || run.Config != oldConfig ||
		oldAPI.deleteCalls != 0 || d.targets["build"].Set.ID != 20 {
		t.Fatal("retained cleanup lost its old registration/configuration", response)
	}
	d.workers.Wait()
	if err := d.cleanup(run); err != nil {
		t.Fatal("old execution could not be cleaned after apply", err)
	}
	if _, err := os.Stat(filepath.Join(oldConfig.Cleanup.LogRoot, "runs", state.RunID, "metadata.json")); err != nil {
		t.Fatal("retained execution used the new log policy", err)
	}
}

func TestApplyWaitsForDrainAndKeepsStatusResponsive(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets = []Target{}
	writeApplyConfig(t, next)
	d.targets["build"].Assigned = 1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan controlResponse, 1)
	go func() { done <- d.apply(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := d.handle(t.Context(), controlRequest{Command: "status"})
		if status.Status.Targets[0].Draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("apply did not start draining")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if response := d.apply(t.Context()); response.Error == "" {
		t.Fatal("concurrent apply was accepted")
	}
	select {
	case <-done:
		t.Fatal("apply completed before accepted jobs were drained")
	default:
	}
	d.mu.Lock()
	d.targets["build"].Assigned = 0
	d.mu.Unlock()
	select {
	case response := <-done:
		if response.Error != "" {
			t.Fatal(response.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not complete after drain")
	}
}

func TestApplyDoesNotLeakFormerCredentialEnvironment(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets[0].Credential.TokenEnv = "NEXT_TOKEN"
	next.Runner.Env = map[string]string{"GHALET_TEST_TOKEN": "not-a-secret"}
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error == "" || !strings.Contains(response.Error, "management credential") || d.targets["build"].Draining {
		t.Fatal("apply exposed the old credential variable to runners", response)
	}
}

func TestApplyRequiresRunningDaemon(t *testing.T) {
	c := testConfig(t)
	if _, err := Apply(t.Context(), c.Path); err == nil || !strings.Contains(err.Error(), "connect to running daemon") {
		t.Fatal("apply silently fell back to offline operation", err)
	}
	if _, err := os.Stat(c.Runner.WorkRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("offline apply created managed state", err)
	}
}

func TestApplyRegistersNewTargetAndReportsPartialFailure(t *testing.T) {
	d, oldAPI := testDaemon(t)
	useFakeManagement(d, oldAPI)
	next := nextConfig(d)
	second := next.Targets[0]
	second.ID, second.ScaleSetName = "second", "second"
	next.Targets = append(next.Targets, second)
	newAPI := newFakeAPI()
	newAPI.set = nil
	d.clientFactory = func(_ context.Context, target Target) (scaleAPI, error) {
		if target.ID == "build" {
			return nil, scaleset.UnauthorizedError
		}
		return newAPI, nil
	}
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error == "" || len(d.targets) != 2 || newAPI.createCalls != 1 ||
		d.targets["build"].Fault == "" || d.targets["second"].Set == nil || oldAPI.deleteCalls != 0 {
		t.Fatal("apply failed to report partial registration while applying healthy targets", response)
	}
}

func TestConfigAllowsExplicitEmptyTargets(t *testing.T) {
	for _, content := range []string{"targets: []\n", "targets: null\n", "{}\n"} {
		_, err := LoadConfig(writeConfig(t, content))
		if (err == nil) != (content == "targets: []\n") {
			t.Fatalf("%q: unexpected validation result: %v", content, err)
		}
	}
}

func TestRetiredDeletionWaitsForPendingObservers(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	target := d.targets["build"]
	d.beginDrain(target)
	state := testState(t, d.config)
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}

	run := &activeRun{State: state, WaitDone: make(chan struct{})}
	d.runs[state.RunID] = run
	if err := d.deleteRetiredScaleSet(t.Context(), target); err == nil || api.deleteCalls != 0 {
		t.Fatal("pending execution observers allowed deletion")
	}
	close(run.WaitDone)
	run.Exited, state.ProcessDone = true, true
	if err := d.cleanup(run); err != nil {
		t.Fatal(err)
	}
	if err := d.deleteRetiredScaleSet(t.Context(), target); err != nil || api.deleteCalls != 1 {
		t.Fatal("finished cleanup did not allow scale set deletion", err)
	}
}

func TestApplyDrainsLiveRunnerBeforeDeletingScaleSet(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 1\n")
	d.targets["build"].Assigned = 1
	pumpUntil(t, d, func() bool {
		for _, run := range d.runs {
			if run.State.Ready && !run.Preparing {
				run.State.Busy = true
				return true
			}

		}
		return false
	})
	d.mu.Lock()
	d.targets["build"].Assigned = 0
	d.mu.Unlock()
	next := nextConfig(d)
	next.Targets = []Target{}
	writeApplyConfig(t, next)
	engineCtx, cancel := context.WithCancel(t.Context())
	engineDone := make(chan error, 1)
	go func() { engineDone <- d.serve(engineCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-engineDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("test daemon did not shut down")
		}
	})
	response := d.apply(t.Context())
	if response.Error != "" || len(response.Status.Runs) != 0 || api.deleteCalls != 1 || api.removeCalls != 1 {
		t.Fatal("apply did not finish runner cleanup before scale set deletion", response)
	}
}

func TestShutdownInterruptsApplyWithoutRestartingListeners(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	second := next.Targets[0]
	second.ID, second.ScaleSetName = "second", "second"
	next.Targets = append(next.Targets, second)
	writeApplyConfig(t, next)
	registering := make(chan struct{})
	d.clientFactory = func(ctx context.Context, _ Target) (scaleAPI, error) {
		close(registering)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	applyCtx, cancelApply := context.WithCancel(t.Context())
	defer cancelApply()
	applied := make(chan controlResponse, 1)
	go func() { applied <- d.apply(applyCtx) }()
	select {
	case <-registering:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not begin re-registration")
	}
	shutdown, cancelShutdown := context.WithCancel(t.Context())
	cancelShutdown()
	stopped := make(chan error, 1)
	go func() { stopped <- d.serve(shutdown) }()
	select {
	case response := <-applied:
		if response.Error == "" || !strings.Contains(response.Error, "incomplete") {
			t.Fatal("interrupted apply returned success", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not interrupt apply")
	}
	select {
	case err := <-stopped:
		if err == nil || !strings.Contains(err.Error(), "unresolved diagnostics") {
			t.Fatal("shutdown hid the interrupted reconfiguration", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown raced with listener startup")
	}
	if !d.stopping || d.applying || len(d.targets) != 2 || len(d.order) != 2 ||
		d.targets["second"].Fault == "" || api.deleteCalls != 0 {
		t.Fatal("shutdown lost target diagnostics or deleted the old registration")
	}
	for _, target := range d.targets {
		if target.Cancel != nil {
			t.Fatal("apply restarted a listener during shutdown")
		}
	}
}
