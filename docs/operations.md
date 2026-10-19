# Operations

## Configuration and credentials

All commands select `~/.config/ghalet/config.yaml` unless `--config` is
explicit. No working-directory or `/etc` search is performed. Relative paths
are based on the selected config's directory, not the shell's current
directory. Use absolute paths in service definitions; `~` is not expanded
inside YAML.

Configuration is immutable while a daemon is running. Stop it normally,
edit the file, and restart it to change targets, paths, limits, users,
credentials, or runner versions. Editing the file does not change the
control address: it is derived from the normalized absolute config path.
`status`, `doctor`, `drain`, and connected management commands query the
running snapshot even if the file's contents are now invalid.

`examples/config.yaml` includes repository, organization, `gh`, PAT and App
examples. Each credential is named and referenced by scopes. App installation
IDs belong to scopes, not credentials. Multiple installations can share a
single App key. Unknown keys, duplicate IDs, conflicting registrations,
invalid references, unsupported enterprise scopes, repository runner groups,
and impossible minimum capacities are rejected before any GitHub operation.

For `gh`, log in as the account that actually starts the service. ghalet runs
`gh auth token --hostname HOST` with a 15-second deadline and does not write
the token to configuration. A login in an interactive user's keychain does
not automatically grant access to a different service user or to a locked
keychain. Test the command as that user in the actual service environment.

For PATs, select exactly one of `tokenEnv` and `tokenFile`. Credential files,
including App private keys, must be owner-only regular files (normally
`chmod 600`). Values are never accepted inline in YAML. A systemd credential
can be referenced by an absolute `tokenFile` or `privateKeyFile` path under
`/run/credentials/ghalet.service/` with the corresponding `LoadCredential=`.
Do not put secrets into `Environment=` or workflow files unnecessarily.

Use repository administration permissions for repository registration and
organization self-hosted runner administration permissions for organization
registration; App installations must include the intended repositories.
Classic PAT organization management generally uses `admin:org`, while
repository administration requires access to the repository. Token type,
organization policy and GitHub host affect the exact permission requirements.
Group access policy and App installation access are separate. Create groups
and grant their repository access in GitHub first: ghalet never creates,
changes access to, or deletes runner groups.

The selected SDK supports PAT/App-based renewal of management tokens. It
does not expose a standalone one-time registration-token provider that can
reacquire credentials indefinitely. A registration-token-only mode is
therefore **not offered**; indefinite operation/restart with just such a token
has not been demonstrated. This is an SDK/support limitation, not a claim
that an alternative future protocol is impossible.

## Direct execution and environment

```sh
ghalet config validate --config /path/to/config.yaml
ghalet run --config /path/to/config.yaml
```

Jobs run as the launching user unless `runner.user` names another existing
account. Switching users requires root; root must explicitly select a
non-root account. ghalet never creates or deletes users and refuses root
jobs. Both users need appropriate directory traversal permissions. New
execution parents are traversable but state files remain owner-only.
Managed directory paths may not contain symlinks. On macOS use canonical
paths such as `/private/var/...` instead of the `/var` symlink.

The execution user's HOME, USER and LOGNAME are used. PATH and other
non-management variables are inherited and `runner.env` overrides them.
No login shell or shell initialization files are read. Standard GitHub token
variables, explicitly configured PAT variables and runner input variables
are stripped. A JIT secret is supplied only as
`ACTIONS_RUNNER_INPUT_JITCONFIG`, never a command-line argument or a state
field. Do not intentionally copy management secrets into another variable.
The runner's own files are private to its execution directory and removed
after execution.

Install build tools and the official runner's OS prerequisites on the host
before use. Docker actions, job containers and services are unsupported.
No Docker daemon, Kubernetes, VM runtime or service manager is needed for
native Linux/macOS execution. macOS ARM64/x64 and Linux ARM64/x64/ARM runner
packages are selected from the pinned version. Windows is not supported.

The pinned archive is fetched from `actions/runner` releases, checked against
the release asset's official SHA-256 digest, and checked again whenever the
cached archive is reused. Releases without an official asset digest fail
closed. `distributionRoot` stores the pinned digest and `cacheRoot` the
archive; each execution gets an independent extraction. A corrupt cache is
not silently replaced: remove the indicated bad archive, then restart.
Automatic runner updates are disabled on the explicit scale set.

## Capacity, retries and shutdown

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
retries remains faulted until a normal stop/restart; other targets continue.
Workflow results are not retried by ghalet.

SIGINT/SIGTERM starts drain for all targets. `ghalet drain --target ID`
drains only that target and leaves the daemon running. New acquisitions and
warm replenishment stop, but already assigned, unstarted jobs can still
provision runners within both limits. The drain deadline starts at the
request, not at each runner's start. The default `drainTimeout` is 10 minutes.
After that deadline, preparation is canceled and verified process groups,
including busy jobs, receive SIGTERM, followed by SIGKILL if they remain.
The controller waits for runner process groups to stop and local exit/output
observers to finish before exiting. Unknown identities are not signaled by
name or PID alone and continue to block shutdown until their groups are
confirmed absent. Their records and diagnostics remain for inspection;
absence alone does not authorize cleanup. Forced shutdown is reported as
incomplete, not success.

On native macOS, each new runner has a small foreground guard as its process
group leader. The durable launch gate also supplies controller liveness:
if the controller exits or is killed, the pipe closes and the guard sends
SIGKILL to its own group, including itself. Its live leadership pins the group
ID, so this does not depend on guessing an uncertain PID. A normal runner
exit is reported separately, followed by TERM and a two-second grace before
the final group-wide KILL. The controller uses the runner's actual process
result, not the guard's deliberate SIGKILL, for exit diagnostics. The guard
does not rewrite state or remove logs, and its descriptors are not inherited
by the runner. This is child supervision, not self-daemonization.

