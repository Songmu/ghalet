//go:build linux || darwin

package ghalet

import "github.com/Songmu/ghalet/telemetry"

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
		case run.Stopping || run.State.Phase == "exiting":
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
