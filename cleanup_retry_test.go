package ghalet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestCleanupRetriesArePacedAndCapped(t *testing.T) {
	for _, stage := range []string{"lookup", "removal"} {
		t.Run(stage, func(t *testing.T) {
			d, api := testDaemon(t)
			state := testState(t, d.config)
			state.ProcessDone, state.Phase, state.RunnerID = true, "cleaning", 1
			api.runners[1] = &scaleset.RunnerReference{ID: 1, Name: state.RunnerName, RunnerScaleSetID: state.ScaleSetID}
			if err := d.store.save(state); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(state.RunnerDir, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(state.RunnerDir, "output.log")
			if err := os.WriteFile(source, []byte("retained output"), 0600); err != nil {
				t.Fatal(err)
			}
			run := &activeRun{State: state}
			d.runs[state.RunID] = run
			if stage == "lookup" {
				api.getErr = context.DeadlineExceeded
			} else {
				api.removeErr = syscall.ECONNRESET
			}
			for attempt := 1; attempt <= 9; attempt++ {
				before := time.Now()
				reconcileAndWait(d)
				delay := min(5*time.Second<<uint(min(attempt, 7)-1), 5*time.Minute)
				if run.CleanupFailures != min(attempt, 7) ||
					run.CleanupRetryAt.Before(before.Add(delay)) || run.CleanupRetryAt.After(time.Now().Add(delay)) {
					t.Fatal("cleanup retry did not use capped exponential waiting", attempt, run.CleanupRetryAt)
				}
				retryAt := run.CleanupRetryAt
				calls := api.removeCalls
				for range 3 {
					reconcileAndWait(d)
				}
				if run.CleanupRetryAt != retryAt || api.removeCalls != calls ||
					d.occupied() != 0 || !d.drained("") || state.Error == "" || run.Uncertain {
					t.Fatal("cleanup retry spun, blocked capacity/drain or lost diagnostics")
				}
				if data, err := os.ReadFile(source); err != nil || string(data) != "retained output" {
					t.Fatal("failed cleanup removed original logs", err)
				}
				run.CleanupRetryAt = time.Now().Add(-time.Second)
			}
			api.getErr, api.removeErr = nil, nil
			reconcileAndWait(d)
			if d.runs[state.RunID] != nil {
				t.Fatal("ordinary reconcile did not finish recovered cleanup")
			}
			data, err := os.ReadFile(filepath.Join(d.config.Logs.Root, "runs", state.RunID, "output.log"))
			if err != nil || string(data) != "retained output" {
				t.Fatal("recovered cleanup did not archive original logs", err)
			}
			if _, err := os.Stat(filepath.Dir(state.RunnerDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovered cleanup retained execution files", err)
			}
			if d.targets[buildTargetID].Failures != 0 || !d.targets[buildTargetID].RetryAt.IsZero() {
				t.Fatal("run cleanup affected target retry policy")
			}
		})
	}
}

func TestCleanupPermanentFailureRequiresManualRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"authentication", scaleset.UnauthorizedError},
		{"invalid request", scaleset.BadRequestError},
		{"permission", os.ErrPermission},
		{"unknown error", errTestAPI},
		{"registration mismatch", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := testDaemon(t)
			state := testState(t, d.config)
			state.ProcessDone, state.Phase, state.RunnerID = true, "cleaning", 1
			api.runners[1] = &scaleset.RunnerReference{ID: 1, Name: state.RunnerName, RunnerScaleSetID: state.ScaleSetID}
			if err := d.store.save(state); err != nil {
				t.Fatal(err)
			}
			run := &activeRun{State: state}
			d.runs[state.RunID] = run
			api.getErr = context.DeadlineExceeded
			d.reconcileRun(run, nil)
			if run.CleanupRetryAt.IsZero() {
				t.Fatal("transient failure did not schedule a retry")
			}
			run.CleanupRetryAt = time.Now().Add(-time.Second)
			api.getErr = tc.err
			if tc.err == nil {
				api.runners[1].Name = "different-registration"
			}
			d.reconcileRun(run, nil)
			if !run.CleanupTried || !run.CleanupRetryAt.IsZero() || api.removeCalls != 0 || d.occupied() != 0 || !d.drained("") {
				t.Fatal("permanent cleanup failure retried or held capacity/drain")
			}
			api.getErr = nil
			api.runners[1].Name = state.RunnerName
			d.reconcileRun(run, nil)
			if d.runs[state.RunID] == nil {
				t.Fatal("permanent cleanup failure retried automatically")
			}
			// Manual gc is allowed even while a transient retry is waiting.
			run.CleanupRetryAt = time.Now().Add(time.Hour)
			if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] != nil {
				t.Fatal("manual gc did not retry cleanup immediately", response)
			}
		})
	}
}

func TestCleanupRetryableErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{fmt.Errorf("connection: %w", syscall.ECONNRESET), true},
		{io.ErrUnexpectedEOF, true},
		{&os.PathError{Op: "write", Path: "logs", Err: syscall.ENOSPC}, true},
		{&os.PathError{Op: "open", Path: "logs", Err: syscall.EACCES}, false},
		{context.Canceled, false},
		{errors.New("runner registration does not match the recovery record"), false},
		{errors.New(`invalid state: request GET https://example.com failed(status="503 Service Unavailable")`), false},
	} {
		if got := cleanupRetryable(tc.err); got != tc.want {
			t.Errorf("cleanupRetryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	// Exercise the pinned SDK, including retryablehttp's status-dropping 429 path.
	for _, status := range []int{400, 401, 403, 404, 408, 409, 429, 500, 501, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
				GitHubConfigURL: server.URL + "/owner/repository", PersonalAccessToken: "test-token",
			}, scaleset.WithRetryMax(0))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetRunner(t.Context(), 1)
			want := status == 408 || status == 429 || (status >= 500 && status != 501)
			if err == nil || cleanupRetryable(err) != want {
				t.Fatalf("HTTP %d cleanup classification: %v (want retryable=%v)", status, err, want)
			}
		})
	}
}
