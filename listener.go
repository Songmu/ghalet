//go:build linux || darwin

package ghalet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

func (d *daemon) startListener(target *targetRuntime) {
	ctx, cancel := context.WithCancel(d.ctx)
	target.Cancel = cancel
	target.ListenerDone = make(chan struct{})
	target.ListenerReady = make(chan struct{})
	d.workers.Go(func() {
		d.runListener(ctx, target)
	})
}

func (d *daemon) runListener(ctx context.Context, target *targetRuntime) {
	defer close(target.ListenerDone)
	signalReady := sync.OnceFunc(func() { close(target.ListenerReady) })
	var lastErr error
	attempt := 0
	if target.Set == nil {
		attempt = maxAttempts
	}
	for ; ; attempt = min(attempt+1, 7) {
		if attempt > 0 || target.Set == nil {
			signalReady()
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
			case <-time.After(targetRetryWait(max(1, attempt))):
			}
			if ctx.Err() != nil {
				break
			}
		}
		if target.Set == nil {
			d.mu.Lock()
			draining := target.Draining
			d.mu.Unlock()
			if draining {
				break
			}
			stopped, err := d.recoverListenerRegistration(ctx, target)
			lastErr = err
			if stopped || permanentTargetFailure(err) {
				break
			}
			if err != nil {
				continue
			}
			attempt = 0
		}
		client, ok := target.Client.(*scaleset.Client)
		if !ok {
			lastErr = errors.New("listener requires the official scaleset client")
			break
		}
		lastErr = d.runListenerSession(ctx, target, client, signalReady, attempt)
		if ctx.Err() != nil || permanentTargetFailure(lastErr) {
			break
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() == nil && lastErr != nil {
		target.Fault = d.secrets.Redact(lastErr.Error())
		slog.Error("Target stopped accepting work", "event", "target.failed", "targetId", d.secrets.Redact(target.Config.ID), "error", target.Fault)
	}
	signalReady()
}

func (d *daemon) recoverListenerRegistration(ctx context.Context, target *targetRuntime) (stopped bool, err error) {
	candidate := &targetRuntime{Config: target.Config}
	err = d.registrationAttempt(ctx, candidate, true)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil && candidate.Set == nil {
		err = fmt.Errorf("%w: scale set is not registered", errUnsafeTarget)
	}
	if err == nil {
		// Keep the remote identity for cleanup even when registration was canceled,
		// but never revive demand after cancellation or draining.
		target.Client, target.Set = candidate.Client, candidate.Set
		if ctx.Err() == nil && !target.Draining {
			target.Assigned = candidate.Assigned
		}
		target.RetryError = ""
		d.retired = slices.DeleteFunc(d.retired, func(old *targetRuntime) bool {
			if old.Set == nil {
				return false
			}
			_, adopted := d.scaleSetReference(d.config, old)
			return adopted
		})
	} else if ctx.Err() == nil {
		target.RetryError = d.secrets.Redact(err.Error())
	}
	return ctx.Err() != nil || target.Draining, err
}

func (d *daemon) runListenerSession(ctx context.Context, target *targetRuntime, client *scaleset.Client, signalReady func(), attempt int) error {
	setupCtx, setupCancel := context.WithTimeout(ctx, apiTimeout)
	session, err := client.MessageSessionClient(setupCtx, target.Set.ID, "ghalet", scaleset.WithTimeout(60*time.Second), scaleset.WithRetryMax(0))
	setupCancel()
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		d.mu.Lock()
		target.RetryError = d.secrets.Redact(err.Error())
		signalReady()
		d.mu.Unlock()
		if !permanentTargetFailure(err) {
			slog.Warn("Message session setup failed; retrying", "event", "listener.retry", "targetId", d.secrets.Redact(target.Config.ID), d.secrets.ReplaceAttr(nil, slog.Any("error", err)))
		}
		return err
	}
	d.mu.Lock()
	capacity := min(target.Config.Scaling.MaxRunners, d.config.Capacity.MaxRunners)
	if target.Draining || target.Fault != "" {
		capacity = 0
	}
	l, err := listener.New(session, listener.Config{ScaleSetID: target.Set.ID, MaxRunners: capacity})
	if err == nil {
		target.Listener = l
		target.HasMessage = false
		target.RetryError = ""
		signalReady()
	} else {
		err = errors.Join(errUnsafeTarget, err)
	}
	d.mu.Unlock()
	if err == nil {
		err = l.Run(ctx, &targetScaler{daemon: d, target: target, session: session})
	}
	d.closeListenerSession(target, session)
	if err != nil && ctx.Err() == nil {
		d.mu.Lock()
		target.RetryError = d.secrets.Redact(err.Error())
		d.mu.Unlock()
	}
	if ctx.Err() == nil && !permanentTargetFailure(err) {
		slog.Warn("Listener failed; retrying", "event", "listener.retry", "targetId", d.secrets.Redact(target.Config.ID), "attempt", attempt+1, d.secrets.ReplaceAttr(nil, slog.Any("error", err)))
	}
	return err
}

