package ghalet

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

func TestControlResponseSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "snapshot", payload: `{"status":{"config":"/config.yaml"}}`},
		{name: "unknown field", payload: `{"unknown":"compatible","status":{"config":"/config.yaml"}}`},
		{name: "exact limit", payload: `{"error":"` + strings.Repeat("x", maxControlResponseSize-len(`{"error":""}`)) + `"}`},
		{name: "oversized known field", payload: `{"error":"` + strings.Repeat("x", maxControlResponseSize) + `"}`, wantErr: true},
		{name: "oversized unknown field first", payload: `{"unknown":"` + strings.Repeat("x", maxControlResponseSize) + `","status":{}}`, wantErr: true},
		{name: "oversized unknown field last", payload: `{"status":{},"unknown":"` + strings.Repeat("x", maxControlResponseSize) + `"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.payload)
			_, err := decodeControlResponse(reader)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
					t.Fatal("oversized response was not rejected", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(tc.payload)-reader.Len() > maxControlResponseSize+1 {
				t.Fatal("decoder read beyond its response limit")
			}
		})
	}
}

func TestControlRejectsWrongPeerUIDBeforeSendingRequest(t *testing.T) {
	path := filepath.Join(discoveryRoot(t), "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	type received struct {
		data []byte
		err  error
	}
	done := make(chan received, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- received{err: err}
			return
		}
		defer conn.Close()
		data, err := io.ReadAll(conn)
		done <- received{data: data, err: err}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()
	_, err = controlCallSocket(ctx, path, os.Geteuid()+1, controlRequest{Command: "apply"})
	if err == nil || !strings.Contains(err.Error(), "does not match expected UID") {
		t.Fatal("mismatched control peer was accepted", err)
	}
	result := <-done
	if result.err != nil || len(result.data) != 0 {
		t.Fatal("client sent a command before checking the peer UID", string(result.data), result.err)
	}
}

func TestDiscoveredSocketReplacementCannotRetargetCommands(t *testing.T) {
	original, _ := testDaemon(t)
	server, err := listenControl(original.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(original)
	daemons, err := ListDaemons(t.Context(), original.config.Path)
	if err != nil || len(daemons) != 1 {
		t.Fatal("could not discover the original daemon", daemons, err)
	}
	endpoint, err := selectControl(original.config.Path, daemons)
	if err != nil {
		t.Fatal(err)
	}
	other, api := testDaemon(t)
	otherConfig := other.config
	otherServer, err := listenControl(other.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherServer.close(); os.Remove(otherServer.path + ".lock") })
	go otherServer.serve(other)
	server.close()
	if err := os.Symlink(otherServer.path, server.path); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"status", "doctor", "apply", "gc"} {
		response, err := endpoint.call(t.Context(), controlRequest{Command: command})
		if err != nil || !strings.Contains(response.Error, "does not match expected config") ||
			response.Status != nil || len(response.Results) != 0 {
			t.Fatal("socket replacement retargeted a management command", command, response, err)
		}
	}
	if other.targets["build"].Draining || other.config != otherConfig || api.deleteCalls != 0 {
		t.Fatal("socket replacement changed the other daemon")
	}
}

func useFakeManagement(d *daemon, api *fakeScaleAPI) {
	d.clientFactory = func(context.Context, settings.Target) (scaleAPI, error) { return api, nil }
	d.listen = func(target *targetRuntime) {
		target.ListenerReady = make(chan struct{})
		close(target.ListenerReady)
		target.ListenerDone = make(chan struct{})
		target.Cancel = sync.OnceFunc(func() { close(target.ListenerDone) })
	}
	target := d.targets["build"]
	target.Cancel = func() {}
	target.ListenerDone = make(chan struct{})
	close(target.ListenerDone)
}

func TestCleanupDoesNotCountPendingObserversAsAttempt(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ProcessDone = true
	run := &activeRun{State: state, output: &runOutputState{done: make(chan struct{})}}
	d.runs[state.RunID] = run
	if err := d.cleanup(run); err == nil || run.CleanupTried || !run.CleanupRetryAt.IsZero() || d.drained("build") {
		t.Fatal("pending output counted as a cleanup attempt", err)
	}
	close(run.output.done)
	if err := d.cleanup(run); err == nil || run.CleanupTried || !run.CleanupRetryAt.IsZero() || d.drained("build") {
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

func TestDisconnectedControlCannotBypassWorkRootLock(t *testing.T) {
	c := testConfig(t)
	lock, err := lockWorkRoot(c.Runner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, err = GC(t.Context(), c.Path, false)
	if err == nil || !strings.Contains(err.Error(), "refusing direct operation") {
		t.Fatal("disconnected socket bypassed live workRoot lock", err)
	}
}

func TestControlRejectsRemovedCommands(t *testing.T) {
	d, api := testDaemon(t)
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	for _, command := range []string{"drain", "destroy", "destroy-plan"} {
		response, err := controlCall(t.Context(), d.config.Path, controlRequest{Command: command})
		if err != nil || response.Error != "unknown control command" || response.Status != nil {
			t.Fatal("removed command was not rejected", response, err)
		}
	}
	if d.targets["build"].Draining || api.deleteCalls != 0 {
		t.Fatal("rejected control request changed runtime state")
	}
}

func TestDrainSelectsTarget(t *testing.T) {
	d, _ := testDaemon(t)
	useFakeManagement(d, &fakeScaleAPI{})
	if response := d.drain(t.Context(), "missing"); response.Error == "" {
		t.Fatal("unknown target was accepted")
	}
	response := d.drain(t.Context(), "build")
	if response.Error != "" || response.Status == nil || len(response.Status.Targets) != 1 || !response.Status.Targets[0].Draining {
		t.Fatal("selected target was not drained", response)
	}
}

func TestConfigPathDoesNotSearchCurrentDirectory(t *testing.T) {
	home := testDirectory(t)
	t.Setenv("HOME", home)
	path, err := settings.ResolvePath("")
	if err != nil || path != filepath.Join(home, ".config", "ghalet", "config.yaml") {
		t.Fatal(path, err)
	}
}

func TestGCWaitsForOutputCollectionBeforeArchiving(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.Phase = "registered"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state.RunnerDir, "output.log")
	if err := os.WriteFile(source, []byte("partial output"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, output: &runOutputState{done: make(chan struct{})}, JITSecrets: d.secrets.AddJIT("test-jit-secret")}
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
	run.output.ready = true
	close(run.output.done)
	if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] == nil || state.ProcessDone {
		t.Fatal("gc cleaned output before observing the runner exit", response.Error)
	}
	d.processEvent(runEvent{id: state.RunID, kind: "exit"})
	if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] != nil {
		t.Fatal("gc did not resume after output collection", response.Error)
	}
	data, err := os.ReadFile(filepath.Join(d.config.Logs.Root, "runs", state.RunID, "output.log"))
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
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state.RunnerDir, "output.log")
	if err := os.WriteFile(source, []byte("retained output"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state, output: &runOutputState{done: make(chan struct{}), err: errTestAPI}, Exited: true}
	close(run.output.done)
	d.runs[state.RunID] = run
	response := d.gc(t.Context(), false)
	if response.Error == "" || d.runs[state.RunID] == nil || !strings.Contains(state.Error, "collect runner output") || !run.CleanupRetryAt.IsZero() {
		t.Fatal("output failure was ignored", response)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "retained output" {
		t.Fatal("collection failure removed the original output", err)
	}
}
