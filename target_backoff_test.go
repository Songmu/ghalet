package ghalet

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
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
				time.Sleep(6 * time.Minute)
				synctest.Wait()
				if failures != 10 || target.Listener == nil || target.Fault != "" || target.ListenerDone {
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
				if calls != maxAttempts || target.Fault != "" || target.RegistrationError == "" {
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
		if calls != 7 || target.Set == nil || target.RegistrationError != "" ||
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
