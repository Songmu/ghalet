# Changelog

## [v0.2.2](https://github.com/Songmu/ghalet/compare/v0.2.1...v0.2.2) - 2026-10-22

- Fix non-root runner preparation for safe distribution symlinks by @Songmu in https://github.com/Songmu/ghalet/pull/83

## [v0.2.1](https://github.com/Songmu/ghalet/compare/v0.2.0...v0.2.1) - 2026-10-22

- Fix non-root systemd credential ACL validation by @Songmu in https://github.com/Songmu/ghalet/pull/81

## [v0.2.0](https://github.com/Songmu/ghalet/compare/v0.1.1...v0.2.0) - 2026-10-22

- docs: separate user guides from contributor documentation by @Songmu in https://github.com/Songmu/ghalet/pull/76
- Fix concurrent state access in the shutdown regression test by @Songmu in https://github.com/Songmu/ghalet/pull/79
- Share complete App credentials through setup defaults by @Songmu in https://github.com/Songmu/ghalet/pull/78
- Run the systemd controller as ghalet and jobs as ghalet-runner by @Songmu in https://github.com/Songmu/ghalet/pull/80

## [v0.1.1](https://github.com/Songmu/ghalet/compare/v0.1.0...v0.1.1) - 2026-10-21

- Wait for daemon readiness when starting systemd services by @Songmu in https://github.com/Songmu/ghalet/pull/73
- Keep the Hugo documentation builder up to date with Renovate by @Songmu in https://github.com/Songmu/ghalet/pull/72

## [v0.1.0](https://github.com/Songmu/ghalet/compare/v0.0.5...v0.1.0) - 2026-10-21

- Avoid idle process snapshots without delaying job scheduling by @Songmu in https://github.com/Songmu/ghalet/pull/36
- Release recovery capacity after verified process termination by @Songmu in https://github.com/Songmu/ghalet/pull/38
- Avoid unrelated PID churn in Linux stop verification by @Songmu in https://github.com/Songmu/ghalet/pull/40
- Limit filesystem synchronization to changed directories by @Songmu in https://github.com/Songmu/ghalet/pull/37
- Support administrator symlink layouts with canonical storage bases by @Songmu in https://github.com/Songmu/ghalet/pull/39
- Guide GitHub App setup from creation to installation by @Songmu in https://github.com/Songmu/ghalet/pull/41
- Limit kernel identity inspection to monitored process groups by @Songmu in https://github.com/Songmu/ghalet/pull/42
- Retry recoverable run cleanup with capped backoff by @Songmu in https://github.com/Songmu/ghalet/pull/43
- Allow independent runner extractions to proceed concurrently by @Songmu in https://github.com/Songmu/ghalet/pull/44
- Avoid repeated ancestor validation during state operations by @Songmu in https://github.com/Songmu/ghalet/pull/46
- Prevent unrelated work-root files from consuming runner capacity by @Songmu in https://github.com/Songmu/ghalet/pull/47
- Recover transient target failures automatically with capped backoff by @Songmu in https://github.com/Songmu/ghalet/pull/45
- Keep daemon scheduling responsive during slow execution I/O by @Songmu in https://github.com/Songmu/ghalet/pull/49
- Share credential update rollback and input validation by @Songmu in https://github.com/Songmu/ghalet/pull/50
- Make App setup lifecycle stages explicit by @Songmu in https://github.com/Songmu/ghalet/pull/52
- Reduce duplicated atomic state persistence code by @Songmu in https://github.com/Songmu/ghalet/pull/54
- Simplify daemon discovery with focused directory and socket probes by @Songmu in https://github.com/Songmu/ghalet/pull/55
- Clarify configuration apply reconciliation stages by @Songmu in https://github.com/Songmu/ghalet/pull/51
- refactor: separate listener recovery and session lifecycle by @Songmu in https://github.com/Songmu/ghalet/pull/56
- Share configuration editing across setup and credentials by @Songmu in https://github.com/Songmu/ghalet/pull/57
- Unify runner output lifecycle state by @Songmu in https://github.com/Songmu/ghalet/pull/58
- Separate configuration policy from daemon implementation by @Songmu in https://github.com/Songmu/ghalet/pull/53
- Separate verified process-group operations from daemon state by @Songmu in https://github.com/Songmu/ghalet/pull/59
- Reuse configuration path resolution in setup and credentials by @Songmu in https://github.com/Songmu/ghalet/pull/61
- Share credential registration preparation without merging backend pipelines by @Songmu in https://github.com/Songmu/ghalet/pull/62
- Wait for listener cleanup directly instead of polling during apply by @Songmu in https://github.com/Songmu/ghalet/pull/63
- Isolate runner log collection and archive lifecycle by @Songmu in https://github.com/Songmu/ghalet/pull/64
- Refactor setup locking and remove the redundant work lock wrapper by @Songmu in https://github.com/Songmu/ghalet/pull/66
- Clarify daemon recovery and execution lifecycle boundaries by @Songmu in https://github.com/Songmu/ghalet/pull/65
- Reduce duplicated setup in daemon tests by @Songmu in https://github.com/Songmu/ghalet/pull/67
- Keep static configuration immutable with managed setup information by @Songmu in https://github.com/Songmu/ghalet/pull/68
- Identify targets by scope, runner group, and scale set by @Songmu in https://github.com/Songmu/ghalet/pull/69
- Allow unknown configuration properties by @Songmu in https://github.com/Songmu/ghalet/pull/70
- Publish bilingual documentation with Hugo and GitHub Pages by @Songmu in https://github.com/Songmu/ghalet/pull/71

## [v0.0.5](https://github.com/Songmu/ghalet/compare/v0.0.4...v0.0.5) - 2026-10-20

- Fix non-root runner startup by making execution parents listable by @Songmu in https://github.com/Songmu/ghalet/pull/33

## [v0.0.4](https://github.com/Songmu/ghalet/compare/v0.0.3...v0.0.4) - 2026-10-20

- Simplify GitHub App onboarding and integrate systemd credential provisioning by @Songmu in https://github.com/Songmu/ghalet/pull/31

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
