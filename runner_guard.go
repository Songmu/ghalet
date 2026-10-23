//go:build linux || darwin

package ghalet

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const runnerGuardArgument = "__ghalet_runner_guard"

type runnerGuardResult struct {
	Completed bool   `json:"completed"`
	Error     string `json:"error,omitempty"`
}

func runnerGuardCommand(runner string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(executable, runnerGuardArgument, runner), nil
}

func killGuardGroup() {
	if err := syscall.Kill(-os.Getpid(), syscall.SIGKILL); err != nil {
		fmt.Fprintln(os.Stderr, "runner guard: kill owned process group:", err)
	}
}

// RunRunnerGuard supervises a runner using the inherited controller-liveness
// and exit-report descriptors and the process's standard streams.
func RunRunnerGuard(runner string) error {
	gate, report := os.NewFile(3, "controller-liveness"), os.NewFile(4, "runner-exit")
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	if err := guardRunner(runner, bufio.NewReader(gate), report, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("runner guard: %w", err)
	}
	return errors.New("runner guard exited without terminating its process group")
}

func guardRunner(runner string, reader *bufio.Reader, report io.WriteCloser, input io.Reader, stdout, stderr io.Writer) (retErr error) {
	if os.Getpid() <= 1 || syscall.Getpgrp() != os.Getpid() {
		return errors.New("refusing to guard a process group not led by this process")
	}
	// Keeping the leader alive pins the group ID until the final group-wide
	// kill, even when controller-side process inspection cannot verify it.
	defer func() {
		if retErr != nil {
			fmt.Fprintln(os.Stderr, "runner guard:", retErr)
		}
		killGuardGroup()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE)
	ready, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read launch gate: %w", err)
	}
	if ready != "ready\n" {
		return errors.New("invalid launch gate")
	}
	go func() {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			fmt.Fprintln(os.Stderr, "runner guard: controller connection:", err)
		}
		killGuardGroup()
	}()
	command := exec.Command(runner, "run")
	command.Stdin, command.Stdout, command.Stderr = input, stdout, stderr
	err = command.Run()
	result := runnerGuardResult{Completed: true}
	if err != nil {
		result.Error = err.Error()
	}
	if err := json.NewEncoder(report).Encode(result); err != nil {
		return fmt.Errorf("report runner exit: %w", err)
	}
	if err := report.Close(); err != nil {
		return fmt.Errorf("close runner exit report: %w", err)
	}
	// The guard catches TERM, but the runner and its descendants receive it.
	// KILL must include the guard itself so no fork race can leave survivors.
	if err := syscall.Kill(-os.Getpid(), syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate owned process group: %w", err)
	}
	time.Sleep(2 * time.Second)
	return nil
}

func readRunnerGuardResult(report *os.File, waitErr error) error {
	defer report.Close()
	var result runnerGuardResult
	decoder := json.NewDecoder(io.LimitReader(report, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return errors.Join(waitErr, fmt.Errorf("read runner guard result: %w", err))
	}
	if !result.Completed {
		return errors.Join(waitErr, errors.New("runner guard did not report a completed runner process"))
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
