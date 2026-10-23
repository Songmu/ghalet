# Getting started

[日本語](./getting-started.ja.md)

This guide takes you from installation to running your first GitHub Actions job
on a dedicated Linux or macOS host. Use only trusted workflows; runners do not
provide strict job isolation. See the [trust boundary](./operations.md#trust-boundary).

## Installation

Install with Homebrew:

```sh
brew install Songmu/tap/ghalet
```

Or install with Go:

```sh
go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

Linux `.deb`/`.rpm` packages with a systemd unit and a GHCR container image
are also available. See [packages and containers](./distribution.md) for
installation, configuration, and updates.

Add the installation directory to your `PATH`. Install the build tools your
workflows need and the official runner's OS prerequisites on the host.
Normally, run ghalet as a non-root user. If you run the daemon as root, you
must set `runner.user` to an existing non-root account; root runner jobs are
refused. See [operations](./operations.md#direct-execution-and-environment)
for execution users and environment settings.

## Quick start

These steps run a runner for one repository.
Run the host commands as the non-root user who will run ghalet.

For ongoing operation, run ghalet as a service under systemd on Linux or
launchd on macOS (`brew services` for Homebrew installations).
The steps below first verify foreground operation before switching to a service.
For service setup, see:

- Linux: [the packaged service setup](./distribution.md#debian-and-rpm-packages).
- macOS: [the Homebrew service](#homebrew-service-macos-only).

### 1. Authenticate with GitHub

Install GitHub CLI (`gh`) and log in with an account that can manage runners
for the repository. See [authentication setup](./authentication.md) for
the required permissions.

```sh
gh auth login
```

This guide uses your `gh` login. For PAT or GitHub App authentication,
see [authentication setup](./authentication.md).

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
For more settings, see the [commented example](../examples/config.yaml).

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

## Homebrew service (macOS only)

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
