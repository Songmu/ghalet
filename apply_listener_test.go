package ghalet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	settings "github.com/Songmu/ghalet/config"
)

func TestApplyWaitsForListenerStartup(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	writeApplyConfig(t, nextConfig(d))
	starting := make(chan *targetRuntime, 1)
	d.listen = func(target *targetRuntime) {
		target.ListenerReady = make(chan struct{})
		target.ListenerDone = make(chan struct{})
		target.Cancel = sync.OnceFunc(func() { close(target.ListenerDone) })
		starting <- target
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	applied := make(chan controlResponse, 1)
	go func() { applied <- d.apply(ctx) }()
	var target *targetRuntime
	select {
	case target = <-starting:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not start the new listener")
	}
	select {
	case response := <-applied:
		t.Fatal("apply returned before listener startup completed", response)
	default:
	}
	if status := d.handle(t.Context(), controlRequest{Command: "status"}); status.Status == nil {
		t.Fatal("status unavailable during listener startup")
	}
	close(target.ListenerReady)
	select {
	case response := <-applied:
		if response.Error != "" {
			t.Fatal(response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not observe listener readiness")
	}
}

func listenerCompleted(target *targetRuntime) bool {
	select {
	case <-target.ListenerDone:
		return true
	default:
		return false
	}
}

func TestWaitListeners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := testDaemon(t)
		old := d.targets["build"]
		old.ListenerDone = make(chan struct{})
		// Applying a new instance with the same ID must not replace the wait.
		replacement := &targetRuntime{Config: old.Config, ListenerDone: make(chan struct{})}
		close(replacement.ListenerDone)
		d.targets[old.Config.ID] = replacement
		done := make(chan error, 1)
		go func() {
			done <- d.waitListeners(t.Context(), []*targetRuntime{
				{Cancel: func() {}}, old, replacement,
			})
		}()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatal("wait ignored the old listener", err)
		default:
		}
		old.CloseError = "final cleanup diagnostic"
		close(old.ListenerDone)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil || old.CloseError != "final cleanup diagnostic" {
				t.Fatal("wait lost completion diagnostics", err)
			}
		default:
			t.Fatal("listener completion did not wake the waiter immediately")
		}
	})
}

func TestWaitListenersCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := testDaemon(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- d.waitListeners(ctx, []*targetRuntime{{ListenerDone: make(chan struct{})}})
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("listener wait did not honor cancellation", err)
		}
	})
}

func TestApplyReportsListenerStartupFailure(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	d.listen = d.startListener
	writeApplyConfig(t, nextConfig(d))
	response := d.apply(t.Context())
	if response.Error == "" || len(response.Status.Problems) == 0 ||
		!strings.Contains(response.Status.Targets[0].Fault, "official scaleset client") {
		t.Fatal("listener startup failure was reported as successful apply", response)
	}
	if len(response.Results) != 2 || response.Results[1].Action != "start listener" ||
		response.Results[1].Error == "" {
		t.Fatal("listener startup failure lost per-target diagnostics", response)
	}
}

func TestApplyWaitsForOldListenerClosure(t *testing.T) {
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	target := d.targets["build"]
	target.ListenerDone = make(chan struct{})
	closing := make(chan struct{})
	target.Cancel = func() { close(closing) }
	next := nextConfig(d)
	next.Targets = []settings.Target{}
	writeApplyConfig(t, next)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	applied := make(chan controlResponse, 1)
	go func() { applied <- d.apply(ctx) }()
	select {
	case <-closing:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not close the old listener")
	}
	select {
	case response := <-applied:
		t.Fatal("apply deleted the scale set before its listener closed", response)
	default:
	}
	close(target.ListenerDone)
	select {
	case response := <-applied:
		if response.Error != "" || api.deleteCalls != 1 {
			t.Fatal(response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not complete after listener closure")
	}
}
