# Validation and API decisions

## Pinned interfaces

The implementation pins `actions/scaleset` at
`e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5`
(`v0.4.1-0.20260916214619-e6daac702355`). Its `listener.Scaler.Scale`
callback gives the controller explicit acquisition control and authoritative
statistics before the official Listener acknowledges a message. The v0.4.0
tag has a different callback contract; selecting that tag is not an
interchangeable dependency downgrade.

The example and optional runner smoke test pin `actions/runner` 2.338.0.
Source inspection of that tag establishes:

- `CommandSettings` reads `ACTIONS_RUNNER_INPUT_*`, removes input environment
  variables and registers secret arguments with the masker.
- JIT is a standard secret input.
- `Runner.Listener run` handles JIT configuration without a shell wrapper that
  exposes it in arguments.
- The listener prints `Listening for Jobs` after session establishment.

During a live launch that stdout readiness line, or a matching SDK JobStarted
event, ends the startup clock. Readiness is not recovered by parsing old
logs. Restarts use persisted state and OS/API identities. The official
`DisableUpdate` scale set setting disables runner self-update; ghalet does
not rewrite the JIT payload.

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
| Strict declarative config, defaults and config-relative paths | Config/CLI invalid-input tables |
| Version queries never enter daemon startup regardless of common-flag placement | Mixed pre/post-command flags, valid/missing config and untouched execution-directory regressions |
| Day durations reject mixed units; enterprise scope rejection is case-insensitive | Duration and scope URL regression tables |
| Named PAT/gh/App settings, protected files, environment filtering and JSONL secret redaction | Credential, gh subprocess and environment tests |
| Runner input environment overrides are blocked case-insensitively | Uppercase/lowercase/mixed-case config rejection and inherited-environment filtering regressions |
| Multiline secrets are masked without cross-line buffering | Split-write LF/CRLF live/archive regressions, final partial lines and shared-component/JIT reference release |
| Overlapping secrets are masked independently of registration order | Original-input union masking for prefixes, partial/chained/self overlaps, structured logs and split-write live/archive capture |
| Explicit registration/group lookup, reuse and update disablement | Fake API request/counter tests |
| Digest validation, independent extraction, bad-cache refusal and traversal rejection | Archive/digest/HTTP fixtures |
| SDK acknowledgment/redelivery and shared acquisition reservations | Official Listener + scripted session tests |
| Reservations accumulate across statistics-free messages, including ambiguous acquisitions | Successive-message acquisition regressions with authoritative-statistics reset |
| One job per directory, cleanup/log archives, no idle supply at minimum zero | Real process lifecycle tests |
| Shared capacity, warm-slot yield, busy protection and deletion refusal races | Multi-target real process tests |
| Accepted backlog during drain, startup deadline and forced drain reporting | Lifecycle timeout tests |
| Atomic state, OS locking, malformed/unknown/PID-reused state | Filesystem and identity tests |
| State writes never replace a readable record with an oversized one | Large member-list rejection, exact 1 MiB read boundary and over-limit trailing-data regressions |
| Empty and zombie-only process snapshots cannot conceal live descendants | Real forked descendants with reaped or zombie leaders and stale ps output; stop/recovery/GC retain capacity and state; native kernel checks accept genuine zombie-only groups |
| Linux kernel group verification rejects incomplete enumerations | Vanished-PID, omitted-live-child and newly observed zombie fixtures, including a zombie leader with live worker threads |
| Invalid nonzero process IDs never authorize cleanup | Negative/reserved PID checks and malformed-record recovery/GC retention |
| Process commands retain consecutive spaces; exited PIDs are absent rather than unverifiable | Real managed-path drain and reaped-PID kernel lookup regressions |
| Monitoring after the original process parent exits | Subprocess helper exits; another controller recovers and stops the surviving group |
| Shutdown waits for uncertain groups and local exit/output observation without signaling unverified processes | Live-process shutdown, uncertain-group disappearance and delayed-observer regressions |
| Native runner guards reclaim their own groups on controller death | Real controller SIGKILL with a TERM-resistant runner/descendant, unrelated-process survival and preserved recovery state |
| Guards preserve actual runner exit results and prevent execution before the durable gate | Successful/failed runner exit, descriptor closure, final output, closed-gate and shared-group refusal regressions |
| Recovered busy runs release capacity after confirmed exit | Missed-completion recovery regression |
| Runner exit is observed independently of descendant-held output pipes | Real subprocess/FIFO test with an observed descendant and a final unterminated log line |
| Termination persists newly verified descendants before signaling and refreshes membership during shutdown | SIGTERM-resistant subprocess tests for children created before/during termination, plus a failed-save no-signal test |
| Readiness-dependent exit classification waits for collected output | Deterministic exit-before-readiness tests, including real failures and intentional stops |
| Completed target drains do not become forced after their deadline | Drain followed by delayed reconciliation and confirmed destruction |
| File-only cleanup and archive failures, retry and log retention | Fault injection and retained-file assertions |
| Log processing is bounded by omitting lines over 64 KiB in full | Live/archive large single-write and chunked unterminated lines, boundary secrets, split readiness and capture resumption |
| Archive synchronization and collection completion precede source deletion | Nested diagnostic archives, failed existing-archive retries, pending-output GC and output-error tests; not a power-loss simulation |
| State durability includes newly created directory ancestry | Unreadable-ancestor failure after state rename, followed by a successful retry; not a power-loss simulation |
| Retention rejects symlinked ancestry without creating missing directories | Symlinks at runs/logRoot/ancestor levels, dry-run and actual retention |
| Failed destruction cannot publish a provisioning target or partially adopt executions | Invalid-user, missing-statistics and adoption-save failure regressions |
| Adopted executions report cleanup errors under their runtime target and block deletion | Old-target archive failure followed by an explicitly confirmed destruction attempt |
| Destruction adoption never invents local exit/output completion | Pending-exit/output rejection, unchanged uncertainty, resumed adoption after both observers finish and cleanup-attempt gating |
| Config edits preserve control address and running snapshot | Unix socket CLI tests |
| Partial GC, status/doctor exit behavior and confirmation refusal | Control/CLI tests |

Real process tests skip under root rather than accidentally running root
jobs. Run them as a non-root user. Tests never automatically delete real
GitHub registrations or run untrusted workflows.

## Optional official runner smoke test

```sh
GHALET_TEST_OFFICIAL_RUNNER=1 \
  go test -run '^TestOfficialRunnerJITInputSmoke$' -v
```

This downloads the official pinned archive, verifies its release digest,
starts `Runner.Listener run` with invalid JIT input and verifies that the
encoded input is masked in output/diagnostic logs. It does **not** register a
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

The initial implementation was developed with Go 1.27.2 on macOS ARM64.
The following local checks were executed on October 19, 2026 (JST):
race-enabled unit/subprocess tests, vet, current staticcheck and actionlint,
native macOS ARM64 plus Linux AMD64/ARM64 and macOS AMD64 builds, both plist
parsers, Lima template validation and the official runner 2.338.0 JIT-input
smoke test. The latter downloaded the official macOS ARM64 archive and
passed digest verification and encoded-input masking.

The PR verification record must distinguish executed checks from the
optional live scenarios. Do not describe those scenarios as passed unless
they were actually executed on the stated host and GitHub registration.

Automatic latest-runner tracking, extra executors, hot configuration,
fairness/priorities and other explicitly deferred design phases are not
implemented. There is no hidden fallback to containers, group management,
health servers or persistent message/job databases.
