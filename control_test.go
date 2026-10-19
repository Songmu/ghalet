package ghalet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useFakeManagement(d *daemon, api *fakeScaleAPI) {
	d.clientFactory = func(context.Context, Target) (scaleAPI, error) { return api, nil }
	target := d.targets["build"]
	target.Cancel = func() {}
	target.ListenerDone = true
}

func TestConfirmedDestroyRechecksIdentityAndNeverDeletesGroup(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	plan, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build"})
	if err != nil {
		t.Fatal(err)
	}
	response := d.destroy(t.Context(), plan.Confirmation)
	if response.Error != "" || api.deleteCalls != 1 || api.group == nil || d.targets["build"].Set != nil || !d.targets["build"].Draining {
		t.Fatal("approved scale set was not deleted exactly once, or its group/target lifecycle changed incorrectly", response.Error)
	}
	if response := d.destroy(t.Context(), plan.Confirmation); response.Error == "" || api.deleteCalls != 1 {
		t.Fatal("deletion approval was reused")
	}
}

func TestDestroyUsesTargetCredentialsAndExplicitRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, url, group string
	}{
		{"repository", "https://github.com/owner/repository", ""},
		{"organization", "https://github.com/owner", "old-group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := testDaemon(t)
			d.config.Targets[0].URL = tc.url
			d.config.Targets[0].ScaleSetName = "current"
			original := d.config.Targets[0]
			var received Target
			d.clientFactory = func(_ context.Context, target Target) (scaleAPI, error) {
				received = target
				return api, nil
			}
			plan, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "old", RunnerGroup: tc.group})
			if err != nil {
				t.Fatal(err)
			}
			expected := original
			expected.ScaleSetName, expected.RunnerGroup = "old", tc.group
			if received != expected || d.config.Targets[0] != original {
				t.Fatal("deletion did not use the selected target's credentials or changed its config", received)
			}
			if plan.Target != "build" || plan.URL != tc.url || plan.ScaleSet != "old" ||
				api.lastScaleSetName != "old" || api.lastGroupName != runnerGroupName(tc.group) || api.createCalls != 0 {
				t.Fatal("deletion ignored the explicitly requested old registration", plan)
			}
		})
	}
}

func TestDestroyRefusesChangedRegistrationAndForcedDrain(t *testing.T) {
	for _, forced := range []bool{false, true} {
		d, api := testDaemon(t)
		useFakeManagement(d, api)
		plan, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build"})
		if err != nil {
			t.Fatal(err)
		}
		if forced {
			d.targets["build"].Forced = true
		} else {
			api.set.ID++
		}
		if response := d.destroy(t.Context(), plan.Confirmation); response.Error == "" || api.deleteCalls != 0 {
			t.Fatal("changed registration or forced drain authorized deletion")
		}
	}
}

func TestExplicitDestroyCanReverifyAnOldTargetWithoutPersistingApproval(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	state := testState(t, d.config)
	state.TargetID = "old-target"
	dir := filepath.Join(d.config.Runner.WorkRoot, state.TargetID, state.RunID)
	state.RunnerDir, state.Workspace = filepath.Join(dir, "runner"), filepath.Join(dir, "work")
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, Uncertain: true}
	d.runs[state.RunID] = run
	target := d.targets["build"]
	plan := destroyPlan{URL: state.ScopeURL, ScaleSet: state.ScaleSetName, ScaleSetID: 10, GroupID: 1}
	if err := d.adoptForDestroy(t.Context(), target, plan); err != nil {
		t.Fatal(err)
	}
	if run.Uncertain || run.Target != target || !run.State.ProcessDone || d.drained("build") {
		t.Fatal("explicit old-target recovery skipped verification or cleanup attempt")
	}
	records, err := d.store.load()
	if err != nil || len(records) != 1 || records[0].State.TargetID != "old-target" {
		t.Fatal("operational override rewrote registration ownership into state")
	}
	if err := d.cleanup(run); err != nil || !d.drained("build") {
		t.Fatal("explicit old execution could not be cleaned", err)
	}
}

