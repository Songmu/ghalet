# ghalet

[![Test Status](https://github.com/Songmu/ghalet/actions/workflows/test.yaml/badge.svg?branch=main)][actions]
[![Coverage Status](https://codecov.io/gh/Songmu/ghalet/branch/main/graph/badge.svg)][codecov]
[![MIT License](https://img.shields.io/github/license/Songmu/ghalet)][license]
[![PkgGoDev](https://pkg.go.dev/badge/github.com/Songmu/ghalet)][PkgGoDev]
[![Ask DeepWiki](https://img.shields.io/badge/Ask-DeepWiki-blue)][deepwiki]

[actions]: https://github.com/Songmu/ghalet/actions?workflow=test
[codecov]: https://codecov.io/gh/Songmu/ghalet
[license]: https://github.com/Songmu/ghalet/blob/main/LICENSE
[PkgGoDev]: https://pkg.go.dev/github.com/Songmu/ghalet
[deepwiki]: https://deepwiki.com/Songmu/ghalet

[English](README.md) | [日本語](README.ja.md)

ghalet turns a Linux or macOS host into an automatically scaling
self-hosted GitHub Actions runner environment. Put a spare Mac, a dedicated
Linux machine, or an always-on VM to work for your trusted repositories,
without setting up Kubernetes, Docker, or a VM for every job.

It is a Go daemon that manages GitHub Actions Runner Scale Sets and starts
native runner processes as jobs arrive. Each runner handles one job, then
exits; ghalet cleans up its runner directory and workspace. Several
repositories or organizations can share the host with per-target and
host-wide concurrency limits.

## Use cases

- **Use a spare Mac for macOS builds.** Run jobs directly on macOS with
  tools already installed on the machine.
- **Share a Linux host across trusted projects.** Register several
  repository or organization targets and let ghalet start runners on
  demand within a shared capacity limit.
- **Keep the runner setup small and disposable.** Run one daemon on a
  dedicated account or an always-on VM, including a Lima VM on macOS.
  Configuration defines the setup; small local state files support recovery
  without a separate database.

ghalet scales **runner processes on one host**, not the hosts themselves.
It does not orchestrate multiple machines or provision per-job VMs or
containers. **Ephemeral registration is not job isolation:** jobs share the
execution user's HOME, tools, caches and privileges. Use only trusted
workflows, not untrusted fork PRs or hostile code. Docker actions, job
containers and service containers are unsupported.

## Installation

Linux `.deb`/`.rpm` packages with a systemd unit and a GHCR container image
are also built for releases. See [packages and containers](docs/distribution.md)
for installation, configuration, and updates.

```console
# Install the latest version into ./bin/ by default.
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s

# Specify the installation directory and version.
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s -- -b $(go env GOPATH)/bin [vX.Y.Z]

# On Alpine Linux without curl.
% wget -O - -q https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s [vX.Y.Z]

# Or install with Go.
% go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

Add the installation directory to your `PATH`. Install the build tools your
workflows need and the official runner's OS prerequisites on the host.
Run ghalet as a non-root user; root jobs are
refused. See [operations](docs/operations.md#direct-execution-and-environment)
for execution users and environment settings.

## Quick start

Create a configuration and authenticate with GitHub CLI as the user who
will run ghalet. Replace `OWNER/REPO` below with your repository
(or `ORG` for an organization):

```sh
mkdir -p ~/.config/ghalet
cat > ~/.config/ghalet/config.yaml <<'YAML'
targets:
  - id: project
    scope: OWNER/REPO
YAML
gh auth login
ghalet config validate
ghalet run
```

The default configuration path is `~/.config/ghalet/config.yaml`; use
`--config /path/to/config.yaml` to select another file. The default
authentication uses your `gh` login; [PAT and GitHub App
credentials](docs/operations.md#configuration-and-credentials) are also
supported. See [authentication setup](docs/authentication.md) for the required
permissions, PAT creation and GitHub App installation.
For more settings, see the [commented example](examples/config.yaml).

Use the configured scale set name (`scaleSet`, or `id` when omitted) in a
trusted workflow:

```yaml
jobs:
  build:
    runs-on: project
    steps:
      - run: uname -a
```

In another terminal:

```sh
ghalet status
ghalet doctor
```

To stop the daemon, press Ctrl-C in its terminal. Normal shutdown stops new
job acceptance for all targets and waits for accepted work, using the
configured drain deadline. After that deadline, remaining jobs may be
forcibly stopped. After editing configuration, use `ghalet apply` (or
`sudo systemctl reload ghalet` for the packaged service) to drain and
reconfigure without exiting the daemon.

## Commands

All commands accept `--config PATH`. A **target** is one configured
repository or organization registration, identified locally by its `id`.

| Command | Purpose |
| --- | --- |
| `ghalet run` | Run the daemon in the foreground for all configured targets. |
| `ghalet apply` | Drain targets, reload configuration, and reconcile registrations, attempting to delete unused scale sets. |
| `ghalet config validate` | Validate configuration and local paths without contacting GitHub. |
| `ghalet app setup --scope OWNER/REPO` | Create and install a private GitHub App, deriving the target ID from scope and creating or updating its config. See [App setup](docs/app-setup.md) for organization scopes, SSH and resumption. |
| `ghalet systemd app setup --scope OWNER/REPO` | Set up an App with encrypted service credentials and the target's runtime key reference (Linux/root). See [App setup](docs/app-setup.md#packaged-systemd-service). |
| `ghalet credential set NAME --prompt` | Save an owner-only credential file and update `defaults.credential`. See [ordinary credentials](docs/file-credentials.md) for App keys and replacement. |
| `ghalet systemd credential set NAME --prompt` | Register an encrypted service PAT and update `defaults.credential` (Linux/root). See [credentials](docs/credentials.md) for App keys, automation and rotation. |
| `ghalet list` | List accessible daemons, management UIDs, sockets and running snapshots; filter with `--config PATH`. |
| `ghalet status` | Read state and capacity; a successful response does not imply health. |
| `ghalet doctor` | Diagnose the daemon; exit nonzero for any problem. |
| `ghalet gc [--dry-run]` | Clean verified stopped executions and expired logs; preview with `--dry-run`. |

`ghalet --version` prints the installed version. Management responses are
JSON, except for the App/credential setup instructions. `systemd app setup` and `systemd credential set`
defaults to `/etc/ghalet/config.yaml`. Without `--config`, `status`, `doctor`,
`apply`, and `gc` automatically select the single accessible daemon; multiple
daemons require an explicit config. With none found, the per-user default
`~/.config/ghalet/config.yaml` is used, as it is for `run` and `config validate`.
See [configuration apply](docs/operations.md#applying-configuration) and
[diagnosis and cleanup](docs/operations.md#diagnosis-and-garbage-collection)
for exit codes, offline maintenance and deletion safeguards.

## Runtime behavior

Host capacity defaults to two, with per-target limits and optional idle warm
pools. Assigned jobs take priority. Atomic state files and kernel process
identities support crash recovery; unverifiable executions retain capacity.

Runner archives are automatically downloaded for the pinned version and
verified against official SHA-256 digests. Daemon stdout is JSONL; runner logs
are archived before cleanup by default. See [operations](docs/operations.md)
for retries, recovery and log policy.

## Documentation

- [Authentication setup](docs/authentication.md): method tradeoffs, minimum
  permissions, creation steps and credential rotation for PATs and GitHub Apps.
- [Operations](docs/operations.md): authentication, configuration, execution
  environment, recovery, logs and systemd/launchd/Lima deployment.
- [Validation](docs/validation.md): automated tests, opt-in live
  acceptance tests and what those tests do not establish.
- [Example configuration](examples/config.yaml) and [configuration schema](schema.yaml).

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/ghalet
```

After changing GitHub Actions dependencies, run `gh actions-lock` and commit
`.github/workflows/actions.lock` with the workflow changes. Install the tool
with `gh extension install github/gh-actions-lock`.

## Author

[Songmu](https://github.com/Songmu)
