# Security policy

ddl-mt connects to databases with privileged credentials and executes schema
changes, so we take security reports seriously.

## Supported versions

ddl-mt is in initial development (`0.x`). Security fixes are released for the
latest minor version only. Please upgrade to the newest release before reporting.

| Version | Supported |
| --- | --- |
| Latest `0.x` minor release | Yes |
| Older releases | No |

## Reporting a vulnerability

**Please do not open a public issue, discussion, or pull request for security
problems.**

Report vulnerabilities privately through GitHub:
[**Report a vulnerability**](https://github.com/qbite-tech/ddl-mt/security/advisories/new)
(Security tab → *Report a vulnerability*).

If you cannot use GitHub's private reporting, email
[opensource@qbite.tech](mailto:opensource@qbite.tech).

Please include:

- the affected version (`ddl-mt version`) and PostgreSQL / PgBouncer versions;
- a description of the issue and its impact;
- minimal reproduction steps, configuration, or migration files.

**Never include real credentials, connection strings, hostnames, or production
data** in a report. Use placeholders or a disposable database.

We aim to acknowledge reports within five business days and to keep you
informed while we investigate and prepare a fix. We will coordinate disclosure
with you and credit you in the advisory unless you prefer to remain anonymous.

## Scope

In scope are defects in ddl-mt itself, for example:

- credentials or connection strings leaking into output, errors, or logs;
- applying migrations to the wrong database or bypassing the database-name check;
- checksum, history, or locking flaws that let a migration run twice, run out of
  order, or be silently altered;
- unsafe handling of project, credential, or migration file paths.

Out of scope: the content of your own migration SQL (it is trusted code that runs
with the connecting role's privileges), vulnerabilities in PostgreSQL or PgBouncer
themselves, and issues that require an attacker who can already modify your
migration files or credentials.

## Operator guidance

- Keep credentials out of version control; prefer `dsn_env` or a `credentials_file`
  with `chmod 600` outside the repository.
- Use a dedicated migration role with only the privileges your migrations need.
- Use `sslmode=verify-full` for connections over untrusted networks.
- Verify downloaded release archives against `checksums.txt`.
