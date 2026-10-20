# Changelog

## [v0.0.3](https://github.com/Songmu/ghalet/compare/v0.0.2...v0.0.3) - 2026-10-20

- docs: help operators configure and rotate least-privilege credentials by @Songmu in https://github.com/Songmu/ghalet/pull/25
- Stop orphaned Linux runner groups with shared runner guards by @Songmu in https://github.com/Songmu/ghalet/pull/28
- feat: register credentials without systemd by @Songmu in https://github.com/Songmu/ghalet/pull/27
- feat: set up GitHub App authentication without a preexisting token by @Songmu in https://github.com/Songmu/ghalet/pull/29

## [v0.0.2](https://github.com/Songmu/ghalet/compare/v0.0.1...v0.0.2) - 2026-10-20

- Add Linux packages and GHCR images with goxz and nFPM by @Songmu in https://github.com/Songmu/ghalet/pull/15
- Isolate packaged runner jobs from management credentials by @Songmu in https://github.com/Songmu/ghalet/pull/17
- Remove standalone drain in favor of normal shutdown by @Songmu in https://github.com/Songmu/ghalet/pull/19
- Add shared credential defaults and encrypted systemd registration by @Songmu in https://github.com/Songmu/ghalet/pull/18
- Reconcile configuration through apply and systemd reload by @Songmu in https://github.com/Songmu/ghalet/pull/20
- Discover accessible daemons for configuration-free management by @Songmu in https://github.com/Songmu/ghalet/pull/21
- Rename cleanup configuration to logs to reflect log archiving by @Songmu in https://github.com/Songmu/ghalet/pull/22
- Simplify runner storage with systemd and XDG directories by @Songmu in https://github.com/Songmu/ghalet/pull/23
- Simplify code and documentation to reduce redundancy by @Songmu in https://github.com/Songmu/ghalet/pull/24

## [v0.0.1](https://github.com/Songmu/ghalet/commits/v0.0.1) - 2026-10-19

- Implement process-based ephemeral runner scale sets by @Songmu in https://github.com/Songmu/ghalet/pull/1
- Simplify configuration with self-contained targets by @Songmu in https://github.com/Songmu/ghalet/pull/2
- Run runners with a minimal explicit environment by @Songmu in https://github.com/Songmu/ghalet/pull/4
- Move default log archives to XDG_STATE_HOME by @Songmu in https://github.com/Songmu/ghalet/pull/5
- Refactor tests with table-driven cases and config fixtures by @Songmu in https://github.com/Songmu/ghalet/pull/6
- Replace config apiVersion with an editor schema by @Songmu in https://github.com/Songmu/ghalet/pull/7
- Simplify reconciliation and configuration validation, consolidate docs by @Songmu in https://github.com/Songmu/ghalet/pull/8
- Move CLI subcommands into cmd with standard flag parsing by @Songmu in https://github.com/Songmu/ghalet/pull/9
- Separate management APIs from CLI output and prompting by @Songmu in https://github.com/Songmu/ghalet/pull/10
- Extract log masking into the public logreduct package by @Songmu in https://github.com/Songmu/ghalet/pull/11
- Extract runner distribution handling into runnerdist by @Songmu in https://github.com/Songmu/ghalet/pull/12
- docs: explain ghalet's purpose and use cases, with Japanese guides by @Songmu in https://github.com/Songmu/ghalet/pull/13
