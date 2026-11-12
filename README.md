# ddl-mt

[![CI](https://github.com/qbite-tech/ddl-mt/actions/workflows/ci.yml/badge.svg)](https://github.com/qbite-tech/ddl-mt/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/qbite-tech/ddl-mt.svg)](https://pkg.go.dev/github.com/qbite-tech/ddl-mt)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**damn delightful migrations** — a standalone PostgreSQL migration CLI for many
repositories, databases, and environments.

Each application repository owns its SQL and a `ddl-mt.json` project file. One
installed binary manages them all. Named targets connect a migration directory to
an expected database, credentials, and a history table. Development and production
can share a directory even when their database names differ.

Databases with existing schema can be adopted by baselining audited migrations. See [adopting an existing database](docs/adoption.md).

## Install

ddl-mt supports PostgreSQL only; other database engines are not supported.

Download a prebuilt binary for Linux, macOS, or Windows from the
[releases page](https://github.com/qbite-tech/ddl-mt/releases) and verify it
against the published `checksums.txt`. Alternatively, with Go 1.26 or newer:

```sh
go install github.com/qbite-tech/ddl-mt@latest
```

## Build from source

```sh
make build
bin/ddl-mt --help
# Or install the current checkout into your Go binary directory:
go install .
```

## Versioning

`ddl-mt version` and `ddl-mt --version` report a semantic version, currently `0.3.0`.
The `VERSION` file supplies the version for local builds and `go install .`.
Installation from a published Go module uses that module's version; a build-time
`-X main.version=...` override takes precedence. For example:

```sh
make build VERSION=0.3.0-rc.1
bin/ddl-mt version
```

For each release, update `VERSION` and `CHANGELOG.md`, then tag the corresponding
commit as `vX.Y.Z`. Pushing the tag builds and publishes release archives.
Use patch versions for fixes, minor versions for compatible features, and major
versions for breaking changes once the tool reaches `1.0.0`. The `0.x` series is
initial development and may introduce breaking changes in a new minor version.

## First project

Create a starter configuration from your application repository:

```sh
cd /path/to/your/application
ddl-mt create-config
```

Or choose a directory or JSON filename explicitly:

```sh
ddl-mt create-config --path /path/to/your/application
ddl-mt create-config --path /path/to/your/application/ddl-mt.json
```

The default destination is `./ddl-mt.json`. A directory path gets a `ddl-mt.json`
file inside it; a path ending in `.json` selects that file. Missing parent directories
are created. An existing file is never overwritten; the command reports that it already exists
and exits successfully.
Running plain `ddl-mt`, `help`, or `version` only prints information.

The starter contains blank `project` and `database` fields, a `default` target,
`migrations_dir: "migrations"`, and `dsn_env: "DB_DSN"`. Fill in the required
fields before using it; it does not connect to a database or create migrations.
To adopt a database that already has schema, see the
[adoption guide](docs/adoption.md).

For example, edit `ddl-mt.json` to configure development and production:

```json
{
  "project": "shop",
  "targets": {
    "development": {
      "database": "shop_dev",
      "migrations_dir": "migrations/main",
      "dsn_env": "SHOP_DEV_DSN"
    },
    "production": {
      "database": "shop",
      "migrations_dir": "migrations/main",
      "dsn_env": "SHOP_PROD_DSN"
    }
  }
}
```

```sh
mkdir -p migrations/main
ddl-mt targets
ddl-mt --target development new create_widgets
```

Edit `migrations/main/001_create_widgets.sql`:

```sql
CREATE TABLE widgets (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL
);
```

Then validate and apply:

```sh
ddl-mt --target development validate
# Supply SHOP_DEV_DSN through your shell or secret manager.
ddl-mt --target development init
ddl-mt --target development status
ddl-mt --target development apply
ddl-mt --target development check
```

The database must already exist. `init` creates its migration history table.
`new` writes a template that must be filled in before validation. Empty migrations
are rejected. Global flags go **before** the command.

Run from any directory with an explicit project file:

```sh
ddl-mt --project /srv/shop/ddl-mt.json --target production status
ddl-mt --project /srv/billing/ddl-mt.json --target production status
```

Project discovery walks upward from the working directory and stops at a Git
repository boundary. There is no implicit default target. Migration directories
and configured credential paths are relative to the project file, not the shell's
working directory. Targets can refer to the same SQL directory or independent
sequences. The [shop example](examples/shop/ddl-mt.json) demonstrates both.

## Commands

| Command | Behavior |
| --- | --- |
| `create-config [--path <path>]` | Create a starter JSON configuration without overwriting existing files. |
| `targets` | List configured targets without connecting. |
| `new <snake_case_name>` | Create the next `NNN_name.sql` file without connecting. |
| `validate` | Check filenames, sequence, and supported SQL structure offline; report non-transactional files. |
| `init` | Create the selected history table idempotently. |
| `status` | Show `PENDING`, `BASELINE`, `APPLIED`, `APPLYING`, or `FAILED`. |
| `check` | Exit nonzero for pending, dirty, missing, or changed history. |
| `apply` | Apply pending files in order. |
| `baseline --versions 001-003` | Record audited existing changes without executing their SQL. |
| `repair --version 003 --action retry` | Remove a dirty record after an operator has made retry safe. |
| `repair --version 003 --action applied` | Mark a dirty record applied after an operator has verified the complete change. |
| `version` | Print the build version. |

All errors produce a nonzero exit code. Database commands default to a five-minute
deadline, including connection and advisory-lock waits. Override with
`--timeout 30m` before the command for a longer migration. Lock acquisition also
has its own 30-second limit, adjustable with `--lock-timeout 2m`. The shorter
of the command deadline and lock deadline applies. Lock waits use try-lock polling
so a timeout does not leave a server-side advisory-lock wait behind.

## Configuration and credentials

Project and credential JSON reject duplicate keys (including keys differing only
by case), unknown fields, control characters in target values, and trailing
objects. An unreadable nearer project or invalid migration directory stops
discovery instead of selecting an ancestor project. Each target supports:

| Field | Default / purpose |
| --- | --- |
| `database` | Required actual PostgreSQL database name; checked after connecting. |
| `migrations_dir` | Required directory of this target's SQL files. |
| `dsn_env` | Optional environment variable containing the connection string. |
| `credentials_file` | Optional JSON credentials file; cannot be combined with `dsn_env`. |
| `ledger_table` | `public.schema_migrations`. Use lowercase `schema.table`. |
| `role` | Optional PostgreSQL role that ddl-mt switches to for all database work, so the history table and migrated objects are owned by it. See [Creating objects as an owner role](#creating-objects-as-an-owner-role). |
| `lock_key` | Optional 64-bit integer advisory-lock key; defaults to a fixed ddl-mt key. Set it only to coordinate with another runner that already uses a specific key. |
| `connection_mode` | `transaction` (default): safe for direct PostgreSQL and transaction/session pooling. `session`: requires a direct or verified session-pooled endpoint; enables non-transactional execution when opted in. |
| `allow_non_transactional` | `false`; explicit opt-in for SQL that cannot run atomically. |

Credential precedence:

1. An explicit `--config /path/to/credentials.json`.
2. The selected target's `credentials_file` or `dsn_env`.
3. `DB_DSN`, if no target credential source was specified.
4. `<user-config>/ddl-mt/<project>/<target>.json`.

A selected file or environment variable must be valid and non-empty. It never
falls back to a different source. Project and target names keep default credential
files separate even when multiple repositories use the same database name.

Credential files contain only:

```json
{"DB_DSN": "postgresql://username:password@hostname:5432/shop"}
```

Keep credentials outside version control and restrict file access to the operator
(`chmod 600` on Unix). Linux uses `$XDG_CONFIG_HOME` or `~/.config`; other platforms
use Go's standard user configuration directory. Files are parsed as JSON, with no
shell expansion or `.env` loading. Connection strings are not accepted as CLI
arguments. Offline commands do not read credentials.

For independent repositories sharing **one physical database**, assign different
history tables, such as `public.shop_migrations` and `public.billing_migrations`.
One history table must correspond to exactly one migration sequence. Custom
schemas must already exist. Advisory locks conservatively serialize all
histories within the database.

Every writer of one history table must use the same `lock_key`. Runners with
different keys do not exclude each other and can apply the same migration
concurrently, so change the key only while all other writers are stopped.

### Creating objects as an owner role

PostgreSQL makes the creating role the owner of each new object. To keep
ownership with one role regardless of who runs migrations, connect as a login
that has no rights of its own and name the owner in the target:

```json
"role": "shop_owner"
```

```sql
CREATE ROLE shop_owner NOLOGIN;
CREATE ROLE shop_migrate LOGIN NOINHERIT PASSWORD '...';
GRANT shop_owner TO shop_migrate;
```

`NOINHERIT` matters: the login may switch to the owner but holds none of its
privileges until it does, so SQL that runs without the switch fails with a
permission error instead of creating an object owned by the login.

With `role` set:

- Every transaction ddl-mt opens (`init`, `status`, `check`, `apply`, `baseline`,
  `repair`) starts with `SET LOCAL ROLE`, before the lock, the migration SQL,
  and the history write. The switch ends at commit or rollback, so it is safe
  with PgBouncer transaction pooling.
- In `connection_mode: "session"`, `apply` also runs `SET ROLE` for the session,
  covering non-transactional migrations, and `RESET ROLE` when the run ends.
- Every database command first verifies the switch and refuses to start if the
  role does not exist or the login is not a member of it.

The history table must be usable by the role; `init` creates it as the role.
`--role <name>` before the command overrides the target and also works in direct
mode. The name is quoted as an identifier, so it is case-sensitive. A migration
that changes the role itself (`SET ROLE`, `RESET ROLE`) overrides the switch for
its remaining statements.

### Direct mode

Simple usage does not require a project file:

```sh
ddl-mt --database example --migrations-root /srv/app/migrations validate
ddl-mt --database example --config /secure/example.json status
```

Direct mode selects `migrations/<database>`, supports `MIGRATIONS_DATABASE` and
`MIGRATIONS_ROOT`, and uses `<user-config>/ddl-mt/<database>.json` as its default
credential file. It uses the default `public.schema_migrations` history table; use
a named target to choose another. Direct-mode selectors cannot be combined with project
targets, including when supplied through environment variables.

## PostgreSQL and PgBouncer

The default is `connection_mode: "transaction"`. Each atomic migration
acquires a transaction advisory lock, rechecks the complete history, executes SQL,
and records success in the same transaction. Commit or rollback releases the lock.
Concurrent runners may alternate between files; each rereads history under the lock
before executing, so a file is not applied twice. Earlier files remain committed
if a later file fails. Initialization, baseline, and repair also use transaction locks.

This mode works with direct PostgreSQL, PgBouncer transaction pooling, and session
pooling. Ports do not determine behavior, and the CLI does not guess pooling mode.
PgBouncer statement pooling is not supported. Keep each migration self-contained;
use `SET LOCAL` for transaction settings, and do not rely on session state, temporary
objects, or session locks surviving between migration files. Migration SQL remains
trusted code; ddl-mt cannot make arbitrary session-dependent SQL pool-safe.

For a transaction-pooled target, add this field (or omit it to use the default):

```json
"connection_mode": "transaction"
```

Non-transactional SQL requires a direct PostgreSQL or verified session-pooled
endpoint and both target settings:

```json
"connection_mode": "session",
"allow_non_transactional": true
```

In session mode, `apply` holds a session advisory lock across the entire run and
explicitly releases it on completion or failure while the connection is usable.
Do not select session mode on a transaction-pooled endpoint: PostgreSQL backend
connections can change between transactions and the lock no longer protects the run.
See [PgBouncer's compatibility table](https://www.pgbouncer.org/features.html).

Use separate targets with the same database, migration directory, and ledger table
when you need a pooled connection for normal migrations and a direct connection for
non-transactional migrations. The default mode refuses **all pending work** before
executing SQL if any pending file requires non-transactional execution. Already
applied non-transactional files do not prevent later atomic migrations.
`--connection-mode transaction|session` overrides the target setting; it does not
change the credentials or endpoint.

When upgrading from 0.1.x, existing non-transactional workflows must explicitly
select session mode with a suitable endpoint. Existing ledgers and lock keys are
unchanged. Old leaked session locks still block the new transaction locks: a database
administrator must inspect and clear those stale sessions separately. The CLI never
terminates other connections automatically.

## Connection diagnostics

Connection failures report safe categories such as authentication rejection,
connection refusal, DNS failure, timeout, or TLS certificate verification failure.
When PostgreSQL supplies a SQLSTATE code, it is included. Raw connection strings
and server messages are omitted because they can contain credentials. Unclassified
failures require inspecting the database or connection-pool logs.

`status` and `apply` use the same credentials and connection settings. The write
capability check and migration lock happen after connecting. A connection failure
before those checks is distinct from a read-only connection or SQL permission error.

## SQL and execution guarantees

- Files use three-digit versions starting at `001`, with no gaps or duplicate
  versions. The current format supports up to `999`.
- File bytes are SHA-256 checksummed before transaction wrappers are removed.
  Applied files must never be renamed or edited.
- SQL files may contain plain SQL or one outer `BEGIN;` / `COMMIT;` pair.
  PostgreSQL dollar-quoted function and `DO` bodies are supported. Embedded
  transaction commands and `psql` commands are rejected.
- Each atomic migration and its history insert share one transaction. A failure
  rolls back that migration; earlier successfully committed migrations remain.
- The database name is checked for every connection. Write commands reject
  read-only connections. A `Target database` comment, when present, must also match.
- Transaction mode locks and validates history for each atomic migration; session
  mode holds the lock across the run. Both conflict with baseline, repair, and
  initialization locks.
- History must be a contiguous prefix of repository files. Missing files, changed
  checksums, and dirty states block further application. `status` displays dirty
  states and also exits nonzero.
- Metadata directives are standalone line comments between statements. Text inside
  strings, dollar-quoted bodies, and block comments is never migration metadata.
- `validate` checks structure, not full PostgreSQL syntax or schema semantics.
  Migration SQL is trusted code and must be reviewed. Database errors can include
  SQL details; consider log access when migrations contain sensitive data.

Non-transactional SQL such as `CREATE INDEX CONCURRENTLY` requires
`allow_non_transactional: true` and `connection_mode: "session"` on a direct or
verified session-pooled endpoint. Mark a file with `-- migration:transaction=false`
to declare it non-transactional; concurrent index operations are also detected
automatically. The marker does not bypass the configuration requirements. A durable `applying` row is written before any SQL runs. If execution
fails, a `failed` or `applying` row blocks retries until explicit operator repair.
These migrations can leave partial changes. See [recovery](docs/adoption.md#recovering-a-non-transactional-migration).

There is no automatic rollback/down-migration, schema diffing, cross-database
transaction, or automatic application across all targets. Correct a deployed
change with a new forward migration.

## Development

```sh
make check          # race tests, vet, build; DB tests skip without a test DSN
make test-postgres  # full suite using a temporary local PostgreSQL cluster
make test-pgbouncer # also test real transaction/session pools (needs PgBouncer)
```

`test-postgres` needs PostgreSQL server binaries and `pg_config`. Set `PG_BINDIR`
if necessary. It opens only a private Unix socket and deletes the cluster on exit.
`test-pgbouncer` also starts two private PgBouncer processes; set `PGBOUNCER_BIN`
if it is not on PATH. Tests cover concurrent application, rollback, repair, lock
timeouts and cleanup, preflight rejection, and session-pooled concurrent indexes.
Alternatively set `DDL_MT_TEST_DSN` to a **disposable test server** using libpq
`key=value` syntax, with a role allowed to create databases, then run `go test -race
./...`. Integration tests create and drop their own databases. CI runs them against
PostgreSQL 16, 17, and 18; it never deploys or migrates an application database.

See the [testing guide](docs/testing.md) for test contracts, isolation, coverage,
and fuzzing, and [CONTRIBUTING.md](CONTRIBUTING.md) for contribution requirements.

## Contributing and security

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Notable changes are listed in the
[changelog](CHANGELOG.md).

Please report vulnerabilities privately as described in [SECURITY.md](SECURITY.md),
not in public issues.

## Credits

ddl-mt is developed, maintained, and open-sourced by
[**QBite Technologies**](https://qbite.tech). Contact:
[opensource@qbite.tech](mailto:opensource@qbite.tech).

Thanks to everyone who reports issues and contributes improvements, and to the
[pgx](https://github.com/jackc/pgx) project, which ddl-mt uses for PostgreSQL connectivity.

## License

Copyright © 2026 QBite Technologies. Released under the [MIT License](LICENSE).
