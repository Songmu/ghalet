package ghalet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitGuardFile(t *testing.T, path string) []byte {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("guard fixture did not create", path)
	return nil
}

func TestRunnerGuardKillsGroupWhenControllerIsKilled(t *testing.T) {
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	state := testState(t, d.config)
	state.Phase = "registered"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := safeDirectory(filepath.Join(state.RunnerDir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(state.RunnerDir, "child-ready")
	script := fmt.Sprintf("#!/bin/sh\ntrap '' TERM\nsleep 60 &\nprintf '%%s %%s\\n' \"$$\" \"$!\" > %q\nwait\n", ready)
	if err := os.WriteFile(filepath.Join(state.RunnerDir, "bin", "Runner.Listener"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(state.RunnerDir, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	controller := exec.Command(os.Args[0], "-test.run=^TestRunnerGuardControllerHelper$")
	dataPath := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
	controller.Env = append(os.Environ(), "GHALET_GUARD_TEST_CONFIG="+d.config.Path, "GHALET_GUARD_TEST_STATE="+dataPath)
	controller.Stdout, controller.Stderr = logFile, logFile
	controller.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			if err := controller.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			controller.Wait()
		}
	})
	waitGuardFile(t, filepath.Join(filepath.Dir(state.RunnerDir), "controller-ready"))
	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, state); err != nil {
		t.Fatal(err)
	}
	d.runs[state.RunID] = &activeRun{State: state}
	children := strings.Fields(string(waitGuardFile(t, ready)))
	if len(children) != 2 {
		t.Fatal("runner fixture did not start its signal-resistant descendant", children)
	}
	table, err := processTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		pid, err := strconv.Atoi(child)
		if err != nil || table[pid].PGID != state.Process.PGID {
			t.Fatal("runner or descendant escaped its owned group", child, err)
		}
	}
	sentinel := exec.Command("/bin/sleep", "60")
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sentinel.Process.Kill()
		sentinel.Wait()
	})
	if err := controller.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = controller.Wait()
	waited = true
	if err == nil {
		t.Fatal("controller was not killed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		table, err = processTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		alive, _, err := inspectGroup(state, table)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("runner group survived controller SIGKILL")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := sentinel.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("guard cleanup signaled an unrelated process", err)
	}
	data, err = os.ReadFile(dataPath)
	if err != nil {
		t.Fatal("parent-death cleanup removed durable recovery state", err)
	}
	var retained RunState
	if err := json.Unmarshal(data, &retained); err != nil || retained.ProcessDone {
		t.Fatal("guard rewrote controller-owned recovery state", err)
	}
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.runs[state.RunID].Uncertain || !d.runs[state.RunID].State.ProcessDone || d.occupied() != 0 {
		t.Fatal("recovery did not confirm the guard's group cleanup")
	}
}

func TestRunnerGuardControllerHelper(t *testing.T) {
	path := os.Getenv("GHALET_GUARD_TEST_STATE")
	if path == "" {
		return
	}
	config, err := LoadConfig(os.Getenv("GHALET_GUARD_TEST_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDaemon(config, slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.guardRunners = true
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state RunState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if err := d.launch(&activeRun{State: &state}, "test-jit-secret"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(state.RunnerDir), "controller-ready"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Hour)
}

func TestRunnerGuardPreservesRunnerExitAndKillsRemainingChildren(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			d, _ := testDaemon(t)
			d.guardRunners = true
			cleanupProcesses(t, d)
			release := filepath.Join(testDirectory(t), "release")
			if err := syscall.Mkfifo(release, 0600); err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("#!/bin/sh\nif ( : <&3 ) 2>/dev/null || ( : <&4 ) 2>/dev/null; then exit 89; fi\ntrap '' TERM\nsleep 60 &\nprintf 'Listening for Jobs\\n'\nread -r release < %q\nprintf 'final output'\nexit %d\n", release, code)
			seedDistribution(t, d.config, script)
			d.targets["build"].Assigned = 1
			var run *activeRun
			pumpUntil(t, d, func() bool {
				for _, candidate := range d.runs {
					if candidate.State.Ready && len(candidate.State.Members) >= 3 {
						run = candidate
						return true
					}
				}
				return false
			})
			d.mu.Lock()
			run.State.Busy, run.State.Phase = true, "busy"
			d.targets["build"].Assigned = 0
			d.mu.Unlock()
			gate, err := os.OpenFile(release, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := gate.WriteString("exit\n")
			if err := errors.Join(writeErr, gate.Close()); err != nil {
				t.Fatal(err)
			}
			pumpUntil(t, d, func() bool { return d.runs[run.State.RunID] == nil })
			wantError := ""
			if code != 0 {
				wantError = "exit status " + strconv.Itoa(code)
			}
			if run.ExitError != wantError || !run.Exited || !run.outputCollected() || d.occupied() != 0 {
				t.Fatal("guard exit replaced runner exit or left unfinished observers", run.ExitError)
			}
			data, err := os.ReadFile(filepath.Join(d.config.Cleanup.LogRoot, "runs", run.State.RunID, "output.log"))
			if err != nil || string(data) != "Listening for Jobs\nfinal output" {
				t.Fatalf("guard lost final runner output: %q (%v)", data, err)
			}
		})
	}
}

func TestRunnerGuardRefusesSharedGroup(t *testing.T) {
	command, err := runnerGuardCommand("/not-executed/Runner.Listener")
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "not led by this process") {
		t.Fatal("guard did not refuse the caller's process group", err, string(output))
	}
}

func TestRunnerGuardClosedGateDoesNotExecuteRunner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runner tests require a non-root execution user")
	}
	dir := testDirectory(t)
	runner, marker := filepath.Join(dir, "Runner.Listener"), filepath.Join(dir, "executed")
	if err := os.WriteFile(runner, []byte(fmt.Sprintf("#!/bin/sh\nprintf executed > %q\n", marker)), 0700); err != nil {
		t.Fatal(err)
	}
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	report, reportOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer report.Close()
	defer reportOutput.Close()
	command, err := runnerGuardCommand(runner)
	if err != nil {
		t.Fatal(err)
	}
	command.ExtraFiles = []*os.File{input, reportOutput}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	input.Close()
	output.Close()
	reportOutput.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		command.Process.Kill()
	})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed launch gate returned a successful runner execution")
		}
	case <-time.After(5 * time.Second):
		command.Process.Kill()
		<-done
		t.Fatal("guard remained alive after controller pipe closed")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runner executed without a durably released launch gate", err)
	}
}

func TestRunnerGuardResultValidation(t *testing.T) {
	for _, tc := range []struct {
		report string
		want   string
	}{
		{report: `{"completed":true}`},
		{report: `{"completed":true,"error":"exit status 7"}`, want: "exit status 7"},
		{report: `{}`, want: "did not report a completed"},
		{report: `{"completed":`, want: "read runner guard result"},
	} {
		t.Run(tc.report, func(t *testing.T) {
			input, output, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := output.WriteString(tc.report); err != nil {
				t.Fatal(err)
			}
			output.Close()
			err = readRunnerGuardResult(input, errors.New("signal: killed"))
			if tc.want == "" {
				if err != nil {
					t.Fatal("successful runner inherited guard SIGKILL", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal("guard result was not validated", err)
			}
		})
	}
}
