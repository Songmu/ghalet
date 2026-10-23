package ghalet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/telemetry"
)

// ApplyResult includes the applied snapshot and individual reconciliation results.
type ApplyResult struct {
	Status  *Status
	Results []OperationResult
}

// Apply asks the running daemon to reload its configuration and waits for completion.
func Apply(ctx context.Context, path string) (ApplyResult, error) {
	endpoint, err := resolveControl(ctx, path)
	if err != nil {
		return ApplyResult{}, err
	}
	response, err := endpoint.call(ctx, controlRequest{Command: "apply"})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("connect to running daemon: %w", err)
	}
	return ApplyResult{Status: response.Status, Results: response.Results}, responseError(response)
}

func (d *daemon) apply(ctx context.Context) controlResponse {
	d.mu.Lock()
	if d.applying || d.stopping || d.offline {
		d.mu.Unlock()
		return controlResponse{Error: "apply requires an idle, running daemon"}
	}
	d.applying = true
	ctx, cancel := context.WithCancel(ctx)
	d.applyCancel = cancel
	path := d.config.Path
	d.mu.Unlock()
	defer func() {
		cancel()
		d.mu.Lock()
		d.applying, d.applyCancel = false, nil
		d.reloading = false
		d.mu.Unlock()
	}()

	next, err := settings.LoadConfig(path)
	if err != nil {
		return controlResponse{Error: d.secrets.Redact(err.Error())}
	}
	d.mu.Lock()
	err = d.validateApply(next)
	telemetryChanged := !sameTelemetrySettings(next.Telemetry, d.config.Telemetry)
	telemetryOnly := telemetryChanged && sameNonTelemetrySettings(d.config, next)
	d.mu.Unlock()
	if err != nil {
		return controlResponse{Error: d.secrets.Redact(err.Error())}
	}
	var update *telemetry.Update
	if telemetryChanged {
		update, err = d.metrics.Load().Prepare(ctx, Version, next.Telemetry, d.telemetrySnapshot)
		if err != nil {
			return controlResponse{Error: d.secrets.Redact(err.Error())}
		}
		defer update.Close()
	}
	if telemetryOnly {
		return d.applyTelemetry(ctx, next, update)
	}

	response, old := d.drainForApply(ctx)
	if response.Error != "" {
		return response
	}
	retired, replaced := d.replaceApplyConfig(ctx, next, old, update)
	if !replaced {
		return controlResponse{Error: "apply interrupted by shutdown"}
	}
	closeTelemetry(retired)
	d.registerApplyTargets(ctx, next, &response)
	retained := d.retireApplyRegistrations(ctx, next, &response)
	return d.resumeAfterApply(ctx, next, retained, response)
}

func (d *daemon) drainForApply(ctx context.Context) (controlResponse, []*targetRuntime) {
	response := d.drain(ctx, "")
	if ctx.Err() != nil {
		response.Error = "apply interrupted: " + ctx.Err().Error()
		return response, nil
	}
	response.Error = ""
	d.mu.Lock()
	for _, problem := range d.problems {
		response.Results = append(response.Results, OperationResult{Path: problem.Path, Error: problem.Reason})
	}
	for _, run := range d.runs {
		if run.Uncertain || run.State.Error != "" {
			reason := run.State.Error
			if run.Uncertain {
				reason = "execution identity remains uncertain: " + reason
			}
			response.Results = append(response.Results, OperationResult{TargetID: run.State.TargetID, RunID: run.State.RunID, Error: reason})
		}
	}
	old := make([]*targetRuntime, 0, len(d.order))
	for _, id := range d.order {
		target := d.targets[id]
		if target.Forced {
			response.Results = append(response.Results, OperationResult{TargetID: id, Error: "previous target drain was forced; accepted jobs may be unfinished"})
		}
		old = append(old, target)
		if target.Cancel != nil {
			target.Cancel()
		}
	}
	d.mu.Unlock()
	if err := d.waitListeners(ctx, old); err != nil {
		response.Error = "apply interrupted: " + err.Error()
		return response, old
	}
	for _, target := range old {
		if target.CloseError != "" {
			response.Results = append(response.Results, OperationResult{TargetID: target.Config.ID, Error: target.CloseError})
		}
	}
	return response, old
}

func (d *daemon) replaceApplyConfig(ctx context.Context, next *settings.Config, old []*targetRuntime, update *telemetry.Update) (*telemetry.Update, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil || d.stopping {
		return nil, false
	}
	// Retained executions keep their old credentials and log policy, even
	// when the same registration remains in the new configuration.
	for _, run := range d.runs {
		if run.Target == nil {
			run.Target = d.targets[run.State.TargetID]
		}
		if run.Config == nil {
			run.Config = d.config
		}
	}
	d.retired = append(d.retired, old...)
	d.config = next
	var retired *telemetry.Update
	if update != nil {
		retired = d.metrics.Load().Commit(update)
	}
	d.reloading = true
	d.targets = make(map[string]*targetRuntime, len(next.Targets))
	d.order = nil
	for _, config := range next.Targets {
		d.targets[config.ID] = &targetRuntime{Config: config, Draining: true, DrainStarted: time.Now(), Fault: "configuration apply has not registered this target"}
		d.order = append(d.order, config.ID)
	}
	d.nextRetention = time.Time{}
	return retired, true
}

func (d *daemon) registerApplyTargets(ctx context.Context, next *settings.Config, response *controlResponse) {
	for _, config := range next.Targets {
		if ctx.Err() != nil {
			break
		}
		target := &targetRuntime{Config: config, Draining: true, DrainStarted: time.Now()}
		d.registerTarget(ctx, target, true)
		d.mu.Lock()
		d.targets[config.ID] = target
		d.mu.Unlock()
		response.Results = append(response.Results, OperationResult{
			TargetID: config.ID, Action: "apply target", Error: targetError(target),
		})
		if ctx.Err() != nil {
			break
		}
	}
}

