package ghalet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
)

func TestTargetRetryWait(t *testing.T) {
	for failures, seconds := range []int{1, 2, 4, 8, 16, 32, 60, 60, 60} {
		if got := targetRetryWait(failures + 1); got != time.Duration(seconds)*time.Second {
			t.Fatalf("failure %d: got %s, want %ds", failures+1, got, seconds)
		}
	}
	if targetRetryWait(1_000_000) != time.Minute {
		t.Fatal("long outages must not overflow or exceed the waiting cap")
	}
}

func TestPermanentTargetFailuresAreIsolated(t *testing.T) {
	for _, err := range []error{scaleset.UnauthorizedError, scaleset.BadRequestError, errUnsafeTarget} {
		d, _ := testDaemon(t)
		other := *d.targets["build"]
		d.startFailure(d.targets["build"], fmt.Errorf("wrapped: %w", err))
		d.targets["build"].RetryAt = time.Time{}
		if d.targets["build"].Fault == "" || d.canStart(d.targets["build"]) || !d.canStart(&other) {
			t.Fatal("permanent failure was retried or affected another target")
		}
	}
}

type targetRetryTransport func(*http.Request) (*http.Response, error)

func (f targetRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func retryTestClient(t *testing.T, transport targetRetryTransport) *scaleset.Client {
	t.Helper()
	httpClient := retryablehttp.NewClient()
	httpClient.RetryMax = 0
	httpClient.HTTPClient.Transport.(*http.Transport).RegisterProtocol("https", transport)
	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: "https://github.com/owner/repository", PersonalAccessToken: "test",
	}, scaleset.WithRetryableHTTPClint(httpClient), scaleset.WithRetryMax(0))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func retryTestResponse(r *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
}

func retryTestAuthentication(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/registration-token") {
		return retryTestResponse(r, http.StatusCreated, `{"token":"test"}`)
	}
	token := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(
		fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".test"
	return retryTestResponse(r, http.StatusOK, `{"url":"https://actions.invalid/","token":"`+token+`"}`)
}

func TestListenerTransientRecoveryAndCancellation(t *testing.T) {
	for _, setupFailure := range []bool{true, false} {
		t.Run(fmt.Sprintf("setup=%t", setupFailure), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, _ := testDaemon(t)
				target := d.targets["build"]
				var failures, sessions, closed int
				var attempts []time.Time
				target.Client = retryTestClient(t, func(r *http.Request) (*http.Response, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/registration-token"), strings.HasSuffix(r.URL.Path, "/runner-registration"):
						return retryTestAuthentication(r)
					case r.Method == http.MethodDelete:
						closed++
						return retryTestResponse(r, http.StatusNoContent, "")
					case r.Method == http.MethodPost:
						if setupFailure && failures < 10 {
							attempts = append(attempts, time.Now())
							failures++
							return retryTestResponse(r, http.StatusServiceUnavailable, "")
						}
						sessions++
						return retryTestResponse(r, http.StatusOK, `{"sessionId":"11111111-1111-1111-1111-111111111111","messageQueueUrl":"https://queue.invalid/","messageQueueAccessToken":"test","statistics":{}}`)
					default:
						if !setupFailure && failures < 10 {
							attempts = append(attempts, time.Now())
							failures++
							return retryTestResponse(r, http.StatusServiceUnavailable, "")
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
				})
				d.startListener(target)
				synctest.Wait()
				d.mu.Lock()
				diagnosed := targetError(target) != "" && len(d.statusLocked().Problems) > 0
				d.mu.Unlock()
				if !diagnosed {
					t.Fatal("transient listener failure was hidden from apply/status")
				}
				time.Sleep(6 * time.Minute)
				synctest.Wait()
				if failures != 10 || target.Listener == nil || target.Fault != "" || target.RetryError != "" || target.ListenerDone {
					t.Fatal("listener did not recover after a long transient outage", failures, target.Fault)
				}
				for i := 1; i < len(attempts); i++ {
					if got := attempts[i].Sub(attempts[i-1]); got != targetRetryWait(i) {
						t.Fatalf("retry %d waited %s instead of %s", i, got, targetRetryWait(i))
					}
				}
				target.Cancel()
				d.workers.Wait()
				if !target.ListenerDone || target.Fault != "" || closed != sessions {
					t.Fatal("cancellation faulted the target or leaked a message session", closed, sessions, target.Fault)
				}
			})
		})
	}
}

