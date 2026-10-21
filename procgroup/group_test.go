package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessTableScopesKernelIdentities(t *testing.T) {
	snapshot := []byte(
		"10 10 1000 S Mon Oct 19 00:00:00 2026 /runner/bin/Runner.Listener\n" +
			"11 10 1000 S Mon Oct 19 00:00:00 2026 unknown-descendant\n" +
			"12 10 1001 S Mon Oct 19 00:00:00 2026 foreign-user\n" +
			"20 30 1000 S Mon Oct 19 00:00:00 2026 reused-leader\n" +
			"40 40 1000 S Mon Oct 19 00:00:00 2026 unrelated\n")
	leader := ProcessIdentity{PID: 10, PGID: 10, UID: 1000, Started: "kernel:10"}
	for _, tc := range []struct {
		name       string
		identities []ProcessIdentity
		want       []int
	}{
		{"owned group", []ProcessIdentity{leader}, []int{10, 11, 12}},
		{"leader in different group", []ProcessIdentity{{PID: 20, PGID: 20, UID: 1000, Started: "old"}}, []int{20}},
		{"multiple groups", []ProcessIdentity{leader, {PID: 40, PGID: 40}}, []int{10, 11, 12, 40}},
		{"launch PID only", []ProcessIdentity{{PID: 10}}, []int{10}},
		{"no identities", nil, nil},
		{"unlaunched identity", []ProcessIdentity{{}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queried []int
			table, err := parseProcessTable(snapshot, tc.identities, func(pid int) (string, error) {
				queried = append(queried, pid)
				return fmt.Sprintf("kernel:%d", pid), nil
			})
			if err != nil || !slices.Equal(queried, tc.want) || len(table) != len(tc.want) {
				t.Fatal("unexpected kernel identity queries or retained entries", queried, table, err)
			}
			for _, pid := range tc.want {
				if table[pid].Started != fmt.Sprintf("kernel:%d", pid) {
					t.Fatal("kernel start identity was replaced by ps time", table[pid])
				}
			}
			switch tc.name {
			case "owned group":
				state := &Group{UID: 1000, Command: "/runner/bin/Runner.Listener", Process: leader}
				if _, _, err := Inspect(*state, table); err == nil || !strings.Contains(err.Error(), "unexpected user") {
					t.Fatal("foreign-UID descendant was hidden by scoping", err)
				}
				delete(table, 12)
				alive, members, err := Inspect(*state, table)
				if err != nil || !alive || len(members) != 2 || members[1].PID != 11 {
					t.Fatal("unknown owned-group descendant was not verified", members, err)
				}
			case "leader in different group":
				state := &Group{UID: 1000, Process: tc.identities[0]}
				if _, _, err := Inspect(*state, table); err == nil || !strings.Contains(err.Error(), "PID was reused") {
					t.Fatal("reused PID outside its recorded group was hidden", err)
				}
			case "launch PID only":
				if identity, err := LaunchedIdentity(table, 10, 1000); err != nil || identity != leader {
					t.Fatal("gated launch identity was not established", identity, err)
				}
			}
		})
	}
}

func TestScopedProcessTableRetainsUnverifiableMembers(t *testing.T) {
	snapshot := []byte(
		"10 10 1000 S Mon Oct 19 00:00:00 2026 /runner/bin/Runner.Listener\n" +
			"11 10 1000 S Mon Oct 19 00:00:00 2026 inaccessible-descendant\n" +
			"12 10 1000 S Mon Oct 19 00:00:00 2026 vanished-descendant\n")
	identity := ProcessIdentity{PID: 10, PGID: 10, UID: 1000, Started: "old"}
	table, err := parseProcessTable(snapshot, []ProcessIdentity{identity}, func(pid int) (string, error) {
		if pid == 12 {
			return "", syscall.ESRCH
		}
		return "", os.ErrPermission
	})
	if err != nil || len(table) != 2 || table[11].PID != 11 || table[11].Started != "" {
		t.Fatal("unverifiable member was hidden or vanished member retained", table, err)
	}
	if _, _, err := Inspect(Group{UID: 1000, Process: identity}, table); err == nil {
		t.Fatal("unreadable leader identity authorized recovery")
	}
}

func waitForZombie(t *testing.T, pid int) map[int]Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		table, err := Snapshot(t.Context(), []ProcessIdentity{{PID: pid, PGID: pid}})
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

func TestInspectGroupPreservesZombieOnlyHandling(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 0")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Wait() })
	table := waitForZombie(t, command.Process.Pid)
	state := &Group{UID: os.Geteuid(), Process: table[command.Process.Pid].ProcessIdentity}
	if stopped, err := kernelGroupStopped(state.Process.PGID, table); err != nil || !stopped {
		t.Fatal("kernel did not confirm a genuinely zombie-only group", err)
	}
	if stopped, err := kernelGroupStopped(state.Process.PGID, nil); err != nil || stopped {
		t.Fatal("a newly observed zombie was accepted as a stable group", err)
	}
	if alive, _, err := Inspect(*state, table); err != nil || alive {
		t.Fatal("zombie-only group was treated as live", err)
	}
}

func TestKernelProcessStartReportsExitedPID(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	_, err := ProcessStart(command.Process.Pid)
	if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exited process was not reported as absent", err)
	}
}
