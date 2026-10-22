//go:build linux || darwin

package ghalet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/procgroup"
)

const runnerUserArgument = "__ghalet_runner_user"
const maxRunnerUserRequestBytes = 1024 * 1024

type runnerUserRequest struct {
	Operation   string          `json:"operation"`
	RunnerDir   string          `json:"runnerDir,omitempty"`
	Workspace   string          `json:"workspace,omitempty"`
	Environment []string        `json:"environment,omitempty"`
	Group       procgroup.Group `json:"group,omitempty"`
	Signal      syscall.Signal  `json:"signal,omitempty"`
}

type runnerUserResult struct {
	Error string        `json:"error,omitempty"`
	Errno syscall.Errno `json:"errno,omitempty"`
}

func init() {
	if (len(os.Args) != 2 && len(os.Args) != 3) || os.Args[1] != runnerUserArgument {
		return
	}
	err := runRunnerUser(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner user:", err)
	}
	if len(os.Args) == 2 {
		result := runnerUserResult{}
		if err != nil {
			result.Error = err.Error()
			errors.As(err, &result.Errno)
		}
		err = errors.Join(err, json.NewEncoder(os.Stdout).Encode(result))
	}
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func readRunnerUserRequest(input io.Reader) (runnerUserRequest, *bufio.Reader, error) {
	var request runnerUserRequest
	reader := bufio.NewReader(input)
	// Limit only the request line, not the controller liveness stream.
	var data []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(data)+len(part) > maxRunnerUserRequestBytes {
			return request, nil, errors.New("runner operation exceeds 1 MiB")
		}
		data = append(data, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return request, nil, fmt.Errorf("read runner operation: %w", err)
		}
		break
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, nil, fmt.Errorf("decode runner operation: %w", err)
	}
	return request, reader, nil
}

func runRunnerUser(input io.Reader) error {
	if os.Geteuid() == 0 {
		return errors.New("runner operations require a non-root execution user")
	}
	request, reader, err := readRunnerUserRequest(input)
	if err != nil {
		return err
	}
	switch request.Operation {
	case "check":
		command := exec.Command("/bin/sh", "-c", `test -d "$HOME" && test -w "$1" && test -x "$2"`, "ghalet-access", request.Workspace, filepath.Join(request.RunnerDir, "bin", "Runner.Listener"))
		command.Dir, command.Env = request.RunnerDir, request.Environment
		return command.Run()
	case "launch":
		runner := filepath.Join(request.RunnerDir, "bin", "Runner.Listener")
		if len(os.Args) != 3 || os.Args[2] != runner {
			return errors.New("runner command does not match the launch request")
		}
		if syscall.Getpgrp() != os.Getpid() {
			if err := syscall.Setpgid(0, 0); err != nil {
				return err
			}
		}
		os.Clearenv()
		for _, value := range request.Environment {
			key, text, found := strings.Cut(value, "=")
			if !found {
				return errors.New("invalid runner environment")
			}
			if err := os.Setenv(key, text); err != nil {
				return err
			}
		}
		if err := binary.Write(os.Stdout, binary.LittleEndian, int64(os.Getpid())); err != nil {
			return fmt.Errorf("report runner identity: %w", err)
		}
		return guardRunner(runner, reader, os.Stdout, nil, os.Stderr, os.Stderr)
	case "release":
		for _, path := range []string{request.RunnerDir, request.Workspace} {
			if err := releaseRunnerDirectory(path); err != nil {
				return err
			}
		}
		return nil
	case "signal":
		if request.Group.UID != os.Geteuid() ||
			(request.Signal != syscall.SIGTERM && request.Signal != syscall.SIGKILL) {
			return errors.New("invalid runner signal or execution user")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		table, err := procgroup.Snapshot(ctx, []procgroup.ProcessIdentity{request.Group.Process})
		if err != nil {
			return err
		}
		alive, _, err := procgroup.Inspect(request.Group, table)
		if err != nil || !alive {
			return err
		}
		err = syscall.Kill(-request.Group.Process.PGID, request.Signal)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	default:
		return errors.New("unknown runner operation")
	}
}

func runnerUserCommand(ctx context.Context, account executionUser, request runnerUserRequest) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "sudo", "-n",
		"-u", "#"+strconv.Itoa(account.UID), "-g", "#"+strconv.Itoa(account.GID),
		"--", executable, runnerUserArgument)
	if request.Operation == "launch" {
		command.Args = append(command.Args, filepath.Join(request.RunnerDir, "bin", "Runner.Listener"))
	}
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	command.Stdin = bytes.NewReader(append(data, '\n'))
	return command, nil
}

func callRunnerUser(ctx context.Context, account executionUser, request runnerUserRequest) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	command, err := runnerUserCommand(ctx, account, request)
	if err != nil {
		return err
	}
	command.WaitDelay = time.Second
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	err = command.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var result runnerUserResult
	decodeErr := json.Unmarshal(output.Bytes(), &result)
	var helperErr error
	if result.Error != "" {
		helperErr = errors.New(result.Error)
		if result.Errno != 0 {
			helperErr = fmt.Errorf("%s: %w", result.Error, result.Errno)
		}
	}
	if err = errors.Join(err, decodeErr, helperErr); err != nil {
		if text := strings.TrimSpace(diagnostic.String()); text != "" {
			err = fmt.Errorf("%w: %s", err, text)
		}
		return fmt.Errorf("sudo runner %s failed: %w", request.Operation, err)
	}
	return nil
}

func shareRunnerDirectory(directory string, gid int) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("runner preparation requires a private directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	// Other jobs share the runner UID. Keep them outside until all descendant
	// ownership and permission changes finish, including on preparation failure.
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fshelper.CheckSymlink(root, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := root.Chown(relative, -1, gid); err != nil {
			return err
		}
		mode := info.Mode().Perm()
		mode |= (mode & 0700) >> 3
		if entry.IsDir() {
			mode |= 0770 | os.ModeSetgid
		}
		return root.Chmod(relative, mode)
	}); err != nil {
		return err
	}
	if err := root.Chown(".", -1, gid); err != nil {
		return err
	}
	return root.Chmod(".", 0770|os.ModeSetgid)
}

// Release only runner-owned children after the group is confirmed stopped.
// Their shared group lets the controller archive private logs and remove files.
func releaseRunnerDirectory(path string) error {
	if err := fshelper.CheckDirectory(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("cannot identify runner file owner")
		}
		if int(stat.Uid) != os.Geteuid() || (!entry.IsDir() && !info.Mode().IsRegular()) {
			return nil
		}
		relative, err := filepath.Rel(path, name)
		if err != nil {
			return err
		}
		if err := root.Chown(relative, -1, os.Getegid()); err != nil {
			return err
		}
		mode := info.Mode().Perm() & 0700
		mode |= 0600
		mode |= (mode & 0700) >> 3
		if entry.IsDir() {
			mode |= 0770
		}
		return root.Chmod(relative, mode)
	})
}
