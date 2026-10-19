//go:build linux || darwin

package ghalet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type controlRequest struct {
	Command      string `json:"command"`
	Target       string `json:"target,omitempty"`
	DryRun       bool   `json:"dryRun,omitempty"`
	RunnerGroup  string `json:"runnerGroup,omitempty"`
	ScaleSet     string `json:"scaleSet,omitempty"`
	Confirmation string `json:"confirmation,omitempty"`
}

type controlResponse struct {
	Status  *Status           `json:"status,omitempty"`
	Results []OperationResult `json:"results,omitempty"`
	Plan    *destroyPlan      `json:"plan,omitempty"`
	Error   string            `json:"error,omitempty"`
}

type destroyPlan struct {
	Target       string `json:"target"`
	URL          string `json:"url"`
	RunnerGroup  string `json:"runnerGroup"`
	ScaleSet     string `json:"scaleSet"`
	ScaleSetID   int    `json:"scaleSetId"`
	GroupID      int    `json:"groupId"`
	Confirmation string `json:"confirmation"`
}

func socketPath(config string) string {
	sum := sha256.Sum256([]byte(config))
	return filepath.Join("/tmp", "ghalet-"+strconv.Itoa(os.Geteuid()), hex.EncodeToString(sum[:12])+".sock")
}

type controlServer struct {
	listener net.Listener
	lock     *os.File
	path     string
}

func listenControl(config string) (*controlServer, error) {
	file, err := lockControl(config)
	if err != nil {
		return nil, err
	}
	path := socketPath(config)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		file.Close()
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		file.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		file.Close()
		return nil, err
	}
	return &controlServer{listener: listener, lock: file, path: path}, nil
}

func lockControl(config string) (*os.File, error) {
	path := socketPath(config)
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("control socket directory is not private and owned by the management user")
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another daemon is using this config path")
	}
	return file, nil
}

func (s *controlServer) close() {
	s.listener.Close()
	os.Remove(s.path)
	s.lock.Close()
}

func (s *controlServer) serve(d *daemon) {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				d.log.Error("Control socket accept failed", "error", err)
			}
			return
		}
		go func() {
			defer connection.Close()
			if err := connection.SetReadDeadline(time.Now().Add(commandTimeout)); err != nil {
				d.log.Error("Control socket deadline failed", "error", err)
				return
			}
			var request controlRequest
			decoder := json.NewDecoder(io.LimitReader(connection, 64*1024))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				json.NewEncoder(connection).Encode(controlResponse{Error: "invalid control request"})
				return
			}
			response := d.handle(d.ctx, request)
			if err := connection.SetWriteDeadline(time.Now().Add(commandTimeout)); err != nil {
				d.log.Error("Control socket write deadline failed", "error", err)
				return
			}
			if err := json.NewEncoder(connection).Encode(response); err != nil {
				d.log.Error("Control response failed", "error", err)
			}
		}()
	}
}

func controlCall(ctx context.Context, path string, request controlRequest) (controlResponse, error) {
	var response controlResponse
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socketPath(path))
	if err != nil {
		return response, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return response, err
		}
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return response, err
	}
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return response, err
	}
	return response, nil
}

func (d *daemon) handle(ctx context.Context, request controlRequest) controlResponse {
	switch request.Command {
	case "status", "doctor":
		d.mu.Lock()
		status := d.statusLocked()
		d.mu.Unlock()
		response := controlResponse{Status: &status}
		if request.Command == "doctor" && len(status.Problems) > 0 {
			response.Error = fmt.Sprintf("%d diagnostic problem(s); inspect targets and executions before restarting", len(status.Problems))
		}
		return response
	case "drain":
		return d.drain(ctx, request.Target)
	case "gc":
		return d.gc(ctx, request.DryRun)
	case "destroy-plan":
		d.mu.Lock()
		defer d.mu.Unlock()
		plan, err := d.planDestroy(ctx, request)
		if err != nil {
			return controlResponse{Error: d.secrets.redact(err.Error())}
		}
		return controlResponse{Plan: &plan}
	case "destroy":
		return d.destroy(ctx, request.Confirmation)
	default:
		return controlResponse{Error: "unknown control command"}
	}
}

