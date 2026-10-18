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
