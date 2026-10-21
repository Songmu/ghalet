# Validation

[日本語](validation.ja.md)

How to run automated tests and opt-in checks against the official runner
and GitHub. For setup and deployment, see [getting started](getting-started.md) and
[operations](operations.md).

## Automated tests

```sh
go test -race ./...
go vet ./...
```

CI runs the suite on Linux and macOS without real GitHub registrations.
Run as a non-root user: process tests skip under root.

Linux fixtures cover unrelated PIDs disappearing after membership is established,
bounded confirmation of unclassified PID churn and its late-forked descendants,
ambiguous disappearance of known group members, omitted live descendants,
zombie leaders with worker threads, newly observed zombies and reused PIDs.
The incomplete-snapshot process tests exercise surviving forked descendants.
Cross-compilation alone does not validate these Linux runtime checks.

The `test-packaging.yaml` workflow builds amd64/arm64 test archives and deb/rpm packages,
checks installation, reinstallation and removal on Ubuntu 26.04 hosts and
in Rocky Linux 9 containers, and builds the Docker image without publishing it.
On Ubuntu it verifies the installed unit and that package operations leave
the service inactive.
It validates the packaged configuration and runs the optional official
runner smoke test inside the image. These checks do not exercise a live
GitHub job or a systemd-managed drain.
CI fetches the official runner digest with authenticated GitHub API access
on the host, then mounts it read-only for `GHALET_TEST_RUNNER_DIGEST_FILE`.
Archive verification still runs inside the container; no API token is passed in.

## Optional official runner smoke test

```sh
GHALET_TEST_OFFICIAL_RUNNER=1 \
  go test -run '^TestOfficialRunnerJITInputSmoke$' -v
```

This downloads and verifies the pinned official runner, then checks that
invalid JIT input is masked in output and diagnostic logs. It does not
register a runner or execute a job. Install the runner's OS prerequisites
and allow outbound HTTPS.
When run as root on a disposable host/container, it launches the runner as
`nobody`, reproduces the legacy `0711` parent error, and verifies startup
passes that check after migration. `TestExecutionDirectoryAccessAsRunner`
also runs as root to verify cross-UID directory access and private state/lock
files without downloading a runner.

## Explicit live acceptance

This test runs real jobs on GitHub. Use a disposable trusted host and a
dedicated repository/scale set, with no existing managed executions.

1. Install [the integration workflow](../.github/workflows/ghalet-integration.yaml)
   in the repository's default branch.
2. Prepare a configuration with exactly one repository target,
   `minRunners: 0` and `logs.preserve: true`.
3. Supply credentials that can administer runners and dispatch/read the
   integration workflow.

```sh
GHALET_INTEGRATION_CONFIG=/absolute/path/to/dedicated/config.yaml \
  go test -run '^TestGitHubEphemeralLifecycle$' -timeout 20m -v
```

Optionally set `GHALET_INTEGRATION_REF` to the workflow revision to dispatch.
The test creates/reuses the scale set, runs two sequential jobs, checks
separate execution logs, and drains the daemon with a 15-second deadline.
It **leaves the scale set registered** and stops its daemon. Afterwards,
delete the dedicated scale set manually in GitHub. During normal operation,
removing a target and running [`ghalet apply`](operations.md#applying-configuration)
attempts to delete its unused scale set before losing the old runtime context.

## Verification limits

Automated tests use fake APIs; they do not establish GitHub's server-side
assignment behavior or job isolation. Check organization/App permissions,
multi-target scheduling, service shutdown, reboot and sleep/wake in the
actual deployment environment. See the [trust boundary](operations.md#trust-boundary).

When reporting results, distinguish executed checks from scenarios that
were not run.
