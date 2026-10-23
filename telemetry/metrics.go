// Package telemetry provides opt-in OpenTelemetry metrics for the daemon.
package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const shutdownTimeout = 3 * time.Second

var headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Runner identifies a bounded execution- or worker-state series, not an individual execution.
type Runner struct {
	TargetID string
	State    string
}

// Snapshot contains the daemon's coherent capacity and runner counts.
type Snapshot struct {
	Capacity int64
	Occupied int64
	Runners  map[Runner]int64
	Workers  map[Runner]int64
}

// Metrics routes lifecycle measurements to the current provider.
// A nil Metrics or one without a provider is disabled.
type Metrics struct {
	mu         sync.RWMutex
	current    *generation
	instanceID string
}

type generation struct {
	provider        *sdkmetric.MeterProvider
	active          atomic.Bool
	startAttempts   metric.Int64Counter
	systemErrors    metric.Int64Counter
	startupDuration metric.Float64Histogram
}

// Update owns a prepared provider until Commit consumes it.
type Update struct {
	generation *generation
}

// NewRecorder creates a stable lifecycle recorder without an SDK or background work.
func NewRecorder() *Metrics {
	return &Metrics{instanceID: rand.Text()}
}

// New enables OTLP/HTTP metrics when configuration or OTEL_METRICS_EXPORTER selects "otlp".
// It does not modify the global OTel providers or start a network listener.
func New(ctx context.Context, version string, config settings.TelemetryConfig, snapshot func() Snapshot) (*Metrics, error) {
	m := NewRecorder()
	update, err := m.Prepare(ctx, version, config, snapshot)
	if err != nil {
		return nil, err
	}
	if update.generation == nil {
		return nil, nil
	}
	m.Commit(update)
	return m, nil
}

// Prepare validates and constructs a replacement without collecting or exporting.
// Close the returned update if it is not committed.
func (m *Metrics) Prepare(ctx context.Context, version string, config settings.TelemetryConfig, snapshot func() Snapshot) (*Update, error) {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return &Update{}, nil
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	selected := config.Metrics.Exporter
	if value := os.Getenv("OTEL_METRICS_EXPORTER"); value != "" {
		selected = value
	}
	switch selected {
	case "", "none":
		return &Update{}, nil
	case "otlp":
	default:
		return nil, errors.New("metrics exporter must be otlp or none")
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return nil, errors.New("OTLP metrics require the http/protobuf protocol")
	}
	if err := validateExporterEnvironment(); err != nil {
		return nil, err
	}
	attributes := []attribute.KeyValue{semconv.ServiceName("ghalet")}
	for name, value := range config.ResourceAttributes {
		attributes = append(attributes, attribute.String(name, value))
	}
	if config.ServiceName != "" {
		attributes = append(attributes, semconv.ServiceName(config.ServiceName))
	}
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(attributes...),
		resource.WithFromEnv(),
		resource.WithAttributes(
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(m.instanceID),
		),
	)
	if err != nil {
		return nil, errors.New("invalid OpenTelemetry resource configuration")
	}
	exporterOptions := []otlpmetrichttp.Option{
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
	}
	if config.Metrics.Endpoint != "" && os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		exporterOptions = append(exporterOptions, otlpmetrichttp.WithEndpointURL(config.Metrics.Endpoint))
	}
	var readerOptions []sdkmetric.PeriodicReaderOption
	if config.Metrics.Interval != nil && os.Getenv("OTEL_METRIC_EXPORT_INTERVAL") == "" {
		readerOptions = append(readerOptions, sdkmetric.WithInterval(time.Duration(*config.Metrics.Interval)))
	}
	if config.Metrics.Timeout != nil {
		timeout := time.Duration(*config.Metrics.Timeout)
		if os.Getenv("OTEL_METRIC_EXPORT_TIMEOUT") == "" {
			readerOptions = append(readerOptions, sdkmetric.WithTimeout(timeout))
		}
		if os.Getenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT") == "" {
			exporterOptions = append(exporterOptions, otlpmetrichttp.WithTimeout(timeout))
		}
	}
	exporter, err := otlpmetrichttp.New(ctx, exporterOptions...)
	if err != nil {
		return nil, errors.New("cannot initialize OTLP metrics exporter; check its endpoint and TLS configuration")
	}
	g := &generation{}
	reader := sdkmetric.NewPeriodicReader(redactedExporter{Exporter: exporter, active: &g.active}, readerOptions...)
	g.provider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		sdkmetric.WithCardinalityLimit(2000),
	)
	if err := g.instrument(version, snapshot); err != nil {
		_ = (&Update{generation: g}).Close()
		return nil, err
	}
	return &Update{generation: g}, nil
}

func validateExporterEnvironment() error {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if _, err := url.Parse(value); err != nil {
				return fmt.Errorf("%s is not a valid OTLP endpoint URL", name)
			}
		}
	}
	// The SDK parses general settings even when signal-specific settings override them.
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		for _, header := range strings.Split(value, ",") {
			key, value, found := strings.Cut(header, "=")
			if !found || !headerNamePattern.MatchString(strings.TrimSpace(key)) {
				return fmt.Errorf("%s must contain URL-encoded key=value headers", name)
			}
			if _, err := url.PathUnescape(value); err != nil {
				return fmt.Errorf("%s must contain URL-encoded key=value headers", name)
			}
		}
	}
	return nil
}

