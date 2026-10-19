# Linux packages and containers

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
GitHub CLI is not required: the supplied configuration uses a PAT file.
Install Git and the build tools your workflows need separately.
The package smoke tests cover Ubuntu 24.04 hosts and Rocky Linux 9 containers on both
architectures; other distributions need their own compatibility checks.

Installation creates an unprivileged `ghalet` account, its HOME at
`/var/lib/ghalet`, storage directories, and a systemd unit. It does **not**
create a live configuration, enable the service, or start it.

```sh
sudo install -o root -g ghalet -m 0640 \
  /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
sudoedit /etc/ghalet/config.yaml
sudo install -o ghalet -g ghalet -m 0600 \
  /path/to/pat-file /etc/ghalet/credentials/token
sudo -u ghalet ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
```

Replace `OWNER/REPO` in the configuration and supply a PAT with the
appropriate runner administration permissions. For a GitHub App, replace
the target's `credential` and supply its owner-only private key file as
described in [operations](operations.md#configuration-and-credentials).
Do not put credentials inline in YAML or in shell command arguments.

The sample places executions and runner metadata under `/var/lib/ghalet`,
downloads under `/var/cache/ghalet`, and saved runner logs under
`/var/log/ghalet`. Daemon output goes to the journal:

```sh
sudo journalctl -u ghalet -f
sudo -u ghalet ghalet doctor --config /etc/ghalet/config.yaml
```

For updates, stop the service normally and inspect any incomplete shutdown
before replacing the package. Package upgrade scripts also stop the service,
but never restart it automatically. Start it explicitly after the update;
an existing enable-at-boot setting is preserved.

```sh
sudo systemctl stop ghalet
# Install the new package using apt or dnf.
sudo systemctl start ghalet
```

Removal stops and disables the service. Configuration, credentials, state,
logs, and the account remain, including after `apt purge`; remove them only
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
Linux packages. `packaging/build.sh` extracts the exact Linux binaries from
those archives, generates the packaged unit from `examples/ghalet.service`,
and stages only the binaries and license notices for the container build:

```sh
go install github.com/Songmu/goxz/cmd/goxz@v0.13.0
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0
goxz -pv v0.0.0 -os linux,darwin -arch amd64,arm64 -static \
  -d /tmp/ghalet-release ./cmd/ghalet
sh packaging/build.sh v0.0.0 /tmp/ghalet-release /tmp/ghalet-release /tmp/ghalet-binaries
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-context binaries=/tmp/ghalet-binaries .
```

The Dockerfile uses an explicit named context containing only binaries and
license notices.
`.dockerignore` excludes the rest of the checkout, including local
configuration, credentials, worktrees, and Git metadata.
Release publication uploads packages alongside archives and their combined
checksums/provenance, then a separate job publishes and attests the GHCR
image. Pull-request checks build and test packages/images without publishing
or receiving registry credentials.

After the first image publication, the repository owner must check the
GHCR package settings and make the package public for anonymous pulls.
Repository visibility does not automatically make a new package public.
