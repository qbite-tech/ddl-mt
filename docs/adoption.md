# Adopting an existing database

`ddl-mt` records history in its own table (`public.schema_migrations` by default,
or the target's `ledger_table`). It does not read or convert history tables
written by other migration tools; if another tool already owns the default table
name, choose a different `ledger_table`.

1. Add `ddl-mt.json` and put the SQL files that describe the existing schema in
   the target's migration directory as `001_...sql`, `002_...sql`, and so on.
2. Run `validate`, then `init` to create the history table.
3. Audit which files are already reflected in the database and record exactly
   those with `baseline --versions 001-010`. Baselining runs no SQL.
4. Run `status` or `check`, then `apply` for the remaining pending files.
5. Update application CI and deployment commands to use the installed `ddl-mt`.

Do not baseline merely to silence a failed check. A baseline selection must leave
a contiguous history. Once recorded, file bytes must not change: checksums are
compared on every run.

Validation is strict: malformed SQL filenames, embedded transaction control, and
empty SQL files fail. A file may keep one outer `BEGIN; ... COMMIT;` wrapper and
an optional `-- Target database: <name>` comment, which must match the target.

## Reusing an existing history table

A table that already has the ddl-mt columns can be used as is: set `ledger_table`
to its name. If the runner that wrote it used its own advisory-lock key, set the
target's `lock_key` to that number so both runners exclude each other during the
switch. Run `status` before the first `apply` and confirm the recorded history.

### Upgrading a table without status columns

A history table that records only applied rows (`version`, `filename`,
`checksum`, `applied_at`, `applied_by`, `git_sha`, `execution_ms`, `is_baseline`)
needs the status columns once. Stop the previous runner first: it does not know
the new columns. The change only adds columns and relaxes one constraint, so
read-only consumers keep working. Adjust the table name and run it in the target
database:

```sql
BEGIN;
ALTER TABLE public.schema_migrations
    ADD COLUMN status text NOT NULL DEFAULT 'applied'
        CHECK (status IN ('applying', 'applied', 'failed')),
    ADD COLUMN started_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN error_message text,
    ALTER COLUMN applied_at DROP NOT NULL,
    ALTER COLUMN execution_ms DROP NOT NULL;
UPDATE public.schema_migrations SET started_at = applied_at;
ALTER TABLE public.schema_migrations
    ALTER COLUMN status DROP DEFAULT,
    ADD CHECK ((status = 'applied' AND applied_at IS NOT NULL AND error_message IS NULL)
        OR (status <> 'applied' AND applied_at IS NULL));
COMMIT;
```

Then run `status` with ddl-mt and confirm every existing row is reported as
`APPLIED` or `BASELINE`.

## Recovering a non-transactional migration

A failed non-transactional file may have committed some statements. A lost
connection or interrupted process may leave status `applying` even when SQL
finished. Both states block further application and fail `check`.

Inspect the database and the exact migration file before choosing either action:

```sh
# After restoring a state where executing the unchanged migration is safe:
ddl-mt --target production repair --version 026 --action retry
ddl-mt --target production apply

# After verifying that every intended change is complete:
ddl-mt --target production repair --version 026 --action applied
ddl-mt --target production check
```

`retry` removes only the dirty history row; it does not undo partial SQL or execute
anything. `applied` updates only the history record. Neither permits changing the
recorded checksum, rewriting an applied row, or repairing beneath later history.
Concurrent index failures may leave an invalid index; `IF NOT EXISTS` alone does
not prove that a retry is safe. These actions are operator assertions, not
automatic database repair.

Atomic migrations need no history repair after failure: their changes and ledger
insert roll back together. Correct the pending file and run `apply` again.
