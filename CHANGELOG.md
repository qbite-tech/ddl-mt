# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While in `0.x`, a new minor version may contain breaking changes.

## [Unreleased]

## [0.2.0]

First public release.

### Added

- Named targets in `ddl-mt.json` connecting a migration directory to an expected
  database, credential source, and history table.
- Commands: `create-config`, `targets`, `new`, `validate`, `init`, `status`,
  `check`, `apply`, `baseline`, `repair`, and `version`.
- `connection_mode` (`transaction` default, or `session`) with support for direct
  PostgreSQL and PgBouncer transaction/session pooling.
- Opt-in non-transactional migrations (e.g. `CREATE INDEX CONCURRENTLY`) with
  durable `applying`/`failed` markers and explicit operator repair.
- `baseline` for recording audited existing schema changes without running SQL
  ([adoption guide](docs/adoption.md)).
- Optional per-target `lock_key` to coordinate with a runner that already uses
  a specific advisory-lock key.
- Connection diagnostics that report safe failure categories and never print
  connection strings or raw server messages.

### Changed

- Minimum Go version is now 1.26.
- Upgraded `github.com/jackc/pgx/v5` to v5.12.0 and `golang.org/x/text` to v0.44.0,
  resolving GO-2026-5004, GO-2026-5970, and GO-2026-6629.

### Fixed

- Project discovery no longer falls back to an ancestor project when a nearer
  configuration or migration path is broken.
- Duplicate keys in project and credential JSON are rejected, including keys
  that differ only by letter case.
- Concurrent-index detection and migration metadata parsing no longer
  misclassify SQL inside strings, dollar-quoted bodies, or comments.

[Unreleased]: https://github.com/qbite-tech/ddl-mt/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/qbite-tech/ddl-mt/releases/tag/v0.2.0
