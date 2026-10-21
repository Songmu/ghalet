//go:build linux || darwin

package ghalet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	settings "github.com/Songmu/ghalet/config"
)

type executionUser struct {
	Name   string
	Home   string
	UID    int
	GID    int
	Groups []uint32
}

func resolveUser(name string) (executionUser, error) {
	var account *user.User
	var err error
	if name == "" {
		account, err = user.Current()
	} else {
		account, err = user.Lookup(name)
	}
	if err != nil {
		return executionUser{}, fmt.Errorf("resolve execution user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return executionUser{}, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return executionUser{}, err
	}
	if uid == 0 {
		return executionUser{}, errors.New("root jobs are prohibited; root must specify an existing non-root runner.user")
	}
	if os.Geteuid() != 0 && uid != os.Geteuid() {
		return executionUser{}, errors.New("switching runner.user requires root; refusing to fall back to the launching user")
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return executionUser{}, err
	}
	result := executionUser{Name: account.Username, Home: account.HomeDir, UID: uid, GID: gid}
	for _, groupID := range groupIDs {
		n, err := strconv.ParseUint(groupID, 10, 32)
		if err != nil {
			return executionUser{}, err
		}
		result.Groups = append(result.Groups, uint32(n))
	}
	return result, nil
}

func processAttributes(account executionUser) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(account.UID), Gid: uint32(account.GID), Groups: account.Groups}
	}
	return attr
}

func runnerEnvironment(c *settings.Config, account executionUser, jit string) []string {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if runtime.GOOS == "darwin" {
		path = "/opt/homebrew/bin:/opt/homebrew/sbin:" + path
	}
	values := map[string]string{"PATH": path}
	for key, value := range c.Runner.Env {
		values[key] = value
	}
	values["HOME"], values["USER"], values["LOGNAME"] = account.Home, account.Name, account.Name
	if jit != "" {
		values["ACTIONS_RUNNER_INPUT_JITCONFIG"] = jit
	}
	var result []string
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

type processEntry struct {
	ProcessIdentity
	Command string
	Zombie  bool
	Exiting bool
}

func processTable(ctx context.Context, identities []ProcessIdentity) (map[int]processEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,pgid=,uid=,stat=,lstart=,command=")
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect process table: %w", err)
	}
	return parseProcessTable(data, identities, kernelProcessStart)
}

func parseProcessTable(data []byte, identities []ProcessIdentity, processStart func(int) (string, error)) (map[int]processEntry, error) {
	pids, groups := map[int]bool{}, map[int]bool{}
	for _, identity := range identities {
		if identity.PID > 1 {
			pids[identity.PID] = true
		}
		if identity.PGID > 1 {
			groups[identity.PGID] = true
		}
	}
	entries := map[int]processEntry{}
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
		entries[pid] = processEntry{
			ProcessIdentity: ProcessIdentity{PID: pid, PGID: pgid, UID: uid, Started: start},
			Command:         commandText, Zombie: strings.HasPrefix(fields[3], "Z"),
			Exiting: strings.Contains(fields[3], "E"),
		}
	}
	return entries, nil
}

func sameProcess(a, b ProcessIdentity) bool {
	return a.PID > 1 && a.PID == b.PID && a.PGID == b.PGID && a.UID == b.UID && a.Started != "" && a.Started == b.Started
}

func launchedProcessIdentity(table map[int]processEntry, pid, uid int) (ProcessIdentity, error) {
	entry, ok := table[pid]
	if !ok || pid <= 1 || entry.PID != pid || entry.UID != uid || entry.PGID != pid || entry.Started == "" {
		return ProcessIdentity{}, errors.New("cannot establish launched process identity")
	}
	return entry.ProcessIdentity, nil
}

// inspectGroup never treats an absent leader as proof that its children exited.
func inspectGroup(state *RunState, table map[int]processEntry) (bool, []ProcessIdentity, error) {
	if state.Process.PID == 0 {
		if state.Phase == "preparing" || state.Phase == "registered" || state.ProcessDone {
			return false, nil, nil
		}
		return false, nil, errors.New("launch boundary has no recorded process identity; manual inspection required")
	}
	if state.Process.PID < 2 || state.Process.PGID != state.Process.PID || state.Process.UID != state.UID {
		return false, nil, errors.New("state has an invalid process group or execution UID")
	}
	leader, exists := table[state.Process.PID]
	verifiedLeader := exists && sameProcess(state.Process, leader.ProcessIdentity) && !leader.Zombie
	if exists && !sameProcess(state.Process, leader.ProcessIdentity) {
		return false, nil, errors.New("PID was reused or identity changed; refusing to signal or clean")
	}
	if verifiedLeader && !leader.Exiting && !strings.Contains(leader.Command, filepath.Join(state.RunnerDir, "bin", "Runner.Listener")) {
		return false, nil, errors.New("process command does not match the recorded runner directory")
	}
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
			return false, nil, errors.New("process group contains an unexpected user")
		}
		if !verifiedLeader && !sameProcess(known[entry.PID], entry.ProcessIdentity) {
			return false, nil, errors.New("leader exited with unverified group members; inspect remaining processes manually")
		}
		members = append(members, entry.ProcessIdentity)
	}
	if len(members) == 0 {
		// Neither an empty nor a zombie-only ps snapshot rules out a late fork.
		probeErr := syscall.Kill(-state.Process.PGID, 0)
		if errors.Is(probeErr, syscall.ESRCH) {
			return false, nil, nil
		}
		stopped, err := kernelGroupStopped(state.Process.PGID, table)
		if err != nil || !stopped {
			if errors.Is(syscall.Kill(-state.Process.PGID, 0), syscall.ESRCH) {
				return false, nil, nil
			}
			if err != nil {
				return false, nil, fmt.Errorf("cannot verify zombie-only process group: %w", err)
			}
			if probeErr != nil {
				return false, nil, fmt.Errorf("cannot confirm process group absence: %w", probeErr)
			}
			return false, nil, errors.New("kernel inspection cannot rule out live group members; retry inspection")
		}
	}
	if state.ProcessDone && len(members) > 0 {
		return false, nil, errors.New("processes exist for a state already marked stopped")
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return len(members) > 0, members, nil
}

func stopGroup(ctx context.Context, state *RunState, persist func() error) error {
	saved := false
	inspect := func() (bool, error) {
		table, err := processTable(ctx, []ProcessIdentity{state.Process})
		if err != nil {
			return false, err
		}
		alive, members, err := inspectGroup(state, table)
		if err != nil || !alive {
			return alive, err
		}
		if !saved || !slices.Equal(state.Members, members) {
			state.Members = members
			if err := persist(); err != nil {
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
	if err := syscall.Kill(-state.Process.PGID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
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
			if err := syscall.Kill(-state.Process.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
	return errors.New("process group termination could not be confirmed")
}

type workLock struct{ file *os.File }

func lockWorkRoot(path string) (*workLock, error) {
	if err := ensureExecutionDirectory(path); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(path, ".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("workRoot is locked by another daemon or management operation: %w", err)
	}
	return &workLock{file}, nil
}

func (l *workLock) close() error {
	return l.file.Close()
}
