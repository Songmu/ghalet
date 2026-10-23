# Contributing to ghalet

[日本語](./CONTRIBUTING.ja.md)

This guide is for changing, testing, and releasing ghalet itself.
For installation and operation, use the [user documentation](./docs/index.md).

## Development

Use Linux or macOS with the Go version specified in [go.mod](./go.mod).
Run from the repository root as a non-root user:

```sh
go build ./cmd/ghalet
go test -race ./...
go vet ./...
```

The resulting `./ghalet` binary can be used with a dedicated test configuration.
For testing unpublished changes in the Apple Silicon Lima VM, cross-compile
with `GOOS=linux GOARCH=arm64 go build -o /tmp/ghalet-linux-arm64 ./cmd/ghalet`
and copy the binary into the guest with `limactl copy`.
See [service deployment](./docs/operations.md#service-deployment) for the VM setup.

- [Testing](./contributing/testing.md): automated coverage, official runner
  smoke tests, live GitHub acceptance, and verification limits.
- [Runtime design](./contributing/design.md): scheduling, process supervision,
  recovery, persistence, and redaction invariants.
- [Packaging and releases](./contributing/releasing.md): distribution builds,
  publication, and dependency updates.
- [Documentation site](./docs/site/README.md): local builds, previews,
  and GitHub Pages deployment.

After changing GitHub Actions dependencies, run `gh actions-lock` and commit
`.github/workflows/actions.lock` with the workflow changes. Install the tool
with `gh extension install github/gh-actions-lock`.

## Documentation policy

Keep documentation with its intended reader:

- `README.md` and `README.ja.md` introduce ghalet, its uses, and the user guides.
  Keep development instructions here in `CONTRIBUTING`, with only a link from
  the README.
- `docs/` contains user-facing installation, configuration, authentication,
  operation, troubleshooting, and supported behavior. It is the source of the
  public documentation site.
- `contributing/` contains detailed instructions for changing, testing,
  packaging, releasing, and maintaining ghalet.
- `docs/site/` is an exception: keep the site build tools and their maintenance
  guide colocated with the user documentation. They are not published as pages.
- Package README files and GoDoc describe local responsibilities, API contracts,
  and implementation invariants. Keep those explanations close to the code.
- `AGENTS.md` records repository-wide editing rules, not duplicate development
  instructions.

Technical detail belongs in user guides when it affects an operational decision:
capacity, drain, recovery guarantees, credential protection, and isolation limits.
Implementation algorithms, test fixtures, CI, and release machinery belong in
contributor documentation. Split mixed sections rather than moving away useful
operational guidance.

Maintain one authoritative description of each procedure and link to it instead
of duplicating it. Update English and Japanese documentation together, including
language switchers and same-language cross-references. Validate site changes with
the [site build](./docs/site/README.md#local-build).
