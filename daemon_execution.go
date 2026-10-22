//go:build linux || darwin

package ghalet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/procgroup"
	"github.com/Songmu/ghalet/runnerdist"
	"github.com/Songmu/ghalet/runnerlog"
	"github.com/actions/scaleset"
)

func (d *daemon) reserve(target *targetRuntime) error {
	id, err := newID()
	if err != nil {
		return err
	}
	dir := filepath.Join(d.config.Runner.WorkRoot, target.Config.ID, id)
	state := &RunState{
		Schema: 1, RunID: id, TargetID: target.Config.ID,
		ScopeURL: target.Config.URL, GroupName: target.Config.RunnerGroup,
		GroupID: target.Set.RunnerGroupID, ScaleSetName: target.Config.ScaleSetName, ScaleSetID: target.Set.ID,
		RunnerName: "runner-" + id, User: d.user.Name, UID: d.user.UID, Home: d.user.Home,
		RunnerDir: filepath.Join(dir, "runner"), Workspace: filepath.Join(dir, "work"), Phase: "preparing",
	}
	run := &activeRun{State: state, Preparing: true, Target: target, Config: d.config}
	d.runs[id] = run
	ctx, cancel := context.WithTimeout(d.ctx, preparationTimeout)
	run.Cancel = cancel
	if target.directoryMu == nil {
		target.directoryMu = &sync.Mutex{}
	}
	directoryMu := target.directoryMu
	target.RetryAt = time.Now().Add(time.Second)
	d.runIO(run, func(work *activeRun) error {
		defer cancel()
		// A second reservation must not observe a newly created target
		// directory before its creator has synced the WorkRoot entry.
		directoryMu.Lock()
		err := d.makeRunDirectory(dir)
		directoryMu.Unlock()
		if err != nil {
			work.Preparing, work.ioResult.removed, work.ioResult.startError = false, true, err
			return err
		}
		err = d.save(work)
		if err == nil {
			err = d.prepare(ctx, target, work)
		}
		work.Preparing = false
		if err != nil {
			work.State.Error = d.secrets.Redact(err.Error())
			if work.Uncertain {
				err = errors.Join(errUnsafeTarget, err)
			}
			// Rollback may wait on GitHub or files; stop accepting work for
			// a failed target before entering that slow cleanup.
			d.mu.Lock()
			d.startFailure(target, err)
			d.mu.Unlock()
			if !work.Uncertain {
				if stopErr := d.stop(work, true); stopErr != nil {
					return errors.Join(err, fmt.Errorf("cleanup: %w", stopErr))
				}
			}
		}
		return err
	})
	return nil
}

func createRunDirectory(dir string) error {
	if err := ensureExecutionDirectory(filepath.Dir(dir)); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return err
		}
		return errors.Join(errUnsafeTarget, err)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	if err := fshelper.SyncDirectory(filepath.Dir(dir)); err != nil {
		return errors.Join(err, os.Remove(dir))
	}
	return nil
}

