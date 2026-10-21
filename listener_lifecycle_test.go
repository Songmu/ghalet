//go:build linux || darwin

package ghalet

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestListenerCompletionWaitsForCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := testDaemon(t)
		target := d.targets["build"]
		d.secrets.Add("cleanup-secret")
		cleanupStarted := make(chan struct{})
		finishCleanup := make(chan struct{})
		target.Client = retryTestClient(t, func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/registration-token"), strings.HasSuffix(r.URL.Path, "/runner-registration"):
				return retryTestAuthentication(r)
			case r.Method == http.MethodDelete:
				close(cleanupStarted)
				<-finishCleanup
				return retryTestResponse(r, http.StatusServiceUnavailable, `{"message":"cleanup-secret"}`)
			case r.Method == http.MethodPost:
				return retryTestResponse(r, http.StatusOK, `{"sessionId":"11111111-1111-1111-1111-111111111111","messageQueueUrl":"https://queue.invalid/","messageQueueAccessToken":"test","statistics":{}}`)
			default:
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		})
		d.startListener(target)
		<-target.ListenerReady
		if listenerCompleted(target) {
			t.Fatal("readiness signaled listener completion")
		}
		target.Cancel()
		<-cleanupStarted
		if listenerCompleted(target) {
			t.Fatal("listener completed before session cleanup")
		}
		close(finishCleanup)
		<-target.ListenerDone
		if target.CloseError == "" || strings.Contains(target.CloseError, "cleanup-secret") || target.Fault != "" {
			t.Fatal("completion preceded final cleanup diagnostics", target)
		}
		d.workers.Wait()
	})
}

func TestListenerCompletionPublishesFinalFault(t *testing.T) {
	d, _ := testDaemon(t)
	target := d.targets["build"]
	d.startListener(target)
	<-target.ListenerDone
	if !strings.Contains(target.Fault, "official scaleset client") {
		t.Fatal("completion preceded final listener fault", target)
	}
	d.workers.Wait()
}

func TestListenerRetainsCloseErrorAfterSessionRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := testDaemon(t)
		target := d.targets["build"]
		d.secrets.Add("close-secret")
		var sessions, closed int
		target.Client = retryTestClient(t, func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/registration-token"), strings.HasSuffix(r.URL.Path, "/runner-registration"):
				return retryTestAuthentication(r)
			case r.Method == http.MethodDelete:
				closed++
				if closed == 1 {
					return retryTestResponse(r, http.StatusServiceUnavailable, `{"message":"close-secret"}`)
				}
				return retryTestResponse(r, http.StatusNoContent, "")
			case r.Method == http.MethodPost:
				sessions++
				return retryTestResponse(r, http.StatusOK, `{"sessionId":"11111111-1111-1111-1111-111111111111","messageQueueUrl":"https://queue.invalid/","messageQueueAccessToken":"test","statistics":{}}`)
			default:
				if sessions == 1 {
					return retryTestResponse(r, http.StatusServiceUnavailable, "")
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		})
		d.startListener(target)
		synctest.Wait()
		if target.CloseError == "" || strings.Contains(target.CloseError, "close-secret") {
			t.Fatal("session cleanup failure was hidden or exposed a secret", target.CloseError)
		}
		closeError := target.CloseError
		select {
		case <-target.ListenerReady:
		default:
			t.Fatal("listener startup was not signaled")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if sessions != 2 || target.CloseError != closeError || target.RetryError != "" || target.Fault != "" {
			t.Fatal("session recovery cleared cleanup diagnostics or retained retry diagnostics", target)
		}
		target.Cancel()
		d.workers.Wait()
		if closed != sessions || !listenerCompleted(target) || target.CloseError != closeError || target.Fault != "" {
			t.Fatal("successful cancellation cleanup cleared an earlier cleanup failure", target)
		}
	})
}
