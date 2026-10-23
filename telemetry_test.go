package ghalet

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/telemetry"
	"github.com/actions/scaleset"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func clearTelemetryEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_SDK_DISABLED", "OTEL_METRICS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
		"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_METRIC_EXPORT_TIMEOUT",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

func TestTelemetrySnapshot(t *testing.T) {
	d, _ := testDaemon(t)
	d.problems = []Problem{{Reason: "unidentified reservation"}}
	d.runs = map[string]*activeRun{
		"preparing": {State: &RunState{TargetID: "target"}, Preparing: true},
		"idle":      {State: &RunState{TargetID: "target", RunnerID: 1, Ready: true}},
		"busy":      {State: &RunState{TargetID: "target", RunnerID: 2, Ready: true, Busy: true}},
		"exiting":   {State: &RunState{TargetID: "target", RunnerID: 3, Ready: true, Phase: "exiting"}},
		"cleaning":  {State: &RunState{TargetID: "target", RunnerID: 4, Ready: true, ProcessDone: true}},
		"uncertain": {State: &RunState{TargetID: "target", RunnerID: 5, Ready: true, ProcessDone: true}, Uncertain: true},
		"exited":    {State: &RunState{TargetID: "target", RunnerID: 6, Ready: true, Busy: true}, Exited: true},
		"stopped":   {State: &RunState{TargetID: "target", RunnerID: 7, Ready: true}, GroupStopped: true},
	}
	snapshot := d.telemetrySnapshot()
	want := map[telemetry.Runner]int64{}
	for _, state := range []string{"preparing", "idle", "busy", "exiting", "cleaning", "uncertain"} {
		want[telemetry.Runner{TargetID: "target", State: state}] = 1
	}
	want[telemetry.Runner{TargetID: "target", State: "exiting"}] = 3
	if snapshot.Capacity != int64(d.config.Capacity.MaxRunners) || snapshot.Occupied != 8 || !reflect.DeepEqual(snapshot.Runners, want) {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	wantWorkers := map[telemetry.Runner]int64{
		{TargetID: "target", State: "available"}: 1,
		{TargetID: "target", State: "busy"}:      1,
	}
	if !reflect.DeepEqual(snapshot.Workers, wantWorkers) {
		t.Fatalf("retained executions misclassified as online workers: %v", snapshot.Workers)
	}
	d.runs["busy"].InspectFailures = 1
	if d.telemetrySnapshot().Workers[telemetry.Runner{TargetID: "target", State: "busy"}] != 0 {
		t.Fatal("worker with failed inspection counted as online")
	}
	delete(d.runs, "idle")
	if snapshot.Runners[telemetry.Runner{TargetID: "target", State: "idle"}] != 1 {
		t.Fatal("snapshot aliases daemon state")
	}
}

func TestTelemetryStartupTimeout(t *testing.T) {
	for _, outcome := range []string{"cleanup succeeds", "cleanup fails", "assignment wins", "assignment wins with save failure"} {
		t.Run(outcome, func(t *testing.T) {
			assignmentWins := strings.HasPrefix(outcome, "assignment wins")
			saveFails := outcome == "assignment wins with save failure"
			clearTelemetryEnvironment(t)
			d, api := testDaemon(t)
			cleanupProcesses(t, d)
			requests := make(chan *collectormetrics.ExportMetricsServiceRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				request := &collectormetrics.ExportMetricsServiceRequest{}
				if err := proto.Unmarshal(data, request); err != nil {
					t.Error(err)
					return
				}
				requests <- request
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			m, err := telemetry.New(t.Context(), Version, settings.TelemetryConfig{
				Metrics: settings.MetricsConfig{Exporter: "otlp", Endpoint: server.URL},
			}, d.telemetrySnapshot)
			if err != nil {
				t.Fatal(err)
			}
			d.metrics.Store(m)
			defer m.Close()
			seedDistribution(t, d.config, "#!/bin/sh\nsleep 30\n")
			target := d.targets[buildTargetID]
			target.Assigned = 1
			run := waitForRun(t, d, func(run *activeRun) bool {
				return run.State.Process.PID > 0 && !run.Preparing
			})
			state := run.State
			if state.StartedAt.IsZero() || state.Ready {
				t.Fatal("startup clock or readiness is invalid")
			}
			if outcome == "cleanup fails" {
				if err := os.MkdirAll(filepath.Dir(d.config.Logs.Root), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(d.config.Logs.Root, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if assignmentWins {
				api.removeErr = scaleset.JobStillRunningError
			}
			if saveFails {
				target.Failures, target.StartError = 2, "previous startup failure"
				path := filepath.Join(filepath.Dir(state.RunnerDir), "state.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(path); err != nil {
						t.Error(err)
					}
				})
			}
			d.mu.Lock()
			state.StartedAt = time.Now().Add(-3 * time.Minute)
			if saveFails {
				d.runIO(run, func(work *activeRun) error {
					d.enforceStartupTimeout(work)
					return nil
				})
			} else {
				d.reconcile()
			}
			d.mu.Unlock()
			settleRuns(d)
			if assignmentWins {
				wantFailures := 0
				if saveFails {
					wantFailures = 2
					if !run.Uncertain || target.Fault == "" || target.StartError != "previous startup failure" {
						t.Fatal("failed persistence lost its diagnostics or reset startup backoff")
					}
				}
				if !state.Ready || !state.Busy || state.ProcessDone || target.Failures != wantFailures {
					t.Fatal("assignment arbitration did not preserve the ready runner")
				}
			} else if target.Failures != 1 || !state.ProcessDone ||
				(outcome == "cleanup succeeds" && d.runs[state.RunID] != nil) ||
				(outcome == "cleanup fails" && (d.runs[state.RunID] == nil || state.Error == "")) {
				t.Fatalf("startup timeout outcome lost: state=%+v, failures=%d", state, target.Failures)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			var timeouts, readiness uint64
			for _, resource := range (<-requests).ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					for _, metric := range scope.Metrics {
						if metric.Name == "cicd.system.errors" {
							for _, point := range metric.GetSum().DataPoints {
								attributes := map[string]string{}
								for _, attr := range point.Attributes {
									attributes[attr.Key] = attr.Value.GetStringValue()
								}
								if attributes["ghalet.operation"] == "runner.start" {
									if attributes["error.type"] != "timeout" {
										t.Fatalf("startup timeout misclassified: %v", attributes)
									}
									timeouts += uint64(point.GetAsInt())
								}
							}
						} else if metric.Name == "ghalet.runner.startup.duration" {
							for _, point := range metric.GetHistogram().DataPoints {
								readiness += point.Count
							}
						}
					}
				}
			}
			wantTimeouts, wantReadiness := uint64(1), uint64(0)
			if assignmentWins {
				wantTimeouts, wantReadiness = 0, 1
			}
			if timeouts != wantTimeouts || readiness != wantReadiness {
				t.Fatalf("timeouts=%d, readiness=%d; want %d, %d", timeouts, readiness, wantTimeouts, wantReadiness)
			}
		})
	}
}

func TestTelemetryConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		wantErr    bool
	}{
		{name: "default"},
		{name: "file settings", yaml: "telemetry:\n  serviceName: configured-ghalet\n" +
			"  resourceAttributes:\n    deployment.environment.name: test\n" +
			"  metrics:\n    exporter: otlp\n    endpoint: http://127.0.0.1:4318/v1/metrics\n" +
			"    interval: 500ms\n    timeout: 2s\n"},
		{name: "unsupported exporter", yaml: "telemetry:\n  metrics:\n    exporter: console\n", wantErr: true},
		{name: "invalid endpoint", yaml: "telemetry:\n  metrics:\n    endpoint: localhost:4318\n", wantErr: true},
		{name: "endpoint credentials", yaml: "telemetry:\n  metrics:\n    endpoint: https://user:secret@collector/v1/metrics\n", wantErr: true},
		{name: "zero interval", yaml: "telemetry:\n  metrics:\n    interval: 0s\n", wantErr: true},
		{name: "negative timeout", yaml: "telemetry:\n  metrics:\n    timeout: -1s\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := settings.LoadConfig(writeConfig(t, "targets: []\n"+tc.yaml))
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected configuration validation: %v", err)
			}
			if tc.name == "file settings" {
				got := config.Telemetry
				if got.ServiceName != "configured-ghalet" || got.ResourceAttributes["deployment.environment.name"] != "test" ||
					got.Metrics.Exporter != "otlp" || got.Metrics.Endpoint != "http://127.0.0.1:4318/v1/metrics" ||
					got.Metrics.Interval == nil || time.Duration(*got.Metrics.Interval) != 500*time.Millisecond ||
					got.Metrics.Timeout == nil || time.Duration(*got.Metrics.Timeout) != 2*time.Second {
					t.Fatalf("lost file configuration: %+v", got)
				}
			}
		})
	}
}

