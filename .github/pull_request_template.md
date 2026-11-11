## What changes

<!-- Describe the concrete behavior change and why. Link related issues. -->

## How it was tested

<!-- Commands run, e.g. `make check`, `make test-postgres`, `make test-pgbouncer`. -->

## Checklist

- [ ] `make check` passes; `make test-pgbouncer` for execution, parsing, locking, or ledger changes
- [ ] Regression tests added for behavior changes
- [ ] Docs (README / docs/) and CHANGELOG.md updated where behavior changed
- [ ] No compatibility change to checksums, history columns, version numbering, lock keys, or config precedence, or it is explained with a migration path
- [ ] No credentials, private SQL, database dumps, or binaries included
