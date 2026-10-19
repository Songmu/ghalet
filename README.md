ghalet
=======

[![Test Status](https://github.com/Songmu/ghalet/actions/workflows/test.yaml/badge.svg?branch=main)][actions]
[![Coverage Status](https://codecov.io/gh/Songmu/ghalet/branch/main/graph/badge.svg)][codecov]
[![MIT License](https://img.shields.io/github/license/Songmu/ghalet)][license]
[![PkgGoDev](https://pkg.go.dev/badge/github.com/Songmu/ghalet)][PkgGoDev]

[actions]: https://github.com/Songmu/ghalet/actions?workflow=test
[codecov]: https://codecov.io/gh/Songmu/ghalet
[license]: https://github.com/Songmu/ghalet/blob/main/LICENSE
[PkgGoDev]: https://pkg.go.dev/github.com/Songmu/ghalet

ghalet runs ephemeral GitHub Actions Runner Scale Sets as native Linux/macOS
processes. It scales several repository/organization targets within a shared
host capacity, without Kubernetes, Docker, or a VM per job.

## Synopsis

```sh
mkdir -p ~/.config/ghalet
cp examples/config.yaml ~/.config/ghalet/config.yaml
# Set the target ID and repository or organization scope.
gh auth login
ghalet config validate
ghalet run
```

Use the configured scale set name in a trusted workflow:

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
ghalet drain --target project
ghalet gc --dry-run
```

## Behavior and trust

Each job receives a new JIT runner, runner directory and workspace. The
execution user, HOME, host tools and caches are shared. **This is not job
isolation: do not run untrusted fork PRs or hostile code.** Root jobs and
Docker-dependent workflows are unsupported.

The default host capacity is two. Targets support minimum idle runners and
per-target limits; real assigned jobs take precedence over idle warm pools.
Normal termination drains accepted work. Crashes recover from small atomic
state files and kernel process identities, retaining uncertain executions
rather than guessing which process to kill.

Runner archives are automatically downloaded for the pinned version and
verified against official SHA-256 digests. Daemon stdout is JSONL; optional
runner logs are archived before execution cleanup. Configuration changes and
faulted targets require a normal stop/restart. Scale sets remain registered
until an explicit, interactively confirmed `destroy`.

See [operations](docs/operations.md) for authentication, configuration,
recovery, command exit codes and systemd/launchd/Lima examples.
See [validation](docs/validation.md) for automated coverage and the explicit
opt-in live acceptance tests and their limits.

## Installation

```console
# Install the latest version. (Install it into ./bin/ by default).
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s

# Specify installation directory ($(go env GOPATH)/bin/) and version.
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s -- -b $(go env GOPATH)/bin [vX.Y.Z]

# In alpine linux (as it does not come with curl by default)
% wget -O - -q https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s [vX.Y.Z]

# go install
% go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

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
