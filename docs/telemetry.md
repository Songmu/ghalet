# OpenTelemetry metrics

[日本語](telemetry.ja.md)

ghalet can send daemon metrics to an OpenTelemetry Collector or another
OTLP-compatible receiver. Metrics are **disabled by default**: no provider,
periodic collection, export, or telemetry listener is started. Existing JSON
logs and management commands are unchanged.
Metrics start after daemon initialization and recovery. Standalone management
commands, including offline `gc`, do not initialize telemetry.

## Enable metrics

Add these settings to the configuration file:

```yaml
telemetry:
  metrics:
    exporter: otlp
    endpoint: http://127.0.0.1:4318/v1/metrics
```

`endpoint` is a full metrics URL, including the path. Other settings are
optional; keep their defaults unless you need the
[advanced configuration](#advanced-configuration).
Do not put credentials in URLs or resource attributes; use the standard
header [environment variables](#environment-variables) for authentication.

Only OTLP over HTTP with binary protobuf is supported. ghalet makes outbound
requests; it does not open a metrics port or require a Collector on the same
host. Traces, OTLP logs, stdout metric export, and a Prometheus endpoint are
not implemented.

Reload configuration-file changes with
`ghalet apply --config /path/to/config.yaml`.

A telemetry-only change does not drain runners or replace target listeners.
When other settings also change, normal apply reconciliation still runs.
The replacement is prepared before any drain and does not collect or send
until committed. Invalid replacement settings leave the old configuration
and provider active. Retiring the old provider makes a bounded final export;
a failed delivery is logged but does not roll back the new configuration.

## Advanced configuration

Service identity, resource attributes, export interval, and timeouts can be
customized when needed. Specify only values you want to override:

```yaml
telemetry:
  serviceName: ghalet
  resourceAttributes:
    deployment.environment.name: production
  metrics:
    exporter: otlp
    endpoint: http://127.0.0.1:4318/v1/metrics
    interval: 60s
    timeout: 10s
```

`interval` and `timeout` use the configuration's duration syntax and must be
at least `1ms`. `timeout` supplies both the collection/export and HTTP
timeouts; if omitted, their SDK defaults are 30s and 10s respectively.
Resource values must be strings.

### Environment variables

Instead of file settings, you can set the environment of the process running
`ghalet run`:

```sh
OTEL_METRICS_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:4318/v1/metrics \
ghalet run --config /path/to/config.yaml
```

For systemd, set these variables in a service drop-in, run
`systemctl daemon-reload`, and restart the service. For launchd, use the
service's `EnvironmentVariables` and reload it. Changing the shell environment
does not change the running daemon's telemetry; service environment changes
require a restart. See [service deployment](operations.md#service-deployment).

Nonempty environment variables override the corresponding file settings.
For endpoints, precedence is metrics-specific environment, general environment,
file, then SDK default. `OTEL_METRICS_EXPORTER=none` disables file-enabled
metrics, and `OTEL_SDK_DISABLED=true` takes precedence over both sources.
The file's `serviceName` overrides `resourceAttributes.service.name`;
environment resource attributes override matching file attributes, and
`OTEL_SERVICE_NAME` takes precedence for the service name.

| Variable | Behavior |
| --- | --- |
| `OTEL_METRICS_EXPORTER` | `otlp` enables metrics; `none` disables them. Unset or empty uses the file setting, which defaults to disabled. Other values are startup errors. |
| `OTEL_SDK_DISABLED` | Case-insensitive `true` disables metrics, taking precedence over exporter settings. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Full metrics URL, including its path. Overrides the general endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base URL, with `/v1/metrics` appended. Without an environment or file endpoint, this Go exporter's default is `https://localhost:4318/v1/metrics`. |
| `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | Overrides `OTEL_EXPORTER_OTLP_PROTOCOL`. Unset defaults to `http/protobuf`; other protocols are rejected when metrics are enabled. |
| `OTEL_EXPORTER_OTLP_METRICS_HEADERS` | Authentication headers; overrides `OTEL_EXPORTER_OTLP_HEADERS`. |
| `OTEL_METRIC_EXPORT_INTERVAL` | Export interval in milliseconds; the SDK default is 60000. |
| `OTEL_METRIC_EXPORT_TIMEOUT` | Collection/export timeout in milliseconds; the SDK default is 30000. |
| `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` | HTTP request timeout in milliseconds; overrides `OTEL_EXPORTER_OTLP_TIMEOUT`, whose default is 10000. |
| `OTEL_RESOURCE_ATTRIBUTES` | Explicit resource attributes, such as `deployment.environment.name=production`. Do not include secrets. |
| `OTEL_SERVICE_NAME` | Overrides the file/environment resource service name; the default is `ghalet`. |

The SDK's standard OTLP/HTTP configuration also supports compression, TLS
certificates, and metric temporality. See the
[Go exporter reference](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp).
No environment variables enable metrics implicitly: setting only an endpoint
does not opt in.

Following the [service resource conventions](https://opentelemetry.io/docs/specs/semconv/resource/service/),
the resource includes ghalet's `service.version` and a fresh
`service.instance.id` for each daemon start. These two attributes are assigned
by ghalet rather than inherited from file or environment attributes.
`service.instance.id` stays the same when telemetry is reconfigured with `apply`.
Controller environment variables are not automatically copied to runner jobs.
OTLP authentication-header variables cannot be configured through `runner.env`.

## Metrics

Where meanings match, instruments follow the
[CI/CD semantic conventions](https://opentelemetry.io/docs/specs/semconv/cicd/cicd-metrics/):
`cicd.worker.count` and `cicd.system.errors` (currently Release Candidate).
Resource and instrumentation scopes identify semantic schema `1.43.0`.
The other instruments remain ghalet-specific, following the
[general metrics conventions](https://opentelemetry.io/docs/specs/semconv/general/metrics/).
Target-scoped measurements use `ghalet.target.id`, the same derived target key
shown by `status`.

| Instrument | Type | Unit | Meaning |
| --- | --- | --- | --- |
| `ghalet.capacity.limit` | Gauge | `{slot}` | Configured host-wide runner capacity. |
| `ghalet.capacity.occupied` | Gauge | `{slot}` | Occupied capacity, including retained diagnostic reservations. |
| `cicd.worker.count` | Non-monotonic sum | `{worker}` | Known online workers managed by this daemon, grouped by target and `cicd.worker.state` (`available` or `busy`). |
| `cicd.system.errors` | Counter | `{error}` | Controller startup/cleanup failures, with `cicd.system.component=controller`, `error.type`, and `ghalet.operation`. |
| `ghalet.execution.count` | Non-monotonic sum | `{execution}` | Retained execution records, grouped by target and exclusive `ghalet.runner.state`. |
| `ghalet.runner.start.attempts` | Counter | `{attempt}` | Runner reservation attempts, including warm-pool starts. |
| `ghalet.runner.startup.duration` | Histogram | `s` | Reservation to first observed readiness, recorded once per successful startup. |

Worker counts include only ready, registered runners without failed process
inspection. `idle` maps to `available`; `busy` maps to `busy`. Preparing,
stopping, cleanup, and uncertain records are excluded. ghalet does not infer
`offline` workers from retained execution records or inventory remote workers.

System errors cover `ghalet.operation=runner.start` (one pre-readiness failure
per startup, excluding cancellation) and `runner.cleanup` (each failed attempt,
including retries). They do not cover every daemon error or workflow failures.
The low-cardinality `error.type` values are `timeout`, `permission_denied`,
`not_found`, `canceled` (cleanup only), and `_OTHER`. Error text is never used.

Execution states are classified in this order: `uncertain`, `preparing`,
`cleaning` (process stopped), `exiting`, `busy`, `idle` (ready); an execution
not yet ready falls back to `preparing`. States are mutually exclusive.
Counts include retained cleanup records, so their sum need not equal occupied
capacity. States with no executions have no series.

The startup histogram's bucket boundaries, in seconds, are
`0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600`.
Startup measurements are not reconstructed for recovered executions or
startups begun while telemetry was disabled. A startup already being measured
keeps its original timer and records its outcome through the provider active
at completion. An outcome observed while disabled is not replayed later.
Runner readiness and exit are not workflow results; no workflow success rate
is inferred. Scale set notifications include workflow run IDs/references,
job results, and timestamps, but cover jobs for the managed scale set, not
necessarily all jobs in a workflow. Runner startup time is therefore not
`cicd.pipeline.run.duration`, and execution records are not
`cicd.pipeline.run.active`. These metrics require a workflow-level data source
rather than treating each job as a pipeline run.

Counters and histograms reset on daemon restart or when changed telemetry
settings replace the provider. Applying other settings without a telemetry
change does not reset them. Counters appear after their first measurement.
Removed runner-state series disappear on the next collection. Historical
counter/histogram series can remain for
the provider's lifetime. Each instrument is limited to 2000 series, including
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
