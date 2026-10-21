//go:build linux || darwin

package ghalet

import (
	"log/slog"
	"time"

	settings "github.com/Songmu/ghalet/config"
)

func (d *daemon) occupied() int {
	count := len(d.problems)
	for _, run := range d.runs {
		if run.occupiesCapacity() {
			count++
		}
	}
	return count
}

func (d *daemon) active(targetID string) int {
	count := 0
	for _, run := range d.runs {
		if d.runTargetID(run) == targetID && run.occupiesCapacity() {
			count++
		}
	}
	return count
}

func (d *daemon) retiring(targetID string) int {
	count := 0
	for _, run := range d.runs {
		if (targetID == "" || d.runTargetID(run) == targetID) && run.Retiring && run.occupiesCapacity() {
			count++
		}
	}
	return count
}

func (d *daemon) retire(run *activeRun) {
	run.Retiring = true
	d.runIO(run, func(work *activeRun) error {
		defer func() { work.Retiring = false }()
		return d.retireIdle(work)
	})
}

func (d *daemon) beginDrain(target *targetRuntime) {
	if !target.Draining {
		target.Draining, target.DrainStarted = true, time.Now()
		if target.Set == nil && target.Cancel != nil {
			target.Cancel()
		}
		if target.Listener != nil {
			target.Listener.SetMaxRunners(0)
		}
		slog.Info("Target draining", "event", "target.draining", "targetId", d.secrets.Redact(target.Config.ID))
	}
}

func (d *daemon) reconcile() {
	for _, target := range d.targets {
		if !d.reloading {
			d.enforceDrainDeadline(target)
		}
	}
	// Real assigned jobs get all available slots before warm-pool maintenance.
	for _, id := range d.order {
		target := d.targets[id]
		if d.canStart(target) && target.Assigned > d.active(id) {
			if d.occupied() >= d.config.Capacity.MaxRunners {
				d.yieldIdle(id)
			}
			d.fill(target, min(target.Assigned, target.Config.Scaling.MaxRunners))
		}
	}
	d.expireLogsIfDue()
	realDemand := false
	for _, target := range d.targets {
		if target.Assigned+target.Pending > d.active(target.Config.ID) {
			realDemand = true
		}
	}
	for _, id := range d.order {
		d.reconcileIdle(d.targets[id], realDemand)
	}
	d.inspectRuns()
}

func (d *daemon) enforceDrainDeadline(target *targetRuntime) {
	if !target.Draining || target.Forced || d.drained(target.Config.ID) ||
		time.Since(target.DrainStarted) < time.Duration(d.config.Runner.DrainTimeout) {
		return
	}
	if target.Assigned+target.Pending == 0 && d.active(target.Config.ID) == 0 {
		return
	}
	target.Forced = true
	slog.Error("Drain deadline exceeded", "event", "drain.forced", "targetId", d.secrets.Redact(target.Config.ID), "unstartedJobs", max(0, target.Assigned-d.active(target.Config.ID)))
	for _, run := range d.runs {
		if d.runTarget(run) != target {
			continue
		}
		if run.Cancel != nil {
			run.Cancel()
		}
		if run.ioDone != nil {
			continue
		}
		if !run.Preparing && !run.Uncertain && !run.State.ProcessDone {
			d.runIO(run, func(work *activeRun) error {
				err := d.stop(work, true)
				if err != nil && !work.State.ProcessDone {
					work.Uncertain = true
				}
				return err
			})
		}
	}
}

func (d *daemon) expireLogsIfDue() {
	if d.retaining || !time.Now().After(d.nextRetention) {
		return
	}
	d.nextRetention = time.Now().Add(time.Hour)
	d.retaining = true
	config := d.config
	d.workers.Go(func() {
		d.expireLogs(config)
		d.mu.Lock()
		d.retaining = false
		d.mu.Unlock()
	})
}

func (d *daemon) expireLogs(config *settings.Config) {
	d.retentionMu.Lock()
	defer d.retentionMu.Unlock()
	results, err := expireLogs(d.ctx, config, false)
	if err != nil {
		slog.Error("Log retention failed", "event", "logs.expire_failed", "error", d.secrets.Redact(err.Error()))
	}
	for _, result := range results {
		if result.Error != "" {
			slog.Error("Log retention failed", "event", "logs.expire_failed", "path", d.secrets.Redact(result.Path), "error", d.secrets.Redact(result.Error))
		}
	}
}

func (d *daemon) reconcileIdle(target *targetRuntime, realDemand bool) {
	id := target.Config.ID
	wanted := min(target.Config.Scaling.MaxRunners, target.Assigned+target.Config.Scaling.MinRunners)
	if target.Draining {
		wanted = min(target.Config.Scaling.MaxRunners, target.Assigned)
	}
	if !realDemand && !target.Draining && d.canStart(target) {
		d.fill(target, wanted)
	}
	if d.active(id)-d.retiring(id) <= wanted && !(target.Draining && target.Assigned == 0 && target.Pending == 0) {
		return
	}
	for _, run := range d.runs {
		if d.runTargetID(run) == id && run.canRetireIdle() {
			d.retire(run)
			break
		}
	}
}

func (d *daemon) canStart(target *targetRuntime) bool {
	for _, run := range d.runs {
		// Do not accumulate failed runner trees while their cleanup is
		// unresolved, even though a stopped process releases runner capacity.
		if d.runTarget(run) == target && run.State.ProcessDone && (!run.State.Ready || run.ExitError != "") &&
			run.CleanupTried && run.State.Error != "" {
			return false
		}
	}
	return !d.reloading && (!d.offline || target.Draining) && target.Client != nil && target.Set != nil && target.Fault == "" && !target.Forced && !time.Now().Before(target.RetryAt)
}

func (d *daemon) runTarget(run *activeRun) *targetRuntime {
	if run.Target != nil {
		return run.Target
	}
	return d.targets[run.State.TargetID]
}

func (d *daemon) runTargetID(run *activeRun) string {
	if target := d.runTarget(run); target != nil {
		return target.Config.ID
	}
	return run.State.TargetID
}

func (d *daemon) fill(target *targetRuntime, wanted int) {
	if d.active(target.Config.ID) >= wanted || d.occupied() >= d.config.Capacity.MaxRunners {
		return
	}
	// One reservation per target per tick bounds preparation spikes.
	if err := d.reserve(target); err != nil {
		d.startFailure(target, err)
	}
}

func (d *daemon) yieldIdle(forTarget string) {
	if d.retiring("") != 0 {
		return
	}
	for _, id := range d.order {
		target := d.targets[id]
		if id == forTarget || d.active(id)-d.retiring(id) <= target.Assigned+target.Pending {
			continue
		}
		for _, run := range d.runs {
			if d.runTargetID(run) == id && run.canRetireIdle() {
				d.retire(run)
				return
			}
		}
	}
}

func (d *daemon) drained(id string) bool {
	if id == "" && d.gcActive != 0 {
		return false
	}
	for _, target := range d.targets {
		if id != "" && id != target.Config.ID {
			continue
		}
		if target.Acquiring || (!target.Forced && (target.Assigned+target.Pending > 0)) {
			return false
		}
	}
	for _, run := range d.runs {
		if id != "" && d.runTargetID(run) != id {
			continue
		}
		if run.Preparing || run.ioDone != nil {
			return false
		}
		if run.Uncertain {
			if run.occupiesCapacity() {
				return false
			}
			continue
		}
		if !run.State.ProcessDone || !run.CleanupTried {
			return false
		}
	}
	return true
}
