//go:build linux || darwin

package ghalet

import (
	"context"
	"log/slog"
	"maps"
	"reflect"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/telemetry"
)

func sameTelemetrySettings(current, next settings.TelemetryConfig) bool {
	if !maps.Equal(current.ResourceAttributes, next.ResourceAttributes) {
		return false
	}
	current.ResourceAttributes, next.ResourceAttributes = nil, nil
	return reflect.DeepEqual(current, next)
}

func sameNonTelemetrySettings(current, next *settings.Config) bool {
	currentRunner, nextRunner := current.Runner, next.Runner
	if !maps.Equal(currentRunner.Env, nextRunner.Env) {
		return false
	}
	currentRunner.Env, nextRunner.Env = nil, nil
	return current.Path == next.Path && current.ManagedFile == next.ManagedFile &&
		current.Capacity == next.Capacity && current.Logs == next.Logs &&
		reflect.DeepEqual(current.Defaults, next.Defaults) &&
		reflect.DeepEqual(current.Targets, next.Targets) &&
		reflect.DeepEqual(currentRunner, nextRunner)
}

func closeTelemetry(update *telemetry.Update) {
	if err := update.Close(); err != nil {
		slog.Warn("Retired telemetry shutdown incomplete", "event", "telemetry.shutdown_failed")
	}
}

func (d *daemon) applyTelemetry(ctx context.Context, next *settings.Config, update *telemetry.Update) controlResponse {
	d.mu.Lock()
	if ctx.Err() != nil || d.stopping {
		d.mu.Unlock()
		return controlResponse{Error: "apply interrupted by shutdown"}
	}
	d.config = next
	old := d.metrics.Load().Commit(update)
	status := d.statusLocked()
	d.mu.Unlock()
	closeTelemetry(old)
	slog.Info("Telemetry configuration applied", "event", "config.applied", "targets", len(next.Targets))
	return controlResponse{Status: &status}
}

func (d *daemon) telemetrySnapshot() telemetry.Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := telemetry.Snapshot{
		Capacity: int64(d.config.Capacity.MaxRunners),
		Occupied: int64(d.occupied()),
		Runners:  make(map[telemetry.Runner]int64),
		Workers:  make(map[telemetry.Runner]int64),
	}
	for _, run := range d.runs {
		state := "preparing"
		switch {
		case run.Uncertain:
			state = "uncertain"
		case run.Preparing:
		case run.State.ProcessDone:
			state = "cleaning"
		case run.Stopping || run.Exited || run.GroupStopped || run.State.Phase == "exiting":
			state = "exiting"
		case run.State.Busy:
			state = "busy"
		case run.State.Ready:
			state = "idle"
		}
		targetID := d.runTargetID(run)
		snapshot.Runners[telemetry.Runner{TargetID: targetID, State: state}]++
		if (state == "idle" || state == "busy") && run.State.Ready &&
			run.State.RunnerID > 0 && run.InspectFailures == 0 {
			workerState := "available"
			if state == "busy" {
				workerState = "busy"
			}
			snapshot.Workers[telemetry.Runner{TargetID: targetID, State: workerState}]++
		}
	}
	return snapshot
}
