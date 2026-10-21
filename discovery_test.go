package ghalet

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
)

func discoveryRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ghalet-discovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func discoverySocket(t *testing.T, root, config string, reply any) string {
	t.Helper()
	dir := filepath.Join(root, "ghalet-"+strconv.Itoa(os.Geteuid()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, socketName(config))
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		listener.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if reply == nil {
				<-stop
			} else {
				var request controlRequest
				if json.NewDecoder(conn).Decode(&request) == nil {
					json.NewEncoder(conn).Encode(reply)
				}
			}
			conn.Close()
		}
	}()
	return path
}

func TestDiscoverDaemonsIgnoresGarbageWithoutRemovingIt(t *testing.T) {
	root := discoveryRoot(t)
	config := "/nonexistent/ghalet/config.yaml"
	socket := discoverySocket(t, root, config, &controlResponse{Status: &Status{Config: config, Capacity: 3}})
	dir := filepath.Dir(socket)
	regular := filepath.Join(dir, socketName("/regular"))
	if err := os.WriteFile(regular, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, socketName("/link"))
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, socketName("/stale"))
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	if err := os.Chmod(stale, 0600); err != nil {
		t.Fatal(err)
	}
	discoverySocket(t, root, "/malformed", "not a control response")
	discoverySocket(t, root, "/wrong-path", &controlResponse{Status: &Status{Config: config}})
	discoverySocket(t, root, "/relative", &controlResponse{Status: &Status{Config: "relative.yaml"}})
	discoverySocket(t, root, "/error", &controlResponse{Error: "not ready"})
	discoverySocket(t, root, "/empty", &controlResponse{})
	insecure := discoverySocket(t, root, "/insecure", &controlResponse{Status: &Status{Config: "/insecure"}})
	if err := os.Chmod(insecure, 0666); err != nil {
		t.Fatal(err)
	}
	daemons, err := discoverDaemons(t.Context(), root, "")
	if err != nil || len(daemons) != 1 || daemons[0].Socket != socket || daemons[0].UID != os.Geteuid() ||
		daemons[0].Status.Config != config || daemons[0].Status.Capacity != 3 {
		t.Fatalf("unexpected discovery result: %+v (%v)", daemons, err)
	}
	for _, path := range []string{regular, link, stale, insecure} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("discovery modified a filesystem entry", path, err)
		}
	}
}

func TestDiscoverDaemonsFiltersConfigAndSelectsUnambiguousDaemon(t *testing.T) {
	root := discoveryRoot(t)
	first, second := "/first/config.yaml", "/second/config.yaml"
	firstSocket := discoverySocket(t, root, first, &controlResponse{Status: &Status{Config: first}})
	discoverySocket(t, root, second, &controlResponse{Status: &Status{Config: second}})
	daemons, err := discoverDaemons(t.Context(), root, "")
	if err != nil || len(daemons) != 2 {
		t.Fatal(daemons, err)
	}
	if _, err := selectControl("", daemons); err == nil || !strings.Contains(err.Error(), "multiple accessible daemons") {
		t.Fatal("ambiguous discovery selected a daemon", err)
	}
	daemons, err = discoverDaemons(t.Context(), root, "/first/./config.yaml")
	if err != nil || len(daemons) != 1 {
		t.Fatal("explicit config was not normalized and filtered", daemons, err)
	}
	endpoint, err := selectControl("", daemons)
	if err != nil || endpoint.config != first || endpoint.socket != firstSocket || !endpoint.discovered {
		t.Fatal("single daemon was not selected without reading its config", endpoint, err)
	}
	daemons, err = discoverDaemons(t.Context(), root, "/other/config.yaml")
	if err != nil || len(daemons) != 0 {
		t.Fatal("explicit config selected a different daemon", daemons, err)
	}
}

