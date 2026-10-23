# ghalet documentation

[日本語](./index.ja.md)

ghalet is a lightweight controller for GitHub Actions self-hosted runners on
Linux and macOS. A single Go daemon starts native runner processes on demand
and manages capacity across repositories and organizations.

![One ghalet daemon starts native runners on demand within host-wide and per-target limits](./images/ghalet-overview.png)

It is intended for dedicated, trusted hosts, from build machines and
single-purpose VMs to home servers. Runners are not strictly isolated from
one another; use only trusted workflows.

## Get started

- [Getting started](./getting-started.md): installation, authentication,
  configuration, your first workflow, and macOS service setup.
- [Packages and containers](./distribution.md): Linux packages with systemd,
  Docker Compose, and updates.

## Authentication

- [Authentication setup](./authentication.md): choose GitHub CLI, PAT, or
  GitHub App authentication and configure the required permissions.
- [GitHub App setup](./app-setup.md): create and install an App with the
  setup wizard, including remote hosts and systemd services.
- [Ordinary credential files](./file-credentials.md): register, replace, and
  roll back credentials stored as files.
- [Encrypted systemd credentials](./credentials.md): manage service credentials
  on Linux.

## Operation and reference

- [Operations](./operations.md): configuration, execution users, scaling,
  recovery, logs, service deployment, and trust boundaries.
- [Commands](../README.md#commands): CLI command overview.
- [Example configuration](../examples/config.yaml) and
  [configuration schema](../schema.yaml).
