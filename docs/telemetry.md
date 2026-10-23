# OpenTelemetry metrics

[日本語](telemetry.ja.md)

ghalet can send daemon metrics to an OpenTelemetry Collector or another
OTLP-compatible receiver. Metrics are **disabled by default**: no provider,
periodic collection, export, or telemetry listener is started. Existing JSON
logs and management commands are unchanged.
Metrics start after daemon initialization and recovery. Standalone management
commands, including offline `gc`, do not initialize telemetry.

## Enable metrics

Set the environment of the process running `ghalet run`:

```sh
OTEL_METRICS_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:4318/v1/metrics \
ghalet run --config /path/to/config.yaml
```

Only OTLP over HTTP with binary protobuf is supported. ghalet makes outbound
requests; it does not open a metrics port or require a Collector on the same
host. Traces, OTLP logs, stdout metric export, and a Prometheus endpoint are
not implemented.

For systemd, set these variables in a service drop-in, run
`systemctl daemon-reload`, and restart the service. For launchd, use the
service's `EnvironmentVariables` and reload it. Changing the shell environment
or running `ghalet apply` does not change the running daemon's telemetry.
See [service deployment](operations.md#service-deployment).

| Variable | Behavior |
| --- | --- |
| `OTEL_METRICS_EXPORTER` | Unset, empty, or `none` disables metrics. Explicit `otlp` enables them. Other values are startup errors. |
| `OTEL_SDK_DISABLED` | Case-insensitive `true` disables metrics, taking precedence over exporter settings. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Full metrics URL, including its path. Overrides the general endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base URL; defaults to `http://localhost:4318`, with `/v1/metrics` appended. |
| `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | Overrides `OTEL_EXPORTER_OTLP_PROTOCOL`. Unset defaults to `http/protobuf`; other protocols are rejected when metrics are enabled. |
| `OTEL_EXPORTER_OTLP_METRICS_HEADERS` | Authentication headers; overrides `OTEL_EXPORTER_OTLP_HEADERS`. |
| `OTEL_METRIC_EXPORT_INTERVAL` | Export interval in milliseconds; the SDK default is 60000. |
| `OTEL_METRIC_EXPORT_TIMEOUT` | Collection/export timeout in milliseconds; the SDK default is 30000. |
| `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` | HTTP request timeout in milliseconds; overrides `OTEL_EXPORTER_OTLP_TIMEOUT`, whose default is 10000. |
| `OTEL_RESOURCE_ATTRIBUTES` | Explicit resource attributes, such as `deployment.environment.name=production`. Do not include secrets. |
| `OTEL_SERVICE_NAME` | Overrides the default service name, `ghalet`. |

The SDK's standard OTLP/HTTP configuration also supports compression, TLS
certificates, and metric temporality. See the
[Go exporter reference](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp).
No environment variables enable metrics implicitly: setting only an endpoint
does not opt in.

The resource includes ghalet's `service.version` and a fresh
`service.instance.id` for each daemon start. These two attributes are assigned
by ghalet rather than inherited from `OTEL_RESOURCE_ATTRIBUTES`.
Controller environment variables are not automatically copied to runner jobs.
OTLP authentication-header variables cannot be configured through `runner.env`.

## Metrics

These are ghalet-specific instrument names, not standard CI/CD metrics.
Target-scoped measurements use `ghalet.target.id`, the same derived target key
shown by `status`. Only runner counts also have `ghalet.runner.state`.

| Instrument | Type | Unit | Meaning |
| --- | --- | --- | --- |
| `ghalet.capacity.limit` | Gauge | `{slot}` | Configured host-wide runner capacity. |
| `ghalet.capacity.occupied` | Gauge | `{slot}` | Occupied capacity, including retained diagnostic reservations. |
| `ghalet.runner.count` | Non-monotonic sum | `{runner}` | Retained execution records, grouped by target and exclusive state. |
| `ghalet.runner.start.attempts` | Counter | `{attempt}` | Runner reservation attempts, including warm-pool starts. |
| `ghalet.runner.start.failures` | Counter | `{failure}` | Attempts that fail before readiness; cancellation is excluded. |
| `ghalet.runner.startup.duration` | Histogram | `s` | Reservation to first observed readiness, recorded once per successful startup. |
| `ghalet.cleanup.failures` | Counter | `{failure}` | Failed cleanup attempts; each failed retry counts again. |

Runner states are classified in this order: `uncertain`, `preparing`,
`cleaning` (process stopped), `exiting`, `busy`, `idle` (ready); an execution
not yet ready falls back to `preparing`. States are mutually exclusive.
Counts include retained cleanup records, so their sum need not equal occupied
capacity. States with no executions have no series.

The startup histogram's bucket boundaries, in seconds, are
`0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600`.
Startup measurements are not reconstructed for recovered executions.
Runner readiness and exit are not workflow results; no workflow success rate
is inferred.

Counters and histograms reset on daemon restart, not on `apply`; counters
appear after their first measurement. Removed runner-state series disappear
on the next collection. Historical counter/histogram series can remain for
the daemon's lifetime. Each instrument is limited to 2000 series, including
the SDK's overflow series (`otel.metric.overflow=true`).
Run IDs, runner names, paths, URLs, and error messages are not metric labels.

## Export failures and shutdown

Collection reads an in-memory snapshot, without querying GitHub or reading
runner files. The daemon's shared lock is released before SDK observation and
network export. Export runs in the background with SDK timeouts and no export
retry loop or durable queue. A failed delivery does not fail runner operations
or make `doctor` unhealthy. The SDK reports a generic export error to its
error handler (stderr by default); collector URLs, authentication values, and
response bodies are not included in that error.

After workers stop, shutdown attempts one final export with a three-second
deadline. A failure is logged but does not change the daemon's exit result.
Telemetry is best effort, not an execution audit trail. Use a Collector to
provide downstream retry or durable buffering when needed.
