# Linux packages and containers

[English](distribution.md) | [日本語](distribution.ja.md)

Release tags publish amd64 and arm64 binaries, `.deb` and `.rpm` packages,
and the multi-platform image `ghcr.io/songmu/ghalet:TAG`.
Existing archives, `install.sh`, and `go install` remain available.
APT/DNF repositories and Homebrew service integration are not provided by
this packaging configuration.

## Debian and RPM packages

Download the package for your architecture from GitHub Releases and check
it against `SHA256SUMS`. Build provenance also covers the packages:

```sh
gh attestation verify ./PACKAGE --repo Songmu/ghalet
sudo apt install ./PACKAGE.deb
# Or, on an RPM-based system:
sudo dnf install ./PACKAGE.rpm
```

Packages include the official runner's OS dependencies and `procps`.
GitHub CLI is not required: the supplied configuration uses an encrypted PAT
provided by systemd. Registration requires systemd 250 or later.
Install Git and the build tools your workflows need separately.
The package smoke tests cover Ubuntu 26.04 hosts and Rocky Linux 9 containers on both
architectures; other distributions need their own compatibility checks.

Installation creates an unprivileged `ghalet-runner` account with HOME at
`/var/lib/ghalet-runner`, storage directories, and a systemd unit. The controller
runs as root to switch users; the package configuration sets
`runner.user: ghalet-runner` so runners and jobs run without root privileges.
Configuration, management credentials, caches, and saved logs are root-only.
The runner can traverse `/var/lib/ghalet` to reach its own execution directories,
but cannot modify that parent directory. Installation does **not** create a live
configuration, enable the service, or start it.

To create a GitHub App and configure its encrypted service key:

```sh
sudo ghalet systemd app setup --scope OWNER/REPO
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
```