func TestFailedDestructionDoesNotPublishSyntheticTarget(t *testing.T) {
	for _, unavailableStatistics := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid execution user", true: "missing statistics"}[unavailableStatistics], func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			delete(d.targets, "build")
			d.order = nil
			good := testState(t, d.config)
			bad := testState(t, d.config)
			if unavailableStatistics {
				api.set.Statistics = nil
			} else {
				api.set.Statistics.TotalAssignedJobs = 2
				bad.UID++
			}
			for _, state := range []*RunState{good, bad} {
				if err := d.store.save(state); err != nil {
					t.Fatal(err)
				}
				d.runs[state.RunID] = &activeRun{State: state, Uncertain: true}
			}
			plan, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build"})
			if err != nil {
				t.Fatal(err)
			}
			if response := d.destroy(t.Context(), plan.Confirmation); response.Error == "" {
				t.Fatal("unverified destruction succeeded")
			}
			d.reconcile()
			if len(d.targets) != 0 || len(d.order) != 0 || len(d.runs) != 2 || api.nextID != 100 || api.deleteCalls != 0 {
				t.Fatal("failed destruction left a schedulable synthetic target")
			}
			for _, run := range d.runs {
				if run.Target != nil || !run.Uncertain || run.State.ProcessDone || run.Preparing {
					t.Fatal("failed validation partially adopted retained executions")
				}
			}
		})
	}
}

func TestAdoptedCleanupFailurePreventsDestroy(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	state := testState(t, d.config)
	state.TargetID = "old-target"
	dir := filepath.Join(d.config.Runner.WorkRoot, state.TargetID, state.RunID)
	state.RunnerDir, state.Workspace = filepath.Join(dir, "runner"), filepath.Join(dir, "work")
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := safeDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config.Cleanup.LogRoot, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, Uncertain: true}
	d.runs[state.RunID] = run
	target := d.targets["build"]
	adoption := destroyPlan{URL: state.ScopeURL, ScaleSet: state.ScaleSetName, ScaleSetID: state.ScaleSetID, GroupID: state.GroupID}
	if err := d.adoptForDestroy(t.Context(), target, adoption); err != nil {
		t.Fatal(err)
	}
	if err := d.cleanup(run); err == nil {
		t.Fatal("cleanup fixture did not fail")
	}
	status := d.statusLocked()
	if len(status.Problems) != 1 || status.Problems[0].TargetID != target.Config.ID ||
		state.TargetID != "old-target" || !d.drained(target.Config.ID) {
		t.Fatal("adopted cleanup diagnostics lost the runtime target or changed durable ownership", status)
	}
	plan, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build"})
	if err != nil {
		t.Fatal(err)
	}
	response := d.destroy(t.Context(), plan.Confirmation)
	if response.Error == "" || api.deleteCalls != 0 || d.runs[state.RunID] == nil {
		t.Fatal("adopted cleanup failure allowed scale set deletion", response.Error)
	}
}

func TestFailedAdoptionSaveKeepsEveryExecutionUncertain(t *testing.T) {
	d, _ := testDaemon(t)
	var runs []*activeRun
	for range 2 {
		state := testState(t, d.config)
		if err := d.store.save(state); err != nil {
			t.Fatal(err)
		}
		run := &activeRun{State: state, Uncertain: true}
		d.runs[state.RunID] = run
		runs = append(runs, run)
	}
	blocked := filepath.Dir(runs[1].State.RunnerDir)
	if err := os.Chmod(blocked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blocked, 0711); err != nil {
			t.Error(err)
		}
	})
	plan := destroyPlan{URL: runs[0].State.ScopeURL, ScaleSet: "build", ScaleSetID: 10, GroupID: 1}
	if err := d.adoptForDestroy(t.Context(), d.targets["build"], plan); err == nil {
		t.Fatal("failed adoption save was ignored")
	}
	d.reconcile()
	if len(d.runs) != 2 || d.occupied() != 2 {
		t.Fatal("partial adoption released executions or capacity")
	}
	for _, run := range runs {
		if !run.Uncertain || !strings.Contains(run.State.Error, "adoption did not finish") {
			t.Fatal("partial adoption left an execution eligible for automatic cleanup")
		}
	}
}

