package ghalet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// DestroyOptions identifies the registration to delete explicitly.
type DestroyOptions struct {
	Target      string
	RunnerGroup string
	ScaleSet    string
}

// DestroyPlan describes the registration requiring approval before deletion.
type DestroyPlan struct {
	Target      string
	URL         string
	RunnerGroup string
	ScaleSet    string
	ScaleSetID  int
	GroupID     int
}

// DestroyResult includes the drain snapshot when destruction stops before deletion.
type DestroyResult struct {
	Status  *Status
	Results []OperationResult
}

// OperationError reports a management failure returned with the daemon's results.
type OperationError string

func (e OperationError) Error() string { return string(e) }

func selectedConfigPath(path string) (string, error) {
	path, err := configPath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("selected config: %w", err)
	}
	return path, nil
}

func responseError(response controlResponse) error {
	if response.Error != "" {
		return OperationError(response.Error)
	}
	return nil
}

func requestStatus(ctx context.Context, path string, request controlRequest) (*Status, error) {
	path, err := selectedConfigPath(path)
	if err != nil {
		return nil, err
	}
	response, err := controlCall(ctx, path, request)
	if err != nil {
		return nil, fmt.Errorf("connect to running daemon: %w", err)
	}
	return response.Status, responseError(response)
}

// GetStatus returns the running daemon's snapshot without reloading its config.
func GetStatus(ctx context.Context, path string) (*Status, error) {
	return requestStatus(ctx, path, controlRequest{Command: "status"})
}

// Doctor returns diagnostics, including a snapshot when problems are found.
func Doctor(ctx context.Context, path string) (*Status, error) {
	return requestStatus(ctx, path, controlRequest{Command: "doctor"})
}

// Drain stops accepting work and waits for the selected target, or all targets.
func Drain(ctx context.Context, path, target string) (*Status, error) {
	return requestStatus(ctx, path, controlRequest{Command: "drain", Target: target})
}

// GC cleans stopped executions and expired logs, returning partial results on error.
func GC(ctx context.Context, path string, dryRun bool) ([]OperationResult, error) {
	path, err := selectedConfigPath(path)
	if err != nil {
		return nil, err
	}
	request := controlRequest{Command: "gc", DryRun: dryRun}
	response, connectionErr := controlCall(ctx, path, request)
	if connectionErr == nil {
		return response.Results, responseError(response)
	}
	d, close, err := offlineDaemon(ctx, path, connectionErr)
	if err != nil {
		return nil, err
	}
	defer close()
	response = d.handle(ctx, request)
	return response.Results, responseError(response)
}

// Destroy drains and deletes an explicitly selected registration only after confirm approves its plan.
// When the daemon is unavailable, it uses locked offline execution.
func Destroy(ctx context.Context, path string, options DestroyOptions, confirm func(DestroyPlan) error) (DestroyResult, error) {
	path, err := selectedConfigPath(path)
	if err != nil {
		return DestroyResult{}, err
	}
	if confirm == nil {
		return DestroyResult{}, errors.New("destroy requires explicit confirmation; nothing was deleted")
	}
	request := controlRequest{Command: "destroy-plan", Target: options.Target, RunnerGroup: options.RunnerGroup, ScaleSet: options.ScaleSet}
	response, connectionErr := controlCall(ctx, path, request)
	call := func(request controlRequest) (controlResponse, error) {
		return controlCall(ctx, path, request)
	}
	var d *daemon
	if connectionErr != nil {
		var close func()
		d, close, err = offlineDaemon(ctx, path, connectionErr)
		if err != nil {
			return DestroyResult{}, err
		}
		defer close()
		call = func(request controlRequest) (controlResponse, error) {
			return d.handle(ctx, request), nil
		}
		response = d.handle(ctx, request)
	}
	if err := responseError(response); err != nil {
		return DestroyResult{Status: response.Status, Results: response.Results}, err
	}
	plan := response.Plan
	if plan == nil {
		return DestroyResult{}, errors.New("missing deletion plan")
	}
	if err := confirm(DestroyPlan{
		Target: plan.Target, URL: plan.URL, RunnerGroup: plan.RunnerGroup,
		ScaleSet: plan.ScaleSet, ScaleSetID: plan.ScaleSetID, GroupID: plan.GroupID,
	}); err != nil {
		return DestroyResult{}, err
	}
	if d != nil {
		engineCtx, stopEngine := context.WithCancel(ctx)
		engineDone := make(chan struct{})
		go func() {
			defer close(engineDone)
			d.runOfflineEngine(engineCtx)
		}()
		defer func() {
			stopEngine()
			<-engineDone
		}()
	}
	response, err = call(controlRequest{Command: "destroy", Confirmation: plan.Confirmation})
	if err != nil {
		return DestroyResult{}, err
	}
	return DestroyResult{Status: response.Status, Results: response.Results}, responseError(response)
}

func offlineDaemon(ctx context.Context, path string, connectionErr error) (*daemon, func(), error) {
	config, err := LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	lock, err := lockWorkRoot(config.Runner.WorkRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("control connection failed (%v); refusing direct operation: %w", connectionErr, err)
	}
	controlLock, err := lockControl(path)
	if err != nil {
		lock.close()
		return nil, nil, fmt.Errorf("refusing direct operation while the config's control address is owned: %w", err)
	}
	close := func() {
		controlLock.Close()
		lock.close()
	}
	// A socket failure alone is never evidence that a daemon is absent.
	if _, err := controlCall(ctx, path, controlRequest{Command: "status"}); err == nil {
		close()
		return nil, nil, errors.New("daemon became available; retry the operation through its control socket")
	}
	d, err := newDaemon(config)
	if err != nil {
		close()
		return nil, nil, err
	}
	cleanup := func() {
		d.cancel()
		d.workers.Wait()
		close()
	}
	if err := d.initialize(ctx, true); err != nil {
		cleanup()
		return nil, nil, err
	}
	return d, cleanup, nil
}

func (d *daemon) runOfflineEngine(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.mu.Lock()
			d.reconcile()
			d.mu.Unlock()
		case event := <-d.events:
			d.mu.Lock()
			d.processEvent(event)
			d.mu.Unlock()
		}
	}
}

// RunDaemon runs the daemon until ctx is canceled and its runners are drained.
func RunDaemon(ctx context.Context, config *Config) error {
	lock, err := lockWorkRoot(config.Runner.WorkRoot)
	if err != nil {
		return err
	}
	defer lock.close()
	server, err := listenControl(config.Path)
	if err != nil {
		return err
	}
	defer server.close()
	d, err := newDaemon(config)
	if err != nil {
		return err
	}
	defer d.cancel()
	if err := d.initialize(ctx, false); err != nil {
		return err
	}
	go server.serve(d)
	slog.Info("Daemon ready", "event", "daemon.ready", "config", d.secrets.redact(config.Path), "capacity", config.Capacity.MaxRunners)
	return d.serve(ctx)
}
