package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const advisoryLockKey = int64(6_617_432_915)

type ledgerQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type sqlExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// table quotes the validated schema and table components as PostgreSQL
// identifiers; user-provided names are never interpolated as raw SQL.
func (o options) table() string {
	return pgx.Identifier(strings.Split(o.LedgerTable, ".")).Sanitize()
}

// lockKey returns the target's configured advisory-lock key, or the default
// database-scoped key. Every writer of one history table must use the same key.
func (o options) lockKey() int64 {
	if o.LockKey != nil {
		return *o.LockKey
	}
	return advisoryLockKey
}

// acquireMigrationLock polls for a session or transaction advisory lock until
// the earlier command/lock deadline. Try-lock polling avoids leaving a server-side
// lock wait behind when a pool cannot forward cancellation reliably.
func acquireMigrationLock(ctx context.Context, q ledgerQueryer, o options, session bool) error {
	wait := o.LockTimeout
	if wait == 0 {
		wait = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	query := "SELECT pg_try_advisory_xact_lock($1)"
	if session {
		query = "SELECT pg_try_advisory_lock($1)"
	}
	for {
		var acquired bool
		if err := q.QueryRow(ctx, query, o.lockKey()).Scan(&acquired); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if acquired {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("migration lock unavailable: another runner or a stale session holds it; check active migrations and pool connections (lock wait limit %s): %w", wait, ctx.Err())
		case <-timer.C:
		}
	}
}

// setRole switches the connection or transaction to the configured role and does
// nothing when no role is configured. A local switch lasts until the enclosing
// transaction ends, which keeps it safe under transaction pooling; a session
// switch lasts until RESET ROLE and requires a direct or session-pooled endpoint.
func setRole(ctx context.Context, q sqlExecutor, o options, local bool) error {
	if o.Role == "" {
		return nil
	}
	statement := "SET ROLE "
	if local {
		statement = "SET LOCAL ROLE "
	}
	if _, err := q.Exec(ctx, statement+pgx.Identifier{o.Role}.Sanitize()); err != nil {
		return fmt.Errorf("cannot switch to role %q; the role must exist and the connecting login must be a member of it: %w", o.Role, err)
	}
	return nil
}

// beginAsRole starts a transaction and, when a role is configured, switches to
// it with SET LOCAL ROLE before any other statement. The switch reverts at commit
// or rollback. The caller owns the returned transaction.
func beginAsRole(ctx context.Context, c *pgx.Conn, o options, txOptions pgx.TxOptions) (pgx.Tx, error) {
	tx, err := c.BeginTx(ctx, txOptions)
	if err != nil {
		return nil, err
	}
	if err := setRole(ctx, tx, o, true); err != nil {
		tx.Rollback(context.Background())
		return nil, err
	}
	return tx, nil
}

// lockTransaction acquires the migration advisory lock on an existing transaction.
// The caller must commit or roll back that transaction to release the lock.
func lockTransaction(ctx context.Context, q ledgerQueryer, o options) error {
	return acquireMigrationLock(ctx, q, o, false)
}

// bootstrapSQL builds the history table with safely quoted identifiers. It
// creates a fresh table and never rewrites an existing one.
func bootstrapSQL(o options) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
        version text PRIMARY KEY CHECK (version ~ '^[0-9]{3}$'),
        filename text NOT NULL UNIQUE,
        checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
        status text NOT NULL CHECK (status IN ('applying','applied','failed')),
        started_at timestamptz NOT NULL DEFAULT now(), error_message text,
        CHECK ((status = 'applied' AND applied_at IS NOT NULL AND error_message IS NULL)
            OR (status <> 'applied' AND applied_at IS NULL)),
        applied_at timestamptz DEFAULT now(), applied_by text NOT NULL,
        git_sha text, execution_ms integer, is_baseline boolean NOT NULL DEFAULT false
    )`, o.table())
}

// readLedger checks that the selected history table exists and has the ddl-mt
// status column, then reads ordered records. Callers performing
// writes must hold the migration lock while reading and acting on these records.
func readLedger(ctx context.Context, q ledgerQueryer, o options) ([]migrationRecord, error) {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", o.LedgerTable).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check migration ledger: %w", err)
	}
	if !exists {
		return nil, errors.New("migration ledger is missing; run ddl-mt with the same target and init")
	}
	var hasStatus bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass($1)
        AND attname='status' AND NOT attisdropped
    )`, o.LedgerTable).Scan(&hasStatus); err != nil {
		return nil, err
	}
	if !hasStatus {
		return nil, fmt.Errorf("table %s is not a ddl-mt history table; refusing to reinterpret it (set ledger_table to a different name)", o.LedgerTable)
	}
	rows, err := q.Query(ctx, "SELECT version, filename, checksum, is_baseline, status FROM "+o.table()+" ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	var records []migrationRecord
	for rows.Next() {
		var r migrationRecord
		if err := rows.Scan(&r.Version, &r.Filename, &r.Checksum, &r.IsBaseline, &r.Status); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// ledgerByVersion validates filenames, checksums, unique versions, and the
// contiguous applied prefix against repository files. It returns indexed records
// without interpreting dirty status; requireCleanLedger performs that check.
func ledgerByVersion(repository, ledger []migrationRecord) (map[string]migrationRecord, error) {
	known := make(map[string]migrationRecord, len(repository))
	for _, m := range repository {
		known[m.Version] = m
	}
	result := make(map[string]migrationRecord, len(ledger))
	for _, row := range ledger {
		m, ok := known[row.Version]
		if !ok {
			return nil, fmt.Errorf("ledger contains migration %s without a repository file", row.Version)
		}
		if row.Filename != m.Filename {
			return nil, fmt.Errorf("%s: ledger filename is %s", m.Filename, row.Filename)
		}
		if row.Checksum != m.Checksum {
			return nil, fmt.Errorf("%s: checksum differs from the applied migration", m.Filename)
		}
		if _, ok := result[row.Version]; ok {
			return nil, fmt.Errorf("duplicate ledger version %s", row.Version)
		}
		result[row.Version] = row
	}
	gap := false
	for _, m := range repository {
		if _, ok := result[m.Version]; !ok {
			gap = true
		} else if gap {
			return nil, fmt.Errorf("ledger is not a contiguous prefix: found %s after a missing version", m.Version)
		}
	}
	return result, nil
}

// requireCleanLedger rejects applying, failed, or unknown execution states so
// partial non-transactional changes cannot be retried automatically.
func requireCleanLedger(ledger []migrationRecord) error {
	for _, r := range ledger {
		if r.Status != "applied" {
			return fmt.Errorf("%s has status %s; audit the database and use repair before continuing", r.Filename, r.Status)
		}
	}
	return nil
}

// insertRecord writes one history row with actor, revision, timing, and baseline
// metadata. gitSHA is a string or nil. The supplied
// executor determines whether the row commits atomically with SQL or serves as
// a durable non-atomic marker.
func insertRecord(ctx context.Context, q sqlExecutor, o options, m migrationRecord, gitSHA any, ms int, baseline bool, status string) error {
	columns := "version, filename, checksum, applied_by, git_sha, execution_ms, is_baseline, status, applied_at"
	values := "$1,$2,$3,$4,$5,$6,$7,$8,CASE WHEN $8 = 'applied' THEN now() ELSE NULL END"
	args := []any{m.Version, m.Filename, m.Checksum, getAppliedBy(), gitSHA, ms, baseline, status}
	_, err := q.Exec(ctx, "INSERT INTO "+o.table()+" ("+columns+") VALUES ("+values+")", args...)
	if err != nil {
		return fmt.Errorf("record migration %s: %w", m.Filename, err)
	}
	return nil
}

// initCommand verifies the writable target and initializes its history table
// idempotently under a transaction lock. It does not convert existing schemas
// or execute application-specific bootstrap SQL.
func initCommand(ctx context.Context, o options, out io.Writer) error {
	c, err := connectAndVerify(ctx, o, true)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	tx, err := beginAsRole(ctx, c, o, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := lockTransaction(ctx, tx, o); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, bootstrapSQL(o)); err != nil {
		return fmt.Errorf("initialize ledger: %w", err)
	}
	if _, err := readLedger(ctx, tx, o); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "Migration ledger %s initialized in %s.\n", o.LedgerTable, o.Database)
	return nil
}

// statusCommand reads and validates history, then prints migration states or
// performs a complete-application check. Dirty history fails in either mode;
// pending files fail only when check is true. It never changes the database.
func statusCommand(ctx context.Context, o options, migrations []migration, check bool, out io.Writer) error {
	c, err := connectAndVerify(ctx, o, false)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	// A read-only transaction scopes the optional role switch, so status also
	// works through transaction pooling for a login without rights of its own.
	tx, err := beginAsRole(ctx, c, o, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	ledger, err := readLedger(ctx, tx, o)
	if err != nil {
		return err
	}
	byVersion, err := ledgerByVersion(migrationRecords(migrations), ledger)
	if err != nil {
		return err
	}
	pending := 0
	for _, m := range migrations {
		status := "PENDING"
		if row, ok := byVersion[m.Version]; ok {
			status = strings.ToUpper(row.Status)
			if row.IsBaseline && row.Status == "applied" {
				status = "BASELINE"
			}
		} else {
			pending++
		}
		if !check {
			fmt.Fprintf(out, "%s\t%s\t%s\n", m.Version, status, m.Filename)
		}
	}
	if err := requireCleanLedger(ledger); err != nil {
		return err
	}
	if check && pending > 0 {
		return fmt.Errorf("migration verification failed: %d migration(s) pending", pending)
	}
	if check {
		fmt.Fprintf(out, "Migration check passed for %s: %d migration(s) applied.\n", o.Database, len(migrations))
	}
	return nil
}

// baselineCommand records an explicitly audited selection without executing SQL.
// It locks history, requires a contiguous prefix, and commits all new baseline
// rows together; existing matching rows remain unchanged.
func baselineCommand(ctx context.Context, o options, migrations []migration, root, selection string, out io.Writer) error {
	gitSHA := getGitSHA(ctx, root)
	known := make(map[string]struct{})
	for _, m := range migrations {
		known[m.Version] = struct{}{}
	}
	versions, err := parseVersionSelection(selection, known)
	if err != nil {
		return err
	}
	selected := make(map[string]bool)
	for _, v := range versions {
		selected[v] = true
	}
	c, err := connectAndVerify(ctx, o, true)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	tx, err := beginAsRole(ctx, c, o, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := lockTransaction(ctx, tx, o); err != nil {
		return err
	}
	ledger, err := readLedger(ctx, tx, o)
	if err != nil {
		return err
	}
	repository := migrationRecords(migrations)
	byVersion, err := ledgerByVersion(repository, ledger)
	if err != nil {
		return err
	}
	if err := requireCleanLedger(ledger); err != nil {
		return err
	}
	gap := false
	var added []string
	for _, m := range repository {
		_, exists := byVersion[m.Version]
		if !exists && !selected[m.Version] {
			gap = true
			continue
		}
		if gap {
			return fmt.Errorf("baseline must remain a contiguous prefix; cannot include %s", m.Version)
		}
		if exists {
			continue
		}
		if err := insertRecord(ctx, tx, o, m, gitSHA, 0, true, "applied"); err != nil {
			return err
		}
		added = append(added, m.Filename)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, name := range added {
		fmt.Fprintf(out, "Baselined: %s\n", name)
	}
	return nil
}

// applyCommand opens the verified target and applies pending files using the
// configured locking mode. Transaction mode supports transaction pooling; session
// mode requires the operator to select a direct or session-pooled endpoint.
func applyCommand(ctx context.Context, o options, migrations []migration, root string, out io.Writer) error {
	// Resolve audit metadata before connecting so Git never runs under a lock.
	gitSHA := getGitSHA(ctx, root)
	if err := validateConnectionMode(o.ConnectionMode); err != nil {
		return err
	}
	c, err := connectAndVerify(ctx, o, true)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if o.ConnectionMode == "session" {
		return applySession(ctx, c, o, migrations, gitSHA, out)
	}
	for {
		done, err := applyNextTransaction(ctx, c, o, migrations, gitSHA, out)
		if err != nil || done {
			return err
		}
	}
}

// pendingMigrations validates complete history and every pending execution mode
// before returning work. Its caller must hold the migration lock, preventing a
// stale read from authorizing SQL while another runner changes history.
func pendingMigrations(ctx context.Context, q ledgerQueryer, o options, migrations []migration) ([]migration, error) {
	ledger, err := readLedger(ctx, q, o)
	if err != nil {
		return nil, err
	}
	byVersion, err := ledgerByVersion(migrationRecords(migrations), ledger)
	if err != nil {
		return nil, err
	}
	if err := requireCleanLedger(ledger); err != nil {
		return nil, err
	}
	var pending []migration
	// Preflight the entire pending sequence before executing its first file.
	for _, m := range migrations {
		if _, exists := byVersion[m.Version]; exists {
			continue
		}
		if m.NonTransactional {
			if !o.AllowNonTransactional {
				return nil, fmt.Errorf("%s is non-transactional; configure allow_non_transactional after reviewing its recovery procedure", m.Filename)
			}
			if o.ConnectionMode != "session" {
				return nil, fmt.Errorf("%s is non-transactional and cannot run with connection_mode=transaction; use a direct PostgreSQL or verified session-pooled endpoint with connection_mode=session", m.Filename)
			}
		}
		pending = append(pending, m)
	}
	return pending, nil
}

// applyNextTransaction locks and rereads history at READ COMMITTED, then executes
// and records at most one pending file atomically. The boolean result is true
// when no pending work remains; callers must reacquire history for the next file.
func applyNextTransaction(ctx context.Context, c *pgx.Conn, o options, migrations []migration, gitSHA any, out io.Writer) (bool, error) {
	// READ COMMITTED is deliberate: history must include a competing runner's
	// commit after we acquire the lock, even with a different database default.
	tx, err := beginAsRole(ctx, c, o, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	if err := lockTransaction(ctx, tx, o); err != nil {
		return false, err
	}
	pending, err := pendingMigrations(ctx, tx, o, migrations)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		return true, tx.Commit(ctx)
	}
	m := pending[0]
	fmt.Fprintf(out, "Applying: %s\n", m.Filename)
	start := time.Now()
	if err := executeAtomic(ctx, tx, o, m, gitSHA); err != nil {
		return false, fmt.Errorf("apply %s: %w", m.Filename, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit %s: %w", m.Filename, err)
	}
	fmt.Fprintf(out, "Applied: %s (%d ms)\n", m.Filename, time.Since(start).Milliseconds())
	// Reacquire and reread history before the next file: another runner may win
	// the lock between commits. Never execute from a stale pending list.
	return false, nil
}

// applySession holds a session advisory lock over preflight and all pending SQL.
// It releases the lock with an independent cleanup deadline and joins cleanup
// errors with execution errors. The connection must preserve session affinity.
func applySession(ctx context.Context, c *pgx.Conn, o options, migrations []migration, gitSHA any, out io.Writer) (result error) {
	if err := acquireMigrationLock(ctx, c, o, true); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if o.Role != "" {
			if _, err := c.Exec(cleanup, "RESET ROLE"); err != nil {
				result = errors.Join(result, fmt.Errorf("reset role: %w", err))
			}
		}
		var unlocked bool
		err := c.QueryRow(cleanup, "SELECT pg_advisory_unlock($1)", o.lockKey()).Scan(&unlocked)
		if err == nil && !unlocked {
			err = errors.New("migration session lock was lost; verify that the endpoint uses direct PostgreSQL or session pooling")
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("release migration lock: %w", err))
		}
	}()
	// Session mode reads history and runs non-transactional SQL outside a
	// transaction, so the role is switched for the session and reset on exit.
	if err := setRole(ctx, c, o, false); err != nil {
		return err
	}
	pending, err := pendingMigrations(ctx, c, o, migrations)
	if err != nil {
		return err
	}
	for _, m := range pending {
		fmt.Fprintf(out, "Applying: %s\n", m.Filename)
		start := time.Now()
		if m.NonTransactional {
			err = applyNonAtomic(ctx, c, o, m, gitSHA)
		} else {
			err = applyAtomic(ctx, c, o, m, gitSHA)
		}
		if err != nil {
			return fmt.Errorf("apply %s: %w", m.Filename, err)
		}
		fmt.Fprintf(out, "Applied: %s (%d ms)\n", m.Filename, time.Since(start).Milliseconds())
	}
	return nil
}

// applyAtomic executes and records one migration in its own transaction. Its
// session-mode caller must already hold the migration lock; failures roll back.
func applyAtomic(ctx context.Context, c *pgx.Conn, o options, m migration, gitSHA any) error {
	tx, err := beginAsRole(ctx, c, o, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := executeAtomic(ctx, tx, o, m, gitSHA); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// executeAtomic runs a migration and inserts its history row in the caller-owned
// transaction. It neither acquires a lock nor commits; the caller owns both.
func executeAtomic(ctx context.Context, tx pgx.Tx, o options, m migration, gitSHA any) error {
	start := time.Now()
	for _, statement := range m.Statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return insertRecord(ctx, tx, o, migrationRecords([]migration{m})[0], gitSHA, int(time.Since(start).Milliseconds()), false, "applied")
}

// applyNonAtomic durably records applying before executing statements outside a
// transaction, then records success or failure. Its caller must hold a session
// lock; interruption can leave an applying row that requires explicit repair.
func applyNonAtomic(ctx context.Context, c *pgx.Conn, o options, m migration, gitSHA any) error {
	// Reassert the session role: an earlier migration may have changed it.
	if err := setRole(ctx, c, o, false); err != nil {
		return err
	}
	if err := insertRecord(ctx, c, o, migrationRecords([]migration{m})[0], gitSHA, 0, false, "applying"); err != nil {
		return err
	}
	start := time.Now()
	for _, statement := range m.Statements {
		if _, err := c.Exec(ctx, statement); err != nil {
			// If the connection is lost, the durable 'applying' row still blocks retries.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, recordErr := c.Exec(cleanup, "UPDATE "+o.table()+" SET status='failed', error_message='SQL execution failed; audit before repair', execution_ms=$2 WHERE version=$1", m.Version, int(time.Since(start).Milliseconds()))
			return errors.Join(err, recordErr)
		}
	}
	_, err := c.Exec(ctx, "UPDATE "+o.table()+" SET status='applied', applied_at=now(), error_message=NULL, execution_ms=$2 WHERE version=$1", m.Version, int(time.Since(start).Milliseconds()))
	return err
}

// repairCommand locks and resolves one audited dirty history row without running
// migration SQL. Retry removes its marker; applied confirms completion. Checksums,
// existing applied rows, and later history cannot be bypassed.
func repairCommand(ctx context.Context, o options, migrations []migration, version, action string, out io.Writer) error {
	if action != "retry" && action != "applied" {
		return errors.New("repair action must be retry or applied")
	}
	c, err := connectAndVerify(ctx, o, true)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	tx, err := beginAsRole(ctx, c, o, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := lockTransaction(ctx, tx, o); err != nil {
		return err
	}
	ledger, err := readLedger(ctx, tx, o)
	if err != nil {
		return err
	}
	rows, err := ledgerByVersion(migrationRecords(migrations), ledger)
	if err != nil {
		return err
	}
	row, exists := rows[version]
	if !exists || (row.Status != "applying" && row.Status != "failed") {
		return errors.New("repair requires an applying or failed migration")
	}
	for _, r := range ledger {
		if r.Version > version {
			return errors.New("cannot repair a migration followed by later history")
		}
	}
	if action == "retry" {
		_, err = tx.Exec(ctx, "DELETE FROM "+o.table()+" WHERE version=$1", version)
	} else {
		_, err = tx.Exec(ctx, "UPDATE "+o.table()+" SET status='applied', applied_at=now(), error_message=NULL, applied_by=$2 WHERE version=$1", version, getAppliedBy())
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "Repaired %s: %s (SQL was not executed).\n", version, action)
	return nil
}