func TestDestroyAdoptionWaitsForLocalObservers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exited    bool
		collected bool
	}{
		{name: "both pending"},
		{name: "output pending", exited: true},
		{name: "exit pending", collected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			state := testState(t, d.config)
			state.Phase = "registered"
			if err := d.store.save(state); err != nil {
				t.Fatal(err)
			}
			run := &activeRun{State: state, Uncertain: true, Exited: tc.exited, WaitDone: make(chan struct{})}
			if tc.collected {
				close(run.WaitDone)
			}
			d.runs[state.RunID] = run
			plan, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build"})
			if err != nil {
				t.Fatal(err)
			}
			response := d.destroy(t.Context(), plan.Confirmation)
			if response.Error == "" || !strings.Contains(response.Error, "awaiting local") ||
				api.deleteCalls != 0 || !run.Uncertain || run.Target != nil || state.ProcessDone ||
				run.Exited != tc.exited || run.CleanupTried {
				t.Fatal("destruction bypassed pending observers or partially adopted the execution", response)
			}
			d.reconcile()
			if d.drained("build") {
				t.Fatal("uncertain execution drained with an observer still pending")
			}
			if !tc.collected {
				close(run.WaitDone)
			}
			if !tc.exited {
				run.Stopping = true
				d.processEvent(runEvent{id: state.RunID, kind: "exit"})
			}
			adoption := destroyPlan{URL: state.ScopeURL, ScaleSet: state.ScaleSetName, ScaleSetID: state.ScaleSetID, GroupID: state.GroupID}
			if err := d.adoptForDestroy(t.Context(), d.targets["build"], adoption); err != nil {
				t.Fatal("finished observers prevented explicit adoption", err)
			}
			if run.Uncertain || !run.Exited || !state.ProcessDone || run.CleanupTried {
				t.Fatal("completed adoption skipped the required cleanup attempt")
			}
			if err := d.cleanup(run); err != nil || !d.drained("build") {
				t.Fatal("cleanup did not complete after both observers finished", err)
			}
		})
	}
}

func TestCleanupDoesNotCountPendingObserversAsAttempt(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ProcessDone = true
	run := &activeRun{State: state, WaitDone: make(chan struct{})}
	d.runs[state.RunID] = run
	if err := d.cleanup(run); err == nil || run.CleanupTried || d.drained("build") {
		t.Fatal("pending output counted as a cleanup attempt", err)
	}
	close(run.WaitDone)
	if err := d.cleanup(run); err == nil || run.CleanupTried || d.drained("build") {
		t.Fatal("pending exit observation counted as a cleanup attempt", err)
	}
}

func TestControlAddressAndRunningSnapshotSurviveConfigEdit(t *testing.T) {
	d, _ := testDaemon(t)
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	original := socketPath(d.config.Path)
	os.WriteFile(d.config.Path, []byte("this is no longer valid YAML\nworkRoot: /somewhere/else"), 0600)
	status, err := GetStatus(t.Context(), d.config.Path)
	if err != nil {
		t.Fatal("status tried to reload edited config", err)
	}
	if status == nil || status.Config != d.config.Path || original != socketPath(d.config.Path) {
		t.Fatal("running daemon address or snapshot changed")
	}
	if lock, err := lockControl(d.config.Path); err == nil {
		lock.Close()
		t.Fatal("offline operation acquired a running daemon's control address")
	}
	if second, err := listenControl(d.config.Path); err == nil {
		second.close()
		t.Fatal("second daemon took over the control socket")
	}
}

