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
		config, err = configPath(config)
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
		owner := strings.TrimPrefix(filepath.Base(directory), "ghalet-")
		uid, err := strconv.Atoi(owner)
		if err != nil || uid < 0 || strconv.Itoa(uid) != owner {
			continue
		}
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect control directory: %w", err)
		}
		if !info.IsDir() || !ownedPrivate(info, uid) {
			continue
		}
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read control directory: %w", err)
		}
		for _, entry := range entries {
			hash, ok := strings.CutSuffix(entry.Name(), ".sock")
			if !ok || len(hash) != 24 || (config != "" && entry.Name() != socketName(config)) {
				continue
			}
			if _, err := hex.DecodeString(hash); err != nil {
				continue
			}
			socket := filepath.Join(directory, entry.Name())
			info, err := os.Lstat(socket)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("inspect control socket: %w", err)
			}
			if info.Mode()&os.ModeSocket == 0 || !ownedPrivate(info, uid) {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			response, err := controlCallSocket(probeCtx, socket, controlRequest{Command: "status"})
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
				continue
			}
			path := response.Status.Config
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || socketName(path) != entry.Name() ||
				(config != "" && config != path) {
				continue
			}
			daemons = append(daemons, DaemonInfo{UID: uid, Socket: socket, Status: *response.Status})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return daemons, nil
}

func ownedPrivate(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && info.Mode().Perm()&0077 == 0
}

type controlEndpoint struct {
	config     string
	socket     string
	discovered bool
}

func (e controlEndpoint) call(ctx context.Context, request controlRequest) (controlResponse, error) {
	if !e.discovered {
		return controlResponse{}, fmt.Errorf("no accessible daemon found for config %s", e.config)
	}
	return controlCallSocket(ctx, e.socket, request)
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
		return controlEndpoint{config: d.Status.Config, socket: d.Socket, discovered: true}, nil
	default:
		return controlEndpoint{}, errors.New("multiple accessible daemons found; use ghalet list and specify --config (or run as the daemon's management user)")
	}
}
