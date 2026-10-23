//go:build linux || darwin

// Package procgroup verifies and terminates process groups using kernel identities.
package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcessIdentity binds a PID and group to its execution user and kernel birth identity.
type ProcessIdentity struct {
	PID     int    `json:"pid"`
	PGID    int    `json:"pgid"`
	UID     int    `json:"uid"`
	Started string `json:"started"`
}

// Group contains only the recorded facts needed to authorize inspection and signaling.
// Command is the expected leader command substring; callers decide which unlaunched
// lifecycle states are safe through AllowUnlaunched.
type Group struct {
	Process         ProcessIdentity
	Members         []ProcessIdentity
	UID             int
	Command         string
	AllowUnlaunched bool
	Done            bool
}

// Entry is one observed process, including status needed to verify group lifetime.
type Entry struct {
	ProcessIdentity
	Command string
	Zombie  bool
	Exiting bool
}

// Snapshot observes recorded PIDs and every member of their recorded groups.
func Snapshot(ctx context.Context, identities []ProcessIdentity) (map[int]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,pgid=,uid=,stat=,lstart=,command=")
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect process table: %w", err)
	}
	return parseProcessTable(data, identities, ProcessStart)
}

func parseProcessTable(data []byte, identities []ProcessIdentity, processStart func(int) (string, error)) (map[int]Entry, error) {
	pids, groups := map[int]bool{}, map[int]bool{}
	for _, identity := range identities {
		if identity.PID > 1 {
			pids[identity.PID] = true
		}
		if identity.PGID > 1 {
			groups[identity.PGID] = true
		}
	}
	entries := map[int]Entry{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 10 {
			return nil, fmt.Errorf("cannot parse process identity: %q", line)
		}
		commandText := strings.TrimLeft(line, " \t")
		for _, field := range fields[:9] {
			commandText = strings.TrimLeft(commandText[len(field):], " \t")
		}
		pid, err1 := strconv.Atoi(fields[0])
		pgid, err2 := strconv.Atoi(fields[1])
		uid, err3 := strconv.Atoi(fields[2])
		if errors.Join(err1, err2, err3) != nil {
			return nil, errors.New("invalid numeric process identity")
		}
		// Match the recorded PID independently of its current group so PID
		// reuse cannot masquerade as an absent leader.
		if !pids[pid] && !groups[pgid] {
			continue
		}
		start, startErr := processStart(pid)
		if errors.Is(startErr, os.ErrNotExist) || errors.Is(startErr, syscall.ESRCH) {
			continue
		}
		if startErr != nil {
			// Keep the group member visible, but an empty start identity can
			// never authorize a signal or recovery.
			start = ""
		}
		entries[pid] = Entry{
			ProcessIdentity: ProcessIdentity{PID: pid, PGID: pgid, UID: uid, Started: start},
			Command:         commandText, Zombie: strings.HasPrefix(fields[3], "Z"),
			Exiting: strings.Contains(fields[3], "E"),
		}
	}
	return entries, nil
}

// SameProcess compares complete process identities, never accepting an empty start.
func SameProcess(a, b ProcessIdentity) bool {
	return a.PID > 1 && a.PID == b.PID && a.PGID == b.PGID && a.UID == b.UID && a.Started != "" && a.Started == b.Started
}

// LaunchedIdentity verifies a newly launched leader before its execution gate opens.
func LaunchedIdentity(table map[int]Entry, pid, uid int) (ProcessIdentity, error) {
	entry, ok := table[pid]
	if !ok || pid <= 1 || entry.PID != pid || entry.UID != uid || entry.PGID != pid || entry.Started == "" {
		return ProcessIdentity{}, errors.New("cannot establish launched process identity")
	}
	return entry.ProcessIdentity, nil
}

