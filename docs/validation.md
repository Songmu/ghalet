# Validation

[English](validation.md) | [日本語](validation.ja.md)

This guide describes the evidence behind ghalet's behavior, how to run
automated and opt-in live checks, and the limits of that evidence. For setup
and deployment, see the [README](../README.md) and [operations](operations.md).

- [Protocol contracts](#protocol-contracts)
- [Automated tests](#automated-tests)
- [Optional official runner smoke test](#optional-official-runner-smoke-test)
- [Explicit live acceptance](#explicit-live-acceptance)
- [Verification limits](#verification-limits)

## Protocol contracts

The `actions/scaleset` revision in [go.mod](../go.mod) provides a
`listener.Scaler.Scale` callback with explicit acquisition control and
authoritative statistics before the official Listener acknowledges a
message. The v0.4.0 tag has a different callback contract and is not an
interchangeable downgrade.

Runner input/readiness handling is based on these `actions/runner` contracts
(source references below use 2.338.0):

- `CommandSettings` reads and removes `ACTIONS_RUNNER_INPUT_*` variables,
  including JIT configuration, and registers secret arguments with the masker.
- `Runner.Listener run` accepts JIT configuration without exposing it in arguments.
- The listener prints `Listening for Jobs` after session establishment.

During a live launch that stdout readiness line, or a matching SDK JobStarted
event, ends the startup clock. Readiness is not recovered by parsing old
logs. Restarts use persisted state and OS/API identities. ghalet does not
rewrite the JIT payload. See [runner distribution and updates](operations.md#direct-execution-and-environment)
for version pinning, digest verification and self-update behavior.

Sources:
[SDK listener](https://github.com/actions/scaleset/blob/e6daac702355/listener/listener.go),
[SDK errors and deletion](https://github.com/actions/scaleset/blob/e6daac702355/client.go),
[runner inputs](https://github.com/actions/runner/blob/v2.338.0/src/Runner.Listener/CommandSettings.cs),
[runner startup](https://github.com/actions/runner/blob/v2.338.0/src/Runner.Listener/Runner.cs).

## Automated tests

```sh
go test -race ./...
go vet ./...
```

CI executes the suite on Linux and macOS. Tests use fake GitHub APIs, the
actual official Listener, temporary independent runner archives and real
non-root subprocesses. They cover:

| Contract | Automated evidence |
| --- | --- |
| Configuration and CLI | Defaults, aliases, shorthand URLs, strict/legacy-key rejection, validation order, directory safety and version-flag placement (`config_test.go`) |
| Credentials and environment | PAT/gh/App settings, protected files, minimal runner environments and case-insensitive runner-input rejection (`auth_test.go`, `process_test.go`, `config_test.go`) |
| Secret redaction | Multiline/overlapping values, shared-secret references, JSONL output and split-write live/archive masking (`auth_test.go`, `cleanup_test.go`) |
| Registration and distribution | Explicit group/scale set lookup, reuse, update disablement, digests, independent extraction, corrupt caches and traversal rejection (`auth_test.go`, `distribution_test.go`) |
| Acquisition and scheduling | Official Listener acknowledgment/redelivery, ambiguous and statistics-free reservations, shared capacity, warm-slot yield and busy/assignment-race protection (`listener_test.go`, `daemon_test.go`) |
| Lifecycle and shutdown | Independent job directories, accepted backlog during drain, startup/deadline failures, completed drains, recovered busy runs and exit/output ordering (`daemon_test.go`) |
| State and recovery | Atomic writes, ancestry synchronization, locks, legacy/unknown/corrupt records, PID reuse and exact 1 MiB boundaries without replacing readable state (`state_test.go`, `daemon_test.go`) |
| Process identity and termination | Reparenting, empty/zombie/stale snapshots, Linux enumeration races and live threads, invalid PIDs, durable descendant tracking before signaling and uncertain-group retention (`process_test.go`, `identity_linux_test.go`, `daemon_test.go`) |
| Native runner guards | Controller SIGKILL, TERM-resistant descendants, unrelated-process survival, actual runner results, durable launch gates and descriptor closure (`runner_guard_test.go`) |
| Cleanup and logs | Archive/output completion before deletion, failure retention/retry, bounded log lines, retention and symlinked ancestry (`cleanup_test.go`) |
| Management | Stable control addresses after config edits, partial GC, command exit behavior, confirmation refusal and transactional destruction adoption with pending observers or cleanup errors (`control_test.go`) |

Real process tests skip under root rather than accidentally running root
jobs. Run them as a non-root user. Tests never automatically delete real
GitHub registrations or run untrusted workflows. Filesystem fault injection
does not simulate power loss.

## Optional official runner smoke test

```sh
GHALET_TEST_OFFICIAL_RUNNER=1 \
  go test -run '^TestOfficialRunnerJITInputSmoke$' -v
```

This downloads the official pinned archive, verifies its release digest,
starts `Runner.Listener run` with a minimal environment and invalid JIT input,
and verifies that the encoded input is masked in output/diagnostic logs.
It does **not** register a
runner, execute a job or prove that a valid JIT secret is absent from a live
job's environment. OS runtime prerequisites and outbound HTTPS are required.

## Explicit live acceptance

Only use a disposable trusted host and a dedicated repository/scale set.
Install `.github/workflows/ghalet-integration.yaml` in that repository's
default branch first. Prepare an initially empty configuration with exactly
one repository target, `minRunners: 0`, `preserveLogs: true`, and credentials
that can administer runners and dispatch/read the integration workflow.
The target does not need a prescribed naming prefix.

```sh
GHALET_INTEGRATION_CONFIG=/absolute/path/to/dedicated/config.yaml \
  go test -run '^TestGitHubEphemeralLifecycle$' -timeout 20m -v
```

Optionally set `GHALET_INTEGRATION_REF` to the workflow revision to dispatch.
This creates/reuses the explicit scale set, runs two sequential trusted
jobs, verifies separate archived executions and JSON daemon output, and
drains the daemon. The workflow checks non-root execution and absence of the
JIT/gh input variables. The test sets a 15-second drain deadline to bound
failed acceptance runs. It deliberately **leaves the scale set registered**;
destroy it afterwards with an interactive explicit confirmation.

Repeat this on native macOS and Linux. Extend the live scenario manually
for organization/App installations, two targets with all slots warm,
simultaneous assignment/idle-deletion races, API loss after acquisition,
listener/session reconnection, host reboot, service-manager termination and
Lima sleep/wake. Capture SDK/runner versions, OS, structured logs, workflow
run URLs and actual server responses without credentials.

## Verification limits

Fake APIs prove local decisions, not GitHub's server-side reassignment
semantics. In particular:

- Capacity headers keep potential demand observable; actual multi-target
  visibility and acquisition limits require live GitHub acceptance.
- `RemoveRunner` is used as the assignment arbitration point. On
  `JobStillRunningError`, registration mismatch or API failure, the process
  is not stopped for idle reclamation. The exact pre-start assignment race
  guarantee must be verified against the service; it is not established by
  a fake returning that error.
- Capacity zero plus the acquisition gate stops new acceptance locally;
  treatment of already acquired work after deadline/session closure remains
  GitHub's responsibility, not a successful local job result.
- The microsecond macOS process identity and Linux boot/start-tick identity
  protect against stale PIDs. Collection races are rechecked before
  quarantining a process; no matching process name alone authorizes signals.
  Detached descendants and a malicious same-user job remain outside the
  isolation guarantee.
- Template parsing and cross-compilation do not establish service drain,
  keychain access, Linux runtime, VM reboot, sleep/wake or App permissions.

The PR verification record must distinguish executed checks from the
optional live scenarios. Do not describe those scenarios as passed unless
they were actually executed on the stated host and GitHub registration.
