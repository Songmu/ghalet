package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func metricEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_SDK_DISABLED", "OTEL_METRICS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_SERVICE_NAME", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
}

func TestOptionalMetrics(t *testing.T) {
	for _, tc := range []struct {
		name, exporter, disabled, protocol, metricsProtocol string
		wantErr                                             bool
	}{
		{name: "default"},
		{name: "none", exporter: "none"},
		{name: "SDK disabled takes precedence", exporter: "invalid", disabled: "TRUE", protocol: "grpc"},
		{name: "unsupported exporter", exporter: "console", wantErr: true},
		{name: "unsupported protocol", exporter: "otlp", protocol: "grpc", wantErr: true},
		{name: "signal protocol takes precedence", exporter: "otlp", protocol: "http/protobuf", metricsProtocol: "grpc", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metricEnvironment(t)
			t.Setenv("OTEL_METRICS_EXPORTER", tc.exporter)
			t.Setenv("OTEL_SDK_DISABLED", tc.disabled)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tc.protocol)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", tc.metricsProtocol)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			m, err := New(t.Context(), "test", func() Snapshot {
				t.Error("disabled metrics collected a snapshot")
				return Snapshot{}
			})
			if (err != nil) != tc.wantErr || m != nil {
				t.Fatalf("got metrics=%v, error=%v", m, err)
			}
			m.StartRunner("target").Ready()
			m.StartRunner("target").Fail(errors.New("failure"))
			m.CleanupFailed("target")
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetricContract(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(t.Context())
	snapshot := Snapshot{Capacity: 3, Occupied: 2, Runners: map[Runner]int64{{"target", "idle"}: 1}}
	m, err := newMetrics(provider, "test", func() Snapshot { return snapshot })
	if err != nil {
		t.Fatal(err)
	}
	ready := m.StartRunner("target")
	ready.Ready()
	ready.Ready()
	ready.Fail(errors.New("late failure"))
	failed := m.StartRunner("target")
	failed.Fail(errors.New("failed"))
	failed.Fail(errors.New("duplicate"))
	failed.Ready()
	canceled := m.StartRunner("target")
	canceled.Fail(context.Canceled)
	canceled.Ready()
	m.CleanupFailed("target")
	m.CleanupFailed("target")
	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	target := attribute.NewSet(attribute.String("ghalet.target.id", "target"))
	for _, scope := range data.ScopeMetrics {
		if scope.Scope.Name != "github.com/Songmu/ghalet" || scope.Scope.Version != "test" {
			t.Fatalf("unexpected scope: %+v", scope.Scope)
		}
		for _, metric := range scope.Metrics {
			seen[metric.Name] = true
			switch metric.Name {
			case "ghalet.capacity.limit", "ghalet.capacity.occupied":
				got := metric.Data.(metricdata.Gauge[int64]).DataPoints
				want := snapshot.Capacity
				if metric.Name == "ghalet.capacity.occupied" {
					want = snapshot.Occupied
				}
				if metric.Unit != "{slot}" || len(got) != 1 || got[0].Value != want || got[0].Attributes.Len() != 0 {
					t.Fatalf("invalid capacity metric: %+v", metric)
				}
			case "ghalet.runner.startup.duration":
				got := metric.Data.(metricdata.Histogram[float64]).DataPoints
				if metric.Unit != "s" || len(got) != 1 || got[0].Count != 1 || got[0].Sum < 0 ||
					!got[0].Attributes.Equals(&target) {
					t.Fatalf("invalid readiness measurement: %+v", metric)
				}
			default:
				got := metric.Data.(metricdata.Sum[int64])
				want := int64(1)
				if metric.Name == "ghalet.runner.start.attempts" {
					want = 3
				} else if metric.Name == "ghalet.cleanup.failures" {
					want = 2
				}
				if len(got.DataPoints) != 1 || got.DataPoints[0].Value != want ||
					got.IsMonotonic != (metric.Name != "ghalet.runner.count") {
					t.Fatalf("invalid counter: %+v", metric)
				}
				attrs := []attribute.KeyValue{attribute.String("ghalet.target.id", "target")}
				if metric.Name == "ghalet.runner.count" {
					attrs = append(attrs, attribute.String("ghalet.runner.state", "idle"))
				}
				wantAttributes := attribute.NewSet(attrs...)
				if !got.DataPoints[0].Attributes.Equals(&wantAttributes) {
					t.Fatalf("unexpected metric attributes: %+v", metric)
				}
			}
		}
	}
	for _, name := range []string{
		"ghalet.capacity.limit", "ghalet.capacity.occupied", "ghalet.runner.count",
		"ghalet.runner.start.attempts", "ghalet.runner.start.failures",
		"ghalet.runner.startup.duration", "ghalet.cleanup.failures",
	} {
		if !seen[name] {
			t.Fatalf("missing metric %s: %v", name, seen)
		}
	}
	snapshot = Snapshot{}
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "ghalet.runner.count" {
				t.Fatal("removed runner series remained observable")
			}
		}
	}
}

func TestExportFailureIsBoundedAndRedacted(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "response body", true: "unresponsive collector"}[slow], func(t *testing.T) {
			metricEnvironment(t)
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if slow {
					<-release
				}
				http.Error(w, "collector-response-secret", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			defer close(release)
			t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/metrics?token=endpoint-secret")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "60000")
			original := otel.GetMeterProvider()
			m, err := New(t.Context(), "test", func() Snapshot { return Snapshot{Capacity: 1} })
			if err != nil {
				t.Fatal(err)
			}
			if otel.GetMeterProvider() != original || calls.Load() != 0 {
				t.Fatal("initialization changed the global provider or made an export")
			}
			start := time.Now()
			err = m.Close()
			if err == nil || !strings.Contains(err.Error(), "OTLP metrics export failed") {
				t.Fatalf("expected safe export failure, got %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("collector credentials or response leaked: %v", err)
			}
			if elapsed := time.Since(start); elapsed > shutdownTimeout+2*time.Second {
				t.Fatalf("shutdown exceeded its bound: %s", elapsed)
			}
			if calls.Load() != 1 {
				t.Fatalf("failed export was retried: %d calls", calls.Load())
			}
		})
	}
}
