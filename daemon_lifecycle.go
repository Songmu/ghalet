//go:build linux || darwin

package ghalet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/Songmu/ghalet/procgroup"
)

// jobEvent is called with mu held and waits only for this execution's I/O.
func (d *daemon) jobEvent(ctx context.Context, target *targetRuntime, runnerID int, name string, busy bool) error {
	var run *activeRun
	for _, candidate := range d.runs {
		if d.runTarget(candidate) == target && candidate.State.RunnerName == name {
			run = candidate
			break
		}
	}
	if run == nil {
		return nil
	}
	for run.ioDone != nil {
		done := run.ioDone
		d.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			d.mu.Lock()
			return ctx.Err()
		}
		d.mu.Lock()
	}
	if d.runs[run.State.RunID] != run || run.State.RunnerID != runnerID {
		return nil
	}
	newlyReady := busy && !run.State.Ready
	if newlyReady {
		run.startup.Ready()
	}
	run.State.Busy = busy
	if busy {
		run.State.Ready, run.State.Phase = true, "busy"
	} else {
		run.State.Phase = "exiting"
	}
	var err error
	done := d.runIO(run, func(work *activeRun) error {
		err = d.save(work)
		work.ioResult.readySaved = err == nil && newlyReady
		return err
	})
	d.mu.Unlock()
	<-done
	d.mu.Lock()
	return err
}

func (d *daemon) serve(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	shutdown := false
	for {
		select {
		case <-ctx.Done():
			if !shutdown {
				d.mu.Lock()
				d.stopping = true
				if d.applyCancel != nil {
					d.applyCancel()
				}
				for _, target := range d.targets {
					d.beginDrain(target)
				}
				d.mu.Unlock()
				shutdown = true
			}
		case event := <-d.events:
			d.mu.Lock()
			d.processEvent(event)
			d.mu.Unlock()
		case <-ticker.C:
			d.mu.Lock()
			done := shutdown && !d.applying && d.drained("")
			if !done {
				d.reconcile()
				done = shutdown && !d.applying && d.drained("")
			}
			if done {
				d.cancel()
			}
			d.mu.Unlock()
			if done {
				d.workers.Wait()
				d.mu.Lock()
				problems := d.statusLocked().Problems
				d.mu.Unlock()
				if len(problems) > 0 {
					return fmt.Errorf("stopped with %d unresolved diagnostics; inspect doctor and run gc", len(problems))
				}
				return nil
			}
		}
		if shutdown {
			// A canceled context must not spin and starve process monitoring.
			ctx = context.WithoutCancel(ctx)
		}
	}
}

func (d *daemon) processEvent(event runEvent) {
	run := d.runs[event.id]
	if run == nil {
		return
	}
	if run.ioDone != nil {
		run.events = append(run.events, event)
		return
	}
	run.generation++
	switch event.kind {
	case "ready":
		if run.State.ProcessDone || run.Stopping || run.Uncertain {
			return
		}
		newlyReady := !run.State.Ready
		if newlyReady {
			run.startup.Ready()
		}
		run.State.Ready = true
		if !run.State.Busy {
			run.State.Phase = "idle"
		}
		d.runIO(run, func(work *activeRun) error {
			err := d.save(work)
			work.ioResult.readySaved = err == nil && newlyReady
			return err
		})
	case "exit":
		run.Exited = true
		run.ExitResult = &runExit{err: event.err, stopping: run.Stopping}
		if event.err != nil {
			run.ExitError = d.secrets.Redact(event.err.Error())
			slog.Warn("Runner process exited", "event", "runner.exited", "runId", d.secrets.Redact(event.id), "error", run.ExitError)
		} else {
			slog.Info("Runner process exited (not a workflow result)", "event", "runner.exited", "runId", d.secrets.Redact(event.id))
		}
		d.finishExit(run)
	}
}

func (d *daemon) finishExit(run *activeRun) {
	if run.ioResult != nil || run.ioDone != nil || run.ExitResult == nil || !run.outputCollected() {
		return
	}
	result := run.ExitResult
	run.ExitResult = nil
	newlyReady := run.output != nil && run.output.ready && !run.State.Ready
	if newlyReady {
		run.State.Ready = true
		run.startup.Ready()
	}
	if !result.stopping && ((!run.State.Ready && !run.State.Busy) || result.err != nil) {
		if target := d.runTarget(run); target != nil && !target.Forced {
			err := result.err
			if err == nil {
				err = errors.New("runner exited before readiness")
			}
			run.startup.Fail(err)
			d.startFailure(target, err)
		}
	} else if !result.stopping && result.err == nil && run.State.Ready {
		if target := d.runTarget(run); target != nil {
			target.Failures = 0
			if newlyReady {
				target.StartError = ""
			}
		}
	}
}

