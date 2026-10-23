// Package telemetry provides opt-in OpenTelemetry metrics for the daemon.
package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

const shutdownTimeout = 3 * time.Second

// Runner identifies a bounded runner-state series, not an individual execution.
type Runner struct {
	TargetID string
	State    string
}

// Snapshot contains the daemon's coherent capacity and runner counts.
type Snapshot struct {
	Capacity int64
	Occupied int64
	Runners  map[Runner]int64
}

// Metrics owns one daemon's provider. A nil Metrics is disabled.
type Metrics struct {
	provider        *sdkmetric.MeterProvider
	startAttempts   metric.Int64Counter
	startFailures   metric.Int64Counter
	startupDuration metric.Float64Histogram
	cleanupFailures metric.Int64Counter
}

// New enables OTLP/HTTP metrics only when OTEL_METRICS_EXPORTER is "otlp".
// It does not modify the global OTel providers or start a network listener.
func New(ctx context.Context, version string, snapshot func() Snapshot) (*Metrics, error) {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return nil, nil
	}
	switch os.Getenv("OTEL_METRICS_EXPORTER") {
	case "", "none":
		return nil, nil
	case "otlp":
	default:
		return nil, errors.New("OTEL_METRICS_EXPORTER must be otlp or none")
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return nil, errors.New("OTLP metrics require the http/protobuf protocol")
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "ghalet")),
		resource.WithFromEnv(),
		resource.WithAttributes(
			attribute.String("service.version", version),
			attribute.String("service.instance.id", rand.Text()),
		),
	)
	if err != nil {
		return nil, errors.New("invalid OpenTelemetry resource configuration")
	}
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, errors.New("cannot initialize OTLP metrics exporter; check its endpoint and TLS configuration")
	}
	reader := sdkmetric.NewPeriodicReader(redactedExporter{exporter})
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		sdkmetric.WithCardinalityLimit(2000),
	)
	m, err := newMetrics(provider, version, snapshot)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = provider.Shutdown(closeCtx)
		return nil, err
	}
	return m, nil
}

func newMetrics(provider *sdkmetric.MeterProvider, version string, snapshot func() Snapshot) (*Metrics, error) {
	m := &Metrics{provider: provider}
	meter := provider.Meter("github.com/Songmu/ghalet", metric.WithInstrumentationVersion(version))
	var err error
	if m.startAttempts, err = meter.Int64Counter("ghalet.runner.start.attempts",
		metric.WithUnit("{attempt}"), metric.WithDescription("Runner startup attempts.")); err != nil {
		return nil, err
	}
	if m.startFailures, err = meter.Int64Counter("ghalet.runner.start.failures",
		metric.WithUnit("{failure}"), metric.WithDescription("Runner startups that failed before readiness.")); err != nil {
		return nil, err
	}
	if m.startupDuration, err = meter.Float64Histogram("ghalet.runner.startup.duration",
		metric.WithUnit("s"), metric.WithDescription("Time from runner reservation to first observed readiness."),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600)); err != nil {
		return nil, err
	}
	if m.cleanupFailures, err = meter.Int64Counter("ghalet.cleanup.failures",
		metric.WithUnit("{failure}"), metric.WithDescription("Failed runner cleanup attempts, including retries.")); err != nil {
		return nil, err
	}
	capacity, err := meter.Int64ObservableGauge("ghalet.capacity.limit",
		metric.WithUnit("{slot}"), metric.WithDescription("Configured host-wide runner capacity."))
	if err != nil {
		return nil, err
	}
	occupied, err := meter.Int64ObservableGauge("ghalet.capacity.occupied",
		metric.WithUnit("{slot}"), metric.WithDescription("Occupied capacity, including retained diagnostic reservations."))
	if err != nil {
		return nil, err
	}
	runners, err := meter.Int64ObservableUpDownCounter("ghalet.runner.count",
		metric.WithUnit("{runner}"), metric.WithDescription("Retained runner executions by target and exclusive state."))
	if err != nil {
		return nil, err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		s := snapshot()
		observer.ObserveInt64(capacity, s.Capacity)
		observer.ObserveInt64(occupied, s.Occupied)
		for runner, count := range s.Runners {
			observer.ObserveInt64(runners, count, metric.WithAttributes(
				attribute.String("ghalet.target.id", runner.TargetID),
				attribute.String("ghalet.runner.state", runner.State),
			))
		}
		return nil
	}, capacity, occupied, runners)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Close makes a final export and releases resources with a bounded wait.
func (m *Metrics) Close() error {
	if m == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return m.provider.Shutdown(ctx)
}

// Startup follows one reservation across worker snapshots and duplicate events.
type Startup struct {
	once    sync.Once
	metrics *Metrics
	started time.Time
	options metric.MeasurementOption
}

// StartRunner counts an attempt and starts its readiness timer when enabled.
func (m *Metrics) StartRunner(targetID string) *Startup {
	if m == nil {
		return nil
	}
	options := metric.WithAttributes(attribute.String("ghalet.target.id", targetID))
	m.startAttempts.Add(context.Background(), 1, options)
	return &Startup{metrics: m, started: time.Now(), options: options}
}

// Ready records the first observed readiness, not a workflow result.
func (s *Startup) Ready() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.metrics.startupDuration.Record(context.Background(), time.Since(s.started).Seconds(), s.options)
	})
}

// Fail counts one pre-readiness failure, excluding cancellation.
func (s *Startup) Fail(err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if !errors.Is(err, context.Canceled) {
			s.metrics.startFailures.Add(context.Background(), 1, s.options)
		}
	})
}

// CleanupFailed counts actual failed cleanup attempts rather than diagnostics.
func (m *Metrics) CleanupFailed(targetID string) {
	if m != nil {
		m.cleanupFailures.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("ghalet.target.id", targetID)))
	}
}

type redactedExporter struct{ sdkmetric.Exporter }

func (e redactedExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	if err := e.Exporter.Export(ctx, data); err != nil {
		// SDK diagnostics must not include collector URLs, credentials, or response bodies.
		return errors.New("OTLP metrics export failed")
	}
	return nil
}
