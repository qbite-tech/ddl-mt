package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// assertNoMigrationLocks verifies that neither held nor waiting migration locks
// remain in the test database, catching stranded direct or pooled backends.
func assertNoMigrationLocks(t *testing.T, c *pgx.Conn, o options) {
	t.Helper()
	var count int
	// Include waiting locks: a client timeout must not strand a pool backend.
	if err := c.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid::bigint=($1::bigint >> 32) AND objid::bigint=($1::bigint & 4294967295) AND objsubid=1`, o.lockKey()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration locks remain: %d (%v)", count, err)
	}
}

// TestPostgresTransactionPreflight verifies an unsupported later migration blocks
// the entire pending sequence before any schema changes or leaked locks occur.
func TestPostgresTransactionPreflight(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	o.AllowNonTransactional = true
	ms := loadTestMigrations(t, dir, o, "CREATE TABLE must_not_run(id int);", "-- migration:transaction=false\nCREATE TABLE non_atomic(id int);")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil || !strings.Contains(err.Error(), "connection_mode=session") {
		t.Fatalf("missing connection mode error: %v", err)
	}
	var exists bool
	if err := c.QueryRow(ctx, "SELECT to_regclass('must_not_run') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("preflight made changes: %v %v", exists, err)
	}
	assertNoMigrationLocks(t, c, o)
}

// TestPgBouncerModes exercises real transaction and session pools with competing
// runners, rollback, dirty recovery, lock timeouts, and non-transactional preflight.
// It skips unless the pool test DSNs are explicitly supplied.
func TestPgBouncerModes(t *testing.T) {
	for _, mode := range []string{"transaction", "session"} {
		t.Run(mode, func(t *testing.T) {
			poolDSN := os.Getenv("DDL_MT_TEST_" + strings.ToUpper(mode) + "_DSN")
			if poolDSN == "" {
				t.Skip("run make test-pgbouncer for real pool tests")
			}
			o, c, dir := testDatabase(t)
			t.Setenv(o.DSNEnv, poolDSN+" dbname="+o.Database)
			o.ConnectionMode = mode
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := initCommand(ctx, o, io.Discard); err != nil {
				t.Fatal(err)
			}
			ms := loadTestMigrations(t, dir, o, "CREATE TABLE existing(id int);", "SELECT pg_sleep(0.1); CREATE TABLE once_only(id int);", "CREATE TABLE last_step(id int);")
			mustExec(t, c, "CREATE TABLE existing(id int)")
			if err := baselineCommand(ctx, o, ms, dir, "001", io.Discard); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			failures := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); failures <- applyCommand(ctx, o, ms, dir, io.Discard) }()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
				t.Fatal(err)
			}
			assertNoMigrationLocks(t, c, o)

			// A failed file rolls back, preserving the earlier committed prefix.
			ms = loadTestMigrations(t, dir, o, "CREATE TABLE existing(id int);", "SELECT pg_sleep(0.1); CREATE TABLE once_only(id int);", "CREATE TABLE last_step(id int);", "CREATE TABLE rollback_me(id int); SELECT 1/0;")
			if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
				t.Fatal("expected SQL failure")
			}
			var exists bool
			if err := c.QueryRow(ctx, "SELECT to_regclass('rollback_me') IS NOT NULL").Scan(&exists); err != nil || exists {
				t.Fatalf("rollback failed: %v %v", exists, err)
			}
			assertNoMigrationLocks(t, c, o)

			// Dirty history and repair work through either pool mode.
			if err := insertRecord(ctx, c, o, migrationRecords(ms)[3], nil, 0, false, "failed"); err != nil {
				t.Fatal(err)
			}
			if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil || !strings.Contains(err.Error(), "repair") {
				t.Fatalf("dirty history: %v", err)
			}
			if err := repairCommand(ctx, o, ms, "004", "retry", io.Discard); err != nil {
				t.Fatal(err)
			}
			assertNoMigrationLocks(t, c, o)

			// A timed-out waiter leaves neither a lock nor a blocked pool connection.
			if _, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", o.lockKey()); err != nil {
				t.Fatal(err)
			}
			o.LockTimeout = 150 * time.Millisecond
			if err := applyCommand(ctx, o, ms[:3], dir, io.Discard); err == nil || !strings.Contains(err.Error(), "migration lock unavailable") {
				t.Fatalf("missing lock timeout: %v", err)
			}
			if err := repairCommand(ctx, o, ms, "004", "retry", io.Discard); err == nil || !strings.Contains(err.Error(), "migration lock unavailable") {
				t.Fatalf("repair lock timeout: %v", err)
			}
			if _, err := c.Exec(ctx, "SELECT pg_advisory_unlock($1)", o.lockKey()); err != nil {
				t.Fatal(err)
			}
			if err := applyCommand(ctx, o, ms[:3], dir, io.Discard); err != nil {
				t.Fatal("pool unusable after timeout:", err)
			}
			assertNoMigrationLocks(t, c, o)

			// The complete pending batch must be refused in transaction mode.
			ms = loadTestMigrations(t, dir, o, "CREATE TABLE existing(id int);", "SELECT pg_sleep(0.1); CREATE TABLE once_only(id int);", "CREATE TABLE last_step(id int);", "CREATE TABLE before_index(id int);", "-- migration:transaction=false\nCREATE INDEX CONCURRENTLY once_idx ON once_only(id);")
			o.AllowNonTransactional = true
			err := applyCommand(ctx, o, ms, dir, io.Discard)
			if mode == "transaction" {
				if err == nil || !strings.Contains(err.Error(), "connection_mode=session") {
					t.Fatalf("non-atomic accepted: %v", err)
				}
				if err := c.QueryRow(ctx, "SELECT to_regclass('before_index') IS NOT NULL").Scan(&exists); err != nil || exists {
					t.Fatalf("preflight changed database: %v %v", exists, err)
				}
			} else if err != nil {
				t.Fatal("session-pool concurrent index:", err)
			}
			assertNoMigrationLocks(t, c, o)
		})
	}
}

// TestPostgresMixedLockModes proves transaction and session runners coordinate
// through the same advisory key and leave no migration locks behind.
func TestPostgresMixedLockModes(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "SELECT pg_sleep(0.1); CREATE TABLE mixed_once(id int);", "CREATE TABLE mixed_next(id int);")
	failures := make(chan error, 2)
	for _, mode := range []string{"transaction", "session"} {
		target := o
		target.ConnectionMode = mode
		go func() { failures <- applyCommand(ctx, target, ms, dir, io.Discard) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertNoMigrationLocks(t, c, o)
}
