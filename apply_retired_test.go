package ghalet

import (
	"context"
	"strings"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/actions/scaleset"
)

func TestApplyPreservesRegistrationAfterFailedReregistration(t *testing.T) {
	for _, action := range []string{"remove", "recover"} {
		t.Run(action, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			old := d.targets["build"]
			next := nextConfig(d)
			writeApplyConfig(t, next)
			d.clientFactory = func(context.Context, settings.Target) (scaleAPI, error) {
				return nil, scaleset.UnauthorizedError
			}
			for range 2 {
				response := d.apply(t.Context())
				if response.Error == "" || api.deleteCalls != 0 ||
					len(d.retired) != 1 || d.retired[0] != old {
					t.Fatal("failed re-registration lost or deleted the known registration", response)
				}
			}
			wantDeletes := 0
			if action == "remove" {
				next.Targets = []settings.Target{}
				wantDeletes = 1
			} else {
				d.clientFactory = func(context.Context, settings.Target) (scaleAPI, error) { return api, nil }
			}
			writeApplyConfig(t, next)
			response := d.apply(t.Context())
			if response.Error != "" || api.deleteCalls != wantDeletes ||
				len(d.retired) != 0 || len(response.Status.Problems) != 0 {
				t.Fatal("apply did not reconcile the preserved registration", response)
			}
		})
	}
}

type blockedRetiredAPI struct {
	scaleAPI
	lookup  bool
	started chan struct{}
}

func (b *blockedRetiredAPI) GetRunnerScaleSet(ctx context.Context, group int, name string) (*scaleset.RunnerScaleSet, error) {
	if !b.lookup {
		return b.scaleAPI.GetRunnerScaleSet(ctx, group, name)
	}
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockedRetiredAPI) DeleteRunnerScaleSet(ctx context.Context, _ int) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestShutdownInterruptsRetiredScaleSetNetworkCalls(t *testing.T) {
	for _, operation := range []string{"lookup", "delete"} {
		t.Run(operation, func(t *testing.T) {
			d, api := testDaemon(t)
			useFakeManagement(d, api)
			blocked := &blockedRetiredAPI{scaleAPI: api, lookup: operation == "lookup", started: make(chan struct{})}
			old := d.targets["build"]
			old.Client = blocked
			next := nextConfig(d)
			next.Targets[0].ScaleSetName = "replacement"
			writeApplyConfig(t, next)
			replacement := newFakeAPI()
			replacement.set.ID = 20
			d.clientFactory = func(context.Context, settings.Target) (scaleAPI, error) { return replacement, nil }
			listening := make(chan struct{}, 1)
			listen := d.listen
			d.listen = func(target *targetRuntime) {
				listening <- struct{}{}
				listen(target)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			applied := make(chan controlResponse, 1)
			go func() { applied <- d.apply(ctx) }()
			select {
			case <-blocked.started:
			case <-time.After(5 * time.Second):
				t.Fatal("apply did not start retired registration cleanup")
			}
			for _, command := range []string{"status", "doctor"} {
				diagnosed := make(chan controlResponse, 1)
				go func() { diagnosed <- d.handle(t.Context(), controlRequest{Command: command}) }()
				select {
				case response := <-diagnosed:
					if response.Status == nil || len(response.Status.Problems) == 0 {
						t.Fatal("pending cleanup diagnostics were lost", response)
					}
				case <-time.After(5 * time.Second):
					t.Fatal(command + " blocked behind a remote request")
				}
			}
			shutdown, cancelShutdown := context.WithCancel(t.Context())
			cancelShutdown()
			stopped := make(chan error, 1)
			go func() { stopped <- d.serve(shutdown) }()
			select {
			case response := <-applied:
				if response.Error == "" {
					t.Fatal("interrupted cleanup returned successful apply", response)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not cancel retired registration cleanup")
			}
			select {
			case err := <-stopped:
				if err == nil || !strings.Contains(err.Error(), "unresolved diagnostics") {
					t.Fatal("shutdown lost pending cleanup diagnostics", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("daemon did not finish shutdown")
			}
			if !d.stopping || d.applying || len(d.retired) != 1 || d.retired[0] != old {
				t.Fatal("shutdown lost the retired registration")
			}
			select {
			case <-listening:
				t.Fatal("apply started a listener during shutdown")
			default:
			}
		})
	}
}