func (d *daemon) prepare(ctx context.Context, target *targetRuntime, run *activeRun) error {
	state := run.State
	config := run.Config
	start := time.Now()
	if err := d.distribution.Prepare(ctx, runnerdist.Config{
		Version: config.Runner.Version, CacheRoot: config.Runner.CacheRoot,
	}, state.RunnerDir); err != nil {
		return fmt.Errorf("prepare runner: %w", err)
	}
	if err := fshelper.EnsureDirectory(state.Workspace, 0700); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		for _, dir := range []string{state.RunnerDir, state.Workspace} {
			if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Lchown(path, d.user.UID, d.user.GID)
			}); err != nil {
				return err
			}
		}
	} else if d.user.Sudo {
		for _, dir := range []string{state.RunnerDir, state.Workspace} {
			if err := shareRunnerDirectory(dir, d.user.GID); err != nil {
				return err
			}
		}
	}
	check := exec.CommandContext(ctx, "/bin/sh", "-c", `test -d "$HOME" && test -w "$1" && test -x "$2"`, "ghalet-access", state.Workspace, filepath.Join(state.RunnerDir, "bin", "Runner.Listener"))
	check.Dir, check.Env, check.SysProcAttr = state.RunnerDir, runnerEnvironment(config, d.user, ""), processAttributes(d.user)
	var checkErr error
	if d.user.Sudo {
		checkErr = callRunnerUser(ctx, d.user, runnerUserRequest{
			Operation: "check", RunnerDir: state.RunnerDir, Workspace: state.Workspace,
			Environment: runnerEnvironment(config, d.user, ""),
		})
	} else {
		checkErr = check.Run()
	}
	if err := checkErr; err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return err
		}
		return fmt.Errorf("%w: execution user cannot access HOME, workspace or runner: %w", errUnsafeTarget, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	jit, err := target.Client.GenerateJitRunnerConfig(apiCtx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name: state.RunnerName, WorkFolder: state.Workspace,
	}, state.ScaleSetID)
	cancel()
	if err != nil {
		return fmt.Errorf("generate JIT runner: %w", err)
	}
	if jit == nil || jit.Runner == nil || jit.Runner.ID <= 0 || jit.EncodedJITConfig == "" ||
		jit.Runner.Name != state.RunnerName || jit.Runner.RunnerScaleSetID != state.ScaleSetID {
		return fmt.Errorf("%w: GitHub returned incomplete JIT configuration; registration must be inspected", errUnsafeTarget)
	}
	run.JITSecrets = d.secrets.AddJIT(jit.EncodedJITConfig)
	for key, value := range config.Runner.Env {
		if d.secrets.Redact(value) != value {
			return fmt.Errorf("%w: runner.env %s contains a management credential; refusing to copy it into a job", errUnsafeTarget, key)
		}
	}
	state.RunnerID, state.Phase = jit.Runner.ID, "registered"
	if err := d.save(run); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.launch(ctx, run, jit.EncodedJITConfig); err != nil {
		return err
	}
	slog.Info("Runner started", "event", "runner.started", "scope", d.secrets.Redact(state.ScopeURL), "targetId", d.secrets.Redact(state.TargetID), "runId", d.secrets.Redact(state.RunID), "durationMs", time.Since(start).Milliseconds())
	return nil
}