// Commit atomically installs a prepared provider and returns the previous one.
// Close the returned update outside application locks; it makes a final export.
func (m *Metrics) Commit(next *Update) *Update {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := &Update{generation: m.current}
	m.current, next.generation = next.generation, nil
	if m.current != nil {
		m.current.active.Store(true)
	}
	return old
}

func (g *generation) instrument(version string, snapshot func() Snapshot) error {
	meter := g.provider.Meter("github.com/Songmu/ghalet",
		metric.WithInstrumentationVersion(version), metric.WithSchemaURL(semconv.SchemaURL))
	var err error
	if g.startAttempts, err = meter.Int64Counter("ghalet.runner.start.attempts",
		metric.WithUnit("{attempt}"), metric.WithDescription("Runner startup attempts.")); err != nil {
		return err
	}
	if g.systemErrors, err = meter.Int64Counter("cicd.system.errors",
		metric.WithUnit("{error}"), metric.WithDescription("Errors in runner startup and cleanup in the controller.")); err != nil {
		return err
	}
	if g.startupDuration, err = meter.Float64Histogram("ghalet.runner.startup.duration",
		metric.WithUnit("s"), metric.WithDescription("Time from runner reservation to first observed readiness."),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600)); err != nil {
		return err
	}
	capacity, err := meter.Int64ObservableGauge("ghalet.capacity.limit",
		metric.WithUnit("{slot}"), metric.WithDescription("Configured host-wide runner capacity."))
	if err != nil {
		return err
	}
	occupied, err := meter.Int64ObservableGauge("ghalet.capacity.occupied",
		metric.WithUnit("{slot}"), metric.WithDescription("Occupied capacity, including retained diagnostic reservations."))
	if err != nil {
		return err
	}
	runners, err := meter.Int64ObservableUpDownCounter("ghalet.execution.count",
		metric.WithUnit("{execution}"), metric.WithDescription("Retained runner executions by target and exclusive state."))
	if err != nil {
		return err
	}
	workers, err := meter.Int64ObservableUpDownCounter("cicd.worker.count",
		metric.WithUnit("{worker}"), metric.WithDescription("Known online workers managed by this daemon, by target and state."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		if !g.active.Load() {
			return nil
		}
		s := snapshot()
		observer.ObserveInt64(capacity, s.Capacity)
		observer.ObserveInt64(occupied, s.Occupied)
		for runner, count := range s.Runners {
			observer.ObserveInt64(runners, count, metric.WithAttributes(
				attribute.String("ghalet.target.id", runner.TargetID),
				attribute.String("ghalet.runner.state", runner.State),
			))
		}
		for worker, count := range s.Workers {
			observer.ObserveInt64(workers, count, metric.WithAttributes(
				attribute.String("ghalet.target.id", worker.TargetID),
				semconv.CICDWorkerStateKey.String(worker.State),
			))
		}
		return nil
	}, capacity, occupied, runners, workers)
	return err
}

// Close disables measurements and shuts down the current provider.
func (m *Metrics) Close() error {
	if m == nil {
		return nil
	}
	return m.Commit(&Update{}).Close()
}

// Close releases a prepared or retired provider with a bounded wait.
// An uncommitted provider does not collect or export.
func (u *Update) Close() error {
	if u == nil || u.generation == nil {
		return nil
	}
	g := u.generation
	u.generation = nil
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return g.provider.Shutdown(ctx)
}

// Startup follows one reservation across worker snapshots and duplicate events.
type Startup struct {
	once     sync.Once
	metrics  *Metrics
	started  time.Time
	options  metric.MeasurementOption
	targetID string
}

// StartRunner counts an attempt and starts its readiness timer when enabled.
func (m *Metrics) StartRunner(targetID string) *Startup {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return nil
	}
	options := metric.WithAttributes(attribute.String("ghalet.target.id", targetID))
	m.current.startAttempts.Add(context.Background(), 1, options)
	return &Startup{metrics: m, started: time.Now(), options: options, targetID: targetID}
}

// Ready records the first observed readiness, not a workflow result.
func (s *Startup) Ready() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.metrics.mu.RLock()
		defer s.metrics.mu.RUnlock()
		if current := s.metrics.current; current != nil {
			current.startupDuration.Record(context.Background(), time.Since(s.started).Seconds(), s.options)
		}
	})
}

// Fail counts one pre-readiness failure, excluding cancellation.
func (s *Startup) Fail(err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if !errors.Is(err, context.Canceled) {
			s.metrics.recordError(s.targetID, "runner.start", err)
		}
	})
}

// CleanupFailed counts actual failed cleanup attempts rather than diagnostics.
func (m *Metrics) CleanupFailed(targetID string, err error) {
	if m != nil {
		m.recordError(targetID, "runner.cleanup", err)
	}
}

func (m *Metrics) recordError(targetID, operation string, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return
	}
	kind := "_OTHER"
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		kind = "timeout"
	case errors.Is(err, os.ErrPermission):
		kind = "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		kind = "not_found"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	}
	m.current.systemErrors.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("ghalet.target.id", targetID),
		attribute.String("ghalet.operation", operation),
		semconv.CICDSystemComponent("controller"),
		semconv.ErrorTypeKey.String(kind),
	))
}

type redactedExporter struct {
	sdkmetric.Exporter
	active *atomic.Bool
}

func (e redactedExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	if !e.active.Load() {
		return nil
	}
	if err := e.Exporter.Export(ctx, data); err != nil {
		// SDK diagnostics must not include collector URLs, credentials, or response bodies.
		return errors.New("OTLP metrics export failed")
	}
	return nil
}
