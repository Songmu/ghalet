package ghalet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Songmu/ghalet/fshelper"
)

func TestEmptyProcessSnapshotRetainsForkedDescendant(t *testing.T) {
	testIncompleteGroupSnapshot(t, false)
}

func TestZombieProcessSnapshotRetainsForkedDescendant(t *testing.T) {
	testIncompleteGroupSnapshot(t, true)
}

func waitForZombie(t *testing.T, pid int) map[int]processEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		table, err := processTable(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if table[pid].Zombie {
			return table
		}
		if time.Now().After(deadline) {
			t.Fatal("process did not become an unreaped zombie", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testIncompleteGroupSnapshot(t *testing.T, zombieLeader bool) {
	t.Helper()
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	gate, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	defer release.Close()
	output, err := os.Create(filepath.Join(testDirectory(t), "child-pid"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	command := exec.Command("/bin/sh", "-c", "read -r release || exit 1\nsleep 60 </dev/null >/dev/null 2>&1 &\nprintf '%s\\n' \"$!\"\n")
	command.Stdin, command.Stdout = gate, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !waited {
			command.Wait()
		}
	})
	start, err := kernelProcessStart(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = "busy"
	state.Process = ProcessIdentity{PID: command.Process.Pid, PGID: command.Process.Pid, UID: state.UID, Started: start}
	state.Members = []ProcessIdentity{state.Process}
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := release.WriteString("fork\n"); err != nil {
		t.Fatal(err)
	}
	if zombieLeader {
		waitForZombie(t, command.Process.Pid)
	} else {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
		waited = true
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	var childPID int
	if _, err := fmt.Sscanf(string(data), "%d", &childPID); err != nil {
		t.Fatal(err)
	}
	table, err := processTable(t.Context())
	if err != nil || table[childPID].PID != childPID || table[childPID].Zombie {
		t.Fatal("forked descendant did not survive its exited leader", err)
	}

	// Reproduce ps listing the leader before the fork but omitting its new child.
	dir := testDirectory(t)
	status, wantEntries := "S", 0
	if zombieLeader {
		status, wantEntries = "Z", 1
	}
	snapshot := fmt.Sprintf("%d %d %d %s Mon Oct 19 00:00:00 2026 %s/bin/Runner.Listener", state.Process.PID, state.Process.PGID, state.UID, status, state.RunnerDir)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s'\n", snapshot)
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	table, err = processTable(t.Context())
	if err != nil || len(table) != wantEntries {
		t.Fatal("test did not reproduce the incomplete process snapshot", err)
	}
	if _, _, err := inspectGroup(state, table); err == nil {
		t.Fatal("incomplete snapshot was accepted while a descendant still existed")
	}
	persisted := false
	if err := stopGroup(t.Context(), state, func() error { persisted = true; return nil }); err == nil || persisted {
		t.Fatal("unverified descendant was treated as terminated or authorized signaling", err)
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatal("inspection signaled the unverified descendant", err)
	}
	if err := d.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.reconcile()
	run := d.runs[state.RunID]
	if run == nil || !run.Uncertain || run.State.ProcessDone || run.GroupStopped || d.occupied() != 1 || d.drained("") {
		t.Fatal("incomplete snapshot released capacity or allowed shutdown")
	}
	if response := d.gc(t.Context(), false); response.Error == "" {
		t.Fatal("gc accepted the incomplete snapshot")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state.RunnerDir), "state.json")); err != nil {
		t.Fatal("incomplete inspection removed the recovery record", err)
	}
}

func TestInspectGroupPreservesZombieOnlyHandling(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 0")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Wait() })
	table := waitForZombie(t, command.Process.Pid)
	state := &RunState{UID: os.Geteuid(), Process: table[command.Process.Pid].ProcessIdentity}
	if stopped, err := kernelGroupStopped(state.Process.PGID, table); err != nil || !stopped {
		t.Fatal("kernel did not confirm a genuinely zombie-only group", err)
	}
	if stopped, err := kernelGroupStopped(state.Process.PGID, nil); err != nil || stopped {
		t.Fatal("a newly observed zombie was accepted as a stable group", err)
	}
	if alive, _, err := inspectGroup(state, table); err != nil || alive {
		t.Fatal("zombie-only group was treated as live", err)
	}
}

func TestKernelProcessStartReportsExitedPID(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	_, err := kernelProcessStart(command.Process.Pid)
	if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exited process was not reported as absent", err)
	}
}

func TestRunnerPathPreservesConsecutiveSpaces(t *testing.T) {
	d, _ := testDaemon(t)
	d.config.Runner.WorkRoot = filepath.Join(testDirectory(t), "work  root")
	d.store = stateStore{root: d.config.Runner.WorkRoot}
	if err := fshelper.EnsureDirectory(d.config.Runner.WorkRoot, 0711); err != nil {
		t.Fatal(err)
	}
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, "#!/bin/sh\nprintf 'Listening for Jobs\\n'\nsleep 60\n")
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
	table, err := processTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table[run.State.Process.PID].Command, run.State.RunnerDir) {
		t.Fatal("process command collapsed consecutive spaces", table[run.State.Process.PID].Command)
	}
	if alive, _, err := inspectGroup(run.State, table); err != nil || !alive {
		t.Fatal("valid runner in a consecutive-space path was rejected", err)
	}
	d.mu.Lock()
	d.targets["build"].Assigned = 0
	d.beginDrain(d.targets["build"])
	d.mu.Unlock()
	pumpUntil(t, d, func() bool { return len(d.runs) == 0 })
}