func TestTelemetryFileExport(t *testing.T) {
	for _, endpoint := range []string{"file", "general environment", "signal environment"} {
		t.Run(endpoint, func(t *testing.T) {
			clearTelemetryEnvironment(t)
			requests := make(chan *collectormetrics.ExportMetricsServiceRequest, 1)
			wantPath := "/file/metrics"
			if endpoint == "general environment" {
				wantPath = "/base/v1/metrics"
			} else if endpoint == "signal environment" {
				wantPath = "/signal/metrics"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != wantPath {
					t.Errorf("endpoint precedence: got %q, want %q", r.URL.Path, wantPath)
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				request := &collectormetrics.ExportMetricsServiceRequest{}
				if err := proto.Unmarshal(data, request); err != nil {
					t.Error(err)
					return
				}
				select {
				case requests <- request:
				default:
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			if endpoint != "file" {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/base")
			}
			wantService := "file-service"
			if endpoint == "general environment" {
				t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=environment-service")
				wantService = "environment-service"
			}
			if endpoint == "signal environment" {
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/signal/metrics")
			}
			config, err := settings.LoadConfig(writeConfig(t, "targets: []\ntelemetry:\n"+
				"  serviceName: file-service\n  resourceAttributes:\n    deployment.environment.name: test\n    service.name: attribute-service\n"+
				"  metrics:\n    exporter: otlp\n    endpoint: "+server.URL+"/file/metrics\n    interval: 20ms\n    timeout: 250ms\n"))
			if err != nil {
				t.Fatal(err)
			}
			m, err := telemetry.New(t.Context(), Version, config.Telemetry, func() telemetry.Snapshot {
				return telemetry.Snapshot{Capacity: 1}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			select {
			case request := <-requests:
				if len(request.ResourceMetrics) != 1 {
					t.Fatalf("missing configured resource: %v", request)
				}
				resource := request.ResourceMetrics[0]
				attrs := map[string]string{}
				for _, attr := range resource.Resource.Attributes {
					attrs[attr.Key] = attr.Value.GetStringValue()
				}
				if resource.SchemaUrl != semconv.SchemaURL || attrs["service.name"] != wantService ||
					attrs["service.version"] != Version || attrs["service.instance.id"] == "" ||
					attrs["deployment.environment.name"] != "test" {
					t.Fatalf("invalid file-configured resource: %v", resource)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("file exporter or export interval was not applied")
			}
			if os.Getenv("OTEL_METRICS_EXPORTER") != "" || os.Getenv("OTEL_SERVICE_NAME") != "" {
				t.Fatal("file configuration was applied through global environment mutation")
			}
		})
	}
}

func TestTelemetryApply(t *testing.T) {
	clearTelemetryEnvironment(t)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	d, api := testDaemon(t)
	useFakeManagement(d, api)
	m := d.metrics.Load()
	target := d.targets[buildTargetID]
	state := testState(t, d.config)
	state.Ready, state.Busy, state.Phase = true, true, "busy"
	run := &activeRun{State: state, Target: target}
	d.runs[state.RunID] = run
	type exported struct {
		path string
		data *collectormetrics.ExportMetricsServiceRequest
	}
	requests := make(chan exported, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request := &collectormetrics.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(data, request); err != nil {
			t.Error(err)
			return
		}
		requests <- exported{path: r.URL.Path, data: request}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	defer m.Close()
	apply := func(next *settings.Config) {
		t.Helper()
		writeApplyConfig(t, next)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if response := d.apply(ctx); response.Error != "" {
			t.Fatalf("telemetry apply failed: %+v", response)
		}
	}
	instanceID := ""
	checkExport := func(name string, attempts int64, startups uint64) {
		t.Helper()
		var request exported
		select {
		case request = <-requests:
		case <-time.After(3 * time.Second):
			t.Fatal("retired provider did not make its final export")
		}
		if request.path != "/"+name || len(request.data.ResourceMetrics) != 1 {
			t.Fatalf("unexpected export: %+v", request)
		}
		resource := request.data.ResourceMetrics[0]
		attrs := map[string]string{}
		for _, attr := range resource.Resource.Attributes {
			attrs[attr.Key] = attr.Value.GetStringValue()
		}
		if instanceID == "" {
			instanceID = attrs["service.instance.id"]
		}
		if instanceID == "" || attrs["service.instance.id"] != instanceID || attrs["service.name"] != name {
			t.Fatalf("resource configuration or daemon identity lost across apply: %v", attrs)
		}
		var gotAttempts int64
		var gotStartups uint64
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				switch metric.Name {
				case "ghalet.runner.start.attempts":
					for _, point := range metric.GetSum().DataPoints {
						gotAttempts += point.GetAsInt()
					}
				case "ghalet.runner.startup.duration":
					for _, point := range metric.GetHistogram().DataPoints {
						gotStartups += point.Count
					}
				}
			}
		}
		if gotAttempts != attempts || gotStartups != startups {
			t.Fatalf("%s: attempts=%d startups=%d, want %d/%d", name, gotAttempts, gotStartups, attempts, startups)
		}
	}
	next := nextConfig(d)
	next.Telemetry.ServiceName = "first"
	next.Telemetry.Metrics = settings.MetricsConfig{Exporter: "otlp", Endpoint: server.URL + "/first"}
	apply(next)
	startup := m.StartRunner(buildTargetID)
	next = nextConfig(d)
	next.Telemetry.ServiceName = "second"
	next.Telemetry.Metrics.Endpoint = server.URL + "/second"
	writeApplyConfig(t, next)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "grpc")
	if response := d.apply(t.Context()); response.Error == "" || !strings.Contains(response.Error, "http/protobuf") {
		t.Fatalf("invalid telemetry update was not rejected: %+v", response)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
	if d.config.Telemetry.ServiceName != "first" || target.Draining || d.runs[state.RunID] != run {
		t.Fatal("failed telemetry preparation changed config or drained a runner")
	}
	apply(next)
	if d.metrics.Load() != m || d.targets[buildTargetID] != target || target.Draining ||
		d.runs[state.RunID] != run || !run.State.Busy || api.createCalls != 0 || api.deleteCalls != 0 {
		t.Fatal("telemetry-only apply replaced the recorder, listener, or busy runner")
	}
	checkExport("first", 1, 0)
	startup.Ready()
	startup.Ready()
	m.StartRunner(buildTargetID)

	d.mu.Lock()
	delete(d.runs, state.RunID)
	d.mu.Unlock()
	next = nextConfig(d)
	next.Capacity.MaxRunners++
	apply(next)
	if len(requests) != 0 {
		t.Fatal("applying other settings retired an unchanged telemetry provider")
	}
	m.StartRunner(buildTargetID)
	previousTarget := d.targets[buildTargetID]
	next = nextConfig(d)
	next.Capacity.MaxRunners++
	next.Telemetry.ServiceName = "third"
	next.Telemetry.Metrics.Endpoint = server.URL + "/third"
	apply(next)
	if d.config.Capacity != next.Capacity || d.targets[buildTargetID] == previousTarget || d.metrics.Load() != m {
		t.Fatal("mixed apply failed to reconcile targets and telemetry together")
	}
	checkExport("second", 2, 1)
	disabledStartup := m.StartRunner(buildTargetID)
	next = nextConfig(d)
	next.Telemetry.Metrics.Exporter = "none"
	apply(next)
	checkExport("third", 1, 0)
	disabledStartup.Ready()
	if m.StartRunner(buildTargetID) != nil {
		t.Fatal("disabled metrics still recorded a startup")
	}
	next = nextConfig(d)
	next.Telemetry.ServiceName = "fourth"
	next.Telemetry.Metrics = settings.MetricsConfig{Exporter: "otlp", Endpoint: server.URL + "/fourth"}
	apply(next)
	disabledStartup.Ready()
	m.StartRunner(buildTargetID).Ready()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	checkExport("fourth", 1, 1)
}

func TestDaemonOTLPMetrics(t *testing.T) {
	d, api := testDaemon(t)
	requests := make(chan *collectormetrics.ExportMetricsServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/metrics" || r.Header.Get("Content-Type") != "application/x-protobuf" ||
			r.Header.Get("Authorization") != "Bearer collector-secret" {
			t.Errorf("unexpected OTLP request: path=%q content-type=%q", r.URL.Path, r.Header.Get("Content-Type"))
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request := &collectormetrics.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(data, request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	for name, value := range map[string]string{
		"OTEL_METRICS_EXPORTER": "otlp", "OTEL_SDK_DISABLED": "false",
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://127.0.0.1:1",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": server.URL + "/custom/metrics",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "grpc", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS":         "Authorization=Bearer%20wrong-secret",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS": "Authorization=Bearer%20collector-secret",
		"OTEL_METRIC_EXPORT_INTERVAL":        "3600000", "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=test",
		"OTEL_SERVICE_NAME": "test-ghalet",
	} {
		t.Setenv(name, value)
	}
	d.config.Telemetry.Metrics.Exporter = "none"
	d.config.Telemetry.Metrics.Endpoint = "http://127.0.0.1:1/file/metrics"
	d.config.Telemetry.ServiceName = "file-service"
	d.config.Telemetry.ResourceAttributes = map[string]string{
		"deployment.environment.name": "file", "service.version": "wrong", "service.instance.id": "wrong",
	}
	m, err := telemetry.New(t.Context(), Version, d.config.Telemetry, d.telemetrySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	d.metrics.Store(m)
	defer m.Close()
	state := testState(t, d.config)
	run := &activeRun{State: state, Target: d.targets[buildTargetID], startup: m.StartRunner(buildTargetID)}
	d.runs[state.RunID] = run
	for range 2 {
		d.mu.Lock()
		d.processEvent(runEvent{id: state.RunID, kind: "ready"})
		d.mu.Unlock()
		settleRuns(d)
	}
	d.makeRunDirectory = func(string) error { return errors.New("cannot create execution directory") }
	d.mu.Lock()
	err = d.reserve(d.targets[buildTargetID])
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	settleRuns(d)
	api.getErr = errTestAPI
	run.State.ProcessDone = true
	if err := d.cleanup(run); err == nil {
		t.Fatal("expected failed cleanup")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	found := map[string]bool{}
	for _, resource := range request.ResourceMetrics {
		attributes := map[string]string{}
		for _, attr := range resource.Resource.Attributes {
			attributes[attr.Key] = attr.Value.GetStringValue()
		}
		if attributes["service.name"] != "test-ghalet" || attributes["service.version"] != Version ||
			attributes["service.instance.id"] == "" || attributes["service.instance.id"] == "wrong" ||
			attributes["deployment.environment.name"] != "test" {
			t.Fatalf("unexpected resource: %v", attributes)
		}
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				found[metric.Name] = true
				switch metric.Name {
				case "ghalet.runner.start.attempts":
					if points := metric.GetSum().DataPoints; len(points) != 1 || points[0].GetAsInt() != 2 {
						t.Fatalf("unexpected %s: %v", metric.Name, metric)
					}
				case "cicd.system.errors":
					points := metric.GetSum().DataPoints
					if len(points) != 2 || points[0].GetAsInt() != 1 || points[1].GetAsInt() != 1 {
						t.Fatalf("startup/cleanup errors not counted: %v", metric)
					}
				case "ghalet.runner.startup.duration":
					if points := metric.GetHistogram().DataPoints; len(points) != 1 || points[0].Count != 1 {
						t.Fatalf("readiness counted more than once: %v", metric)
					}
				}
			}
		}
	}
	if len(found) != 6 || found["cicd.worker.count"] {
		t.Fatalf("missing metrics or stopped worker counted as online: %v", found)
	}
}
