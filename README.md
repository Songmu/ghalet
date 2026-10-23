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

[日本語](./README.ja.md)

**A lightweight, simple controller for GitHub Actions self-hosted runners**

- **Runs as a single Go daemon with a configuration file**
  - No dependencies on components such as Kubernetes or databases
  - A small footprint and minimal stored state make setup and rebuilding easy
- **Scales runners on demand**
  - Runners run as lightweight native processes on the host
  - A configurable execution user provides basic privilege separation
- **Manages multiple repositories and organizations together**
  - Share one host across multiple projects
  - Fine-grained concurrency control through global and per-target scaling settings

![One ghalet daemon starts native runners on demand within host-wide and per-target limits](./docs/images/ghalet-overview.png)

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
are also built for releases. See [packages and containers](./docs/distribution.md)
for installation, configuration, and updates.

Install with Homebrew:

```sh
brew install Songmu/tap/ghalet
```

Or install with Go:

```sh
go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

See [getting started](./docs/getting-started.md#installation) for host prerequisites
and execution user requirements.

## Quick start

Follow [getting started](./docs/getting-started.md#quick-start) for authentication,
configuration, a complete workflow, and your first job. It also covers
[macOS service setup with Homebrew](./docs/getting-started.md#homebrew-service-macos-only).
For ongoing operation, use systemd on Linux or launchd on macOS.

## Commands

All commands accept `--config PATH`. A **target** is one configured
repository or organization registration, identified locally by its `id`.

| Command | Purpose |
| --- | --- |
| `ghalet run` | Run the daemon in the foreground for all configured targets. |
| `ghalet apply` | Drain targets, reload configuration, and reconcile registrations, attempting to delete unused scale sets. |
| `ghalet config validate` | Validate configuration and local paths without contacting GitHub. |
| `ghalet app setup --scope OWNER/REPO --scale-set ghalet-runner` | Create and install a private GitHub App, save managed authentication, and print any required static target configuration. See [App setup](./docs/app-setup.md) for organization scopes, SSH and resumption. |
| `ghalet systemd app setup --scope OWNER/REPO --scale-set ghalet-runner` | Set up an App with encrypted service credentials and the target's runtime key reference (Linux/root). See [App setup](./docs/app-setup.md#packaged-systemd-service). |
| `ghalet credential set NAME --prompt` | Save an owner-only credential file and update managed defaults without rewriting static config. See [ordinary credentials](./docs/file-credentials.md) for App keys and replacement. |
| `ghalet systemd credential set NAME --prompt` | Register an encrypted service PAT and update managed defaults (Linux/root). See [credentials](./docs/credentials.md) for App keys, automation and rotation. |
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
See [configuration apply](./docs/operations.md#applying-configuration) and
[diagnosis and cleanup](./docs/operations.md#diagnosis-and-garbage-collection)
for exit codes, offline maintenance and deletion safeguards.

## Runtime behavior

Host capacity defaults to two, with per-target limits and optional idle warm
pools. Assigned jobs take priority. Atomic state files and kernel process
identities support crash recovery; unverifiable executions retain capacity.

Runner archives are automatically downloaded for the pinned version and
verified against official SHA-256 digests. Daemon stdout is JSONL; runner logs
are archived before cleanup by default. See [operations](./docs/operations.md)
for retries, recovery and log policy.

## Documentation

Start with the [documentation index](./docs/index.md), readable directly in the
repository or on the [documentation site](https://songmu.github.io/ghalet/).
All guides are available in English and Japanese.

- [Getting started](./docs/getting-started.md): installation, authentication,
  your first workflow, and service setup.
- [Authentication setup](./docs/authentication.md): method tradeoffs, minimum
  permissions, creation steps and credential rotation for PATs and GitHub Apps.
- [Operations](./docs/operations.md): authentication, configuration, execution
  environment, recovery, logs and systemd/launchd/Lima deployment.
- [Example configuration](./examples/config.yaml) and [configuration schema](./schema.yaml).

## Development

See [the contributor guide](./CONTRIBUTING.md) for development, testing,
packaging, and documentation site maintenance.

## Author

[Songmu](https://github.com/Songmu)