func TestRegistrationBootstrapIsBoundedAndRecoveryCancelable(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(fmt.Sprintf("drain=%t", drain), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, api := testDaemon(t)
				target := &targetRuntime{Config: d.targets["build"].Config}
				calls := 0
				d.clientFactory = func(context.Context, Target) (scaleAPI, error) {
					calls++
					return nil, errTestAPI
				}
				d.registerTarget(t.Context(), target, true)
				if calls != maxAttempts || target.Fault != "" || target.RetryError == "" {
					t.Fatal("bootstrap did not stop at four transient attempts", calls, target.Fault)
				}
				d.clientFactory = func(context.Context, Target) (scaleAPI, error) { return api, nil }
				healthy := &targetRuntime{Config: target.Config}
				d.registerTarget(t.Context(), healthy, true)
				if healthy.Set == nil || healthy.Fault != "" {
					t.Fatal("another target could not register during the outage")
				}
				d.clientFactory = func(context.Context, Target) (scaleAPI, error) {
					calls++
					return nil, errTestAPI
				}
				d.startListener(target)
				synctest.Wait()
				if drain {
					d.beginDrain(target)
				} else {
					target.Cancel()
				}
				d.workers.Wait()
				time.Sleep(2 * time.Minute)
				if calls != maxAttempts || !target.ListenerDone || target.Fault != "" {
					t.Fatal("canceled registration recovery retried or faulted", calls, target.Fault)
				}
			})
		})
	}
}

func TestRegistrationTransientEventuallyRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, api := testDaemon(t)
		target := &targetRuntime{Config: d.targets["build"].Config}
		calls := 0
		d.clientFactory = func(context.Context, Target) (scaleAPI, error) {
			calls++
			if calls <= 6 {
				return nil, errTestAPI
			}
			return api, nil
		}
		d.registerTarget(t.Context(), target, true)
		d.startListener(target)
		d.workers.Wait()
		// The fake API cannot listen, but registration must have recovered and
		// reached the listener instead of permanently faulting after bootstrap.
		if calls != 7 || target.Set == nil || target.RetryError != "" ||
			!strings.Contains(target.Fault, "official scaleset client") {
			t.Fatal("transient registration did not recover", calls, target)
		}
	})
}

func TestListenerSetupPermanentErrorsAndBackoffCancellation(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, _ := testDaemon(t)
				target := d.targets["build"]
				calls := 0
				target.Client = retryTestClient(t, func(r *http.Request) (*http.Response, error) {
					calls++
					return retryTestResponse(r, status, "")
				})
				d.startListener(target)
				synctest.Wait()
				if status == http.StatusServiceUnavailable {
					target.Cancel()
				}
				d.workers.Wait()
				time.Sleep(2 * time.Minute)
				if calls != 1 || !target.ListenerDone || (target.Fault != "") != (status != http.StatusServiceUnavailable) {
					t.Fatal("permanent error retried or cancellation failed", calls, target.Fault)
				}
			})
		})
	}
}

type cancelRegistrationAPI struct {
	*fakeScaleAPI
	cancel context.CancelFunc
}

func (f *cancelRegistrationAPI) GetRunnerScaleSet(ctx context.Context, group int, name string) (*scaleset.RunnerScaleSet, error) {
	set, err := f.fakeScaleAPI.GetRunnerScaleSet(ctx, group, name)
	f.cancel()
	return set, err
}

