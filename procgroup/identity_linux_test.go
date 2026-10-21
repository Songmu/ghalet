package procgroup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxGroupStoppedRequiresCompleteSnapshot(t *testing.T) {
	boot, err := linuxBootID()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		state    string
		threads  string
		missing  bool
		want     bool
		wantErr  bool
		newPID   bool
		group    int
		groupErr error
		started  string
		fork     string
		queries  int
	}{
		{name: "zombie only", state: "Z", threads: "1", want: true},
		{name: "live omitted child", state: "S", threads: "1"},
		{name: "zombie leader with live threads", state: "Z", threads: "2"},
		{name: "vanished group member", missing: true, wantErr: true, queries: 3},
		{name: "vanished unrelated PID", missing: true, group: 9999, want: true, queries: 1},
		{name: "unclassified PID churn needs confirmation", missing: true, groupErr: unix.ESRCH, newPID: true, want: true, queries: 2},
		{name: "vanished known group member before query", missing: true, groupErr: unix.ESRCH, wantErr: true, queries: 3},
		{name: "unclassified PID forked omitted live child", missing: true, groupErr: unix.ESRCH, newPID: true, fork: "S", queries: 2},
		{name: "unclassified PID forked omitted zombie child", missing: true, groupErr: unix.ESRCH, newPID: true, fork: "Z", queries: 2},
		{name: "unreadable membership", groupErr: unix.EPERM, wantErr: true},
		{name: "membership changed after query", state: "Z", threads: "1", group: 9999},
		{name: "new zombie could have forked", state: "Z", threads: "1", newPID: true},
		{name: "reused zombie PID", state: "Z", threads: "1", started: "43"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			previous := map[int]Entry{
				1234: {ProcessIdentity: ProcessIdentity{PID: 1234, PGID: 1234, Started: fmt.Sprintf("linux:%s:42", boot)}, Zombie: true},
				5678: {ProcessIdentity: ProcessIdentity{PID: 5678, PGID: 1234, Started: fmt.Sprintf("linux:%s:42", boot)}, Zombie: true},
			}
			if tc.newPID {
				delete(previous, 5678)
			}
			for _, process := range []struct{ pid, state, threads string }{
				{"1234", "Z", "1"},
				{"5678", tc.state, tc.threads},
			} {
				dir := filepath.Join(root, process.pid)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if tc.missing && process.pid == "5678" {
					continue
				}
				fields := strings.Fields("Z 1 1234 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1 0 42")
				fields[0], fields[17] = process.state, process.threads
				if process.pid == "5678" {
					if tc.group != 0 {
						fields[2] = fmt.Sprint(tc.group)
					}
					if tc.started != "" {
						fields[19] = tc.started
					}
				}
				data := fmt.Sprintf("%s (test (process)) %s\n", process.pid, strings.Join(fields, " "))
				if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			queries := 0
			groupForPID := func(pid int) (int, error) {
				if pid == 5678 {
					queries++
					if queries == 1 && tc.fork != "" {
						dir := filepath.Join(root, "9012")
						if err := os.Mkdir(dir, 0700); err != nil {
							t.Fatal(err)
						}
						data := fmt.Sprintf("9012 (forked) %s 1 1234 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1 0 42\n", tc.fork)
						if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(data), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if tc.groupErr != nil {
						return 0, tc.groupErr
					}
					if tc.missing && tc.group != 0 {
						return tc.group, nil
					}
				}
				return 1234, nil
			}
			stopped, err := linuxGroupStopped(root, 1234, previous, groupForPID)
			if stopped != tc.want || (err != nil) != tc.wantErr {
				t.Fatal("incomplete kernel snapshot authorized cleanup", stopped, err)
			}
			if tc.queries != 0 && queries != tc.queries {
				t.Fatal("unexpected confirmation count", queries, tc.queries)
			}
		})
	}
}

func TestLinuxGroupStoppedIgnoresVanishedUnrelatedProcess(t *testing.T) {
	leader := exec.Command("/bin/sh", "-c", "exit 0")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leader.Wait() })
	previous := waitForZombie(t, leader.Process.Pid)

	unrelated := exec.Command("/bin/sleep", "60")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unrelated.Process.Kill()
		unrelated.Wait()
	})

	root := t.TempDir()
	for _, pid := range []int{leader.Process.Pid, unrelated.Process.Pid} {
		if err := os.Mkdir(filepath.Join(root, strconv.Itoa(pid)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", leader.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(leader.Process.Pid), "stat"), data, 0600); err != nil {
		t.Fatal(err)
	}
	checkedUnrelated := false
	groupForPID := func(pid int) (int, error) {
		group, err := unix.Getpgid(pid)
		if err == nil && pid == unrelated.Process.Pid {
			checkedUnrelated = true
			if err := unrelated.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			unrelated.Wait()
		}
		return group, err
	}
	stopped, err := linuxGroupStopped(root, leader.Process.Pid, previous, groupForPID)
	if err != nil || !stopped || !checkedUnrelated {
		t.Fatal("unrelated PID disappearance blocked zombie-only group confirmation", stopped, err)
	}
}
