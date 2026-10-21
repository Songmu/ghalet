//go:build linux || darwin

package ghalet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	settings "github.com/Songmu/ghalet/config"
)

// DaemonInfo identifies an accessible daemon and its running snapshot.
type DaemonInfo struct {
	UID    int    `json:"uid"`
	Socket string `json:"socket"`
	Status Status `json:"status"`
}

// ListDaemons finds responsive daemons accessible to the management user.
// A nonempty config path limits discovery to that configuration.
func ListDaemons(ctx context.Context, config string) ([]DaemonInfo, error) {
	return discoverDaemons(ctx, "/tmp", config)
}

func discoverDaemons(ctx context.Context, root, config string) ([]DaemonInfo, error) {
	if config != "" {
		var err error
		config, err = settings.ResolvePath(config)
		if err != nil {
			return nil, err
		}
	}
	directories, err := filepath.Glob(filepath.Join(root, "ghalet-*"))
	if err != nil {
		return nil, err
	}
	daemons := []DaemonInfo{}
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := discoverDirectory(ctx, directory, config)
		if err != nil {
			return nil, err
		}
		daemons = append(daemons, found...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return daemons, nil
}

func discoverDirectory(ctx context.Context, directory, config string) ([]DaemonInfo, error) {
	owner := strings.TrimPrefix(filepath.Base(directory), "ghalet-")
	uid, err := strconv.Atoi(owner)
	if err != nil || uid < 0 || strconv.Itoa(uid) != owner {
		return nil, nil
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect control directory: %w", err)
	}
	if !info.IsDir() || !ownedPrivate(info, uid) {
		return nil, nil
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read control directory: %w", err)
	}
	var daemons []DaemonInfo
	for _, entry := range entries {
		daemon, err := probeDaemon(ctx, directory, entry.Name(), uid, config)
		if err != nil {
			return nil, err
		}
		if daemon != nil {
			daemons = append(daemons, *daemon)
		}
	}
	return daemons, nil
}

func probeDaemon(ctx context.Context, directory, name string, uid int, config string) (*DaemonInfo, error) {
	hash, ok := strings.CutSuffix(name, ".sock")
	if !ok || len(hash) != 24 || (config != "" && name != socketName(config)) {
		return nil, nil
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return nil, nil
	}
	socket := filepath.Join(directory, name)
	info, err := os.Lstat(socket)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || !ownedPrivate(info, uid) {
		return nil, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	response, err := controlCallSocket(probeCtx, socket, uid, controlRequest{Command: "status"})
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// The socket deadline can fire before the context's timer runs.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return nil, context.DeadlineExceeded
	}
	// A filesystem entry alone is not a running daemon. Ignore stale,
	// unresponsive and unrelated sockets without removing anything.
	if err != nil || response.Error != "" || response.Status == nil {
		return nil, nil
	}
	path := response.Status.Config
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || socketName(path) != name ||
		(config != "" && config != path) {
		return nil, nil
	}
	return &DaemonInfo{UID: uid, Socket: socket, Status: *response.Status}, nil
}

func ownedPrivate(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && info.Mode().Perm()&0077 == 0
}

type controlEndpoint struct {
	config     string
	socket     string
	uid        int
	discovered bool
}

func (e controlEndpoint) call(ctx context.Context, request controlRequest) (controlResponse, error) {
	if !e.discovered {
		return controlResponse{}, fmt.Errorf("no accessible daemon found for config %s", e.config)
	}
	request.ExpectedConfig = e.config
	return controlCallSocket(ctx, e.socket, e.uid, request)
}

func resolveControl(ctx context.Context, config string) (controlEndpoint, error) {
	daemons, err := ListDaemons(ctx, config)
	if err != nil {
		return controlEndpoint{}, err
	}
	return selectControl(config, daemons)
}

func selectControl(config string, daemons []DaemonInfo) (controlEndpoint, error) {
	switch len(daemons) {
	case 0:
		path, err := selectedConfigPath(config)
		if err != nil {
			return controlEndpoint{}, err
		}
		return controlEndpoint{config: path, socket: socketPath(path)}, nil
	case 1:
		d := daemons[0]
		return controlEndpoint{config: d.Status.Config, socket: d.Socket, uid: d.UID, discovered: true}, nil
	default:
		return controlEndpoint{}, errors.New("multiple accessible daemons found; use ghalet list and specify --config (or run as the daemon's management user)")
	}
}
