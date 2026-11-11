# Contributing

Thanks for your interest in ddl-mt! This project is maintained by
[QBite Technologies](https://qbite.tech) and follows the
[Code of Conduct](CODE_OF_CONDUCT.md). Report security problems privately as
described in [SECURITY.md](SECURITY.md), never in a public issue or pull request.

For anything beyond a small fix, please open an issue first to discuss the change.

Keep application SQL and credentials in their application repositories. This
repository owns the generic CLI, format compatibility, and public examples.

Run `make check` and `make test-pgbouncer` before proposing changes to execution,
parsing, locking, or ledger semantics. Keep examples runnable with offline
`validate`. Add regression tests for behavior changes that could alter database
state or history. Format Go code with `gofmt`. Read [docs/testing.md](docs/testing.md)
for required test layers, isolation rules, coverage, and regression examples.
Document every named function and method, including tests and helpers, with a Go
doc comment beginning with its name. Explain side effects and caller-owned locks
or transactions where relevant; the suite checks documentation completeness.

Compatibility is part of the interface: do not silently change checksum inputs,
history columns, version numbering, lock keys, or configuration precedence. Explain
any compatibility changes and provide a migration path.

Pull requests should describe the concrete behavior change and how it was tested,
and add an entry under `Unreleased` in [CHANGELOG.md](CHANGELOG.md) for user-visible
changes.
Do not include database credentials, private SQL, database dumps, or generated
binaries. Contributions belong to `github.com/qbite-tech/ddl-mt` and are covered by
the repository's [MIT License](LICENSE).
