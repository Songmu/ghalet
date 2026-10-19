# Validation

[English](validation.md) | [日本語](validation.ja.md)

How to run automated tests and opt-in checks against the official runner
and GitHub. For setup and deployment, see the [README](../README.md) and
[operations](operations.md).

## Automated tests

```sh
go test -race ./...
go vet ./...
```

CI runs the suite on Linux and macOS without real GitHub registrations.
Run as a non-root user: process tests skip under root.

The packaging workflow builds amd64/arm64 archives and deb/rpm packages,
checks installation, reinstallation and removal on Ubuntu 24.04 hosts and
in Rocky Linux 9 containers, and builds the Docker image without publishing it.
On Ubuntu it verifies the installed unit and that package operations leave
the service inactive.
It validates the packaged configuration and runs the optional official
runner smoke test inside the image. These checks do not exercise a live
GitHub job or a systemd-managed drain.

## Optional official runner smoke test

```sh
GHALET_TEST_OFFICIAL_RUNNER=1 \
  go test -run '^TestOfficialRunnerJITInputSmoke$' -v
```

This downloads and verifies the pinned official runner, then checks that
invalid JIT input is masked in output and diagnostic logs. It does not
register a runner or execute a job. Install the runner's OS prerequisites
and allow outbound HTTPS.

## Explicit live acceptance

This test runs real jobs on GitHub. Use a disposable trusted host and a
dedicated repository/scale set, with no existing managed executions.

1. Install [the integration workflow](../.github/workflows/ghalet-integration.yaml)
   in the repository's default branch.
2. Prepare a configuration with exactly one repository target,
   `minRunners: 0` and `preserveLogs: true`.
3. Supply credentials that can administer runners and dispatch/read the
   integration workflow.

```sh
GHALET_INTEGRATION_CONFIG=/absolute/path/to/dedicated/config.yaml \
  go test -run '^TestGitHubEphemeralLifecycle$' -timeout 20m -v
```

Optionally set `GHALET_INTEGRATION_REF` to the workflow revision to dispatch.
The test creates/reuses the scale set, runs two sequential jobs, checks
separate execution logs, and drains the daemon with a 15-second deadline.
It **leaves the scale set registered**. Afterwards, use
[`ghalet destroy`](operations.md#diagnosis-garbage-collection-and-explicit-destruction)
with interactive confirmation to delete it.

## Verification limits

Automated tests use fake APIs; they do not establish GitHub's server-side
assignment behavior or job isolation. Check organization/App permissions,
multi-target scheduling, service shutdown, reboot and sleep/wake in the
actual deployment environment. See the [trust boundary](operations.md#trust-boundary).

When reporting results, distinguish executed checks from scenarios that
were not run.