func (d *daemon) drain(ctx context.Context, id string) controlResponse {
	d.mu.Lock()
	if id != "" && d.targets[id] == nil {
		d.mu.Unlock()
		return controlResponse{Error: fmt.Sprintf("unknown target %q", id)}
	}
	for _, target := range d.targets {
		if id == "" || id == target.Config.ID {
			d.beginDrain(target)
		}
	}
	d.mu.Unlock()
	for {
		d.mu.Lock()
		done := d.drained(id)
		status := d.statusLocked()
		d.mu.Unlock()
		if done {
			response := controlResponse{Status: &status}
			for _, problem := range status.Problems {
				if id == "" || problem.TargetID == id || problem.TargetID == "" {
					response.Error = "drain incomplete: inspect diagnostics; forced or unverified work and pending cleanup are not success"
					break
				}
			}
			return response
		}
		select {
		case <-ctx.Done():
			return controlResponse{Error: "drain interrupted: " + ctx.Err().Error()}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (d *daemon) gc(ctx context.Context, dryRun bool) controlResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	response := controlResponse{}
	table, err := processTable(ctx)
	if err != nil {
		return controlResponse{Error: err.Error()}
	}
	for _, problem := range d.problems {
		response.Results = append(response.Results, OperationResult{Path: problem.Path, Error: problem.Reason})
	}
	for _, run := range d.runs {
		state := run.State
		result := OperationResult{RunID: state.RunID, Path: filepath.Dir(state.RunnerDir), Action: "cleanup stopped execution"}
		if run.Uncertain || run.Preparing {
			result.Error = "execution is preparing or uncertain; inspect manually, never delete state to free capacity"
		} else {
			alive, _, err := inspectGroup(state, table)
			if err != nil {
				result.Error = err.Error()
			} else if alive {
				result.Action = "retain active execution"
			} else if !run.outputCollected() || (run.WaitDone != nil && !run.Exited && !run.Stopping) {
				result.Action = "retain execution awaiting output collection or exit observation"
			} else if !dryRun {
				state.ProcessDone, state.Phase = true, "cleaning"
				if err := d.save(run); err != nil {
					result.Error = err.Error()
				} else if err := d.cleanup(run); err != nil {
					result.Error = d.secrets.redact(err.Error())
				}
			}
		}
		response.Results = append(response.Results, result)
	}
	expired, err := expireLogs(ctx, d.config, dryRun)
	response.Results = append(response.Results, expired...)
	if err != nil {
		response.Error = err.Error()
	}
	for _, result := range response.Results {
		if result.Error != "" {
			response.Error = "garbage collection is incomplete; inspect per-execution results"
		}
	}
	return response
}

func (d *daemon) planDestroy(ctx context.Context, request controlRequest) (destroyPlan, error) {
	if request.Target == "" || request.ScaleSet == "" {
		return destroyPlan{}, errors.New("destroy requires explicit --target and --scale-set")
	}
	config, err := d.config.target(request.Target)
	if err != nil {
		return destroyPlan{}, err
	}
	config.RunnerGroup, config.ScaleSetName = request.RunnerGroup, request.ScaleSet
	u, _ := scopeURL(config.URL)
	if len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 2 && request.RunnerGroup != "" {
		return destroyPlan{}, errors.New("repository destruction cannot specify --runner-group")
	}
	if len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 1 && request.RunnerGroup == "" {
		return destroyPlan{}, errors.New("organization destruction requires explicit --runner-group")
	}
	callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	client, err := d.clientFactory(callCtx, config)
	if err != nil {
		return destroyPlan{}, err
	}
	set, err := registration(callCtx, client, config, false)
	if err != nil {
		return destroyPlan{}, err
	}
	if set == nil {
		return destroyPlan{}, errors.New("explicit scale set does not exist; nothing was deleted")
	}
	nonce, err := newID()
	if err != nil {
		return destroyPlan{}, err
	}
	plan := destroyPlan{
		Target: request.Target, URL: config.URL, RunnerGroup: request.RunnerGroup,
		ScaleSet: request.ScaleSet, ScaleSetID: set.ID, GroupID: set.RunnerGroupID, Confirmation: nonce,
	}
	d.confirmations[nonce] = plan
	return plan, nil
}

func (d *daemon) destroy(ctx context.Context, confirmation string) controlResponse {
	d.mu.Lock()
	plan, ok := d.confirmations[confirmation]
	delete(d.confirmations, confirmation)
	if !ok {
		d.mu.Unlock()
		return controlResponse{Error: "missing, expired or previously used interactive confirmation"}
	}
	config, err := d.config.target(plan.Target)
	if err != nil {
		d.mu.Unlock()
		return controlResponse{Error: err.Error()}
	}
	config.RunnerGroup, config.ScaleSetName = plan.RunnerGroup, plan.ScaleSet
	callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	client, err := d.clientFactory(callCtx, config)
	if err != nil {
		cancel()
		d.mu.Unlock()
		return controlResponse{Error: d.secrets.redact(err.Error())}
	}
	set, err := registration(callCtx, client, config, false)
	cancel()
	if err != nil || set == nil || set.ID != plan.ScaleSetID || set.RunnerGroupID != plan.GroupID {
		d.mu.Unlock()
		return controlResponse{Error: "deletion target changed or cannot be verified; confirm the explicit target again"}
	}
	if len(d.problems) > 0 {
		d.mu.Unlock()
		return controlResponse{Error: "unrecognized or corrupt local executions prevent safe destruction; inspect doctor and resolve them first"}
	}
	if set.Statistics == nil {
		d.mu.Unlock()
		return controlResponse{Error: "scale set statistics are unavailable; registration retained"}
	}
	var target *targetRuntime
	for _, candidate := range d.targets {
		if candidate.Set != nil && candidate.Set.ID == plan.ScaleSetID &&
			candidate.Config.URL == plan.URL {
			target = candidate
			break
		}
	}
	if target == nil {
		id := "destroy-" + confirmation[:12]
		config.ID = id
		config.Scaling = ScalingConfig{MaxRunners: d.config.Capacity.MaxRunners}
		target = &targetRuntime{Config: config, Client: client, Set: set}
	}
	if err := d.adoptForDestroy(ctx, target, plan); err != nil {
		d.mu.Unlock()
		return controlResponse{Error: d.secrets.redact(err.Error())}
	}
	if d.targets[target.Config.ID] == nil {
		d.targets[target.Config.ID] = target
		d.order = append(d.order, target.Config.ID)
	}
	target.Assigned = max(target.Assigned, set.Statistics.TotalAssignedJobs)
	d.beginDrain(target)
	if target.Listener == nil && target.Cancel == nil {
		d.startListener(target)
	}
	id := target.Config.ID
	d.mu.Unlock()
	response := d.drain(ctx, id)
	if response.Error != "" {
		return response
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// No approval survives a restart, and a timeout never authorizes deletion.
	if target.Forced || !d.drained(id) {
		return controlResponse{Error: "drain was forced or incomplete; scale set retained"}
	}
	callCtx, cancel = context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	current, err := registration(callCtx, client, target.Config, false)
	if err != nil || current == nil || current.ID != plan.ScaleSetID {
		return controlResponse{Error: "scale set changed after confirmation; retained"}
	}
	if current.Statistics == nil || current.Statistics.TotalAssignedJobs != 0 ||
		current.Statistics.TotalBusyRunners != 0 || current.Statistics.TotalRegisteredRunners != 0 {
		return controlResponse{Error: "scale set still has assigned work or its statistics are unavailable; retained"}
	}
	if target.Cancel != nil {
		target.Cancel()
		d.mu.Unlock()
		for {
			d.mu.Lock()
			done := target.ListenerDone
			d.mu.Unlock()
			if done {
				break
			}
			select {
			case <-ctx.Done():
				d.mu.Lock()
				return controlResponse{Error: "message session did not finish closing; registration retained"}
			case <-time.After(100 * time.Millisecond):
			}
		}
		d.mu.Lock()
	}
	cancel()
	callCtx, cancel = context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	current, err = registration(callCtx, client, target.Config, false)
	if err != nil || current == nil || current.ID != plan.ScaleSetID || current.RunnerGroupID != plan.GroupID ||
		current.Statistics == nil || current.Statistics.TotalAssignedJobs != 0 || current.Statistics.TotalRegisteredRunners != 0 {
		return controlResponse{Error: "registration changed or work remained after session close; confirm again"}
	}
	if err := client.DeleteRunnerScaleSet(callCtx, plan.ScaleSetID); err != nil {
		return controlResponse{Error: d.secrets.redact(err.Error())}
	}
	target.Set = nil
	return controlResponse{Results: []OperationResult{{Action: "deleted explicitly approved scale set " + plan.ScaleSet}}}
}

func (d *daemon) adoptForDestroy(ctx context.Context, target *targetRuntime, plan destroyPlan) error {
	table, err := processTable(ctx)
	if err != nil {
		return err
	}
	type adoption struct {
		run     *activeRun
		alive   bool
		members []ProcessIdentity
	}
	var pending []adoption
	for _, run := range d.runs {
		state := run.State
		if state.ScopeURL == plan.URL && state.ScaleSetID == plan.ScaleSetID &&
			(state.ScaleSetName != plan.ScaleSet || runnerGroupName(state.GroupName) != runnerGroupName(plan.RunnerGroup)) {
			return errors.New("local execution names conflict with the approved scale set ID; manual inspection required")
		}
		if state.ScopeURL != plan.URL || state.ScaleSetName != plan.ScaleSet || runnerGroupName(state.GroupName) != runnerGroupName(plan.RunnerGroup) {
			continue
		}
		if d.runTarget(run) == target && !run.Uncertain {
			continue
		}
		if run.Preparing {
			return errors.New("retained execution is still preparing; wait for it to settle before destruction")
		}
		if state.ScaleSetID != plan.ScaleSetID || state.GroupID != plan.GroupID ||
			state.User != d.user.Name || state.UID != d.user.UID || state.Home != d.user.Home {
			return errors.New("local execution does not match the explicitly approved registration or execution user")
		}
		if _, err := d.store.directory(state); err != nil {
			return err
		}
		alive, members, err := inspectGroup(state, table)
		if err != nil {
			return fmt.Errorf("cannot drain retained execution %s: %w", state.RunID, err)
		}
		if !alive && run.WaitDone != nil && (!run.Exited || !run.outputCollected()) {
			return fmt.Errorf("retained execution %s is awaiting local exit observation or output collection", state.RunID)
		}
		if alive {
			callCtx, cancel := context.WithTimeout(ctx, apiTimeout)
			runner, err := target.Client.GetRunner(callCtx, state.RunnerID)
			cancel()
			if err != nil || runner == nil || runner.Name != state.RunnerName || runner.RunnerScaleSetID != plan.ScaleSetID {
				return fmt.Errorf("cannot verify retained runner %s at GitHub; registration retained", state.RunID)
			}
		}
		pending = append(pending, adoption{run: run, alive: alive, members: members})
	}
	for _, candidate := range pending {
		run := candidate.run
		state := run.State
		// This association is memory-only. A restart must require a fresh
		// explicit target and confirmation rather than resuming destruction.
		run.Target, run.Uncertain, run.State.Error = target, false, ""
		state.Members = candidate.members
		if !candidate.alive {
			state.ProcessDone, state.Phase = true, "cleaning"
			run.Exited = true
		}
		if err := d.save(run); err != nil {
			for _, item := range pending {
				item.run.Uncertain = true
				item.run.State.Error = "destruction adoption did not finish: " + d.secrets.redact(err.Error())
			}
			return err
		}
	}
	return nil
}
