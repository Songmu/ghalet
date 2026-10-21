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

[日本語](README.ja.md)

**A lightweight, simple supervisor for GitHub Actions self-hosted runners**

- **Runs as a single Go daemon with a configuration file**
  - No dependencies on components such as Kubernetes or databases
  - A small footprint and minimal stored state make setup and rebuilding easy
- **Scales runners on demand**
  - Runners run as lightweight native processes on the host
  - A configurable execution user provides basic privilege separation
- **Manages multiple repositories and organizations together**
  - Share one host across multiple projects
  - Fine-grained concurrency control through global and per-target scaling settings

By design, ghalet does not provide strict isolation between runners. It is
intended for standalone, dedicated hosts, such as:

- High-performance dedicated build machines
- Single-purpose VMs
- Home servers, such as a Mac mini or a spare laptop

ghalet manages GitHub Actions Runner Scale Sets on Linux and macOS.
Each runner handles one job, then exits; ghalet cleans up its runner directory
and workspace. It scales **runner processes on one host**, not the hosts themselves.
It does not orchestrate multiple machines or provision per-job VMs or
containers. **Ephemeral registration is not job isolation:** jobs share the
execution user's HOME, tools, caches and privileges. Use only trusted
workflows, not untrusted fork PRs or hostile code. Docker actions, job
containers and service containers are unsupported.

## Installation

Linux `.deb`/`.rpm` packages with a systemd unit and a GHCR container image
are also built for releases. See [packages and containers](docs/distribution.md)
for installation, configuration, and updates.

Install with Homebrew:

```sh
brew install Songmu/tap/ghalet
```

Or install with Go:

```sh
go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

Add the installation directory to your `PATH`. Install the build tools your
workflows need and the official runner's OS prerequisites on the host.
Run ghalet as a non-root user; root jobs are
refused. See [operations](docs/operations.md#direct-execution-and-environment)
for execution users and environment settings.

## Quick start

These steps run a runner for one repository.
Run the host commands as the non-root user who will run ghalet.

For ongoing operation, run ghalet as a service under systemd on Linux or
launchd on macOS (`brew services` for Homebrew installations).
The steps below first verify foreground operation before switching to a service.
See [the packaged service setup](docs/distribution.md#debian-and-rpm-packages)
for Linux or [the Homebrew service](#homebrew-service-macos-only) for macOS.

### 1. Authenticate with GitHub

Install GitHub CLI (`gh`) and log in with an account that can manage runners
for the repository. See [authentication setup](docs/authentication.md) for
the required permissions.

```sh
gh auth login
```

This guide uses your `gh` login. For PAT or GitHub App authentication,
see [authentication setup](docs/authentication.md).

### 2. Configure the runner

Create `~/.config/ghalet/config.yaml`.
Replace `OWNER/REPO` with your repository, or use `ORG` to make runners
available to the organization.

```sh
mkdir -p ~/.config/ghalet
cat > ~/.config/ghalet/config.yaml <<'YAML'
targets:
  - scope: OWNER/REPO
    scaleSet: ghalet-runner
YAML
ghalet config validate
```

`scope` is the repository or organization where runners are registered.
`scaleSet` is a name you choose and use in the workflow's `runs-on`.
This example uses `ghalet-runner` in both places; it does not need to match
the repository name. To use another configuration file, pass
`--config /path/to/config.yaml` to each command.
For more settings, see the [commented example](examples/config.yaml).

### 3. Use the runner in a workflow

Create `.github/workflows/runner-check.yaml` in the repository.
Set `runs-on` to `ghalet-runner`, matching the `scaleSet` above.
Use only trusted workflows.

```yaml
name: Runner check
on: push

jobs:
  check:
    runs-on: ghalet-runner
    steps:
      - run: uname -a
```

### 4. Start ghalet and run a job

Start the daemon on the host. It starts runners automatically as jobs arrive.

```sh
ghalet run
```

Commit and push the workflow to the repository, then check the Actions tab
on GitHub for the `Runner check` run.
In another terminal on the host, check the daemon's status and diagnostics:

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

### Homebrew service (macOS only)

If you installed with Homebrew, after setting up configuration and authentication,
use `brew services` instead of `ghalet run` to run in the background.
If the foreground daemon is already running, stop it with Ctrl-C first.
Run these commands without `sudo`, as the same user who configured ghalet and
authentication. The service uses the default configuration file,
`~/.config/ghalet/config.yaml`.

```sh
# Start now and enable automatic startup at login.
brew services start ghalet

# Stop and disable automatic startup at login.
brew services stop ghalet
```

The service does not start before login or prevent the host from sleeping.

## Commands

All commands accept `--config PATH`. A **target** is one configured
repository or organization registration, identified locally by its `id`.

| Command | Purpose |
| --- | --- |
| `ghalet run` | Run the daemon in the foreground for all configured targets. |
| `ghalet apply` | Drain targets, reload configuration, and reconcile registrations, attempting to delete unused scale sets. |
| `ghalet config validate` | Validate configuration and local paths without contacting GitHub. |
| `ghalet app setup --scope OWNER/REPO --scale-set ghalet-runner` | Create and install a private GitHub App, save managed authentication, and print any required static target configuration. See [App setup](docs/app-setup.md) for organization scopes, SSH and resumption. |
| `ghalet systemd app setup --scope OWNER/REPO --scale-set ghalet-runner` | Set up an App with encrypted service credentials and the target's runtime key reference (Linux/root). See [App setup](docs/app-setup.md#packaged-systemd-service). |
| `ghalet credential set NAME --prompt` | Save an owner-only credential file and update managed defaults without rewriting static config. See [ordinary credentials](docs/file-credentials.md) for App keys and replacement. |
| `ghalet systemd credential set NAME --prompt` | Register an encrypted service PAT and update managed defaults (Linux/root). See [credentials](docs/credentials.md) for App keys, automation and rotation. |
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
