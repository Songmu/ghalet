package ghalet

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useFakeManagement(d *daemon, api *fakeScaleAPI) {
	d.clientFactory = func(context.Context, string) (scaleAPI, error) { return api, nil }
	target := d.targets["build"]
	target.Cancel = func() {}
	target.ListenerDone = true
}

func TestConfirmedDestroyRechecksIdentityAndNeverDeletesGroup(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	plan, err := d.planDestroy(t.Context(), controlRequest{Scope: "repository", ScaleSet: "build"})
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

func TestDestroyRefusesChangedRegistrationAndForcedDrain(t *testing.T) {
	for _, forced := range []bool{false, true} {
		d, api := testDaemon(t)
		useFakeManagement(d, api)
		plan, err := d.planDestroy(t.Context(), controlRequest{Scope: "repository", ScaleSet: "build"})
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
			plan, err := d.planDestroy(t.Context(), controlRequest{Scope: "repository", ScaleSet: "build"})
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
	plan, err := d.planDestroy(t.Context(), controlRequest{Scope: "repository", ScaleSet: "build"})
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
	var output, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"status", "--config", d.config.Path}, &output, &stderr); err != nil {
		t.Fatal("status tried to reload edited config", err)
	}
	var response controlResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status.Config != d.config.Path || original != socketPath(d.config.Path) {
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
		var output, stderr bytes.Buffer
		err := Run(t.Context(), []string{command, "--config", d.config.Path}, &output, &stderr)
		if (command == "status" && err != nil) || (command != "status" && err == nil) {
			t.Fatalf("incorrect %s exit behavior: %v, %s", command, err, output.String())
		}
		if command == "gc" && !strings.Contains(output.String(), "corrupt state") {
			t.Fatal("partial gc did not report individual failure")
		}
	}
	if _, err := controlCall(t.Context(), d.config.Path, controlRequest{Command: "status"}); err != nil {
		t.Fatal("partial management failure terminated the daemon", err)
	}
}

func TestDestroyRequiresExplicitInteractiveApproval(t *testing.T) {
	plan := &destroyPlan{Scope: "repository", URL: "https://github.com/owner/repository", ScaleSet: "build", ScaleSetID: 10}
	for _, answer := range []string{"no\n", "y\n", "\n", "YES\n", ""} {
		if err := confirmDestroy(plan, strings.NewReader(answer), &bytes.Buffer{}); err == nil {
			t.Fatalf("answer %q approved destruction", answer)
		}
	}
	if err := confirmDestroy(plan, strings.NewReader("yes\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	d, api := testDaemon(t)
	if response := d.destroy(t.Context(), "unapproved"); response.Error == "" || api.deleteCalls != 0 {
		t.Fatal("destruction accepted absent or persisted approval")
	}
	if _, err := d.planDestroy(t.Context(), controlRequest{}); err == nil {
		t.Fatal("destroy accepted implicit target")
	}
	if _, err := d.planDestroy(t.Context(), controlRequest{Scope: "repository", ScaleSet: "build", RunnerGroup: "not-allowed"}); err == nil {
		t.Fatal("repository destroy accepted a group")
	}
}

func TestDisconnectedControlCannotBypassWorkRootLock(t *testing.T) {
	c := testConfig(t)
	lock, err := lockWorkRoot(c.Runner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	var output bytes.Buffer
	err = Run(t.Context(), []string{"gc", "--config", c.Path}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "refusing direct operation") {
		t.Fatal("disconnected socket bypassed live workRoot lock", err)
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
	run := &activeRun{State: state, WaitDone: make(chan struct{}), JITSecrets: d.secrets.addJIT("test-jit-secret")}
	d.runs[state.RunID] = run
	for _, dryRun := range []bool{true, false} {
		response := d.gc(t.Context(), dryRun)
		if response.Error != "" || len(response.Results) != 1 || !strings.Contains(response.Results[0].Action, "awaiting output collection") {
			t.Fatal("gc did not retain an execution with pending output", response)
		}
		if state.ProcessDone || run.CleanupTried || d.runs[state.RunID] == nil || d.secrets.redact("test-jit-secret") == "test-jit-secret" {
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