func TestDiscoverDaemonsBoundsUnresponsiveProbesAndHonorsCancellation(t *testing.T) {
	root := discoveryRoot(t)
	discoverySocket(t, root, "/unresponsive", nil)
	start := time.Now()
	daemons, err := discoverDaemons(t.Context(), root, "")
	if err != nil || len(daemons) != 0 || time.Since(start) > 5*time.Second {
		t.Fatal("unresponsive socket did not time out", daemons, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := discoverDaemons(ctx, root, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("discovery ignored cancellation", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := discoverDaemons(ctx, root, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("probe ignored caller deadline", err)
	}
}

func TestDiscoverDaemonsRejectsUnsafeDirectories(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			root := discoveryRoot(t)
			config := "/config.yaml"
			socket := discoverySocket(t, root, config, &controlResponse{Status: &Status{Config: config}})
			dir := filepath.Dir(socket)
			switch kind {
			case "public":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				renamed := dir + "-original"
				if err := os.Rename(dir, renamed); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(renamed, dir); err != nil {
					t.Fatal(err)
				}
			case "wrong-owner":
				if err := os.Rename(dir, filepath.Join(root, "ghalet-"+strconv.Itoa(os.Geteuid()+1))); err != nil {
					t.Fatal(err)
				}
			}
			daemons, err := discoverDaemons(t.Context(), root, "")
			if err != nil || len(daemons) != 0 {
				t.Fatal("unsafe control directory was accepted", daemons, err)
			}
		})
	}
}

func TestSelectControlPreservesOfflineConfigSelection(t *testing.T) {
	path := writeConfig(t, "targets: []\n")
	endpoint, err := selectControl(path, nil)
	if err != nil || endpoint.config != path || endpoint.socket != socketPath(path) || endpoint.discovered {
		t.Fatal("offline explicit configuration changed", endpoint, err)
	}
	if _, err := endpoint.call(t.Context(), controlRequest{Command: "apply"}); err == nil {
		t.Fatal("operation connected to an undiscovered socket")
	}
	home := testDirectory(t)
	t.Setenv("HOME", home)
	path = filepath.Join(home, ".config", "ghalet", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("targets: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint, err = selectControl("", nil)
	if err != nil || endpoint.config != path || endpoint.discovered {
		t.Fatal("offline default configuration changed", endpoint, err)
	}
}

func TestCanceledManagementDoesNotInitializeOfflineDaemon(t *testing.T) {
	config := testConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := GC(ctx, config.Path, false); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled garbage collection entered offline maintenance", err)
	}
	if _, err := os.Stat(config.Runner.WorkRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled garbage collection created its work root", err)
	}
}

func TestManagementDiscoversDaemonAndApplyKeepsItsConfig(t *testing.T) {
	existing, err := ListDaemons(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(existing) != 0 {
		t.Skip("automatic selection requires an isolated management user")
	}
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	next := nextConfig(d)
	next.Targets = []settings.Target{}
	writeApplyConfig(t, next)
	original, err := os.ReadFile(next.Path)
	if err != nil {
		t.Fatal(err)
	}
	home := testDirectory(t)
	t.Setenv("HOME", home)
	defaultPath := filepath.Join(home, ".config", "ghalet", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0700); err != nil {
		t.Fatal(err)
	}
	const unrelated = "invalid unrelated default config\n"
	if err := os.WriteFile(defaultPath, []byte(unrelated), 0600); err != nil {
		t.Fatal(err)
	}
	serveTestControl(t, d)
	for _, command := range []string{"status", "doctor", "gc", "apply"} {
		switch command {
		case "status":
			status, err := GetStatus(t.Context(), "")
			if err != nil || status.Config != next.Path {
				t.Fatal("status did not discover the daemon", status, err)
			}
		case "doctor":
			status, err := Doctor(t.Context(), "")
			if err != nil || status.Config != next.Path {
				t.Fatal("doctor did not discover the daemon", status, err)
			}
		case "gc":
			if _, err := GC(t.Context(), "", true); err != nil {
				t.Fatal("gc did not discover the daemon", err)
			}
		case "apply":
			result, err := Apply(t.Context(), "")
			if err != nil || result.Status == nil || result.Status.Config != next.Path || len(result.Status.Targets) != 0 {
				t.Fatal("apply did not reload the daemon's own configuration", result, err)
			}
		}
	}
	data, err := os.ReadFile(next.Path)
	if err != nil || string(data) != string(original) {
		t.Fatal("apply modified the daemon's configuration file", err)
	}
	data, err = os.ReadFile(defaultPath)
	if err != nil || string(data) != unrelated {
		t.Fatal("discovery modified the unrelated default configuration", err)
	}
	second, secondAPI := testDaemon(t)
	useFakeManagement(second, secondAPI)
	secondServer := serveTestControl(t, second)
	for _, command := range []string{"status", "doctor", "gc", "apply"} {
		var err error
		switch command {
		case "status":
			_, err = GetStatus(t.Context(), "")
		case "doctor":
			_, err = Doctor(t.Context(), "")
		case "gc":
			_, err = GC(t.Context(), "", false)
		case "apply":
			_, err = Apply(t.Context(), "")
		}
		if err == nil || !strings.Contains(err.Error(), "multiple accessible daemons") {
			t.Fatal("management command accepted ambiguous discovery", command, err)
		}
	}
	status, err := GetStatus(t.Context(), next.Path)
	if err != nil || status.Config != next.Path {
		t.Fatal("explicit config did not disambiguate discovery", status, err)
	}
	if _, err := GetStatus(t.Context(), defaultPath); err == nil {
		t.Fatal("explicit config fell back to an unrelated daemon")
	}
	if second.targets["build"].Draining || secondAPI.deleteCalls != 0 {
		t.Fatal("ambiguous discovery modified a daemon")
	}
	secondServer.close()
	if err := os.Remove(next.Path); err != nil {
		t.Fatal(err)
	}
	status, err = GetStatus(t.Context(), "")
	if err != nil || status.Config != next.Path {
		t.Fatal("discovery requires a configuration file for status", status, err)
	}
	if _, err := Apply(t.Context(), ""); err == nil {
		t.Fatal("apply accepted a missing daemon configuration")
	}
}
