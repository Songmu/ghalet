//go:build linux || darwin

package ghalet

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
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

func runnerEnvironment(c *Config, account executionUser, jit string) []string {
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