func TestCanceledRegistrationRetainsSuccessfulIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, api := testDaemon(t)
		target := d.targets["build"]
		target.Client, target.Set = nil, nil
		api.set.Statistics.TotalAssignedJobs = 5
		d.clientFactory = func(context.Context, Target) (scaleAPI, error) {
			return &cancelRegistrationAPI{fakeScaleAPI: api, cancel: func() {
				d.mu.Lock()
				d.beginDrain(target)
				d.mu.Unlock()
			}}, nil
		}
		d.startListener(target)
		d.workers.Wait()
		if target.Set == nil || target.Set.ID != api.set.ID || target.Client == nil ||
			!target.ListenerDone || target.Listener != nil || target.Fault != "" ||
			target.Assigned != 0 || !d.drained(target.Config.ID) {
			t.Fatal("canceled registration lost its identity or started a listener", target)
		}
		d.reconcile()
		if len(d.runs) != 0 || target.Forced {
			t.Fatal("canceled registration retained stale demand or needed forced drain")
		}
	})
}

func TestRetainedFailedPreparationBoundsNewStarts(t *testing.T) {
	d, _ := testDaemon(t)
	target := d.targets["build"]
	other := *target
	state := testState(t, d.config)
	state.ProcessDone, state.Error = true, "prepare failed; cleanup unavailable"
	run := &activeRun{State: state, CleanupTried: true}
	d.runs[state.RunID] = run
	if d.canStart(target) || !d.canStart(&other) || target.Fault != "" {
		t.Fatal("retained preparation permitted accumulation or affected another target")
	}
	delete(d.runs, state.RunID)
	if !d.canStart(target) {
		t.Fatal("completed cleanup did not unblock target recovery")
	}
}

func TestReservationDirectoryFailuresRemainClassified(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(fmt.Sprint(unsafe), func(t *testing.T) {
			d, _ := testDaemon(t)
			target := d.targets["build"]
			if unsafe {
				if err := os.WriteFile(filepath.Join(d.config.Runner.WorkRoot, target.Config.ID), nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(d.config.Runner.WorkRoot, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(d.config.Runner.WorkRoot, 0711) })
			}
			err := d.reserve(target)
			if err == nil {
				t.Fatal("invalid or inaccessible directory was accepted")
			}
			d.startFailure(target, err)
			if (target.Fault != "") != unsafe {
				t.Fatal("operational and unsafe directory failures were confused", err, target.Fault)
			}
		})
	}
}

func TestCancellationDuringTargetSetupHasNoDiagnostic(t *testing.T) {
	for _, registration := range []bool{false, true} {
		t.Run(fmt.Sprint(registration), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, _ := testDaemon(t)
				target := d.targets["build"]
				if registration {
					target.Client, target.Set = nil, nil
					d.clientFactory = func(ctx context.Context, _ Target) (scaleAPI, error) {
						target.Cancel()
						return nil, ctx.Err()
					}
				} else {
					started := make(chan struct{})
					target.Client = retryTestClient(t, func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/registration-token") || strings.HasSuffix(r.URL.Path, "/runner-registration") {
							return retryTestAuthentication(r)
						}
						close(started)
						<-r.Context().Done()
						return nil, r.Context().Err()
					})
					d.startListener(target)
					<-started
					target.Cancel()
				}
				if registration {
					d.startListener(target)
				}
				d.workers.Wait()
				if !target.ListenerDone || target.Fault != "" || target.RetryError != "" ||
					len(d.statusLocked().Problems) != 0 {
					t.Fatal("canceled setup created an unresolved diagnostic", target)
				}
			})
		})
	}
}

