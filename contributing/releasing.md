# Packaging and releases

[日本語](releasing.ja.md)

This guide covers building and publishing ghalet distributions.
For installing and updating published packages or containers, use the
[user guide](../docs/distribution.md).

## Building distribution artifacts

[`release-build.yaml`](../.github/workflows/release-build.yaml) uses goxz for
Linux/macOS archives and nFPM for Linux packages. All release files are collected
in `dist`; after packaging, `SHA256SUMS` is generated once for every file below it,
excluding the manifest itself.
[`packaging/build.sh`](../packaging/build.sh) extracts the exact Linux binaries
from those archives, generates the packaged unit from `examples/ghalet.service`,
and stages only the binaries and license notices for the container build.

Run from the repository root:

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
license notices. `.dockerignore` excludes the rest of the checkout, including
local configuration, credentials, worktrees, and Git metadata.

## Publication and validation

Release publication uploads packages alongside archives and their combined
checksums/provenance, then a separate job publishes and attests the GHCR image.
The version comes from `version.go`, which tagpr updates.
The separate [`test-packaging.yaml`](../.github/workflows/test-packaging.yaml)
workflow builds and tests packages/images without publishing or receiving
registry credentials. See [testing](testing.md#automated-tests) for coverage
and verification limits.

After the first image publication, the repository owner must check the GHCR
package settings and make the package public for anonymous pulls.
Repository visibility does not automatically make a new package public.

## Dependency updates

The nFPM version is pinned in [`packaging/nfpm.version`](../packaging/nfpm.version).
Renovate proposes weekly updates to nFPM and the bundled runner. For runner
updates, it checks official stable releases and updates the bundled default and
example together; regular CI validates these changes.
Renovate also tracks the documentation site's
[`docs/site/.hugo-version`](../docs/site/.hugo-version).
Go modules, GitHub Actions, and Docker images are managed by Dependabot.
Enable this repository in the [Renovate GitHub App](https://github.com/apps/renovate)
installation settings for automatic update PRs.