Use `--scope ORG` for an organization. The wizard creates a missing configuration
with the package's runner user and log path, or adds/updates the selected target
in an existing configuration. Follow the printed browser URLs; for SSH use the
port forwarding in [App setup](app-setup.md#ssh-and-manual-browser-operation).

To use a PAT instead, copy and edit the package example:

```sh
sudo install -o root -g root -m 0600 \
  /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
sudoedit /etc/ghalet/config.yaml
sudo ghalet systemd credential set default --prompt
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
```

Replace `OWNER/REPO` in the configuration and supply a PAT with the
appropriate runner administration permissions. Registration writes its
reference to `defaults.credential`, shared by targets that omit `credential`.
Package installation and upgrades never request secrets; App setup or credential registration
is a separate, explicit command. For App keys, multiple credentials, automation,
and rotation, see [encrypted credentials](credentials.md).
Do not put credentials inline in YAML or in shell command arguments.
On older systemd installations, configure an owner-only PAT file with
`defaults.credential.tokenFile` instead; see
[operations](operations.md#configuration-and-credentials).

The unit uses `StateDirectory=ghalet` and `CacheDirectory=ghalet`,
which supply `STATE_DIRECTORY` and `CACHE_DIRECTORY`. Executions go under
`/var/lib/ghalet/runs` and runner archives and digests
under `/var/cache/ghalet/runners`. The sample places saved runner logs under
`/var/log/ghalet/runners` through `logs.root`. Daemon stdout and stderr
go to the journal; persistence, rotation, and retention follow the host's
journald configuration.

```sh
sudo journalctl -u ghalet -f
sudo ghalet doctor --config /etc/ghalet/config.yaml
```

For updates, stop the service normally and inspect any incomplete shutdown
before replacing the package. Package upgrade scripts also stop the service,
but never restart it automatically. Start it explicitly after the update;
an existing enable-at-boot setting is preserved.

When upgrading from the original shared-`ghalet`-user package, drain and clean up
existing executions before upgrading. Existing configuration is not rewritten:
add `runner.user: ghalet-runner` to `/etc/ghalet/config.yaml` before starting
the new root-run service. Without it, startup refuses to run root jobs.
Change management credential files to `root:root` with mode `0600`.
The legacy `ghalet` account and retained files are not deleted automatically.

```sh
sudo systemctl stop ghalet
# Install the new package using apt or dnf.
sudo systemctl start ghalet
```

Removal stops and disables the service. Configuration, credentials, state,
logs, and the runner account remain, including after `apt purge`; remove them only
after inspecting retained executions. Package operations never delete GitHub
scale sets.

The unit uses `KillMode=mixed` and a 15-minute outer stop deadline.
Increase that deadline if changing the default 10-minute drain timeout.
See [service deployment](operations.md#service-deployment) for shutdown
and crash-cleanup semantics.

## Docker Compose

The container runs ghalet and its Linux runner processes together, as UID/GID
10001. It contains runner prerequisites, Git, curl, and a shell, not a complete
hosted-runner toolchain. Extend the image to install workflow-specific tools.
It does not use systemd inside the container.

Copy `examples/compose.yaml` and `examples/system-config.yaml` into a new
deployment directory, naming the latter `config.yaml`. Set `OWNER/REPO` and
place the PAT in `credentials/token`. On a Linux Docker host, prepare the
bind-mounted credentials for the image's user:

```sh
mkdir -p credentials
# Write the PAT to credentials/token using your secret-management tool.
sudo chown -R 10001:10001 credentials
sudo chmod 0700 credentials
sudo chmod 0600 credentials/token
export GHALET_VERSION=vX.Y.Z
docker compose run --rm ghalet config validate --config /etc/ghalet/config.yaml
docker compose up -d
docker compose exec ghalet ghalet doctor --config /etc/ghalet/config.yaml
```

Set `GHALET_VERSION` to a published release tag. Verify the image provenance
with `gh attestation verify oci://ghcr.io/songmu/ghalet:TAG --repo Songmu/ghalet`.
For other Docker hosts, check the effective owner and mode inside the
container; making a token world-readable is not a workaround.

Configuration and credentials are mounted read-only. Named volumes preserve
the HOME/state, cache, and logs, and are initialized with the image user's
ownership. Run only one daemon against each set of volumes.
Do not use `docker compose down --volumes` when retaining state.
Management commands must select the same `/etc/ghalet/config.yaml` as the
daemon.

Compose enables a small init process to forward signals and reap orphaned
processes, and allows 15 minutes for normal stop/drain. Stop normally before
recreating the container or changing its image:

```sh
docker compose stop
docker compose pull
docker compose up -d
```

Increase `stop_grace_period` if increasing `runner.drainTimeout`.
Forced termination, container removal, and host failure can interrupt jobs.
Persisted records remain for diagnosis; replacing a container is not live
runner-process recovery.

Jobs run **inside this Linux container**, not on the host's toolchain.
macOS jobs still require a native macOS installation. Container packaging
does not add Docker actions, job/service containers, or per-job isolation.
The example mounts no Docker socket, shares no host PID namespace, and
requires no privileged mode. Trusted jobs running as the same user can
still read mounted management credentials and share HOME, tools, and caches.

## Building distribution artifacts

`release-build.yaml` keeps goxz for Linux/macOS archives and uses nFPM for
Linux packages. All release files are collected in `dist`; after packaging,
`SHA256SUMS` is generated once for every file below it, excluding the manifest
itself. `packaging/build.sh` extracts the exact Linux binaries from
those archives, generates the packaged unit from `examples/ghalet.service`,
and stages only the binaries and license notices for the container build.

The nFPM version is pinned in [`packaging/nfpm.version`](../packaging/nfpm.version).
Renovate proposes weekly updates to nFPM and the bundled runner; Go modules,
GitHub Actions, and Docker images remain managed by Dependabot.
Enable this repository in the [Renovate GitHub App](https://github.com/apps/renovate)
installation settings for automatic update PRs.

```sh
go install github.com/Songmu/goxz/cmd/goxz@v0.13.0
go install "github.com/goreleaser/nfpm/v2/cmd/nfpm@$(cat packaging/nfpm.version)"
VERSION=$(sed -n 's/^const Version = "\([^"]*\)"$/v\1/p' version.go)
test -n "$VERSION"
goxz -pv "$VERSION" -os linux,darwin -arch amd64,arm64 -static \
  -d /tmp/ghalet-release ./cmd/ghalet
sh packaging/build.sh "$VERSION" /tmp/ghalet-release /tmp/ghalet-release /tmp/ghalet-binaries
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-context binaries=/tmp/ghalet-binaries .
```

The Dockerfile uses an explicit named context containing only binaries and
license notices.
`.dockerignore` excludes the rest of the checkout, including local
configuration, credentials, worktrees, and Git metadata.
Release publication uploads packages alongside archives and their combined
checksums/provenance, then a separate job publishes and attests the GHCR
image. The separate `test-packaging.yaml` workflow builds and tests
packages/images without publishing or receiving registry credentials.
Its version comes from `version.go`, which tagpr already updates.

After the first image publication, the repository owner must check the
GHCR package settings and make the package public for anonymous pulls.
Repository visibility does not automatically make a new package public.