func (d *daemon) retireApplyRegistrations(ctx context.Context, next *settings.Config, response *controlResponse) []*targetRuntime {
	d.mu.Lock()
	var retained, pending []*targetRuntime
	seen := map[string]bool{}
	for _, target := range d.retired {
		if target.Set == nil {
			continue
		}
		referenced, adopted := d.scaleSetReference(next, target)
		if adopted {
			continue
		}
		key := fmt.Sprintf("%s/%d", strings.ToLower(target.Config.URL), target.Set.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		if referenced {
			retained = append(retained, target)
		} else {
			pending = append(pending, target)
		}
	}
	d.mu.Unlock()
	for _, target := range pending {
		result := OperationResult{TargetID: target.Config.ID, Action: "delete retired scale set " + target.Config.ScaleSetName}
		if err := d.deleteRetiredScaleSet(ctx, target); err != nil {
			result.Error = d.secrets.Redact(err.Error())
			retained = append(retained, target)
		}
		response.Results = append(response.Results, result)
	}
	return retained
}

func (d *daemon) resumeAfterApply(ctx context.Context, next *settings.Config, retained []*targetRuntime, response controlResponse) controlResponse {
	d.mu.Lock()
	d.retired = retained
	var started []*targetRuntime
	if ctx.Err() != nil || d.stopping {
		response.Error = "apply interrupted by shutdown"
	} else {
		for _, id := range d.order {
			target := d.targets[id]
			target.Draining = false
			if target.Fault == "" {
				d.listen(target)
				started = append(started, target)
			}
		}
	}
	d.mu.Unlock()
	for _, target := range started {
		select {
		case <-target.ListenerReady:
		case <-ctx.Done():
			response.Error = "apply interrupted during listener startup: " + ctx.Err().Error()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, target := range started {
		if targetError(target) != "" {
			response.Results = append(response.Results, OperationResult{
				TargetID: target.Config.ID, Action: "start listener", Error: targetError(target),
			})
		}
	}
	status := d.statusLocked()
	response.Status = &status
	for _, result := range response.Results {
		if result.Error != "" {
			response.Error = "apply is incomplete; inspect individual results and retry"
		}
	}
	if response.Error != "" {
		slog.Error("Configuration apply incomplete", "event", "config.apply_failed", "error", response.Error)
	} else {
		slog.Info("Configuration applied", "event", "config.applied", "targets", len(next.Targets))
	}
	return response
}

func targetError(target *targetRuntime) string {
	if target.Fault != "" {
		return target.Fault
	}
	if target.RetryError != "" {
		return target.RetryError
	}
	return target.StartError
}

func (d *daemon) validateApply(next *settings.Config) error {
	current := d.config
	if next.Runner.User != current.Runner.User ||
		next.Runner.WorkRoot != current.Runner.WorkRoot ||
		next.Runner.CacheRoot != current.Runner.CacheRoot ||
		next.Logs.Root != current.Logs.Root {
		return errors.New("changing the execution user or managed directories requires a normal stop/restart")
	}
	next.RetainSecretEnvironment(current)
	if err := next.ValidateEnvironment(); err != nil {
		return err
	}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if next.SecretEnv(name) {
			d.secrets.Add(value)
		}
	}
	return nil
}

func (d *daemon) waitListeners(ctx context.Context, targets []*targetRuntime) error {
	for _, target := range targets {
		if target.ListenerDone == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-target.ListenerDone:
		}
	}
	return nil
}

func (d *daemon) scaleSetReference(next *settings.Config, old *targetRuntime) (referenced, adopted bool) {
	for _, config := range next.Targets {
		if !strings.EqualFold(config.URL, old.Config.URL) {
			continue
		}
		if settings.RunnerGroupName(config.RunnerGroup) == settings.RunnerGroupName(old.Config.RunnerGroup) &&
			config.ScaleSetName == old.Config.ScaleSetName {
			referenced = true
		}
		target := d.targets[config.ID]
		if target.Set != nil && target.Set.ID == old.Set.ID {
			referenced = true
			adopted = adopted || target.Fault == ""
		}
	}
	return referenced, adopted
}

func (d *daemon) deleteRetiredScaleSet(ctx context.Context, target *targetRuntime) error {
	if err := d.validateRetiredScaleSet(target); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	current, err := registration(callCtx, target.Client, target.Config, false)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.ID != target.Set.ID || current.RunnerGroupID != target.Set.RunnerGroupID {
		return errors.New("retired registration identity changed; scale set retained")
	}
	if current.Statistics == nil || current.Statistics.TotalAssignedJobs != 0 ||
		current.Statistics.TotalBusyRunners != 0 || current.Statistics.TotalRegisteredRunners != 0 {
		return errors.New("scale set statistics are unavailable or work remains; scale set retained")
	}
	return target.Client.DeleteRunnerScaleSet(callCtx, target.Set.ID)
}

func (d *daemon) validateRetiredScaleSet(target *targetRuntime) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if target.Set == nil || target.Client == nil || !target.Draining ||
		target.Forced || target.Fault != "" || target.CloseError != "" || len(d.problems) != 0 {
		return errors.New("drain, listener cleanup or local diagnostics are incomplete; scale set retained")
	}
	for _, run := range d.runs {
		if strings.EqualFold(run.State.ScopeURL, target.Config.URL) && run.State.ScaleSetID == target.Set.ID {
			return errors.New("local executions still reference the retired scale set")
		}
	}
	return nil
}