func startTerminationRunner(t *testing.T, script string) (*daemon, *activeRun) {
	t.Helper()
	d, _ := testDaemon(t)
	cleanupProcesses(t, d)
	seedDistribution(t, d.config, script)
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
	return d, run
}

func TestStopGroupPersistsNewDescendants(t *testing.T) {
	for _, duringTermination := range []bool{false, true} {
		t.Run(map[bool]string{false: "before termination", true: "during termination"}[duringTermination], func(t *testing.T) {
			dir := testDirectory(t)
			fifo := filepath.Join(dir, "child-ready")
			release := filepath.Join(dir, "release")
			terminated := filepath.Join(dir, "terminated")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			child := fmt.Sprintf("(\n  trap '' TERM\n  printf 'ready\\n' > %q\n  exec sleep 30\n) &\nread -r childReady < %q\n", fifo, fifo)
			var script string
			if duringTermination {
				script = fmt.Sprintf("#!/bin/sh\nfinish() {\n%swhile [ ! -f %q ]; do :; done\nprintf 'term\\n' > %q\nexit 0\n}\ntrap finish TERM\nsleep 30 &\nprintf 'Listening for Jobs\\n'\nwait\n", child, release, terminated)
			} else {
				script = fmt.Sprintf("#!/bin/sh\nfinish() {\nprintf 'term\\n' > %q\nexit 0\n}\ntrap finish TERM\n%sprintf 'Listening for Jobs\\n'\nwait\n", terminated, child)
			}
			d, run := startTerminationRunner(t, script)
			original := make(map[int]ProcessIdentity)
			for _, member := range run.State.Members {
				original[member.PID] = member
			}
			run.State.Members = []ProcessIdentity{run.State.Process}
			if err := d.save(run); err != nil {
				t.Fatal(err)
			}
			saves := 0
			released := false
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := stopGroup(ctx, run.State, func() error {
				if err := d.store.save(run.State); err != nil {
					return err
				}
				saves++
				if saves == 1 {
					if _, err := os.Stat(terminated); !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("leader received SIGTERM before membership was saved: %v", err)
					}
				}
				if duringTermination && !released {
					for _, member := range run.State.Members {
						if !sameProcess(original[member.PID], member) {
							if err := os.WriteFile(release, []byte("saved\n"), 0600); err != nil {
								return err
							}
							released = true
							break
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal("newly verified descendants prevented group termination", err)
			}
			if saves < 2 || (duringTermination && !released) {
				t.Fatal("verified membership was not refreshed and persisted", saves, released)
			}
			if _, err := os.Stat(terminated); err != nil {
				t.Fatal("leader did not exit through its SIGTERM handler", err)
			}
			table, err := processTable(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if alive, _, err := inspectGroup(run.State, table); err != nil || alive {
				t.Fatal("signal-resistant descendants survived", err)
			}
			records, err := d.store.load()
			if err != nil || len(records) != 1 || records[0].Err != nil ||
				!slices.Equal(records[0].State.Members, run.State.Members) {
				t.Fatal("verified termination membership was not persisted", records, err)
			}
			d.mu.Lock()
			d.targets["build"].Assigned = 0
			err = d.stop(run, true)
			d.mu.Unlock()
			if err != nil || d.occupied() != 0 {
				t.Fatal("stopped group did not release capacity and finish cleanup", err)
			}
		})
	}
}

func TestStopGroupDoesNotSignalBeforeMembershipSave(t *testing.T) {
	terminated := filepath.Join(testDirectory(t), "terminated")
	script := fmt.Sprintf("#!/bin/sh\nfinish() {\nprintf 'term\\n' > %q\nexit 0\n}\ntrap finish TERM\nsleep 30 &\nprintf 'Listening for Jobs\\n'\nwait\n", terminated)
	d, run := startTerminationRunner(t, script)
	dir := filepath.Dir(run.State.RunnerDir)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0711); err != nil {
			t.Error(err)
		}
	})
	d.mu.Lock()
	err := d.stop(run, true)
	d.mu.Unlock()
	if !errors.Is(err, os.ErrPermission) || !run.Uncertain || run.State.ProcessDone {
		t.Fatal("membership persistence failure was ignored", err)
	}
	if _, err := os.Stat(terminated); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runner was signaled before membership became durable", err)
	}
	table, err := processTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if alive, _, err := inspectGroup(run.State, table); err != nil || !alive || d.occupied() != 1 {
		t.Fatal("failed persistence stopped the runner or released its capacity", err)
	}
}
