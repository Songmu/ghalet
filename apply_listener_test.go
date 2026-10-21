package ghalet

import (
	"context"
	"strings"
	"testing"
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
		target.Cancel = func() { target.ListenerDone = true }
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
	target.ListenerDone = false
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
	d.mu.Lock()
	target.ListenerDone = true
	d.mu.Unlock()
	select {
	case response := <-applied:
		if response.Error != "" || api.deleteCalls != 1 {
			t.Fatal(response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not complete after listener closure")
	}
}
