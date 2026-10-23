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

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
	"github.com/actions/scaleset"
	"github.com/goccy/go-yaml"
)

func nextConfig(d *daemon) *settings.Config {
	next := *d.config
	next.Targets = append([]settings.Target{}, next.Targets...)
	return &next
}

func writeApplyConfig(t *testing.T, c *settings.Config) {
	t.Helper()
	metrics := map[string]any{
		"exporter": c.Telemetry.Metrics.Exporter,
		"endpoint": c.Telemetry.Metrics.Endpoint,
	}
	if interval := c.Telemetry.Metrics.Interval; interval != nil {
		metrics["interval"] = time.Duration(*interval).String()
	}
	if timeout := c.Telemetry.Metrics.Timeout; timeout != nil {
		metrics["timeout"] = time.Duration(*timeout).String()
	}
	data, err := yaml.Marshal(map[string]any{
		"capacity": c.Capacity, "targets": c.Targets,
		"runner": map[string]any{
			"version": c.Runner.Version, "user": c.Runner.User,
			"startupTimeout": time.Duration(c.Runner.StartupTimeout).String(),
			"drainTimeout":   time.Duration(c.Runner.DrainTimeout).String(),
			"env":            c.Runner.Env,
		},
		"logs": map[string]any{
			"preserve": c.Logs.Preserve, "root": c.Logs.Root,
			"retention": time.Duration(c.Logs.Retention).String(),
		},
		"telemetry": map[string]any{
			"metrics": metrics, "serviceName": c.Telemetry.ServiceName,
			"resourceAttributes": c.Telemetry.ResourceAttributes,
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
	next.Targets = []settings.Target{}
	writeApplyConfig(t, next)
	serveTestControl(t, d)
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

func TestApplyReusesUnchangedRegistration(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Capacity.MaxRunners = 3
	next.Runner.Version = "2.339.0"
	next.Runner.Env = map[string]string{"BUILD_MODE": "new"}
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error != "" || len(response.Status.Targets) != 1 ||
		response.Status.Targets[0].ID != buildTargetID || response.Status.Targets[0].Draining ||
		response.Status.Capacity != 3 {
		t.Fatal("unchanged registration was not reused", response)
	}
	if api.deleteCalls != 0 || api.createCalls != 0 || len(d.retired) != 0 ||
		d.config.Runner.Version != next.Runner.Version || !reflect.DeepEqual(d.config.Runner.Env, next.Runner.Env) {
		t.Fatal("apply replaced a referenced scale set or lost global settings")
	}
}

func TestApplyKeepsStoppedRecoveryDiagnosticsWithoutReservingNewCapacity(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	state := testState(t, d.config)
	state.ScopeURL = "https://github.com/old/repository"
	state.ProcessDone, state.Phase = true, "cleaning"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := nextConfig(d)
	next.Targets[0].URL = "https://github.com/owner/other"
	nextID, _ := settings.TargetKey(next.Targets[0].URL, "", next.Targets[0].ScaleSetName)
	writeApplyConfig(t, next)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response := d.apply(ctx)
	if ctx.Err() != nil || response.Error == "" || response.Status == nil ||
		response.Status.Occupied != 0 || len(response.Status.Problems) == 0 {
		t.Fatal("apply blocked on stopped recovery or lost its diagnostics", response)
	}
	if !d.canStart(d.targets[nextID]) || d.active(nextID) != 0 ||
		d.runs[state.RunID] == nil || api.removeCalls != 0 {
		t.Fatal("retained recovery state blocked the new target or authorized cleanup")
	}
	if err := d.cleanup(d.runs[state.RunID]); err == nil {
		t.Fatal("apply authorized cleanup of an unverified old registration")
	}
}

func TestApplyReplacesRegistrationAndUsesOldCredentialsForDeletion(t *testing.T) {
	for _, change := range []string{"scope", "group", "scale set"} {
		t.Run(change, func(t *testing.T) {
			d, oldAPI := testDaemon(t)
			useFakeManagement(d, oldAPI)
			oldConfig := d.targets[buildTargetID].Config
			newAPI := newFakeAPI()
			newAPI.set.ID = 20
			next := nextConfig(d)
			next.Targets[0].Credential.TokenEnv = "NEW_TOKEN"
			switch change {
			case "scope":
				next.Targets[0].URL = "https://github.com/owner/other"
			case "group":
				oldConfig.URL = "https://github.com/owner"
				d.targets[buildTargetID].Config = oldConfig
				next.Targets[0].URL = oldConfig.URL
				next.Targets[0].RunnerGroup = "new-group"
			case "scale set":
				next.Targets[0].ScaleSetName = "other"
			}
			var received settings.Target
			nextID, _ := settings.TargetKey(next.Targets[0].URL, next.Targets[0].RunnerGroup, next.Targets[0].ScaleSetName)
			d.clientFactory = func(_ context.Context, target settings.Target) (scaleAPI, error) {
				received = target
				return newAPI, nil
			}
			writeApplyConfig(t, next)
			response := d.apply(t.Context())
			if response.Error != "" || received.Credential.TokenEnv != "NEW_TOKEN" ||
				oldAPI.deleteCalls != 1 || newAPI.deleteCalls != 0 || d.targets[nextID].Set.ID != 20 {
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
	for _, field := range []string{"syntax", "user", "state home", "cache home", "state directory", "cache directory", "logs.root"} {
		t.Run(field, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			original := d.config
			next := nextConfig(d)
			switch field {
			case "user":
				next.Runner.User = "another-user"
			case "state home":
				t.Setenv("XDG_STATE_HOME", testDirectory(t))
			case "cache home":
				t.Setenv("XDG_CACHE_HOME", testDirectory(t))
			case "state directory":
				t.Setenv("STATE_DIRECTORY", testDirectory(t))
			case "cache directory":
				t.Setenv("CACHE_DIRECTORY", testDirectory(t))
			case "logs.root":
				next.Logs.Root += "-new"
			}
			writeApplyConfig(t, next)
			if field == "syntax" {
				if err := os.WriteFile(next.Path, []byte("targets: [\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			response := d.apply(t.Context())
			if response.Error == "" || d.config != original || d.targets[buildTargetID].Draining ||
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
	next.Targets = []settings.Target{}
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
			target := d.targets[buildTargetID]
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
			next.Targets = []settings.Target{}
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
	oldConfig, oldTarget := d.config, d.targets[buildTargetID]
	newAPI := newFakeAPI()
	newAPI.set.ID = 20
	d.clientFactory = func(context.Context, settings.Target) (scaleAPI, error) { return newAPI, nil }
	next := nextConfig(d)
	next.Targets[0].ScaleSetName = "new"
	nextID, _ := settings.TargetKey(next.Targets[0].URL, "", "new")
	next.Logs.Preserve = false
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error == "" || run.Target != oldTarget || run.Config != oldConfig ||
		oldAPI.deleteCalls != 0 || d.targets[nextID].Set.ID != 20 {
		t.Fatal("retained cleanup lost its old registration/configuration", response)
	}
	d.workers.Wait()
	if err := d.cleanup(run); err != nil {
		t.Fatal("old execution could not be cleaned after apply", err)
	}
	if _, err := os.Stat(filepath.Join(oldConfig.Logs.Root, "runs", state.RunID, "metadata.json")); err != nil {
		t.Fatal("retained execution used the new log policy", err)
	}
}

func TestApplyWaitsForDrainAndKeepsStatusResponsive(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets = []settings.Target{}
	writeApplyConfig(t, next)
	d.targets[buildTargetID].Assigned = 1
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
	d.targets[buildTargetID].Assigned = 0
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
	if response.Error == "" || !strings.Contains(response.Error, "management credential") || d.targets[buildTargetID].Draining {
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
	second.ScaleSetName = "second"
	next.Targets = append(next.Targets, second)
	newAPI := newFakeAPI()
	newAPI.set = nil
	d.clientFactory = func(_ context.Context, target settings.Target) (scaleAPI, error) {
		if target.ID == buildTargetID {
			return nil, scaleset.UnauthorizedError
		}
		return newAPI, nil
	}
	writeApplyConfig(t, next)
	response := d.apply(t.Context())
	if response.Error == "" || len(d.targets) != 2 || newAPI.createCalls != 1 ||
		d.targets[buildTargetID].Fault == "" || d.targets[secondTargetID].Set == nil || oldAPI.deleteCalls != 0 {
		t.Fatal("apply failed to report partial registration while applying healthy targets", response)
	}
}

func TestConfigAllowsExplicitEmptyTargets(t *testing.T) {
	for _, content := range []string{"targets: []\n", "targets: null\n", "{}\n"} {
		_, err := settings.LoadConfig(writeConfig(t, content))
		if (err == nil) != (content == "targets: []\n") {
			t.Fatalf("%q: unexpected validation result: %v", content, err)
		}
	}
}

func TestRetiredDeletionWaitsForPendingObservers(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	target := d.targets[buildTargetID]
	d.beginDrain(target)
	state := testState(t, d.config)
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}

	run := &activeRun{State: state, output: &runOutputState{done: make(chan struct{})}}
	d.runs[state.RunID] = run
	if err := d.deleteRetiredScaleSet(t.Context(), target); err == nil || api.deleteCalls != 0 {
		t.Fatal("pending execution observers allowed deletion")
	}
	close(run.output.done)
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
	d.targets[buildTargetID].Assigned = 1
	waitForRun(t, d, func(run *activeRun) bool {
		if run.State.Ready && !run.Preparing {
			run.State.Busy = true
			return true
		}
		return false
	})
	d.mu.Lock()
	d.targets[buildTargetID].Assigned = 0
	d.mu.Unlock()
	next := nextConfig(d)
	next.Targets = []settings.Target{}
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
	second.ScaleSetName = "second"
	next.Targets = append(next.Targets, second)
	writeApplyConfig(t, next)
	registering := make(chan struct{})
	d.clientFactory = func(ctx context.Context, _ settings.Target) (scaleAPI, error) {
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
		d.targets[secondTargetID].Fault == "" || api.deleteCalls != 0 {
		t.Fatal("shutdown lost target diagnostics or deleted the old registration")
	}
	for _, target := range d.targets {
		if target.Cancel != nil {
			t.Fatal("apply restarted a listener during shutdown")
		}
	}
}
