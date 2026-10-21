//go:build linux || darwin

package ghalet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/logreduct"
	"github.com/Songmu/ghalet/procgroup"
	"github.com/Songmu/ghalet/runnerdist"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

type targetRuntime struct {
	scaleMu       *sync.Mutex
	directoryMu   *sync.Mutex
	Config        settings.Target
	Client        scaleAPI
	Set           *scaleset.RunnerScaleSet
	Assigned      int
	Pending       int
	Acquiring     bool
	LastMessage   int
	HasMessage    bool
	Draining      bool
	DrainStarted  time.Time
	Forced        bool
	Fault         string
	Listener      *listener.Listener
	Cancel        context.CancelFunc
	Failures      int
	RetryAt       time.Time
	ListenerDone  bool
	ListenerReady chan struct{}
	CloseError    string
	RetryError    string
	StartError    string
}

const maxTargetRetryWait = time.Minute

var errUnsafeTarget = errors.New("unsafe target state")

func targetRetryWait(failures int) time.Duration {
	return min(time.Second<<min(max(failures-1, 0), 6), maxTargetRetryWait)
}

func permanentTargetFailure(err error) bool {
	return errors.Is(err, scaleset.BadRequestError) || errors.Is(err, scaleset.UnauthorizedError) || errors.Is(err, errUnsafeTarget)
}

type activeRun struct {
	ioDone          chan struct{}
	generation      uint64
	events          []runEvent
	ioResult        *runIOResult
	forceStop       bool
	output          *runOutputState
	State           *RunState
	Config          *settings.Config
	Preparing       bool
	Cancel          context.CancelFunc
	Exited          bool
	ExitError       string
	ExitResult      *runExit
	Uncertain       bool
	GroupStopped    bool // Observed absence does not authorize cleanup of uncertain state.
	CleanupTried    bool
	CleanupFailures int
	CleanupRetryAt  time.Time
	Stopping        bool
	Retiring        bool
	Target          *targetRuntime
	InspectFailures int
	JITSecrets      []string
}

type runOutputState struct {
	// ready and err may only be read after done closes.
	ready bool
	err   error
	done  chan struct{}
}

// runIOResult holds worker-only publication metadata, never live execution state.
type runIOResult struct {
	removed      bool
	persistError string
	startError   error
	readySaved   bool
}

type runExit struct {
	err      error
	stopping bool
}

func (r *activeRun) outputCollected() bool {
	if r.output == nil {
		return true
	}
	select {
	case <-r.output.done:
		return true
	default:
		return false
	}
}

func (r *activeRun) canRetireIdle() bool {
	return r.ioDone == nil && r.State.Ready && !r.State.Busy && !r.State.ProcessDone && !r.Uncertain &&
		!r.Preparing && r.InspectFailures == 0 && r.State.Phase != "exiting"
}

func (r *activeRun) occupiesCapacity() bool {
	if r.Uncertain {
		return !r.GroupStopped || !r.outputCollected() || (r.output != nil && !r.Exited)
	}
	return !r.State.ProcessDone
}

type Problem struct {
	Path     string `json:"path,omitempty"`
	TargetID string `json:"targetId,omitempty"`
	RunID    string `json:"runId,omitempty"`
	Reason   string `json:"reason"`
}

type OperationResult struct {
	TargetID string `json:"targetId,omitempty"`
	RunID    string `json:"runId,omitempty"`
	Path     string `json:"path,omitempty"`
	Action   string `json:"action,omitempty"`
	Error    string `json:"error,omitempty"`
}

type TargetStatus struct {
	ID         string `json:"id"`
	Assigned   int    `json:"assignedJobs"`
	Pending    int    `json:"pendingAcquisitions"`
	Active     int    `json:"activeRunners"`
	Draining   bool   `json:"draining"`
	Forced     bool   `json:"forced"`
	Fault      string `json:"fault,omitempty"`
	ScaleSetID int    `json:"scaleSetId,omitempty"`
}

type Status struct {
	Config   string         `json:"config"`
	Capacity int            `json:"capacity"`
	Occupied int            `json:"occupied"`
	Targets  []TargetStatus `json:"targets"`
	Runs     []*RunState    `json:"runs"`
	Problems []Problem      `json:"problems"`
}

