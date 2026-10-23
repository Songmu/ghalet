package ghalet

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Songmu/ghalet/telemetry"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestTelemetrySnapshot(t *testing.T) {
	d, _ := testDaemon(t)
	d.problems = []Problem{{Reason: "unidentified reservation"}}
	d.runs = map[string]*activeRun{
		"preparing": {State: &RunState{TargetID: "target"}, Preparing: true},
		"idle":      {State: &RunState{TargetID: "target", Ready: true}},
		"busy":      {State: &RunState{TargetID: "target", Ready: true, Busy: true}},
		"exiting":   {State: &RunState{TargetID: "target", Ready: true, Phase: "exiting"}},
		"cleaning":  {State: &RunState{TargetID: "target", Ready: true, ProcessDone: true}},
		"uncertain": {State: &RunState{TargetID: "target", Ready: true, ProcessDone: true}, Uncertain: true},
	}
	snapshot := d.telemetrySnapshot()
	want := map[telemetry.Runner]int64{}
	for _, state := range []string{"preparing", "idle", "busy", "exiting", "cleaning", "uncertain"} {
		want[telemetry.Runner{TargetID: "target", State: state}] = 1
	}
	if snapshot.Capacity != int64(d.config.Capacity.MaxRunners) || snapshot.Occupied != 6 || !reflect.DeepEqual(snapshot.Runners, want) {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	delete(d.runs, "idle")
	if snapshot.Runners[telemetry.Runner{TargetID: "target", State: "idle"}] != 1 {
		t.Fatal("snapshot aliases daemon state")
	}
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
	m, err := telemetry.New(t.Context(), Version, d.telemetrySnapshot)
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
			attributes["service.instance.id"] == "" || attributes["deployment.environment.name"] != "test" {
			t.Fatalf("unexpected resource: %v", attributes)
		}
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				found[metric.Name] = true
				switch metric.Name {
				case "ghalet.runner.start.attempts", "ghalet.runner.start.failures", "ghalet.cleanup.failures":
					want := int64(1)
					if metric.Name == "ghalet.runner.start.attempts" {
						want = 2
					}
					if points := metric.GetSum().DataPoints; len(points) != 1 || points[0].GetAsInt() != want {
						t.Fatalf("unexpected %s: %v", metric.Name, metric)
					}
				case "ghalet.runner.startup.duration":
					if points := metric.GetHistogram().DataPoints; len(points) != 1 || points[0].Count != 1 {
						t.Fatalf("readiness counted more than once: %v", metric)
					}
				}
			}
		}
	}
	if len(found) != 7 {
		t.Fatalf("missing metrics: %v", found)
	}
}
