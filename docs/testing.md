# Testing ddl-mt

The suite checks migration behavior and recovery against real PostgreSQL, with
fast unit tests for local configuration and parsing. A successful unit-only run
can still report skipped database tests; use the integration commands before
changing execution, locking, history, or SQL parsing.

## Commands and prerequisites

| Command | Dependencies | Checks |
| --- | --- | --- |
| `make check` | Go 1.26+ | Race-enabled unit tests, vet, and build; database tests run only if their test DSNs are set. |
| `make test-postgres` | Go, PostgreSQL server binaries, `pg_config` | Unit tests plus isolated real-database integration tests. |
| `make test-pgbouncer` | Above plus PgBouncer | All tests, including real transaction and session pools. |
| `go test -run '^$' -fuzz FuzzPrepareMigration -fuzztime=20s -parallel=2` | Go | SQL-boundary mutation testing; normal test runs execute the seed corpus only. |

Set `PG_BINDIR` or `PGBOUNCER_BIN` if the executables are outside PATH. Missing
integration dependencies cause the runner to fail instead of silently reducing
coverage. The disposable runner has a two-minute test deadline; additional Go test
flags can be passed directly:

```sh
bash scripts/test-postgres.sh -run TestPostgresAtomicRollback -v
DDL_MT_TEST_PGBOUNCER=1 bash scripts/test-postgres.sh -coverprofile=/tmp/ddl-mt.cover
go tool cover -func=/tmp/ddl-mt.cover
go tool cover -html=/tmp/ddl-mt.cover
```

Coverage helps locate missing scenarios. It is not a correctness target: transport
failures and process exit paths may remain uncovered, while successful atomicity,
compatibility, and recovery must be demonstrated with observable database state.

## Isolation and skipped tests

The runner creates a temporary PostgreSQL cluster with trust authentication on a
private Unix socket. TCP listening is disabled. Pool tests start separate private
transaction/session PgBouncer instances. Cleanup stops only those processes and
removes the cluster. Run as an ordinary user; PostgreSQL refuses to initialize a
cluster as root. No application credentials are required.

For a separately managed **disposable** server, set `DDL_MT_TEST_DSN` using libpq
`key=value` syntax with a role allowed to create databases. Each database test
creates a uniquely named database and registers cleanup to drop it. The optional
`DDL_MT_TEST_TRANSACTION_DSN` and `DDL_MT_TEST_SESSION_DSN` select real pools that
must route those same database names to that server. Never point these variables
at an application database server.

Without `DDL_MT_TEST_DSN`, real-database tests skip. Without the corresponding pool
DSNs, pool subtests skip. Credential-path tests use temporary XDG configuration
and skip when the platform cannot safely isolate that path. Symlink-specific tests
skip if symlinks are unavailable. `go test -v ./...` reports individual skips.

Tests changing environment variables, working directories, or package globals must
not use `t.Parallel`. Concurrency tests start multiple runners inside one test,
collect their errors, and wait for completion before fixture cleanup.

## Behavior contracts

| Area | Tests and observable assertions |
| --- | --- |
| Configuration and CLI | `config_test.go`, `main_test.go`: explicit target selection, relative paths, repository boundaries, unreadable nearer paths, duplicate JSON rejection, argument errors, deterministic offline target listing. |
| Credentials | `credentials_test.go`: source precedence, no fallback from a broken selected source, separate project defaults, literal JSON values, secret-safe diagnostics. |
| Connection diagnostics | `connection_errors_test.go`: typed/wrapped failures never expose raw driver/server text; a real missing database yields the expected SQLSTATE category. |
| SQL structure | `migrations_test.go`: quotes, continued escape strings, nested comments, wrappers, metadata scope, true concurrent-index options, immutable byte checksums, sequence rules; fuzz seeds guard parser stability. |
| First-time setup | `scaffold_test.go`: destination paths, deliberately incomplete starter settings, preservation of existing files and dangling symlinks, no database access. |
| Audit and version metadata | `helpers_test.go`: fixed checksum digest, line-ending drift, actor/revision precedence, explicit and embedded version selection. |
| Database changes | `integration_test.go`: CLI-to-database workflow, successful application, rollback on SQL or ledger failure, all-or-nothing baselines, partial non-atomic failure, repair restrictions, wrong/read-only databases, isolated histories, SQL lexical cases verified by PostgreSQL. |
| Locking and pooling | `pooling_test.go`: competing runners, complete-pending-list preflight, command/lock deadlines, mixed lock modes, pool reuse after timeout, no held or waiting migration locks after cleanup. |
| Documentation | `documentation_test.go`: every named Go function and method—including tests and helpers—has a comment beginning with its name. |

The SQL splitter is not a PostgreSQL parser. PostgreSQL remains the authority for
SQL syntax and schema semantics. Parser tests should include both accepted and
rejected cases, and an integration test when statement boundaries or execution
mode could change. Reference PostgreSQL's [lexical rules](https://www.postgresql.org/docs/current/sql-syntax-lexical.html)
and [REINDEX options](https://www.postgresql.org/docs/current/sql-reindex.html)
when adding lexical fixtures.

## Adding a regression test

1. Reproduce the defect with a named case and a failure message explaining the
   expected contract. Add a function comment explaining why the case matters.
2. Assert observable outcomes: schema objects, exact file bytes, ledger state,
   history metadata, output, or released locks. Avoid only asserting a helper's
   implementation against another copy of the same logic.
3. Fix the implementation and run the affected tests, followed by the required
   integration suite. For parser changes, run a bounded fuzz session as well.
4. Update configuration/operator documentation when accepted input or execution
   behavior changes. Keep existing SQL bytes, the ledger layout, and the lock key stable
   unless an explicit compatibility transition is documented.

CI runs PostgreSQL 16, 17, and 18 plus a separate real-PgBouncer job. A local run
proves behavior only for the locally installed database/pool versions; the CI
matrix provides the broader version checks.
