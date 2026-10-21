package ghalet

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNotifyReady(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		name := "filesystem"
		if abstract {
			name = "abstract"
		}
		t.Run(name, func(t *testing.T) {
			if abstract && runtime.GOOS != "linux" {
				t.Skip("abstract Unix sockets require Linux")
			}
			socket := filepath.Join(discoveryRoot(t), "notify.sock")
			if abstract {
				socket = "@" + socket
			}
			listener, err := net.ListenPacket("unixgram", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("NOTIFY_SOCKET", socket)
			if err := notifyReady(); err != nil {
				t.Fatal(err)
			}
			if err := listener.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var data [64]byte
			n, _, err := listener.ReadFrom(data[:])
			if err != nil || string(data[:n]) != "READY=1" {
				t.Fatal("missing readiness notification", string(data[:n]), err)
			}
		})
	}
}

func TestNotifyReadyWithoutSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifyReady(); err != nil {
		t.Fatal("direct startup requires systemd", err)
	}
}

func TestRunDaemonNotifyReady(t *testing.T) {
	c := testConfig(t)
	c.Targets = nil
	if os.Geteuid() == 0 {
		c.Runner.User = "nobody"
	}
	t.Cleanup(func() { os.Remove(socketPath(c.Path) + ".lock") })
	socket := filepath.Join(discoveryRoot(t), "notify.sock")
	listener, err := net.ListenPacket("unixgram", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", socket)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunDaemon(ctx, c) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("daemon shutdown failed", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	if err := listener.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [64]byte
	n, _, err := listener.ReadFrom(data[:])
	if err != nil || string(data[:n]) != "READY=1" {
		t.Fatal("daemon did not notify readiness", string(data[:n]), err)
	}
	status, err := Doctor(ctx, c.Path)
	if err != nil || status == nil || status.Config != c.Path {
		t.Fatal("management socket was not ready after notification", status, err)
	}
}

func TestRunDaemonStartupFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "notification failure"
		if canceled {
			name = "canceled initialization"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			c.Targets = nil
			if os.Geteuid() == 0 {
				c.Runner.User = "nobody"
			}
			t.Cleanup(func() { os.Remove(socketPath(c.Path) + ".lock") })
			t.Setenv("NOTIFY_SOCKET", filepath.Join(discoveryRoot(t), "missing.sock"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			err := RunDaemon(ctx, c)
			if err == nil || (!canceled && !strings.Contains(err.Error(), "notify service readiness")) {
				t.Fatal("startup failure was not reported", err)
			}
			if canceled && !errors.Is(err, context.Canceled) {
				t.Fatal("canceled initialization attempted to notify readiness", err)
			}
			if _, err := os.Lstat(socketPath(c.Path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed startup retained the management socket", err)
			}
			lock, err := lockWorkRoot(c.Runner.WorkRoot)
			if err != nil {
				t.Fatal("failed startup retained the workRoot lock", err)
			}
			lock.Close()
		})
	}
}
