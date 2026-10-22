# Runtime design

[日本語](design.ja.md)

Implementation details and invariants for changes to ghalet's runtime.
For configuration, recovery procedures, and operational guarantees, see
[operations](../docs/operations.md). For package-local process APIs, see
[`procgroup`](../procgroup/README.md).

## Scheduling and listener protocol

Each listener advertises at most `min(target.maxRunners, capacity.maxRunners)`,
not the instantaneous free process count, so a full warm pool does not make
other targets invisible. Acquisition is separately serialized against the
shared outstanding-job budget. Pending acquisition responses reserve budget
until the next authoritative statistics message. Scaling demand comes from
`TotalAssignedJobs`, never a count of job messages. An acknowledgment follows
a successful official Listener callback; redelivery must remain idempotent.
Cross-target fairness and distributed writers are not provided.

Slow GitHub requests, runner preparation, process stopping, and cleanup do not
hold the daemon's shared status/scheduling lock. Other targets can continue
scheduling, and `status`/`doctor` return the last coherent execution snapshot.
Reservations remain visible until preparation or cleanup results are published.
Drain and apply wait for execution operations to finish; `gc` waits for the
cleanup it reports rather than returning merely queued work.

## Runner supervision

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
by the runner.

This is child supervision, not self-daemonization or isolation: trusted jobs
must not kill their guard or escape its group, and kernel/host failure can
interrupt supervision.

## Process inspection and recovery

Process snapshots enumerate host processes with `ps`, but kernel start identities
are queried only for the inspected runs' recorded leader PIDs and members of
their recorded process groups, including previously unknown descendants.
A recorded leader PID is checked even if it now belongs to another group, so
PID reuse still prevents signaling or cleanup. The gated launch checks only its
new child PID before recording the identity and releasing the runner.
Recovery of a surviving process does not recreate `Wait` or invent its exit code.

Empty and zombie-only process-group snapshots require kernel revalidation.
Linux checks kernel group membership before reading each enumerated PID's
`/proc` state, so a PID identified outside the group need not remain readable.
Disappearance of a known group member still triggers bounded retries.
If an unclassified PID disappears, a second enumeration must also find only
previously identified, single-threaded zombies in the group, so a child forked
before that disappearance can be discovered. This bounded check tolerates
unrelated PID churn; it is not an atomic snapshot against deliberately timed,
repeated fork/exit chains, consistent with the trusted-job model.
macOS queries `kern.proc.pgrp`. A group is treated as stopped only when absent
or when every fresh member was already observed as a zombie with the same
kernel identity. Newly observed zombies cannot establish safety because they
may have just forked. Failed verification retains uncertainty and capacity.

## State persistence

State files are limited to 1 MiB, including their final newline. Oversized saves
fail without replacing the last readable record, including when a large
descendant list cannot be persisted before signaling.

State updates sync the file and its execution directory. Each newly created
managed directory syncs its immediate parent before use, including missing
storage ancestors and log directories. Unchanged ancestors are not recursively
synced to the filesystem root.

## Output redaction

Live output capture and diagnostic-log archival limit each line to 64 KiB,
excluding the newline. Oversized lines are omitted in full rather than emitting
a partial line that could leak a secret. Ordinary lines support redaction and
readiness detection across write boundaries.

Known management and JIT values are redacted. Nonblank lines of multiline
values, including App private keys, are also masked independently, with shared
components retained until their last registration is released.
Matches are located in the original text; overlapping matches are masked
together regardless of registration order.

## Authentication provider constraints

The SDK supports PAT/App-based management-token renewal, but does not expose a
renewable registration-token-only provider. No registration-token-only mode
is offered.