// Inspect never treats an absent leader as proof that its children exited.
func Inspect(state Group, table map[int]Entry) (bool, []ProcessIdentity, error) {
	if state.Process.PID == 0 {
		if state.AllowUnlaunched || state.Done {
			return false, nil, nil
		}
		return false, nil, errors.New("launch boundary has no recorded process identity; manual inspection required")
	}
	verifiedLeader, err := verifyGroupLeader(state, table)
	if err != nil {
		return false, nil, err
	}
	members, err := liveGroupMembers(state, table, verifiedLeader)
	if err != nil {
		return false, nil, err
	}
	if len(members) == 0 {
		if err := confirmGroupStopped(state.Process.PGID, table); err != nil {
			return false, nil, err
		}
	}
	if state.Done && len(members) > 0 {
		return false, nil, errors.New("processes exist for a state already marked stopped")
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return len(members) > 0, members, nil
}

func verifyGroupLeader(state Group, table map[int]Entry) (bool, error) {
	if state.Process.PID < 2 || state.Process.PGID != state.Process.PID || state.Process.UID != state.UID {
		return false, errors.New("state has an invalid process group or execution UID")
	}
	leader, exists := table[state.Process.PID]
	verifiedLeader := exists && SameProcess(state.Process, leader.ProcessIdentity) && !leader.Zombie
	if exists && !SameProcess(state.Process, leader.ProcessIdentity) {
		return false, errors.New("PID was reused or identity changed; refusing to signal or clean")
	}
	if verifiedLeader && !leader.Exiting && !strings.Contains(leader.Command, state.Command) {
		return false, errors.New("process command does not match the recorded runner directory")
	}
	return verifiedLeader, nil
}

func liveGroupMembers(state Group, table map[int]Entry, verifiedLeader bool) ([]ProcessIdentity, error) {
	known := map[int]ProcessIdentity{}
	for _, member := range state.Members {
		known[member.PID] = member
	}
	var members []ProcessIdentity
	for _, entry := range table {
		if entry.PGID != state.Process.PGID {
			continue
		}
		if entry.Zombie {
			continue
		}
		if entry.UID != state.UID {
			return nil, errors.New("process group contains an unexpected user")
		}
		if !verifiedLeader && !SameProcess(known[entry.PID], entry.ProcessIdentity) {
			return nil, errors.New("leader exited with unverified group members; inspect remaining processes manually")
		}
		members = append(members, entry.ProcessIdentity)
	}
	return members, nil
}

func confirmGroupStopped(pgid int, table map[int]Entry) error {
	// Neither an empty nor a zombie-only ps snapshot rules out a late fork.
	probeErr := syscall.Kill(-pgid, 0)
	if errors.Is(probeErr, syscall.ESRCH) {
		return nil
	}
	stopped, err := kernelGroupStopped(pgid, table)
	if err != nil || !stopped {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot verify zombie-only process group: %w", err)
		}
		if probeErr != nil {
			return fmt.Errorf("cannot confirm process group absence: %w", probeErr)
		}
		return errors.New("kernel inspection cannot rule out live group members; retry inspection")
	}
	return nil
}

// Stop sends TERM, polls through a grace period, then sends KILL if needed.
// persist must durably record verified members before returning nil; it is called
// before the first signal and whenever inspection discovers changed membership.
func Stop(ctx context.Context, state Group, persist func([]ProcessIdentity) error) error {
	return StopWithSignal(ctx, state, persist, func(group Group, signal syscall.Signal) error {
		return syscall.Kill(-group.Process.PGID, signal)
	})
}

// StopWithSignal preserves Stop's identity and persistence checks while
// delegating signals to a caller authorized to act as the execution user.
func StopWithSignal(ctx context.Context, state Group, persist func([]ProcessIdentity) error, signal func(Group, syscall.Signal) error) error {
	saved := false
	inspect := func() (bool, error) {
		table, err := Snapshot(ctx, []ProcessIdentity{state.Process})
		if err != nil {
			return false, err
		}
		alive, members, err := Inspect(state, table)
		if err != nil || !alive {
			return alive, err
		}
		if !saved || !slices.Equal(state.Members, members) {
			state.Members = members
			if err := persist(members); err != nil {
				return false, fmt.Errorf("persist verified process group before signaling: %w", err)
			}
			saved = true
		}
		return true, nil
	}
	alive, err := inspect()
	if err != nil || !alive {
		return err
	}
	if err := signal(state, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		alive, err = inspect()
		if err != nil || !alive {
			return err
		}
		if i == 19 {
			if err := signal(state, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
	return errors.New("process group termination could not be confirmed")
}