func TestStatusAndDoctorExitCodesAndPartialGC(t *testing.T) {
	d, _ := testDaemon(t)
	d.targets["build"].Fault = "credentials expired"
	d.problems = append(d.problems, Problem{Path: "/unrecognized", Reason: "corrupt state"})
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	for _, command := range []string{"status", "doctor", "gc"} {
		var err error
		var results []OperationResult
		switch command {
		case "status":
			_, err = GetStatus(t.Context(), d.config.Path)
		case "doctor":
			_, err = Doctor(t.Context(), d.config.Path)
		case "gc":
			results, err = GC(t.Context(), d.config.Path, false)
		}
		if (command == "status" && err != nil) || (command != "status" && err == nil) {
			t.Fatalf("incorrect %s exit behavior: %v", command, err)
		}
		if command == "gc" && (len(results) == 0 || results[0].Error != "corrupt state") {
			t.Fatal("partial gc did not report individual failure")
		}
	}
	if _, err := controlCall(t.Context(), d.config.Path, controlRequest{Command: "status"}); err != nil {
		t.Fatal("partial management failure terminated the daemon", err)
	}
}

func TestDestroyRequiresExplicitInteractiveApproval(t *testing.T) {
	d, api := testDaemon(t)
	if response := d.destroy(t.Context(), "unapproved"); response.Error == "" || api.deleteCalls != 0 {
		t.Fatal("destruction accepted absent or persisted approval")
	}
	if _, err := d.planDestroy(t.Context(), controlRequest{}); err == nil {
		t.Fatal("destroy accepted implicit target")
	}
	for _, request := range []controlRequest{
		{Target: "build"},
		{ScaleSet: "build"},
		{Target: "missing", ScaleSet: "build"},
	} {
		if _, err := d.planDestroy(t.Context(), request); err == nil {
			t.Fatal("destroy accepted a missing or unknown selector", request)
		}
	}
	if _, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build", RunnerGroup: "not-allowed"}); err == nil {
		t.Fatal("repository destroy accepted a group")
	}
	d.config.Targets[0].URL = "https://github.com/owner"
	if _, err := d.planDestroy(t.Context(), controlRequest{Target: "build", ScaleSet: "build"}); err == nil {
		t.Fatal("organization destroy accepted an implicit group")
	}
}

