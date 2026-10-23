# Process group verification

[日本語](./README.ja.md)

`procgroup` owns process identity, scoped snapshots, group inspection, and
termination on Linux and macOS. It does not depend on daemon run state,
configuration, execution-user resolution, or control-socket peer authentication.

- `Snapshot` retains the recorded leader PID and all processes in its group.
- `Inspect` verifies kernel birth identities, user, leader command, and previously
  observed members. Missing leaders and zombie-only snapshots are not by
  themselves evidence that a group stopped.
- `Stop` persists verified members through its single callback **before signaling**,
  sends TERM, polls during the grace period, then uses KILL if still necessary.
  Cancellation and inspection or persistence errors stop the operation.

The caller projects lifecycle state into `Group`: `AllowUnlaunched` permits an
absent identity only before launch; `Done` records completed execution.
`Command` is the expected leader command substring. The caller must save members
received by the `Stop` callback durably before returning success.

`ProcessIdentity` keeps the existing `pid`, `pgid`, `uid`, and `started` JSON fields
so persisted recovery records retain their format.