type runEvent struct {
	id   string
	kind string
	err  error
}

type daemon struct {
	mu               sync.Mutex
	retentionMu      sync.Mutex
	config           *settings.Config
	secrets          *logreduct.Redactor
	store            stateStore
	user             executionUser
	distribution     *runnerdist.Manager
	targets          map[string]*targetRuntime
	order            []string
	runs             map[string]*activeRun
	problems         []Problem
	events           chan runEvent
	ctx              context.Context
	cancel           context.CancelFunc
	workers          sync.WaitGroup
	retired          []*targetRuntime
	applying         bool
	reloading        bool
	applyCancel      context.CancelFunc
	stopping         bool
	offline          bool
	retaining        bool
	gcActive         int
	inspecting       chan struct{}
	nextRetention    time.Time
	clientFactory    func(context.Context, settings.Target) (scaleAPI, error)
	listen           func(*targetRuntime)
	makeRunDirectory func(string) error
}

func newDaemon(c *settings.Config) (*daemon, error) {
	account, err := resolveUser(c.Runner.User)
	if err != nil {
		return nil, err
	}
	secret := &logreduct.Default
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if c.SecretEnv(name) {
			secret.Add(value)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{
		config: c, user: account, secrets: secret,
		store: stateStore{c.Runner.WorkRoot}, distribution: runnerdist.New(preparationTimeout),
		targets: map[string]*targetRuntime{}, runs: map[string]*activeRun{}, events: make(chan runEvent, 1024),
		ctx: ctx, cancel: cancel,
		makeRunDirectory: createRunDirectory,
	}
	d.clientFactory = func(ctx context.Context, target settings.Target) (scaleAPI, error) {
		return newScaleClient(ctx, target, secret)
	}
	d.listen = d.startListener
	return d, nil
}

func retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second << (attempt - 1)):
			}
		}
		err = fn()
		if err == nil || permanentTargetFailure(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func (d *daemon) initialize(ctx context.Context, offline bool) error {
	d.offline = offline
	for _, config := range d.config.Targets {
		target := &targetRuntime{Config: config}
		d.targets[config.ID] = target
		d.order = append(d.order, config.ID)
		d.registerTarget(ctx, target, !offline)
	}
	if err := d.recover(ctx); err != nil {
		return err
	}
	for _, id := range d.order {
		target := d.targets[id]
		if !offline && target.Fault == "" {
			d.listen(target)
		}
	}
	return nil
}

func (d *daemon) registerTarget(ctx context.Context, target *targetRuntime, create bool) {
	err := retry(ctx, func() error {
		return d.registrationAttempt(ctx, target, create)
	})
	if err != nil {
		target.RetryError = d.secrets.Redact(err.Error())
		if !create || permanentTargetFailure(err) {
			target.Fault = target.RetryError
		}
		slog.Error("Target registration failed", "event", "target.failed", "targetId", d.secrets.Redact(target.Config.ID), "error", target.RetryError)
	} else if target.Set == nil {
		target.Fault = "scale set is not registered"
	}
}

// The caller owns target until the result is published under d.mu.
func (d *daemon) registrationAttempt(ctx context.Context, target *targetRuntime, create bool) error {
	callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	client, err := d.clientFactory(callCtx, target.Config)
	if err != nil {
		return err
	}
	set, err := registration(callCtx, client, target.Config, create)
	if err != nil {
		return err
	}
	if set != nil && (set.ID <= 0 || set.RunnerGroupID <= 0) {
		return fmt.Errorf("%w: GitHub returned an invalid scale set registration", errUnsafeTarget)
	}
	target.Client, target.Set = client, set
	if set != nil {
		copy := *set
		target.Set = &copy
		if set.Statistics != nil {
			target.Assigned = set.Statistics.TotalAssignedJobs
		}
	}
	return nil
}

func (d *daemon) save(run *activeRun) error {
	if err := d.store.save(run.State); err != nil {
		run.Uncertain = true
		run.State.Error = fmt.Sprintf("persist state: %v", err)
		if run.ioResult != nil {
			run.ioResult.persistError = run.State.Error
		} else if target := d.runTarget(run); target != nil {
			target.Fault = run.State.Error
		}
		slog.Error("State update failed; execution retained", "event", "state.failed", "runId", d.secrets.Redact(run.State.RunID), "error", d.secrets.Redact(err.Error()))
		return err
	}
	return nil
}

// runIO transfers a private execution snapshot to one worker. The live record
// remains a coherent, conservative snapshot until the worker publishes its
// result. Callers hold mu; the I/O functions themselves never unlock it.
func (d *daemon) runIO(run *activeRun, operation func(*activeRun) error) <-chan struct{} {
	if run.ioDone != nil {
		return run.ioDone
	}
	done := make(chan struct{})
	run.generation++
	work := *run
	state := *run.State
	state.Members = append([]procgroup.ProcessIdentity(nil), state.Members...)
	result := &runIOResult{}
	work.State, work.ioResult = &state, result
	work.Target, work.Config = d.runTarget(run), run.Config
	if work.Target != nil {
		work.forceStop = work.Target.Forced
	}
	if work.Config == nil {
		work.Config = d.config
	}
	run.ioDone = done
	d.workers.Go(func() {
		err := operation(&work)
		d.mu.Lock()
		defer d.mu.Unlock()
		events := run.events
		*run.State = *work.State
		state := run.State
		*run = work
		run.State, run.ioResult, run.ioDone, run.events = state, nil, nil, nil
		run.generation++
		if result.persistError != "" && work.Target != nil {
			work.Target.Fault = result.persistError
		}
		if result.readySaved && work.Target != nil {
			work.Target.StartError, work.Target.Failures = "", 0
		}
		if result.startError != nil && work.Target != nil {
			d.startFailure(work.Target, result.startError)
		}
		if err != nil {
			run.State.Error = d.secrets.Redact(err.Error())
		}
		if result.removed {
			for _, event := range events {
				if event.kind == "exit" {
					d.processEvent(event)
				}
			}
			d.finishExit(run)
			delete(d.runs, state.RunID)
			d.secrets.Release(run.JITSecrets)
		} else {
			for _, event := range events {
				d.processEvent(event)
			}
			d.finishExit(run)
		}
		close(done)
	})
	return done
}

func (d *daemon) statusLocked() Status {
	status := Status{Config: d.config.Path, Capacity: d.config.Capacity.MaxRunners, Occupied: d.occupied(), Problems: append([]Problem(nil), d.problems...)}
	for _, id := range d.order {
		target := d.targets[id]
		item := TargetStatus{ID: id, Assigned: target.Assigned, Pending: target.Pending, Active: d.active(id), Draining: target.Draining, Forced: target.Forced, Fault: target.Fault}
		if target.Set != nil {
			item.ScaleSetID = target.Set.ID
		}
		status.Targets = append(status.Targets, item)
		if target.Fault != "" {
			status.Problems = append(status.Problems, Problem{TargetID: id, Reason: target.Fault})
		} else if target.RetryError != "" {
			status.Problems = append(status.Problems, Problem{TargetID: id, Reason: target.RetryError})
		}
		if target.Fault == "" && target.StartError != "" {
			status.Problems = append(status.Problems, Problem{TargetID: id, Reason: target.StartError})
		}
		if target.Forced {
			status.Problems = append(status.Problems, Problem{TargetID: id, Reason: "drain deadline exceeded; jobs may be unfinished"})
		}
	}
	for _, run := range d.runs {
		copy := *run.State
		copy.Members = append([]procgroup.ProcessIdentity(nil), run.State.Members...)
		status.Runs = append(status.Runs, &copy)
		if run.State.Error != "" || run.Uncertain {
			status.Problems = append(status.Problems, Problem{TargetID: d.runTargetID(run), RunID: run.State.RunID, Path: run.State.RunnerDir, Reason: run.State.Error})
		}
	}
	for _, target := range d.retired {
		status.Problems = append(status.Problems, Problem{
			TargetID: target.Config.ID,
			Reason:   fmt.Sprintf("retired scale set %q could not be deleted; run apply to retry", target.Config.ScaleSetName),
		})
	}
	return status
}
