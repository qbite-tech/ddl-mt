// Command ddl-mt applies checksum-verified PostgreSQL migrations selected by a
// repository's project configuration. Atomic migrations and their ledger records
// commit together; explicitly enabled non-transactional work requires session
// affinity and durable recovery markers. See README.md for operator configuration
// and docs/testing.md for the test contracts and disposable database workflows.
package main
