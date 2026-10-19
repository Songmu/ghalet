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

func init() {
	if len(os.Args) != 3 || os.Args[1] != runnerGuardArgument {
		return
	}
	if err := runRunnerGuard(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "runner guard:", err)
	}
	os.Exit(1)
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

func runRunnerGuard(runner string) (retErr error) {
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
	gate, report := os.NewFile(3, "controller-liveness"), os.NewFile(4, "runner-exit")
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	reader := bufio.NewReader(gate)
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
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
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