func (d *daemon) closeListenerSession(target *targetRuntime, session *scaleset.MessageSessionClient) {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), apiTimeout)
	closeErr := session.Close(closeCtx)
	closeCancel()
	if closeErr != nil {
		d.mu.Lock()
		target.CloseError = d.secrets.Redact(closeErr.Error())
		d.mu.Unlock()
		slog.Error("Message session cleanup failed", "event", "listener.close_failed", "targetId", d.secrets.Redact(target.Config.ID), "error", d.secrets.Redact(closeErr.Error()))
	}
}

type targetScaler struct {
	daemon  *daemon
	target  *targetRuntime
	session listener.Client
}

func (s *targetScaler) Scale(ctx context.Context, message *scaleset.RunnerScaleSetMessage) error {
	if message == nil {
		return nil
	}
	d := s.daemon
	d.mu.Lock()
	target := s.target
	if target.scaleMu == nil {
		target.scaleMu = &sync.Mutex{}
	}
	scaleMu := target.scaleMu
	d.mu.Unlock()
	// A target's callbacks remain ordered, including ambiguous acquisitions,
	// without serializing any other target or control request.
	scaleMu.Lock()
	defer scaleMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if message.Statistics != nil && (!target.HasMessage || message.MessageID != target.LastMessage) {
		target.Assigned = max(0, message.Statistics.TotalAssignedJobs)
		target.Pending = 0
	}
	for _, event := range message.JobStartedMessages {
		if err := d.jobEvent(ctx, target, event.RunnerID, event.RunnerName, true); err != nil {
			return err
		}
	}
	for _, event := range message.JobCompletedMessages {
		if err := d.jobEvent(ctx, target, event.RunnerID, event.RunnerName, false); err != nil {
			return err
		}
	}
	if !target.Draining && target.Fault == "" && (!target.HasMessage || target.LastMessage != message.MessageID) {
		available := d.config.Capacity.MaxRunners - len(d.problems)
		for _, other := range d.targets {
			available -= other.Assigned + other.Pending
		}
		for _, run := range d.runs {
			if run.Uncertain && run.occupiesCapacity() {
				available--
			}
		}
		available = min(available, target.Config.Scaling.MaxRunners-target.Assigned-target.Pending)
		var requests []int64
		for _, event := range message.JobAvailableMessages {
			if len(requests) >= available {
				break
			}
			requests = append(requests, event.RunnerRequestID)
		}
		if len(requests) > 0 {
			target.Pending += len(requests)
			target.Acquiring = true
			// Mark the message before releasing mu: even a lost response must
			// not cause a duplicate acquisition on redelivery.
			target.LastMessage, target.HasMessage = message.MessageID, true
			acquireCtx, cancel := context.WithTimeout(ctx, apiTimeout)
			d.mu.Unlock()
			acquired, err := s.session.AcquireJobs(acquireCtx, requests)
			cancel()
			d.mu.Lock()
			target.Acquiring = false
			if err != nil {
				// The server may have accepted the request before the response
				// was lost. Keep the reservation until authoritative statistics.
				return err
			}
			target.Pending -= len(requests) - len(acquired)
		}
	}
	target.LastMessage, target.HasMessage = message.MessageID, true
	return nil
}