## Diagnosis, garbage collection and explicit destruction

```sh
ghalet status --config /path/to/config.yaml
ghalet doctor --config /path/to/config.yaml
ghalet gc --config /path/to/config.yaml --dry-run
ghalet gc --config /path/to/config.yaml
ghalet destroy --config /path/to/config.yaml --scope project --scale-set macmini
```

CLI responses are JSON. `status` returns zero when it retrieves a snapshot,
including an unhealthy one. `doctor` is nonzero for any diagnostic problem.
`drain`, `gc`, and `destroy` are nonzero for any failed/incomplete part and
include individual results. A partial management failure does not terminate
the daemon or roll back successful cleanup.

Connected GC/destruction is performed by the daemon, serialized with its
runtime state. If disconnected, direct GC/destruction must acquire the same
OS workRoot lock and the config's control-address lock. A failed socket
connection alone never permits bypassing an existing daemon. Lock files
are not PID files and may remain after a process exits.

Destruction requires a real terminal, explicit scope/name, an organization
group when applicable, and the exact answer `yes`. There is no `--yes`
escape hatch for noninteractive use. The registration ID is checked again
after confirmation and after drain. A changed target, unknown local records,
remaining registered runners, failed cleanup, forced drain or missing
statistics prevents deletion. Approval exists only in memory and is consumed
once. Groups are never deleted. Normal stop/config changes never remove old
registrations.

Scopes can remain without targets so old registrations can be explicitly
destroyed. Remove a scope only after draining/destroying registrations that
need its credentials. Retained executions matching an explicitly approved
old registration are reverified before they can participate in that
destruction; this association and deletion approval are not persisted.

After a crash, records are matched to current explicit registrations, process
UID/group/kernel start identity and, for live executions, GitHub runner IDs.
Linux identity includes the boot ID and `/proc` start ticks; macOS uses the
kernel process start time with microsecond precision. Unmatched, corrupt,
incomplete or unverifiable records reserve conservative capacity and appear
in diagnostics. Recovery of a surviving process does not recreate `Wait` or
invent its exit code. Descendants observed in the original process group can
be recovered; escaped descendants cannot be completely detected or isolated.

Empty and zombie-only process-group snapshots require kernel revalidation.
Linux checks a fresh `/proc` enumeration, retrying disappearing-PID races
without silently skipping them; macOS queries `kern.proc.pgrp`. A group is
treated as stopped only when absent or when every fresh member was already
observed as a zombie with the same kernel identity. Newly observed zombies
cannot establish safety because they may have just forked. Failed
verification retains uncertainty and capacity. State
files are limited to 1 MiB, including their final newline. Oversized saves fail
without replacing the last readable record, including when a large descendant
list cannot be persisted before signaling.

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

With `preserveLogs: true`, `output.log` and `_diag/**/*.log` are archived to
`cleanup.logRoot/runs/RUN_ID` before removing the execution. Credentials and
workspace contents are not archived. Failed archives preserve the original
logs and state. Once processes are confirmed stopped, file-only cleanup
failures do not hold execution capacity; `gc` or the next startup retries
them. `logRetention` expires only complete saved archives (also checked
hourly). There is no disk quota, disk-pressure monitor or automatic external
log shipping.

Live output capture and diagnostic-log archival limit each line to 64 KiB
(excluding the newline). An oversized line is omitted in full and replaced
with `[ghalet: log line over 64 KiB omitted]`; capture resumes at the next
newline. This deliberately favors bounded daemon memory over preserving
arbitrarily large log lines. No prefix is emitted, so a secret spanning the
limit cannot leak through a partially retained line. Ordinary lines still
support redaction and readiness detection across write boundaries.

Known management and JIT values are redacted. Nonblank lines of multiline
values, including App private keys, are also masked independently, with
shared components retained until their last registration is released.
Matches are located in the original text; overlapping matches are masked
together regardless of registration order.
The official runner is responsible for masking job secrets in its own logs.
No mechanism can mask arbitrary secrets deliberately transformed or printed
by a trusted workflow.

## Service deployment

For Linux, precreate a dedicated `ghalet` user and writable storage paths,
copy `examples/ghalet.service` into `/etc/systemd/system/`, and place the
configuration at `/etc/ghalet/config.yaml`. Set workRoot, distributionRoot,
cacheRoot and logRoot to suitable user-writable paths. Authenticate as that
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
is false so launchd also cleans up the controller's own group. Runner groups
are separate: when launchd finally kills a stuck controller, their guards
observe pipe EOF and kill their respective groups. The same mechanism handles
controller crashes and SIGKILL outside launchd. This is not cgroup isolation:
trusted jobs must not kill their guard or escape its group, and kernel/host
failure can interrupt supervision. LaunchAgents start at login, not before
login, and do not prevent sleep. For another execution user, use an
administrator-managed LaunchDaemon and set `runner.user`; do not assume a
LaunchAgent can change users. Test keychain access and shutdown behavior in
the actual login/service context.

For Apple Silicon Lima, `examples/lima.yaml` supplies an Ubuntu ARM64 VM
with VZ, 4 vCPU, 8 GiB, 80 GiB, no host mounts, no containerd, installation
and an enabled systemd service. Provisioning does not copy host credentials.
It builds the published ghalet module with Go's toolchain selection; before
the implementation is merged/published, copy a locally built Linux ARM64
binary instead. Copy a configuration explicitly with `limactl copy`, log in
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
The default same-user model also exposes controller files/processes to jobs;
file permissions, redaction and a private socket are not security boundaries
against that user. No extra sudo or Docker socket is provided and root jobs
are refused, but preexisting privileges are not stripped. Use a dedicated
account/VM and appropriate host controls.
