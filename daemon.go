//go:build linux || darwin

package ghalet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/logreduct"
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

func (d *daemon) recover(ctx context.Context) error {
	records, err := d.store.load()
	if err != nil {
		return err
	}
	var identities []ProcessIdentity
	for _, record := range records {
		if record.Err == nil {
			identities = append(identities, record.State.Process)
		}
	}
	table, err := processTable(ctx, identities)
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
		state := record.State
		run := &activeRun{State: state}
		d.runs[state.RunID] = run
		alive, members, inspectErr := inspectGroup(state, table)
		if inspectErr == nil && !alive {
			state.ProcessDone, state.Phase = true, "cleaning"
			run.Exited, run.GroupStopped = true, true
		}
		target := d.targets[state.TargetID]
		reason := ""
		switch {
		case target == nil:
			reason = "state does not correspond to a currently configured target"
		case target.Set == nil || target.Fault != "":
			reason = "registration cannot be verified; resolve target authentication/registration and restart"
		case state.ScopeURL != target.Config.URL ||
			state.ScaleSetName != target.Config.ScaleSetName || settings.RunnerGroupName(state.GroupName) != settings.RunnerGroupName(target.Config.RunnerGroup) ||
			state.ScaleSetID != target.Set.ID || state.GroupID != target.Set.RunnerGroupID:
			reason = "recorded registration differs from the current explicit scope/group/scale set"
		case state.User != d.user.Name || state.UID != d.user.UID || state.Home != d.user.Home:
			reason = "recorded execution user differs from the current execution user"
		}
		if inspectErr != nil {
			reason = inspectErr.Error()
		} else if reason == "" {
			if alive {
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
		}
		if reason != "" {
			run.Uncertain = true
			state.Error = reason
			slog.Warn("Execution needs manual inspection", "event", "recovery.uncertain", "targetId", d.secrets.Redact(state.TargetID), "runId", d.secrets.Redact(state.RunID), "error", d.secrets.Redact(reason))
			continue
		}
		if !d.offline {
			if err := d.save(run); err != nil {
				run.Uncertain = true
				state.Error = err.Error()
			}
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
	state.Members = append([]ProcessIdentity(nil), state.Members...)
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
	}
	if !result.stopping && ((!run.State.Ready && !run.State.Busy) || result.err != nil) {
		if target := d.runTarget(run); target != nil && !target.Forced {
			err := result.err
			if err == nil {
				err = errors.New("runner exited before readiness")
			}
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

func (d *daemon) inspectRuns() {
	if d.inspecting != nil {
		return
	}
	type candidate struct {
		run        *activeRun
		generation uint64
	}
	var candidates []candidate
	var identities []ProcessIdentity
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
		table, err := processTable(d.ctx, identities)
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

func (d *daemon) reconcileRun(run *activeRun, table map[int]processEntry) {
	if run.Preparing {
		return
	}
	state := run.State
	if run.Uncertain {
		alive, _, err := inspectGroup(state, table)
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
	alive, members, err := inspectGroup(state, table)
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

func (d *daemon) reserve(target *targetRuntime) error {
	id, err := newID()
	if err != nil {
		return err
	}
	dir := filepath.Join(d.config.Runner.WorkRoot, target.Config.ID, id)
	state := &RunState{
		Schema: 1, RunID: id, TargetID: target.Config.ID,
		ScopeURL: target.Config.URL, GroupName: target.Config.RunnerGroup,
		GroupID: target.Set.RunnerGroupID, ScaleSetName: target.Config.ScaleSetName, ScaleSetID: target.Set.ID,
		RunnerName: "runner-" + id, User: d.user.Name, UID: d.user.UID, Home: d.user.Home,
		RunnerDir: filepath.Join(dir, "runner"), Workspace: filepath.Join(dir, "work"), Phase: "preparing",
	}
	run := &activeRun{State: state, Preparing: true, Target: target, Config: d.config}
	d.runs[id] = run
	ctx, cancel := context.WithTimeout(d.ctx, preparationTimeout)
	run.Cancel = cancel
	if target.directoryMu == nil {
		target.directoryMu = &sync.Mutex{}
	}
	directoryMu := target.directoryMu
	target.RetryAt = time.Now().Add(time.Second)
	d.runIO(run, func(work *activeRun) error {
		defer cancel()
		// A second reservation must not observe a newly created target
		// directory before its creator has synced the WorkRoot entry.
		directoryMu.Lock()
		err := d.makeRunDirectory(dir)
		directoryMu.Unlock()
		if err != nil {
			work.Preparing, work.ioResult.removed, work.ioResult.startError = false, true, err
			return err
		}
		err = d.save(work)
		if err == nil {
			err = d.prepare(ctx, target, work)
		}
		work.Preparing = false
		if err != nil {
			work.State.Error = d.secrets.Redact(err.Error())
			if work.Uncertain {
				err = errors.Join(errUnsafeTarget, err)
			}
			// Rollback may wait on GitHub or files; stop accepting work for
			// a failed target before entering that slow cleanup.
			d.mu.Lock()
			d.startFailure(target, err)
			d.mu.Unlock()
			if !work.Uncertain {
				if stopErr := d.stop(work, true); stopErr != nil {
					return errors.Join(err, fmt.Errorf("cleanup: %w", stopErr))
				}
			}
		}
		return err
	})
	return nil
}

func createRunDirectory(dir string) error {
	if err := ensureExecutionDirectory(filepath.Dir(dir)); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return err
		}
		return errors.Join(errUnsafeTarget, err)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	if err := fshelper.SyncDirectory(filepath.Dir(dir)); err != nil {
		return errors.Join(err, os.Remove(dir))
	}
	return nil
}

func (d *daemon) prepare(ctx context.Context, target *targetRuntime, run *activeRun) error {
	state := run.State
	config := run.Config
	start := time.Now()
	if err := d.distribution.Prepare(ctx, runnerdist.Config{
		Version: config.Runner.Version, CacheRoot: config.Runner.CacheRoot,
	}, state.RunnerDir); err != nil {
		return fmt.Errorf("prepare runner: %w", err)
	}
	if err := fshelper.EnsureDirectory(state.Workspace, 0700); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		for _, dir := range []string{state.RunnerDir, state.Workspace} {
			if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Lchown(path, d.user.UID, d.user.GID)
			}); err != nil {
				return err
			}
		}
	}
	check := exec.CommandContext(ctx, "/bin/sh", "-c", `test -d "$HOME" && test -w "$1" && test -x "$2"`, "ghalet-access", state.Workspace, filepath.Join(state.RunnerDir, "bin", "Runner.Listener"))
	check.Dir, check.Env, check.SysProcAttr = state.RunnerDir, runnerEnvironment(config, d.user, ""), processAttributes(d.user)
	if err := check.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return err
		}
		return fmt.Errorf("%w: execution user cannot access HOME, workspace or runner: %w", errUnsafeTarget, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	jit, err := target.Client.GenerateJitRunnerConfig(apiCtx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name: state.RunnerName, WorkFolder: state.Workspace,
	}, state.ScaleSetID)
	cancel()
	if err != nil {
		return fmt.Errorf("generate JIT runner: %w", err)
	}
	if jit == nil || jit.Runner == nil || jit.Runner.ID <= 0 || jit.EncodedJITConfig == "" ||
		jit.Runner.Name != state.RunnerName || jit.Runner.RunnerScaleSetID != state.ScaleSetID {
		return fmt.Errorf("%w: GitHub returned incomplete JIT configuration; registration must be inspected", errUnsafeTarget)
	}
	run.JITSecrets = d.secrets.AddJIT(jit.EncodedJITConfig)
	for key, value := range config.Runner.Env {
		if d.secrets.Redact(value) != value {
			return fmt.Errorf("%w: runner.env %s contains a management credential; refusing to copy it into a job", errUnsafeTarget, key)
		}
	}
	state.RunnerID, state.Phase = jit.Runner.ID, "registered"
	if err := d.save(run); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.launch(ctx, run, jit.EncodedJITConfig); err != nil {
		return err
	}
	slog.Info("Runner started", "event", "runner.started", "scope", d.secrets.Redact(state.ScopeURL), "targetId", d.secrets.Redact(state.TargetID), "runId", d.secrets.Redact(state.RunID), "durationMs", time.Since(start).Milliseconds())
	return nil
}

func (d *daemon) launch(ctx context.Context, run *activeRun, jit string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	state := run.State
	spawned := false
	defer func() {
		if retErr != nil && !spawned && !run.Uncertain {
			state.Phase = "registered"
			if err := d.save(run); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	state.Phase = "starting"
	if err := d.save(run); err != nil {
		return err
	}
	input, output, err := os.Pipe()
	if err != nil {
		return err
	}
	defer input.Close()
	defer func() {
		if retErr != nil {
			output.Close()
		}
	}()
	command, err := runnerGuardCommand(filepath.Join(state.RunnerDir, "bin", "Runner.Listener"))
	if err != nil {
		return err
	}
	exitReport, reportOutput, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reportOutput.Close()
	defer func() {
		if retErr != nil {
			exitReport.Close()
		}
	}()
	command.ExtraFiles = []*os.File{input, reportOutput}
	logFile, err := os.OpenFile(filepath.Join(state.RunnerDir, "output.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	collected := &runOutputState{done: make(chan struct{})}
	writer := &runnerOutput{file: logFile, secrets: d.secrets, ready: func() {
		collected.ready = true
		select {
		case d.events <- runEvent{id: state.RunID, kind: "ready"}:
		case <-d.ctx.Done():
		}
	}}
	logInput, logOutput, err := os.Pipe()
	if err != nil {
		writer.close()
		return err
	}
	defer logOutput.Close()
	defer func() {
		if retErr != nil {
			logInput.Close()
		}
	}()
	config := run.Config
	if config == nil {
		config = d.config
	}
	command.Dir, command.Env, command.SysProcAttr = state.RunnerDir, runnerEnvironment(config, d.user, jit), processAttributes(d.user)
	command.Stdout, command.Stderr = logOutput, logOutput
	if err := command.Start(); err != nil {
		writer.close()
		return err
	}
	spawned = true
	logOutput.Close()
	// The child waits on fd 3. No runner can execute until its identity has
	// been durably recorded; controller death closes the gate without exec.
	table, err := processTable(ctx, []ProcessIdentity{{PID: command.Process.Pid}})
	if err == nil {
		var identity ProcessIdentity
		identity, err = launchedProcessIdentity(table, command.Process.Pid, d.user.UID)
		if err == nil {
			state.Process = identity
			state.Members = []ProcessIdentity{identity}
			state.StartedAt = time.Now().UTC()
			err = d.save(run)
		}
	}
	if err != nil {
		output.Close()
		command.Wait()
		writer.close()
		if ctx.Err() != nil {
			state.Phase = "registered"
		} else {
			run.Uncertain = true
		}
		return err
	}
	// Drain cancellation and gate release have one ordering point. The empty
	// pipe has room for this short write; no API, inspection or fsync is locked.
	d.mu.Lock()
	err = ctx.Err()
	if err == nil {
		_, err = output.Write([]byte("ready\n"))
	}
	d.mu.Unlock()
	if err != nil {
		output.Close()
		command.Wait()
		writer.close()
		return err
	}
	run.output = collected
	go func() {
		_, err := io.Copy(writer, logInput)
		collected.err = errors.Join(err, logInput.Close(), writer.close())
		close(collected.done)
	}()
	// Reaping and output collection remain independent so shutdown can
	// observe both without blocking diagnostics and control access.
	go func() {
		err := command.Wait()
		err = readRunnerGuardResult(exitReport, err)
		output.Close()
		select {
		case d.events <- runEvent{id: state.RunID, kind: "exit", err: err}:
		case <-d.ctx.Done():
		}
	}()
	return nil
}

func (d *daemon) startFailure(target *targetRuntime, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	target.StartError = d.secrets.Redact(err.Error())
	target.Failures = min(target.Failures+1, 7)
	target.RetryAt = time.Now().Add(targetRetryWait(target.Failures))
	if permanentTargetFailure(err) {
		target.Fault = d.secrets.Redact(err.Error())
		if target.Listener != nil {
			target.Listener.SetMaxRunners(0)
		}
	}
	slog.Error("Runner preparation/start failed", "event", "runner.failed", "targetId", d.secrets.Redact(target.Config.ID), "attempt", target.Failures, d.secrets.ReplaceAttr(nil, slog.Any("error", err)))
}

func (d *daemon) unregister(run *activeRun) error {
	state := run.State
	target := d.runTarget(run)
	if target == nil || target.Client == nil {
		return errors.New("cannot verify runner registration for cleanup")
	}
	ctx, cancel := context.WithTimeout(d.ctx, apiTimeout)
	defer cancel()
	var runner *scaleset.RunnerReference
	var err error
	if state.RunnerID > 0 {
		runner, err = target.Client.GetRunner(ctx, state.RunnerID)
	} else {
		runner, err = target.Client.GetRunnerByName(ctx, state.RunnerName)
	}
	if errors.Is(err, scaleset.RunnerNotFoundError) {
		return nil
	}
	if err != nil {
		return err
	}
	if runner == nil {
		return nil
	}
	if runner.Name != state.RunnerName || runner.RunnerScaleSetID != state.ScaleSetID || (state.RunnerID != 0 && state.RunnerID != runner.ID) {
		return errors.New("runner registration does not match the recovery record")
	}
	err = target.Client.RemoveRunner(ctx, int64(runner.ID))
	if errors.Is(err, scaleset.RunnerNotFoundError) {
		return nil
	}
	return err
}

func (d *daemon) retireIdle(run *activeRun) error {
	if run.State.Busy || run.Uncertain || run.Preparing {
		return errors.New("runner is busy, preparing or uncertain")
	}
	// Server-side deletion is the arbitration point with job assignment.
	// Never stop the process if GitHub refuses deletion or cannot be queried.
	if err := d.unregister(run); err != nil {
		if errors.Is(err, scaleset.JobStillRunningError) {
			newlyReady := !run.State.Ready
			run.State.Busy, run.State.Ready, run.State.Phase = true, true, "busy"
			if d.save(run) == nil && newlyReady {
				if run.ioResult != nil {
					run.ioResult.readySaved = true
				} else if target := d.runTarget(run); target != nil {
					target.StartError, target.Failures = "", 0
				}
			}
		}
		return err
	}
	return d.stop(run, false)
}

func (d *daemon) stop(run *activeRun, forced bool) error {
	if run.Uncertain {
		return errors.New("uncertain process identity; manual inspection required")
	}
	if run.State.Busy && !run.Exited && !forced {
		return errors.New("refusing to stop a busy runner")
	}
	run.Stopping = true
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	err := stopGroup(ctx, run.State, func() error { return d.save(run) })
	if err != nil {
		return err
	}
	if run.output != nil {
		select {
		case <-run.output.done:
		case <-ctx.Done():
			return errors.New("process stopped but runner output collection has not completed")
		}
	}
	d.finishExit(run)
	run.GroupStopped = true
	run.State.ProcessDone, run.State.Phase = true, "cleaning"
	if err := d.save(run); err != nil {
		return err
	}
	return d.cleanup(run)
}

func (d *daemon) cleanup(run *activeRun) error {
	if !run.State.ProcessDone || run.Uncertain {
		return errors.New("cannot clean an unverified execution")
	}
	if !run.outputCollected() {
		return errors.New("runner output collection has not completed")
	}
	if run.output != nil && !run.Exited && !run.Stopping {
		return errors.New("runner exit observation has not completed")
	}
	d.finishExit(run)
	run.CleanupTried = true
	fail := func(err error, retryable bool) error {
		run.CleanupRetryAt = time.Time{}
		run.State.Error = d.secrets.Redact(err.Error())
		if d.save(run) == nil && retryable && d.ctx.Err() == nil {
			run.CleanupFailures = min(run.CleanupFailures+1, 7)
			delay := min(5*time.Second<<uint(run.CleanupFailures-1), 5*time.Minute)
			run.CleanupRetryAt = time.Now().Add(delay)
		}
		slog.Error("Cleanup incomplete; state and files retained", "event", "cleanup.failed", "runId", d.secrets.Redact(run.State.RunID), "targetId", d.secrets.Redact(run.State.TargetID), "error", d.secrets.Redact(err.Error()), "retryAt", run.CleanupRetryAt)
		return err
	}
	if run.output != nil && run.output.err != nil {
		return fail(fmt.Errorf("collect runner output: %w", run.output.err), false)
	}
	if err := d.unregister(run); err != nil {
		return fail(err, cleanupRetryable(err))
	}
	config := run.Config
	if config == nil {
		config = d.config
	}
	if err := archiveLogs(config, run.State, d.secrets); err != nil {
		return fail(err, cleanupRetryable(err))
	}
	if err := d.store.removeFiles(run.State); err != nil {
		return fail(err, cleanupRetryable(err))
	}
	if run.ioResult != nil {
		run.ioResult.removed = true
	} else {
		delete(d.runs, run.State.RunID)
		d.secrets.Release(run.JITSecrets)
	}
	slog.Info("Execution cleaned", "event", "runner.removed", "targetId", d.secrets.Redact(run.State.TargetID), "runId", d.secrets.Redact(run.State.RunID))
	return nil
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
		copy.Members = append([]ProcessIdentity(nil), run.State.Members...)
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
