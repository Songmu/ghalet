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
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Songmu/ghalet/procgroup"
)

type controlRequest struct {
	Command        string `json:"command"`
	DryRun         bool   `json:"dryRun,omitempty"`
	ExpectedConfig string `json:"expectedConfig,omitempty"`
}

type controlResponse struct {
	Status  *Status           `json:"status,omitempty"`
	Results []OperationResult `json:"results,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func socketPath(config string) string {
	return filepath.Join("/tmp", "ghalet-"+strconv.Itoa(os.Geteuid()), socketName(config))
}

func socketName(config string) string {
	sum := sha256.Sum256([]byte(config))
	return hex.EncodeToString(sum[:12]) + ".sock"
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
				slog.Error("Control socket accept failed", "error", d.secrets.Redact(err.Error()))
			}
			return
		}
		go func() {
			defer connection.Close()
			if err := connection.SetReadDeadline(time.Now().Add(commandTimeout)); err != nil {
				slog.Error("Control socket deadline failed", "error", d.secrets.Redact(err.Error()))
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
				slog.Error("Control socket write deadline failed", "error", d.secrets.Redact(err.Error()))
				return
			}
			if err := json.NewEncoder(connection).Encode(response); err != nil {
				slog.Error("Control response failed", "error", d.secrets.Redact(err.Error()))
			}
		}()
	}
}

func controlCall(ctx context.Context, path string, request controlRequest) (controlResponse, error) {
	request.ExpectedConfig = path
	return controlCallSocket(ctx, socketPath(path), os.Geteuid(), request)
}

func controlCallSocket(ctx context.Context, socket string, uid int, request controlRequest) (controlResponse, error) {
	var response controlResponse
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return response, err
	}
	defer connection.Close()
	if err := checkControlPeer(connection, uid); err != nil {
		return response, err
	}
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
	return decodeControlResponse(connection)
}

const maxControlResponseSize = 4 * 1024 * 1024

func decodeControlResponse(reader io.Reader) (controlResponse, error) {
	var response controlResponse
	limited := &io.LimitedReader{R: reader, N: maxControlResponseSize + 1}
	err := json.NewDecoder(limited).Decode(&response)
	if limited.N == 0 {
		return controlResponse{}, errors.New("control response exceeds 4 MiB")
	}
	return response, err
}

func checkControlPeer(connection net.Conn, uid int) error {
	conn, ok := connection.(syscall.Conn)
	if !ok {
		return errors.New("control connection does not expose peer credentials")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerUID int
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		peerUID, peerErr = socketPeerUID(int(fd))
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return fmt.Errorf("read control peer UID: %w", peerErr)
	}
	if peerUID != uid {
		return fmt.Errorf("control peer UID %d does not match expected UID %d", peerUID, uid)
	}
	return nil
}

func (d *daemon) handle(ctx context.Context, request controlRequest) controlResponse {
	if request.ExpectedConfig != "" {
		d.mu.Lock()
		matches := request.ExpectedConfig == d.config.Path
		d.mu.Unlock()
		if !matches {
			return controlResponse{Error: "control daemon configuration does not match expected config"}
		}
	}
	switch request.Command {
	case "status":
		d.mu.Lock()
		status := d.statusLocked()
		d.mu.Unlock()
		response := controlResponse{Status: &status}
		if len(status.Problems) > 0 {
			response.Error = fmt.Sprintf("%d diagnostic problem(s); inspect targets and executions before applying or restarting", len(status.Problems))
		}
		return response
	case "apply":
		return d.apply(ctx)
	case "gc":
		return d.gc(ctx, request.DryRun)
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
	if d.ctx.Err() != nil {
		d.mu.Unlock()
		return controlResponse{Error: "daemon has stopped; retry gc after shutdown"}
	}
	d.gcActive++
	defer func() {
		d.mu.Lock()
		d.gcActive--
		d.mu.Unlock()
	}()
	response := controlResponse{}
	config := d.config
	var runs []*activeRun
	identities := map[*activeRun]procgroup.ProcessIdentity{}
	generations := map[*activeRun]uint64{}
	var monitored []procgroup.ProcessIdentity
	for _, problem := range d.problems {
		response.Results = append(response.Results, OperationResult{Path: problem.Path, Error: problem.Reason})
	}
	for _, run := range d.runs {
		runs = append(runs, run)
		identities[run] = run.State.Process
		generations[run] = run.generation
		monitored = append(monitored, run.State.Process)
	}
	d.mu.Unlock()
	table, err := procgroup.Snapshot(ctx, monitored)
	if err != nil {
		return controlResponse{Error: err.Error()}
	}
	for _, run := range runs {
		response.Results = append(response.Results, d.gcRun(ctx, run, dryRun, table, identities[run], generations[run]))
	}
	d.retentionMu.Lock()
	expired, err := expireLogs(ctx, config, dryRun)
	d.retentionMu.Unlock()
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

func (d *daemon) gcRun(ctx context.Context, run *activeRun, dryRun bool, table map[int]procgroup.Entry, identity procgroup.ProcessIdentity, generation uint64) OperationResult {
	d.mu.Lock()
	result := OperationResult{RunID: run.State.RunID, Path: filepath.Dir(run.State.RunnerDir), Action: "cleanup stopped execution"}
	for run.ioDone != nil && !run.Preparing && !run.Uncertain {
		done := run.ioDone
		d.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			result.Error = ctx.Err().Error()
			return result
		}
		d.mu.Lock()
	}
	if d.runs[run.State.RunID] != run {
		d.mu.Unlock()
		return result
	}
	if d.ctx.Err() != nil {
		result.Error = "daemon has stopped; retry gc after shutdown"
		d.mu.Unlock()
		return result
	}
	if run.Preparing || run.Uncertain {
		result.Error = "execution is preparing or unverified for cleanup; inspect manually, never delete state to free capacity"
		d.mu.Unlock()
		return result
	}
	if run.State.Process != identity {
		result.Error = "execution launch changed during inspection; retry gc"
		d.mu.Unlock()
		return result
	}
	refresh := run.generation != generation
	done := d.runIO(run, func(work *activeRun) error {
		state := work.State
		observed := table
		if refresh {
			var err error
			observed, err = procgroup.Snapshot(ctx, []procgroup.ProcessIdentity{state.Process})
			if err != nil {
				result.Error = err.Error()
				return nil
			}
		}
		alive, _, err := procgroup.Inspect(state.processGroup(), observed)
		if err != nil {
			result.Error = err.Error()
		} else if alive {
			result.Action = "retain active execution"
		} else if !work.outputCollected() || (work.output != nil && !work.Exited && !work.Stopping) {
			result.Action = "retain execution awaiting output collection or exit observation"
		} else if !dryRun {
			work.GroupStopped = true
			state.ProcessDone, state.Phase = true, "cleaning"
			if err := d.save(work); err != nil {
				result.Error = err.Error()
			} else if err := d.cleanup(work); err != nil {
				result.Error = d.secrets.Redact(err.Error())
			}
		}
		return nil
	})
	d.mu.Unlock()
	// GC reports completed work, not merely queued operations.
	<-done
	return result
}
