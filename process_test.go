package ghalet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

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
