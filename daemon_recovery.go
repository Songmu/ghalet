//go:build linux || darwin

package ghalet

import (
	"context"
	"log/slog"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/procgroup"
)

func (d *daemon) recover(ctx context.Context) error {
	records, err := d.store.load()
	if err != nil {
		return err
	}
	var identities []procgroup.ProcessIdentity
	for _, record := range records {
		if record.Err == nil {
			identities = append(identities, record.State.Process)
		}
	}
	table, err := procgroup.Snapshot(ctx, identities)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.NonExecutionFile {
			slog.Warn("Non-execution file ignored", "event", "recovery.unrelated_file", "path", d.secrets.Redact(record.Path))
			continue
		}
		if record.Err != nil {
			d.problems = append(d.problems, Problem{Path: record.Path, Reason: record.Err.Error()})
			continue
		}
		d.recoverRun(ctx, record.State, table)
	}
	return nil
}

func (d *daemon) recoverRun(ctx context.Context, state *RunState, table map[int]procgroup.Entry) {
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	alive, members, inspectErr := procgroup.Inspect(state.processGroup(), table)
	if inspectErr == nil && !alive {
		state.ProcessDone, state.Phase = true, "cleaning"
		run.Exited, run.GroupStopped = true, true
	}
	target := d.targets[state.TargetID]
	reason := d.recoveryMismatch(state, target)
	if inspectErr != nil {
		reason = inspectErr.Error()
	} else if reason == "" && alive {
		callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
		runner, runnerErr := target.Client.GetRunner(callCtx, state.RunnerID)
		cancel()
		if runnerErr != nil || runner == nil || runner.ID != state.RunnerID ||
			runner.Name != state.RunnerName || runner.RunnerScaleSetID != state.ScaleSetID {
			reason = "live process cannot be matched to its GitHub runner registration"
		} else {
			state.Members = members
			run.Exited = false
		}
	}
	if reason != "" {
		run.Uncertain = true
		state.Error = reason
		slog.Warn("Execution needs manual inspection", "event", "recovery.uncertain", "targetId", d.secrets.Redact(state.TargetID), "runId", d.secrets.Redact(state.RunID), "error", d.secrets.Redact(reason))
		return
	}
	if !d.offline {
		if err := d.save(run); err != nil {
			run.Uncertain = true
			state.Error = err.Error()
		}
	}
}

func (d *daemon) recoveryMismatch(state *RunState, target *targetRuntime) string {
	switch {
	case target == nil:
		return "state does not correspond to a currently configured target"
	case target.Set == nil || target.Fault != "":
		return "registration cannot be verified; resolve target authentication/registration and restart"
	case state.ScopeURL != target.Config.URL ||
		state.ScaleSetName != target.Config.ScaleSetName || settings.RunnerGroupName(state.GroupName) != settings.RunnerGroupName(target.Config.RunnerGroup) ||
		state.ScaleSetID != target.Set.ID || state.GroupID != target.Set.RunnerGroupID:
		return "recorded registration differs from the current explicit scope/group/scale set"
	case state.User != d.user.Name || state.UID != d.user.UID || state.Home != d.user.Home:
		return "recorded execution user differs from the current execution user"
	default:
		return ""
	}
}
