# Operations

[English](operations.md) | [日本語](operations.ja.md)

This guide covers configuring, running and maintaining ghalet on a single
trusted host. Start with the [README](../README.md#quick-start) for setup and
the [command overview](../README.md#commands).

- [Configuration and credentials](#configuration-and-credentials)
- [Direct execution and environment](#direct-execution-and-environment)
- [Capacity, retries and shutdown](#capacity-retries-and-shutdown)
- [Applying configuration](#applying-configuration)
- [Diagnosis and garbage collection](#diagnosis-and-garbage-collection)
- [Logs](#logs)
- [Service deployment](#service-deployment)
- [Trust boundary](#trust-boundary)

## Configuration and credentials

`run` and `config validate` select `~/.config/ghalet/config.yaml` unless
`--config` is explicit. Without `--config`, `status`, `doctor`, `apply`, and
`gc` discover accessible daemons and select one only when exactly one responds.
Use `ghalet list` to see their management UIDs, sockets and running snapshots,
then `--config PATH` to select among multiple daemons. An explicit config
never falls back to a daemon using another config. If no daemon is found,
management commands use the per-user default config; `gc` retains its locked
offline maintenance behavior. `list --config PATH` filters the list and does
not require the file to exist.

Discovery probes private Unix sockets in `/tmp/ghalet-<UID>/`, subject to
filesystem permissions. Normally only the management user's daemon is
accessible; root can also discover other users' daemons. A candidate must
respond within one second with a config path matching its socket name.
Stale, unresponsive, unrelated, symlinked and insecure socket entries are
ignored, not deleted. Responses are limited to 4 MiB. Management operations
also verify the connected peer's UID and require the daemon's config to match
the selected path. No PID or process-name guessing is used.
Restart the daemon when upgrading the CLI; older daemons reject the new
config-checked requests.

No working-directory or `/etc` config search is performed. Relative paths
are based on the selected config's directory, not the shell's current
directory. Use absolute paths in service definitions; `~` is not expanded
inside YAML.

When set, systemd's `STATE_DIRECTORY` and `CACHE_DIRECTORY` take precedence:
executions and recovery state use `$STATE_DIRECTORY/runs`, and runner archives
and digests use `$CACHE_DIRECTORY/runners`. Each variable must contain a single
absolute directory; relative paths and colon-separated lists are rejected.
These are application-specific directories, not XDG bases; see
[systemd's directory management](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#RuntimeDirectory=).

Otherwise, execution directories and recovery state use `$XDG_STATE_HOME/ghalet/runs`
(default: `~/.local/state/ghalet/runs`). Runner archives and their official
SHA-256 digests share `$XDG_CACHE_HOME/ghalet/runners`
(default: `~/.cache/ghalet/runners`). Unset, empty, or relative XDG values use
these HOME-based defaults. Paths follow the controller's environment, not
`runner.user` or the config directory. Only one daemon can own a given execution
root; separate instances need distinct state directories.
These paths are not YAML settings.

Editing the file alone changes neither the running configuration nor the
control address, which is derived from the normalized absolute config path.
Connected management commands query the running snapshot even if the file
is invalid or missing. See [applying configuration](#applying-configuration)
for reloadable settings and restart requirements.

`examples/config.yaml` includes repository, organization, `gh`, PAT and App
examples. Each target contains its registration `scope`. The optional
`credential` mapping inherits `defaults.credential` when omitted. If no shared
credential is configured, it defaults to `type: gh`. An explicit target
credential replaces the whole shared credential; an empty mapping selects
`gh` instead of inheriting. Within a credential mapping, omitted `type` means
`gh`. PAT and App credentials require an explicit
`type: pat` or `type: github-app`; credential-specific fields do not select
the type automatically. An explicitly empty type is rejected.
For GitHub.com, `scope` accepts
`OWNER/REPO` for a repository or `ORG` for an organization; these are normalized
to HTTPS URLs before validation, authentication, and registration matching.
Full HTTPS URLs are also accepted and are required for other GitHub hosts.
Only repository and organization scopes are supported; `runnerGroup` is
organization-only. Each target accepts exactly one scope. Its required `id`
identifies it in management commands and local state. The optional `scaleSet` defaults
to `id`; set it explicitly to use a different GitHub registration name
and workflow `runs-on` label, including a shared name across scopes.
IDs must be unique across all targets and contain 1-64 ASCII letters,
digits, underscores or hyphens, starting with a letter or digit.
App `installationId` belongs to the target, not its credential, so multiple
installations can share a single App key.

The [commented example](../examples/config.yaml) includes an optional
`yaml-language-server` comment selecting `schema.yaml` for editor completion
and validation. No configuration version field is needed.
Use `ghalet config validate` to also check constraints such as conflicting
registrations, minimum capacities, and managed directory safety.

For older configurations, remove `apiVersion`, move top-level `scopes` /
`credentials` into each target instead of using `scopeRef` / `credentialRef`,
and rename `scaleSetName` to `scaleSet`. These legacy keys and all other
unknown keys are rejected before any GitHub operation.

For the default `gh` authentication, install GitHub CLI where ghalet runs
and log in as the account that actually starts the service. ghalet runs
`gh auth token --hostname HOST` with a 15-second deadline and does not write
the token to configuration. A login in an interactive user's keychain does
not automatically grant access to a different service user or to a locked
keychain. Test the command as that user in the actual service environment.
Missing GitHub CLI or authentication is an error; ghalet does not fall back
to another credential type.

For PATs, select exactly one of `tokenEnv` and `tokenFile`. Credential files,
including App private keys, must be owner-only regular files (normally
`chmod 600`). Values are never accepted inline in YAML. A systemd credential
can be referenced by an absolute `tokenFile` or `privateKeyFile` path under
`/run/credentials/ghalet.service/` with the corresponding `LoadCredential=` or
`LoadCredentialEncrypted=`. For encrypted registration, shared defaults and
rotation, see [encrypted credentials](credentials.md).
Do not put secrets into `Environment=` or workflow files unnecessarily.

See [authentication setup and minimum permissions](authentication.md) for
scope-specific permissions, PAT creation and GitHub App installation.
Group access policy and App installation access are separate. Create groups
and grant their repository access in GitHub first: ghalet never creates,
changes access to, or deletes runner groups.

The SDK supports PAT/App-based management-token renewal, but does not expose
a renewable registration-token-only provider. No registration-token-only
mode is offered.

## Direct execution and environment

```sh
ghalet config validate --config /path/to/config.yaml
ghalet run --config /path/to/config.yaml
```

Jobs run as the launching user unless `runner.user` names another existing
account. Switching users requires root; root must explicitly select a
non-root account. ghalet never creates or deletes users and refuses root
jobs. The official runner must list and traverse every ancestor of its
directory, up to the filesystem root. Managed execution parents (`workRoot`,
target and run directories) are created with mode `0755`; existing `0711`
parents are upgraded when used. The Linux package also upgrades
`/var/lib/ghalet` to `0755`. Directory names are visible, but these parents
remain management-owned and are not writable by other users; state and lock
files remain owner-only. Custom ancestors outside these managed directories
are not changed: make them listable and traversable by the runner user.
Administrator-selected storage bases (`STATE_DIRECTORY`, `CACHE_DIRECTORY`,
XDG bases or their HOME fallbacks, and an explicit `logs.root`) may use
symlinks, including macOS `/var`. ghalet resolves existing base links before
deriving managed paths; new state records and credential paths use these
canonical locations. Keep the bases and their ancestors trusted and stable
while ghalet runs. After stopping the daemon and its executions, a base may
be moved with its old path left as a symlink; recovery and App setup retries
reconcile persisted paths to that canonical base, without resolving managed
children.
Moving live executions is not supported. Systemd drop-ins keep their fixed
`/etc/ghalet/credentials/...` source references, so layout migrations do not
invalidate existing directives or expose target directory names to systemd
specifier expansion. Managed children (including execution directories, runner
caches, credential directories/files and archive deletion roots) must not be
redirected through symlinks. Cleanup does not follow links out of an owned
execution or archive. Config-file parents may also use administrator layout
links, but the selected config path keeps its lexical identity for the control
socket; resolving storage bases does not change that address. Path checks are
not protection against concurrent replacement of trusted directories.

Runner startup and its access check do not inherit the controller's environment.
HOME, USER and LOGNAME are set from the execution user's account, even when
the controller and runner use different users. PATH defaults to
`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`; macOS prefixes
`/opt/homebrew/bin:/opt/homebrew/sbin`.
No login shell or shell initialization files are read.

Use `runner.env` to explicitly add environment variables or override PATH,
for example for user-installed tools, locales or proxies. Controller token
variables, SSH agent sockets, tool configuration paths and other variables
are not copied automatically. Management credentials, account identity and
runner input variables cannot be set through `runner.env`; runner input names
are checked case-insensitively. A JIT secret is supplied only as
`ACTIONS_RUNNER_INPUT_JITCONFIG`, never a command-line argument or a state
field. Do not intentionally copy management secrets into another variable.
The runner's own files are private to its execution directory and removed
after execution.

This minimizes the runner environment; the controller's environment for
authentication, API calls and downloads is unchanged. Filesystem and
keychain access remain subject to the [trust boundary](#trust-boundary).

Install build tools and the official runner's OS prerequisites on the host
before use. Docker actions, job containers and services are unsupported.
No Docker daemon, Kubernetes, VM runtime or service manager is needed for
native Linux/macOS execution. macOS ARM64/x64 and Linux ARM64/x64/ARM runner
packages are selected from the pinned version. Windows is not supported.

`runner` is global and optional. Its version defaults to the numeric version
bundled with ghalet; set `runner.version` to pin a different release.
Renovate checks official stable releases weekly and opens PRs updating the
bundled default and example together; the regular CI validates these changes.
Updating ghalet and restarting picks up the new
default unless the configuration explicitly pins a version. Startup never
resolves `latest`, and runner self-update remains disabled.

The pinned archive is fetched from `actions/runner` releases, checked against
the release asset's official SHA-256 digest, and checked again whenever the
cached archive is reused. Releases without an official asset digest fail
closed. The runner cache stores both the pinned digest and the archive;
each execution gets an independent extraction into its execution directory.
Cache preparation and verification are serialized; independent extractions can
proceed concurrently.
A corrupt cache is not silently replaced: remove the indicated bad archive,
then restart.

## Capacity, retries and shutdown

`scaling` is optional: each target defaults to `minRunners: 0` and
`maxRunners: 2`. Omitted fields retain their defaults; an explicit
`maxRunners: 0` is invalid.

The desired total is `min(maxRunners, assignedJobs + minRunners)`. Running
jobs, launches and uncertain recovered executions occupy the same shared
capacity pool. An accepted job takes precedence over another target's idle
warm runner. Slots are not reused until the previous process group is
confirmed stopped. Busy runners are not stopped merely because demand falls.

Each listener advertises at most `min(target.maxRunners, capacity.maxRunners)`,
not the instantaneous free process count, so a full warm pool does not make
other targets invisible. Acquisition is separately serialized against the
shared outstanding-job budget. Pending acquisition responses reserve budget
until the next authoritative statistics message. Scaling demand comes from
`TotalAssignedJobs`, never a count of job messages. An acknowledgment follows
a successful official Listener callback; redelivery must remain idempotent.
Cross-target fairness and distributed writers are not provided.

Preparation has a 10-minute deadline, API operations 45 seconds, authentication
commands 15 seconds, and listener polls 60 seconds. Registration/listener
failures and preparation/start failures have at most four attempts with
1/2/4-second exponential delays. Permanent invalid-request/authentication
errors stop immediately where the SDK exposes their type. Preparation
starts are rate-limited to one per second per target. A target that exhausts
retries remains faulted until `apply` or a normal stop/restart; other targets continue.
Workflow results are not retried by ghalet.

SIGINT/SIGTERM starts drain for all targets during normal shutdown.
`apply` also drains all targets before switching configuration.
Normal shutdown retains scale sets.
New acquisitions and warm replenishment stop, but already assigned,
unstarted jobs can still provision runners within both limits. The drain
deadline starts at the request, not at each runner's start. The default
`drainTimeout` is 10 minutes.
After that deadline, preparation is canceled and verified process groups,
including busy jobs, receive SIGTERM, followed by SIGKILL if they remain.
The controller waits for runner process groups to stop and local exit/output
observers to finish before exiting. Unknown identities are not signaled by
name or PID alone and continue to block shutdown until their groups are
confirmed absent. Their records and diagnostics remain for inspection;
absence alone does not authorize cleanup. Forced shutdown is reported as
incomplete, not success.

For changes requiring a restart, stop the daemon normally and
verify successful shutdown before starting it again. In the foreground,
check the exit status of `ghalet run`; under a service manager, check the
daemon's exit result and logs rather than only the stop command's result.
Do not proceed with a restart after forced or incomplete drain; inspect
diagnostics and resolve retained executions or cleanup failures first.

On Linux and macOS, each new runner has a small foreground guard as its process
group leader, whether run directly or through a service manager.
The durable launch gate also supplies controller liveness:
if the controller exits or is killed, the pipe closes and the guard sends
SIGKILL to its own group, including itself. Its live leadership pins the group
ID, so this does not depend on guessing an uncertain PID. A normal runner
exit is reported separately, followed by TERM and a two-second grace before
the final group-wide KILL. The controller uses the runner's actual process
result, not the guard's deliberate SIGKILL, for exit diagnostics. The guard
does not rewrite state or remove logs, and its descriptors are not inherited
by the runner. This is child supervision, not self-daemonization or isolation:
trusted jobs must not kill their guard or escape its group, and kernel/host
failure can interrupt supervision.

## Applying configuration

```sh
ghalet apply --config /path/to/config.yaml
# Packaged systemd service:
sudo systemctl reload ghalet
```

Edit the file and run `apply` to change targets, limits, credentials, runner
versions, environment, timeouts or log policy. It requires a running daemon
and reloads that daemon's original config path without modifying the file.
Missing or invalid configuration fails rather than falling back to another file.

`apply` waits for reconciliation. It validates the new configuration before
stopping new job acceptance, drains all targets
under the old limits/timeouts, closes their listeners, then registers targets
from the new configuration and waits for their listener initialization.
Unchanged scale sets are reused. Startup failures are reported by apply;
later listener failures remain visible through `doctor`.
The local `targets[].id` identifies additions, changes and removals; a
scope/group/scale set change replaces that target's registration. Use
`targets: []` to remove all targets. Missing or null `targets` is invalid.

Scale sets from removed or replaced targets are deleted only if no new target
references their scope and registration ID (or scope/group/name), no local
execution remains, drain and listener cleanup succeeded, and GitHub statistics
show no accepted jobs or registered runners. Runner groups are never deleted.
Do not share a managed scale set with another controller.

Failures are reported per target and return nonzero; healthy targets still
use the new configuration. There is no automatic rollback. Failed deletions
retain their old client in memory and can be retried with `apply`; forced drain,
uncertain executions and listener-close failures require inspection. Pending
deletions are not persisted across daemon restarts. After a crash, current
configuration is authoritative; unreachable old registrations may need manual
cleanup in GitHub. Retained execution records are never discarded just to free
capacity. Concurrent apply requests are rejected; shutdown interrupts apply.
If the CLI disconnects or times out, the daemon continues the request; inspect
`status` and `doctor` before retrying.

Execution-user and managed-directory changes require a normal stop/restart
and are rejected before drain.
Environment variables and systemd's decrypted credential files belong to the
running service; the provided units require a restart to refresh those inputs. Ordinary
credential files are reread when apply creates new clients.
The provided systemd units run `ghalet apply` synchronously through
`ExecReload` and allow 15 minutes via `TimeoutStartSec`; extend this reload
deadline and `TimeoutStopSec` when increasing drain timeouts or target counts.
`systemctl daemon-reload` only reloads unit definitions, not ghalet configuration.

## Diagnosis and garbage collection

```sh
ghalet status --config /path/to/config.yaml
ghalet doctor --config /path/to/config.yaml
ghalet gc --config /path/to/config.yaml --dry-run
ghalet gc --config /path/to/config.yaml
```

Management-command responses are JSON. `status` returns zero when it
retrieves a snapshot, including an unhealthy one. `doctor` is nonzero for
any diagnostic problem.
`gc` and `apply` are nonzero for any failed/incomplete part and
include individual results. A partial management failure does not terminate
the daemon or roll back successful cleanup.

Connected GC is performed by the daemon, serialized with its
runtime state. If disconnected, direct GC must acquire the same
OS workRoot lock and the config's control-address lock. A failed socket
connection alone never permits bypassing an existing daemon. Lock files
are not PID files and may remain after a process exits.
Offline GC must use the same storage environment as the service to find its state
(for example, `STATE_DIRECTORY=/var/lib/ghalet` and
`CACHE_DIRECTORY=/var/cache/ghalet` for the packaged service).

After a crash, records are matched to current explicit registrations, process
UID/group/kernel start identity and, for live executions, GitHub runner IDs.
Linux identity includes the boot ID and `/proc` start ticks; macOS uses the
kernel process start time with microsecond precision. Process liveness is checked
independently of registration and execution-user matching. Records whose processes
are verified stopped do not reserve capacity, even if a target was removed,
authentication fails, or registration/user matching prevents cleanup. Their
records, files and diagnostics remain; stopping alone does not authorize deletion.
Potentially live or unverifiable processes still reserve conservative capacity.
Malformed records also reserve capacity because they provide no trusted identity
with which to prove process absence; they are never used to authorize cleanup.
Recovery of a surviving process does not recreate `Wait` or
invent its exit code. Descendants observed in the original process group can
be recovered; escaped descendants cannot be completely detected or isolated.

Process snapshots still enumerate host processes with `ps`, but kernel start
identities are queried only for the inspected runs' recorded leader PIDs and
members of their recorded process groups, including previously unknown descendants.
A recorded leader PID is checked even if it now belongs to another group, so
PID reuse still prevents signaling or cleanup. The gated launch checks only its
new child PID before recording the identity and releasing the runner.

Empty and zombie-only process-group snapshots require kernel revalidation.
Linux checks kernel group membership before reading each enumerated PID's
`/proc` state, so a PID identified outside the group need not remain readable.
Disappearance of a known group member still triggers bounded retries.
If an unclassified PID disappears, a second enumeration must also find only
previously identified, single-threaded zombies in the group, so a child forked
before that disappearance can be discovered. This bounded check tolerates
unrelated PID churn; it is not an atomic snapshot against deliberately timed,
repeated fork/exit chains, consistent with the trusted-job model.
macOS queries `kern.proc.pgrp`. A group is
treated as stopped only when absent or when every fresh member was already
observed as a zombie with the same kernel identity. Newly observed zombies
cannot establish safety because they may have just forked. Failed
verification retains uncertainty and capacity. State
files are limited to 1 MiB, including their final newline. Oversized saves fail
without replacing the last readable record, including when a large descendant
list cannot be persisted before signaling.

State updates sync the file and its execution directory. Each newly created
managed directory syncs its immediate parent before use, including missing
storage ancestors and log directories. Unchanged ancestors are not recursively
synced to the filesystem root.

For manual recovery, inspect the named paths/processes, wait for or explicitly
stop only the identified execution, then run `gc --dry-run` and `gc`. Correct
configuration/permission issues and restart to revalidate. Do not delete
state alone to manufacture free capacity. A corrupt record requires manual
inspection of its actual execution before repairing/removing that record.

## Logs

Daemon stdout is exclusively JSONL (`time`, `level`, `msg`, optional `event`,
`scope`, `targetId`, `runId`, numeric `durationMs`, and `error`). Newlines are
escaped within a record; runner output is never mixed into stdout. Configure
your collector to select structured fields and perform any forwarding or
OTel conversion externally. There is no embedded HTTP health endpoint,
metrics exporter, OTel SDK or collector dependency.

The `scope` field in runner-start events and archive metadata is the
registration URL. Legacy state `scopeRef` fields remain readable but do not
select credentials or determine registration identity.

The `logs` settings control runner log archives, not daemon stdout/stderr.
With `logs.preserve: true`, `output.log` and `_diag/**/*.log` are archived to
`logs.root/runs/RUN_ID` before removing the execution. `logs.root`
defaults to `$STATE_DIRECTORY/logs` when `STATE_DIRECTORY` is set, otherwise
`$XDG_STATE_HOME/ghalet/logs`, or `~/.local/state/ghalet/logs` when
`XDG_STATE_HOME` is unset, empty, or relative.
An explicit `logs.root` overrides this default; relative values are
still resolved against the selected config's directory. Credentials and
workspace contents are not archived. Failed archives preserve the original
logs and state. Once processes are confirmed stopped, cleanup failures do not
hold execution capacity or indefinitely block drain completion. During normal
operation, transient network/API failures (timeouts, rate limits and recoverable
server errors) and temporary filesystem failures are retried after 5 seconds,
with exponential waits capped at 5 minutes. Authentication/permission errors,
identity mismatches, invalid state and output-collection failures require
operator attention, not automatic retries. `gc` retries immediately regardless
of the wait; the next startup also revalidates retained records. Unknown
process or registration identities never authorize cleanup.
`logs.retention` expires only complete saved archives (also checked
hourly). There is no disk quota, disk-pressure monitor or automatic external
log shipping.

Live output capture and diagnostic-log archival limit each line to 64 KiB
(excluding the newline). An oversized line is omitted in full and replaced
with `[ghalet: log line over 64 KiB omitted]`; capture resumes at the next
newline. This bounds memory without leaking a secret through a partial line.
Ordinary lines still support redaction and readiness detection across write
boundaries.

Known management and JIT values are redacted. Nonblank lines of multiline
values, including App private keys, are also masked independently, with
shared components retained until their last registration is released.
Matches are located in the original text; overlapping matches are masked
together regardless of registration order.
The official runner is responsible for masking job secrets in its own logs.
No mechanism can mask arbitrary secrets deliberately transformed or printed
by a trusted workflow.

## Service deployment

For packaged Linux installation or Docker Compose, see
[packages and containers](distribution.md). The instructions below cover
manual service installation.

For Linux, precreate a dedicated `ghalet` user,
copy `examples/ghalet.service` into `/etc/systemd/system/`, and place the
configuration at `/etc/ghalet/config.yaml`. The unit uses `StateDirectory=ghalet`
and `CacheDirectory=ghalet` to create `/var/lib/ghalet` and `/var/cache/ghalet`
for the service user and pass them through `STATE_DIRECTORY` and `CACHE_DIRECTORY`.
No XDG overrides are needed. Set `logs.root`
only if overriding the default state-based log directory. Authenticate as that
user, run configuration validation, then `systemctl enable --now ghalet`.
`KillMode=mixed` sends the initial stop signal only to the controller, allowing
it to drain jobs and terminate runner groups before exiting. The finite
`TimeoutStopSec=15min` is an independent outer deadline: if the controller
hangs or cannot verify a remaining group, systemd forcibly stops all remaining
processes in the service's cgroup, including the controller and runners.
It also kills remaining service processes if the controller crashes/exits.
This last-resort cleanup deliberately takes precedence over retaining live
runners for recovery. State and logs remain for recovery after interruption.
Keep the service deadline longer than the configured drain timeout, with
allowance for termination, API calls and cleanup; the examples allow five
additional minutes. Host shutdown/power loss can still interrupt jobs.

For native macOS, customize absolute paths in
`examples/com.github.ghalet.plist`, create the log directory, and place the
file in `~/Library/LaunchAgents/`. Use `launchctl bootstrap gui/$(id -u) FILE`.
The 900-second service stop limit exceeds the default 10-minute drain by five
minutes; increase it if changing the configured timeout. Zero would mean an
infinite launchd stop wait and is deliberately avoided. `AbandonProcessGroup`
is false so launchd also cleans up the controller's own group. Separate runner
groups use the [guards described above](#capacity-retries-and-shutdown).
LaunchAgents start at login, not before login, and do not prevent sleep.
For another execution user, use an
administrator-managed LaunchDaemon and set `runner.user`; do not assume a
LaunchAgent can change users. Test keychain access and shutdown behavior in
the actual login/service context.

For Apple Silicon Lima, `examples/lima.yaml` supplies an Ubuntu 26.04 ARM64 VM
with VZ, 4 vCPU, 8 GiB, 80 GiB, no host mounts, no containerd, installation
and an enabled systemd service. Provisioning does not copy host credentials.
It builds the published ghalet module with Go's toolchain selection.
To test unpublished changes, copy a locally built Linux ARM64 binary instead.
Copy a configuration explicitly with `limactl copy`, log in
as the guest `ghalet` user or supply guest-only credentials, and start the
service. The service is conditionally skipped until configuration exists.

`examples/com.github.ghalet-lima.plist` starts the already-created VM at
login; customize the limactl path and instance name. It does not restart the
VM per job or promise operation while the Mac sleeps/logs out. Inspect
`limactl list` and guest mount output to confirm no macOS host shares are
exposed. VM lifecycle, sleep/wake and power-loss tests are operator
acceptance checks, not established by template validation.

## Trust boundary

**Ephemeral registration is not isolation.** Use only trusted workflows in
trusted repositories, never hostile fork PRs. Each runner has separate
execution/work paths but shares an OS, execution user's HOME, caches, tools,
network and existing privileges. Jobs can modify that shared environment.
They may access the execution user's saved credentials, including GitHub CLI
and keychain credentials; stripping token variables or supplying a job-scoped
`GH_TOKEN` does not isolate that storage. Use a separate execution user to
separate those credentials.
The default same-user model also exposes controller files/processes to jobs;
file permissions, redaction and a private socket are not security boundaries
against that user. No extra sudo or Docker socket is provided and root jobs
are refused, but preexisting privileges are not stripped. Use a dedicated
account/VM and appropriate host controls.