func (d *daemon) inspectRuns() {
	if d.inspecting != nil {
		return
	}
	type candidate struct {
		run        *activeRun
		generation uint64
	}
	var candidates []candidate
	var identities []procgroup.ProcessIdentity
	for _, run := range d.runs {
		if run.ioDone != nil || run.Preparing {
			continue
		}
		if run.State.ProcessDone && !run.Uncertain {
			if run.CleanupTried && (run.CleanupRetryAt.IsZero() || time.Now().Before(run.CleanupRetryAt)) {
				continue
			}
			d.runIO(run, func(work *activeRun) error {
				d.reconcileRun(work, nil)
				return nil
			})
		} else {
			candidates = append(candidates, candidate{run, run.generation})
			identities = append(identities, run.State.Process)
		}
	}
	if len(candidates) == 0 {
		return
	}
	done := make(chan struct{})
	d.inspecting = done
	d.workers.Go(func() {
		table, err := procgroup.Snapshot(d.ctx, identities)
		d.mu.Lock()
		defer d.mu.Unlock()
		defer close(done)
		d.inspecting = nil
		if err != nil {
			slog.Error("Process inspection failed; executions retained", "event", "process.inspect_failed", "error", d.secrets.Redact(err.Error()))
			return
		}
		for _, candidate := range candidates {
			run := candidate.run
			if d.ctx.Err() != nil || d.runs[run.State.RunID] != run || run.ioDone != nil || run.generation != candidate.generation {
				continue
			}
			d.runIO(run, func(work *activeRun) error {
				d.reconcileRun(work, table)
				return nil
			})
		}
	})
}

func (d *daemon) reconcileRun(run *activeRun, table map[int]procgroup.Entry) {
	if run.Preparing {
		return
	}
	state := run.State
	if run.Uncertain {
		alive, _, err := procgroup.Inspect(state.processGroup(), table)
		run.GroupStopped = err == nil && !alive
		if !run.occupiesCapacity() {
			state.ProcessDone, state.Phase = true, "cleaning"
		}
		return
	}
	target := d.runTarget(run)
	if target == nil || target.Client == nil {
		return
	}
	if state.ProcessDone {
		if !run.CleanupTried || (!run.CleanupRetryAt.IsZero() && !time.Now().Before(run.CleanupRetryAt)) {
			d.cleanup(run)
		}
		return
	}
	if run.forceStop {
		if err := d.stop(run, true); err != nil {
			state.Error = d.secrets.Redact(err.Error())
			if !state.ProcessDone {
				run.Uncertain = true
			}
		}
		return
	}
	d.reconcileRunProcess(run, table)
}

func (d *daemon) reconcileRunProcess(run *activeRun, table map[int]procgroup.Entry) {
	state := run.State
	alive, members, err := procgroup.Inspect(state.processGroup(), table)
	if err != nil {
		run.InspectFailures++
		// ps collects status and argv separately; an exiting process can
		// lose argv between those reads. Recheck without signaling it.
		if run.InspectFailures < 3 {
			return
		}
		run.Uncertain, state.Error = true, err.Error()
		slog.Error("Process identity is uncertain; retaining capacity", "event", "process.uncertain", "runId", d.secrets.Redact(state.RunID), "error", d.secrets.Redact(err.Error()))
		return
	}
	run.InspectFailures = 0
	run.GroupStopped = !alive
	if !reflect.DeepEqual(members, state.Members) {
		state.Members = members
		if d.save(run) != nil {
			return
		}
	}
	if !alive || run.Exited {
		if run.output != nil && !run.Exited {
			return
		}
		if !alive {
			run.Exited = true
		}
		if err := d.stop(run, false); err != nil {
			state.Error = d.secrets.Redact(err.Error())
		}
		return
	}
	d.enforceStartupTimeout(run)
}

func (d *daemon) enforceStartupTimeout(run *activeRun) {
	state := run.State
	config := run.Config
	if config == nil {
		config = d.config
	}
	if !state.Ready && !state.Busy && time.Since(state.StartedAt) > time.Duration(config.Runner.StartupTimeout) {
		if err := d.retireIdle(run); err != nil {
			state.Error = d.secrets.Redact(err.Error())
		} else {
			run.ioResult.startError = errors.New("runner startup timeout before readiness")
		}
	}
}