func (d *daemon) launch(ctx context.Context, run *activeRun, jit string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	state := run.State
	spawned := false
	defer func() {
		if retErr != nil && !spawned && !run.Uncertain {
			state.Phase = "registered"
			if err := d.save(run); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	state.Phase = "starting"
	if err := d.save(run); err != nil {
		return err
	}
	input, output, err := os.Pipe()
	if err != nil {
		return err
	}
	defer input.Close()
	defer func() {
		if retErr != nil {
			output.Close()
		}
	}()
	command, err := runnerGuardCommand(filepath.Join(state.RunnerDir, "bin", "Runner.Listener"))
	if err != nil {
		return err
	}
	exitReport, reportOutput, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reportOutput.Close()
	defer func() {
		if retErr != nil {
			exitReport.Close()
		}
	}()
	command.ExtraFiles = []*os.File{input, reportOutput}
	logFile, err := os.OpenFile(filepath.Join(state.RunnerDir, "output.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	collected := &runOutputState{done: make(chan struct{})}
	writer := runnerlog.NewOutput(logFile, d.secrets, func() {
		collected.ready = true
		select {
		case d.events <- runEvent{id: state.RunID, kind: "ready"}:
		case <-d.ctx.Done():
		}
	})
	logInput, logOutput, err := os.Pipe()
	if err != nil {
		writer.Close()
		return err
	}
	defer logOutput.Close()
	defer func() {
		if retErr != nil {
			logInput.Close()
		}
	}()
	config := run.Config
	if config == nil {
		config = d.config
	}
	command.Dir, command.Env, command.SysProcAttr = state.RunnerDir, runnerEnvironment(config, d.user, jit), processAttributes(d.user)
	var requestInput io.Reader
	if d.user.Sudo {
		command, err = runnerUserCommand(context.Background(), d.user, runnerUserRequest{
			Operation: "launch", RunnerDir: state.RunnerDir,
			Environment: runnerEnvironment(config, d.user, jit),
		})
		if err != nil {
			writer.Close()
			return err
		}
		requestInput, command.Stdin = command.Stdin, input
		command.Dir = state.RunnerDir
	}
	command.Stdout, command.Stderr = logOutput, logOutput
	if d.user.Sudo {
		command.Stdout = reportOutput
	}
	if err := command.Start(); err != nil {
		writer.Close()
		return err
	}
	spawned = true
	input.Close()
	reportOutput.Close()
	logOutput.Close()
	// No runner can execute until its identity has been durably recorded;
	// controller death closes the launch gate without exec.
	pid := command.Process.Pid
	if d.user.Sudo {
		exitReport.SetReadDeadline(time.Now().Add(15 * time.Second))
		output.SetWriteDeadline(time.Now().Add(15 * time.Second))
		cancelRead := context.AfterFunc(ctx, func() {
			exitReport.SetReadDeadline(time.Now())
			output.SetWriteDeadline(time.Now())
		})
		var reported int64
		_, err = io.Copy(output, requestInput)
		if err == nil {
			err = binary.Read(exitReport, binary.LittleEndian, &reported)
		}
		cancelRead()
		exitReport.SetReadDeadline(time.Time{})
		output.SetWriteDeadline(time.Time{})
		if err == nil && (reported <= 1 || int64(int(reported)) != reported) {
			err = errors.New("invalid sudo runner identity")
		}
		pid = int(reported)
	}
	var table map[int]procgroup.Entry
	if err == nil {
		table, err = procgroup.Snapshot(ctx, []procgroup.ProcessIdentity{{PID: pid}})
	}
	if err == nil {
		var identity procgroup.ProcessIdentity
		identity, err = procgroup.LaunchedIdentity(table, pid, d.user.UID)
		if err == nil {
			state.Process = identity
			state.Members = []procgroup.ProcessIdentity{identity}
			state.StartedAt = time.Now().UTC()
			err = d.save(run)
		}
	}
	if err != nil {
		output.Close()
		command.Wait()
		writer.Close()
		if ctx.Err() != nil {
			state.Phase = "registered"
		} else {
			run.Uncertain = true
		}
		return err
	}
	// Drain cancellation and gate release have one ordering point. The empty
	// pipe has room for this short write; no API, inspection or fsync is locked.
	d.mu.Lock()
	err = ctx.Err()
	if err == nil {
		_, err = output.Write([]byte("ready\n"))
	}
	d.mu.Unlock()
	if err != nil {
		output.Close()
		command.Wait()
		writer.Close()
		return err
	}
	run.output = collected
	go func() {
		_, err := io.Copy(writer, logInput)
		collected.err = errors.Join(err, logInput.Close(), writer.Close())
		close(collected.done)
	}()
	// Reaping and output collection remain independent so shutdown can
	// observe both without blocking diagnostics and control access.
	go func() {
		err := command.Wait()
		err = readRunnerGuardResult(exitReport, err)
		output.Close()
		select {
		case d.events <- runEvent{id: state.RunID, kind: "exit", err: err}:
		case <-d.ctx.Done():
		}
	}()
	return nil
}

func (d *daemon) startFailure(target *targetRuntime, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	target.StartError = d.secrets.Redact(err.Error())
	target.Failures = min(target.Failures+1, 7)
	target.RetryAt = time.Now().Add(targetRetryWait(target.Failures))
	if permanentTargetFailure(err) {
		target.Fault = d.secrets.Redact(err.Error())
		if target.Listener != nil {
			target.Listener.SetMaxRunners(0)
		}
	}
	slog.Error("Runner preparation/start failed", "event", "runner.failed", "targetId", d.secrets.Redact(target.Config.ID), "attempt", target.Failures, d.secrets.ReplaceAttr(nil, slog.Any("error", err)))
}

func (d *daemon) unregister(run *activeRun) error {
	state := run.State
	target := d.runTarget(run)
	if target == nil || target.Client == nil {
		return errors.New("cannot verify runner registration for cleanup")
	}
	ctx, cancel := context.WithTimeout(d.ctx, apiTimeout)
	defer cancel()
	var runner *scaleset.RunnerReference
	var err error
	if state.RunnerID > 0 {
		runner, err = target.Client.GetRunner(ctx, state.RunnerID)
	} else {
		runner, err = target.Client.GetRunnerByName(ctx, state.RunnerName)
	}
	if errors.Is(err, scaleset.RunnerNotFoundError) {
		return nil
	}
	if err != nil {
		return err
	}
	if runner == nil {
		return nil
	}
	if runner.Name != state.RunnerName || runner.RunnerScaleSetID != state.ScaleSetID || (state.RunnerID != 0 && state.RunnerID != runner.ID) {
		return errors.New("runner registration does not match the recovery record")
	}
	err = target.Client.RemoveRunner(ctx, int64(runner.ID))
	if errors.Is(err, scaleset.RunnerNotFoundError) {
		return nil
	}
	return err
}

func (d *daemon) retireIdle(run *activeRun) error {
	if run.State.Busy || run.Uncertain || run.Preparing {
		return errors.New("runner is busy, preparing or uncertain")
	}
	// Server-side deletion is the arbitration point with job assignment.
	// Never stop the process if GitHub refuses deletion or cannot be queried.
	if err := d.unregister(run); err != nil {
		if errors.Is(err, scaleset.JobStillRunningError) {
			newlyReady := !run.State.Ready
			run.State.Busy, run.State.Ready, run.State.Phase = true, true, "busy"
			if d.save(run) == nil && newlyReady {
				if run.ioResult != nil {
					run.ioResult.readySaved = true
				} else if target := d.runTarget(run); target != nil {
					target.StartError, target.Failures = "", 0
				}
			}
		}
		return err
	}
	return d.stop(run, false)
}

func (d *daemon) stop(run *activeRun, forced bool) error {
	if run.Uncertain {
		return errors.New("uncertain process identity; manual inspection required")
	}
	if run.State.Busy && !run.Exited && !forced {
		return errors.New("refusing to stop a busy runner")
	}
	run.Stopping = true
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	err := procgroup.StopWithSignal(ctx, run.State.processGroup(), func(members []procgroup.ProcessIdentity) error {
		run.State.Members = members
		return d.save(run)
	}, func(group procgroup.Group, signal syscall.Signal) error {
		if d.user.Sudo {
			return callRunnerUser(ctx, d.user, runnerUserRequest{Operation: "signal", Group: group, Signal: signal})
		}
		return syscall.Kill(-group.Process.PGID, signal)
	})
	if err != nil {
		return err
	}
	if run.output != nil {
		select {
		case <-run.output.done:
		case <-ctx.Done():
			return errors.New("process stopped but runner output collection has not completed")
		}
	}
	d.finishExit(run)
	run.GroupStopped = true
	run.State.ProcessDone, run.State.Phase = true, "cleaning"
	if err := d.save(run); err != nil {
		return err
	}
	return d.cleanup(run)
}

func (d *daemon) cleanup(run *activeRun) error {
	if !run.State.ProcessDone || run.Uncertain {
		return errors.New("cannot clean an unverified execution")
	}
	if !run.outputCollected() {
		return errors.New("runner output collection has not completed")
	}
	if run.output != nil && !run.Exited && !run.Stopping {
		return errors.New("runner exit observation has not completed")
	}
	d.finishExit(run)
	run.CleanupTried = true
	fail := func(err error, retryable bool) error {
		run.CleanupRetryAt = time.Time{}
		run.State.Error = d.secrets.Redact(err.Error())
		if d.save(run) == nil && retryable && d.ctx.Err() == nil {
			run.CleanupFailures = min(run.CleanupFailures+1, 7)
			delay := min(5*time.Second<<uint(run.CleanupFailures-1), 5*time.Minute)
			run.CleanupRetryAt = time.Now().Add(delay)
		}
		slog.Error("Cleanup incomplete; state and files retained", "event", "cleanup.failed", "runId", d.secrets.Redact(run.State.RunID), "targetId", d.secrets.Redact(run.State.TargetID), "error", d.secrets.Redact(err.Error()), "retryAt", run.CleanupRetryAt)
		return err
	}
	if run.output != nil && run.output.err != nil {
		return fail(fmt.Errorf("collect runner output: %w", run.output.err), false)
	}
	if err := d.unregister(run); err != nil {
		return fail(err, cleanupRetryable(err))
	}
	config := run.Config
	if config == nil {
		config = d.config
	}
	if d.user.Sudo && run.State.Process.PID != 0 {
		if err := callRunnerUser(d.ctx, d.user, runnerUserRequest{
			Operation: "release", RunnerDir: run.State.RunnerDir, Workspace: run.State.Workspace,
		}); err != nil {
			return fail(err, cleanupRetryable(err))
		}
	}
	if config.Logs.Preserve {
		state := run.State
		err := runnerlog.Archive(config.Logs.Root, runnerlog.Run{
			RunID: state.RunID, TargetID: state.TargetID, Scope: state.ScopeURL, RunnerDir: state.RunnerDir,
		}, d.secrets)
		if err != nil {
			return fail(err, cleanupRetryable(err))
		}
	}
	if err := d.store.removeFiles(run.State); err != nil {
		return fail(err, cleanupRetryable(err))
	}
	if run.ioResult != nil {
		run.ioResult.removed = true
	} else {
		delete(d.runs, run.State.RunID)
		d.secrets.Release(run.JITSecrets)
	}
	slog.Info("Execution cleaned", "event", "runner.removed", "targetId", d.secrets.Redact(run.State.TargetID), "runId", d.secrets.Redact(run.State.RunID))
	return nil
}
