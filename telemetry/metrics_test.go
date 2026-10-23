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

	settings "github.com/Songmu/ghalet/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func metricEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_SDK_DISABLED", "OTEL_METRICS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_SERVICE_NAME", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
		"OTEL_METRIC_EXPORT_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
}

func TestOptionalMetrics(t *testing.T) {
	for _, tc := range []struct {
		name, exporter, disabled, protocol, metricsProtocol string
		wantErr                                             bool
		configExporter                                      string
	}{
		{name: "default"},
		{name: "none", exporter: "none"},
		{name: "environment disables file configuration", exporter: "none", configExporter: "otlp"},
		{name: "SDK disables file configuration", disabled: "true", configExporter: "invalid"},
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
			config := settings.TelemetryConfig{Metrics: settings.MetricsConfig{Exporter: tc.configExporter}}
			m, err := New(t.Context(), "test", config, func() Snapshot {
				t.Error("disabled metrics collected a snapshot")
				return Snapshot{}
			})
			if (err != nil) != tc.wantErr || m != nil {
				t.Fatalf("got metrics=%v, error=%v", m, err)
			}
			m.StartRunner("target").Ready()
			m.StartRunner("target").Fail(errors.New("failure"))
			m.CleanupFailed("target", errors.New("failure"))
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
	snapshot := Snapshot{
		Capacity: 3, Occupied: 2,
		Runners: map[Runner]int64{{"target", "idle"}: 1},
		Workers: map[Runner]int64{{"target", "available"}: 1},
	}
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
	m.CleanupFailed("target", context.DeadlineExceeded)
	m.CleanupFailed("target", context.DeadlineExceeded)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	target := attribute.NewSet(attribute.String("ghalet.target.id", "target"))
	for _, scope := range data.ScopeMetrics {
		if scope.Scope.Name != "github.com/Songmu/ghalet" || scope.Scope.Version != "test" || scope.Scope.SchemaURL != semconv.SchemaURL {
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
			case "cicd.system.errors":
				got := metric.Data.(metricdata.Sum[int64])
				if metric.Unit != "{error}" || !got.IsMonotonic || len(got.DataPoints) != 2 {
					t.Fatalf("invalid system errors: %+v", metric)
				}
				for _, point := range got.DataPoints {
					operation, _ := point.Attributes.Value("ghalet.operation")
					want, kind := int64(1), "_OTHER"
					if operation.AsString() == "runner.cleanup" {
						want, kind = 2, "timeout"
					} else if operation.AsString() != "runner.start" {
						t.Fatalf("unexpected operation: %v", operation)
					}
					attrs := attribute.NewSet(
						attribute.String("ghalet.target.id", "target"),
						attribute.String("ghalet.operation", operation.AsString()),
						semconv.CICDSystemComponent("controller"), semconv.ErrorTypeKey.String(kind),
					)
					if point.Value != want || !point.Attributes.Equals(&attrs) {
						t.Fatalf("invalid failure classification: %+v", point)
					}
				}
			default:
				got := metric.Data.(metricdata.Sum[int64])
				want := int64(1)
				if metric.Name == "ghalet.runner.start.attempts" {
					want = 3
				}
				if len(got.DataPoints) != 1 || got.DataPoints[0].Value != want ||
					got.IsMonotonic != (metric.Name == "ghalet.runner.start.attempts") {
					t.Fatalf("invalid counter: %+v", metric)
				}
				attrs := []attribute.KeyValue{attribute.String("ghalet.target.id", "target")}
				if metric.Name == "ghalet.execution.count" {
					if metric.Unit != "{execution}" {
						t.Fatalf("invalid execution unit: %s", metric.Unit)
					}
					attrs = append(attrs, attribute.String("ghalet.runner.state", "idle"))
				} else if metric.Name == "cicd.worker.count" {
					if metric.Unit != "{worker}" {
						t.Fatalf("invalid worker unit: %s", metric.Unit)
					}
					attrs = append(attrs, semconv.CICDWorkerStateAvailable)
				}
				wantAttributes := attribute.NewSet(attrs...)
				if !got.DataPoints[0].Attributes.Equals(&wantAttributes) {
					t.Fatalf("unexpected metric attributes: %+v", metric)
				}
			}
		}
	}
	for _, name := range []string{
		"ghalet.capacity.limit", "ghalet.capacity.occupied", "ghalet.execution.count",
		"ghalet.runner.start.attempts", "cicd.worker.count",
		"ghalet.runner.startup.duration", "cicd.system.errors",
	} {
		if !seen[name] {
			t.Fatalf("missing metric %s: %v", name, seen)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("unexpected metric instruments: %v", seen)
	}
	snapshot = Snapshot{}
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "ghalet.execution.count" || metric.Name == "cicd.worker.count" {
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
			m, err := New(t.Context(), "test", settings.TelemetryConfig{}, func() Snapshot { return Snapshot{Capacity: 1} })
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