func TestDisconnectedControlCannotBypassWorkRootLock(t *testing.T) {
	c := testConfig(t)
	lock, err := lockWorkRoot(c.Runner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	_, err = GC(t.Context(), c.Path, false)
	if err == nil || !strings.Contains(err.Error(), "refusing direct operation") {
		t.Fatal("disconnected socket bypassed live workRoot lock", err)
	}
}

func TestDestroyAPIRequiresApproval(t *testing.T) {
	for _, approval := range []string{"missing", "declined", "approved"} {
		t.Run(approval, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			server, err := listenControl(d.config.Path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
			go server.serve(d)
			rejected := errors.New("not approved")
			var confirm func(DestroyPlan) error
			if approval != "missing" {
				confirm = func(plan DestroyPlan) error {
					if plan.Target != "build" || plan.ScaleSet != "build" || plan.ScaleSetID != api.set.ID {
						t.Errorf("incorrect confirmation plan: %+v", plan)
					}
					if approval == "declined" {
						return rejected
					}
					return nil
				}
			}
			results, err := Destroy(t.Context(), d.config.Path, DestroyOptions{Target: "build", ScaleSet: "build"}, confirm)
			if approval == "approved" {
				if err != nil || api.deleteCalls != 1 || len(results.Results) != 1 {
					t.Fatal("approved deletion failed", results, err)
				}
			} else if err == nil || api.deleteCalls != 0 {
				t.Fatal("unapproved deletion succeeded", results, err)
			} else if approval == "declined" && !errors.Is(err, rejected) {
				t.Fatal("confirmation error was lost", err)
			}
		})
	}
}

func TestDestroyAPIPreservesIncompleteDrainSnapshot(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	d.targets["build"].Fault = "credentials expired"
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	result, err := Destroy(t.Context(), d.config.Path, DestroyOptions{Target: "build", ScaleSet: "build"},
		func(DestroyPlan) error { return nil })
	if err == nil || result.Status == nil || len(result.Status.Problems) == 0 || api.deleteCalls != 0 {
		t.Fatal("incomplete drain lost diagnostics or deleted the registration", result, err)
	}
}

func TestDrainAPISelectsTarget(t *testing.T) {
	d, _ := testDaemon(t)
	useFakeManagement(d, &fakeScaleAPI{})
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	if _, err := Drain(t.Context(), d.config.Path, "missing"); err == nil {
		t.Fatal("unknown target was accepted")
	}
	status, err := Drain(t.Context(), d.config.Path, "build")
	if err != nil || status == nil || len(status.Targets) != 1 || !status.Targets[0].Draining {
		t.Fatal("selected target was not drained", status, err)
	}
}

func TestConfigPathDoesNotSearchCurrentDirectory(t *testing.T) {
	home := testDirectory(t)
	t.Setenv("HOME", home)
	path, err := configPath("")
	if err != nil || path != filepath.Join(home, ".config", "ghalet", "config.yaml") {
		t.Fatal(path, err)
	}
}

func TestDefaultGroupIsEquivalentForExplicitOldTargetRecovery(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	run := &activeRun{State: state, Uncertain: true}
	d.runs[state.RunID] = run
	plan := destroyPlan{URL: state.ScopeURL, ScaleSet: state.ScaleSetName, ScaleSetID: 10, GroupID: 1, RunnerGroup: "default"}
	if err := d.adoptForDestroy(t.Context(), d.targets["build"], plan); err != nil {
		t.Fatal("omitted default group did not match an explicitly named default group", err)
	}
	if run.Uncertain || !run.State.ProcessDone {
		t.Fatal("explicit recovery failed to reverify the equivalent default group")
	}
}

func TestGCWaitsForOutputCollectionBeforeArchiving(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.Phase = "registered"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := safeDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state.RunnerDir, "output.log")
	if err := os.WriteFile(source, []byte("partial output"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, WaitDone: make(chan struct{}), JITSecrets: d.secrets.AddJIT("test-jit-secret")}
	d.runs[state.RunID] = run
	for _, dryRun := range []bool{true, false} {
		response := d.gc(t.Context(), dryRun)
		if response.Error != "" || len(response.Results) != 1 || !strings.Contains(response.Results[0].Action, "awaiting output collection") {
			t.Fatal("gc did not retain an execution with pending output", response)
		}
		if state.ProcessDone || run.CleanupTried || d.runs[state.RunID] == nil || d.secrets.Redact("test-jit-secret") == "test-jit-secret" {
			t.Fatal("gc released execution state or redaction secrets before output completion")
		}
		if _, err := os.Stat(source); err != nil {
			t.Fatal("gc removed the uncollected output", err)
		}
	}
	if err := os.WriteFile(source, []byte("complete output test-jit-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run.OutputReady = true
	close(run.WaitDone)
	if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] == nil || state.ProcessDone {
		t.Fatal("gc cleaned output before observing the runner exit", response.Error)
	}
	d.processEvent(runEvent{id: state.RunID, kind: "exit"})
	if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] != nil {
		t.Fatal("gc did not resume after output collection", response.Error)
	}
	data, err := os.ReadFile(filepath.Join(d.config.Cleanup.LogRoot, "runs", state.RunID, "output.log"))
	if err != nil || string(data) != "complete output ***\n" {
		t.Fatalf("gc archived incomplete or unredacted output: %q (%v)", data, err)
	}
}

func TestGCReportsOutputCollectionFailureWithoutDeletingLogs(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.Phase = "registered"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := safeDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state.RunnerDir, "output.log")
	if err := os.WriteFile(source, []byte("retained output"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, WaitDone: make(chan struct{}), Exited: true, OutputError: errTestAPI}
	close(run.WaitDone)
	d.runs[state.RunID] = run
	response := d.gc(t.Context(), false)
	if response.Error == "" || d.runs[state.RunID] == nil || !strings.Contains(state.Error, "collect runner output") {
		t.Fatal("output failure was ignored", response)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "retained output" {
		t.Fatal("collection failure removed the original output", err)
	}
}
