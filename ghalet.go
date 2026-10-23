package ghalet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	settings "github.com/Songmu/ghalet/config"
)

// OperationError reports a management failure returned with the daemon's results.
type OperationError string

func (e OperationError) Error() string { return string(e) }

func selectedConfigPath(path string) (string, error) {
	path, err := settings.ResolvePath(path)
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
	endpoint, err := resolveControl(ctx, path)
	if err != nil {
		return nil, err
	}
	response, err := endpoint.call(ctx, request)
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

// GC cleans stopped executions and expired logs, returning partial results on error.
func GC(ctx context.Context, path string, dryRun bool) ([]OperationResult, error) {
	endpoint, err := resolveControl(ctx, path)
	if err != nil {
		return nil, err
	}
	request := controlRequest{Command: "gc", DryRun: dryRun}
	response, connectionErr := endpoint.call(ctx, request)
	if connectionErr == nil {
		return response.Results, responseError(response)
	}
	if endpoint.discovered {
		return nil, fmt.Errorf("connect to discovered daemon: %w", connectionErr)
	}
	d, close, err := offlineDaemon(ctx, endpoint.config, connectionErr)
	if err != nil {
		return nil, err
	}
	defer close()
	response = d.handle(ctx, request)
	return response.Results, responseError(response)
}

func offlineDaemon(ctx context.Context, path string, connectionErr error) (*daemon, func(), error) {
	config, err := settings.LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	lock, err := lockWorkRoot(config.Runner.WorkRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("control connection failed (%v); refusing direct operation: %w", connectionErr, err)
	}
	controlLock, err := lockControl(path)
	if err != nil {
		lock.Close()
		return nil, nil, fmt.Errorf("refusing direct operation while the config's control address is owned: %w", err)
	}
	close := func() {
		controlLock.Close()
		lock.Close()
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

// RunDaemon runs the daemon until ctx is canceled and its runners are drained.
func RunDaemon(ctx context.Context, config *settings.Config) error {
	lock, err := lockWorkRoot(config.Runner.WorkRoot)
	if err != nil {
		return err
	}
	defer lock.Close()
	server, err := listenControl(config.Path)
	if err != nil {
		return err
	}
	defer server.close()
	d, err := newDaemon(config)
	if err != nil {
		return err
	}
	defer func() {
		d.cancel()
		d.workers.Wait()
		if err := d.metrics.Load().Close(); err != nil {
			slog.Warn("Telemetry shutdown incomplete", "event", "telemetry.shutdown_failed")
		}
	}()
	if err := d.initialize(ctx, false); err != nil {
		return err
	}
	metrics := d.metrics.Load()
	update, err := metrics.Prepare(ctx, Version, config.Telemetry, d.telemetrySnapshot)
	if err != nil {
		return err
	}
	metrics.Commit(update)
	if err := ctx.Err(); err != nil {
		return err
	}
	go server.serve(d)
	if err := notifyReady(); err != nil {
		return fmt.Errorf("notify service readiness: %w", err)
	}
	slog.Info("Daemon ready", "event", "daemon.ready", "config", d.secrets.Redact(config.Path), "capacity", config.Capacity.MaxRunners)
	return d.serve(ctx)
}