func TestPreparationRetryDiagnosticSurvivesUntilStartupRecovers(t *testing.T) {
	d, _ := testDaemon(t)
	target := d.targets["build"]
	d.secrets.Add("preparation-secret")
	for range 10 {
		d.startFailure(target, fmt.Errorf("reserve: preparation-secret"))
	}
	if target.StartError == "" || strings.Contains(target.StartError, "preparation-secret") ||
		target.Fault != "" || len(d.runs) != 0 {
		t.Fatal("recordless preparation failure was hidden or leaked a secret", target)
	}
	response := d.handle(t.Context(), controlRequest{Command: "doctor"})
	if response.Error == "" || len(response.Status.Problems) != 1 {
		t.Fatal("doctor missed the preparation retry diagnostic", response)
	}
	state := testState(t, d.config)
	state.Ready, state.Phase = true, "idle"
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	d.processEvent(runEvent{id: state.RunID, kind: "ready"})
	run.ExitResult = &runExit{}
	d.finishExit(run)
	if target.StartError == "" {
		t.Fatal("an older healthy runner hid the new preparation failure")
	}
	state.Ready = false
	target.RetryError = "listener unavailable"
	d.processEvent(runEvent{id: state.RunID, kind: "ready"})
	if target.StartError != "" || target.RetryError == "" || target.Failures != 0 {
		t.Fatal("startup recovery cleared the wrong diagnostic", target)
	}
	target.RetryError = ""
	if response := d.handle(t.Context(), controlRequest{Command: "doctor"}); response.Error != "" {
		t.Fatal("doctor did not recover after successful startup", response)
	}
	d.startFailure(target, context.Canceled)
	if target.StartError != "" {
		t.Fatal("canceled preparation created a false diagnostic")
	}
}

func TestAssignmentArbitrationClearsOnlyNewStartupRecovery(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			d, api := testDaemon(t)
			target := d.targets["build"]
			d.startFailure(target, errTestAPI)
			state := testState(t, d.config)
			state.RunnerID, state.Ready = 101, ready
			api.runners[101] = &scaleset.RunnerReference{ID: 101, Name: state.RunnerName, RunnerScaleSetID: state.ScaleSetID}
			api.removeErr = scaleset.JobStillRunningError
			run := &activeRun{State: state}
			d.runs[state.RunID] = run
			if err := d.retireIdle(run); !errors.Is(err, scaleset.JobStillRunningError) {
				t.Fatal(err)
			}
			if !state.Ready || !state.Busy || (target.StartError != "") != ready {
				t.Fatal("assignment readiness transition did not preserve the correct diagnostic", target)
			}
			if !ready && target.Failures != 0 {
				t.Fatal("new startup readiness did not reset failure backoff")
			}
		})
	}
}

func TestRecoveredRegistrationRemovesOnlyAdoptedRetiredIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, api := testDaemon(t)
		target := d.targets["build"]
		old := *target
		otherScope := old
		otherScope.Config.URL = "https://github.com/another/repository"
		otherID := old
		set := *old.Set
		set.ID++
		otherID.Set = &set
		unregistered := &targetRuntime{Config: target.Config}
		d.retired = []*targetRuntime{&old, &otherScope, &otherID, unregistered}
		target.Client, target.Set = nil, nil
		d.clientFactory = func(context.Context, Target) (scaleAPI, error) {
			return &cancelRegistrationAPI{fakeScaleAPI: api, cancel: target.Cancel}, nil
		}
		d.startListener(target)
		d.workers.Wait()
		if target.Set == nil || len(d.retired) != 3 || d.retired[0] != &otherScope ||
			d.retired[1] != &otherID || d.retired[2] != unregistered || api.deleteCalls != 0 {
			t.Fatal("recovery retained an adopted identity or removed another retirement", d.retired)
		}
	})
}

func TestReadyRunnerCleanupFailureBoundsAbnormalReplacements(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		busy bool
	}{
		{name: "abnormal idle", err: errTestAPI},
		{name: "abnormal busy", err: errTestAPI, busy: true},
		{name: "successful completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := testDaemon(t)
			target := d.targets["build"]
			other := *target
			state := testState(t, d.config)
			state.Ready, state.Busy, state.ProcessDone = true, tc.busy, true
			run := &activeRun{State: state}
			d.runs[state.RunID] = run
			d.processEvent(runEvent{id: state.RunID, kind: "exit", err: tc.err})
			api.getErr = errTestAPI
			if err := d.cleanup(run); err == nil {
				t.Fatal("simulated cleanup failure was not retained")
			}
			target.RetryAt = time.Time{}
			if d.canStart(target) != (tc.err == nil) || !d.canStart(&other) ||
				d.runs[state.RunID] == nil || target.Fault != "" {
				t.Fatal("abnormal replacements were unbounded or successful completions were blocked")
			}
		})
	}
}
