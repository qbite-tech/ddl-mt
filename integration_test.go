package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDatabase creates an isolated database using DDL_MT_TEST_DSN and registers
// connection/database cleanup. Without that explicit test DSN it skips; the role
// needs CREATEDB and the server must be disposable. Application DSNs are unused.
func testDatabase(t *testing.T) (options, *pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("DDL_MT_TEST_DSN")
	if dsn == "" {
		t.Skip("set DDL_MT_TEST_DSN to run PostgreSQL integration tests")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		t.Fatal("DDL_MT_TEST_DSN must use libpq key=value syntax")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ddl_mt_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
		admin.Close(cleanup)
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	// Store a connection string in an isolated env var; it never reaches output.
	// ConnString retains its original database, so use an explicit libpq dbname override.
	t.Setenv("DDL_MT_INTEGRATION_TARGET", dsn+" dbname="+name)
	o := options{Database: name, DSNEnv: "DDL_MT_INTEGRATION_TARGET", LedgerTable: "public.schema_migrations"}
	return o, c, t.TempDir()
}

// mustExec executes test setup SQL on the supplied isolated connection and fails
// the calling test immediately if the database rejects it.
func mustExec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

// loadTestMigrations writes numbered fixture files into dir and loads them through
// the production validator. Existing fixture names are replaced for recovery tests;
// the caller controls whether any older files should remain in the directory.
func loadTestMigrations(t *testing.T, dir string, o options, sql ...string) []migration {
	t.Helper()
	for i, statement := range sql {
		putFile(t, filepath.Join(dir, fmt.Sprintf("%03d_step.sql", i+1)), statement)
	}
	migrations, err := loadTargetMigrations(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return migrations
}

// TestPostgresHistoryWorkflow exercises initialization, baselining, application,
// checks, and drift errors against one history table.
func TestPostgresHistoryWorkflow(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal("init must be idempotent:", err)
	}
	ms := loadTestMigrations(t, dir, o, "CREATE TABLE existing_table (id int);", "BEGIN; CREATE TABLE applied_table (id int); COMMIT;")
	mustExec(t, c, "CREATE TABLE existing_table (id int)")
	if err := baselineCommand(ctx, o, ms, dir, "002", io.Discard); err == nil {
		t.Fatal("accepted baseline gap")
	}
	if err := baselineCommand(ctx, o, ms, dir, "001", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err == nil {
		t.Fatal("check passed pending migration")
	}
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err != nil {
		t.Fatal("apply must be idempotent:", err)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms[0].Checksum = checksumMigration("changed")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
		t.Fatal("modified history accepted")
	}
	if err := applyCommand(ctx, o, nil, dir, io.Discard); err == nil {
		t.Fatal("empty repository ignored existing history")
	}
}

// TestPostgresAtomicRollback verifies SQL failures and history-insert failures
// both roll back the migration schema and leave no successful history record.
func TestPostgresAtomicRollback(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "CREATE TABLE rolled_back (id int); SELECT 1/0;")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
		t.Fatal("expected SQL failure")
	}
	var exists bool
	if err := c.QueryRow(ctx, "SELECT to_regclass('rolled_back') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("schema was not rolled back: %v %v", exists, err)
	}
	rows, err := readLedger(ctx, c, o)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed migration was recorded: %v %v", rows, err)
	}
	// A ledger insertion failure must also roll back successfully executed schema SQL.
	ms = loadTestMigrations(t, dir, o, "CREATE TABLE rolled_back (id int);")
	mustExec(t, c, "ALTER TABLE public.schema_migrations ADD CONSTRAINT reject_insert CHECK (execution_ms < 0)")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
		t.Fatal("expected ledger failure")
	}
	if err := c.QueryRow(ctx, "SELECT to_regclass('rolled_back') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("ledger failure left schema changes: %v %v", exists, err)
	}
}

// TestPostgresNonAtomicRecovery tests opt-in, durable failure markers, blocked
// retries, explicit repair, and successful concurrent index creation.
func TestPostgresNonAtomicRecovery(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "-- migration:transaction=false\nCREATE TABLE partial_change (id int); SELECT 1/0;")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil || !strings.Contains(err.Error(), "allow_non_transactional") {
		t.Fatalf("missing opt-in accepted: %v", err)
	}
	o.AllowNonTransactional = true
	o.ConnectionMode = "session"
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
		t.Fatal("expected failure")
	}
	rows, err := readLedger(ctx, c, o)
	if err != nil || len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("missing durable failure marker: %v %v", rows, err)
	}
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil || !strings.Contains(err.Error(), "repair") {
		t.Fatalf("dirty migration retried automatically: %v", err)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err == nil {
		t.Fatal("check accepted dirty history")
	}
	mustExec(t, c, "DROP TABLE partial_change") // Operator restores pre-migration state before retry.
	if err := repairCommand(ctx, o, ms, "001", "retry", io.Discard); err != nil {
		t.Fatal(err)
	}
	rows, err = readLedger(ctx, c, o)
	if err != nil || len(rows) != 0 {
		t.Fatalf("retry did not clear marker: %v %v", rows, err)
	}
	// Successful concurrent index creation runs outside a transaction block.
	ms = loadTestMigrations(t, dir, o, "-- migration:transaction=false\nCREATE TABLE partial_change (id int); CREATE INDEX CONCURRENTLY partial_idx ON partial_change(id);")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := repairCommand(ctx, o, ms, "001", "retry", io.Discard); err == nil {
		t.Fatal("repair rewrote applied history")
	}
}

// TestPostgresConcurrentRunnersAndDeadline proves competing runners record a file
// once and that a command deadline bounds waiting for an occupied advisory lock.
func TestPostgresConcurrentRunnersAndDeadline(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "SELECT pg_sleep(0.1); CREATE TABLE exactly_once (id int);")
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
	rows, err := readLedger(ctx, c, o)
	if err != nil || len(rows) != 1 {
		t.Fatalf("history was not exactly once: %v %v", rows, err)
	}
	if _, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", o.lockKey()); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := applyCommand(deadline, o, ms, dir, io.Discard); err == nil {
		t.Fatal("ignored lock deadline")
	}
	if _, err := c.Exec(ctx, "SELECT pg_advisory_unlock($1)", o.lockKey()); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresTargetVerification rejects wrong databases and read-only write
// targets while permitting read-only inspection through the same connection setup.
func TestPostgresTargetVerification(t *testing.T) {
	o, c, _ := testDatabase(t)
	ctx := context.Background()
	wrong := o
	wrong.Database = "not_the_selected_database"
	if conn, err := connectAndVerify(ctx, wrong, true); err == nil {
		conn.Close(ctx)
		t.Fatal("accepted wrong database")
	}
	mustExec(t, c, "ALTER DATABASE "+pgx.Identifier{o.Database}.Sanitize()+" SET default_transaction_read_only=on")
	if conn, err := connectAndVerify(ctx, o, true); err == nil {
		conn.Close(ctx)
		t.Fatal("accepted read-only target")
	}
	if conn, err := connectAndVerify(ctx, o, false); err != nil {
		t.Fatal(err)
	} else {
		conn.Close(ctx)
	}
}

// TestPostgresIsolatedHistoriesAndForeignTable verifies independent repositories
// can share a database with separate ledgers and cannot reinterpret a foreign table.
func TestPostgresIsolatedHistoriesAndForeignTable(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	first := loadTestMigrations(t, dir, o, "CREATE TABLE first_project (id int);")
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := applyCommand(ctx, o, first, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	secondOptions := o
	secondOptions.LedgerTable = "public.second_project_migrations"
	second := loadTestMigrations(t, t.TempDir(), secondOptions, "CREATE TABLE second_project (id int);")
	if err := initCommand(ctx, secondOptions, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := applyCommand(ctx, secondOptions, second, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, target := range []options{o, secondOptions} {
		rows, err := readLedger(ctx, c, target)
		if err != nil || len(rows) != 1 {
			t.Fatalf("histories collided: %v %v", rows, err)
		}
	}
	foreign := o
	foreign.LedgerTable = "public.foreign_migrations"
	mustExec(t, c, "CREATE TABLE public.foreign_migrations (version text PRIMARY KEY)")
	if _, err := readLedger(ctx, c, foreign); err == nil {
		t.Fatal("foreign table silently reinterpreted as history")
	}
}

// TestPostgresInterruptedMigrationRepair simulates lost confirmation after SQL
// completion and verifies audited repair restores a clean, checkable history.
func TestPostgresInterruptedMigrationRepair(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "-- migration:transaction=false\nCREATE TABLE interrupted (id int);")
	// Simulate a process lost after recording intent and completing the SQL.
	if err := insertRecord(ctx, c, o, migrationRecords(ms)[0], nil, 0, false, "applying"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "CREATE TABLE interrupted (id int)")
	o.AllowNonTransactional = true
	o.ConnectionMode = "session"
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
		t.Fatal("interrupted migration was retried")
	}
	if err := repairCommand(ctx, o, ms, "001", "applied", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresConnectionSettings verifies the server receives the runner-owned
// application/string settings and accepts migration SQL using simple-query mode.
func TestPostgresConnectionSettings(t *testing.T) {
	o, _, _ := testDatabase(t)
	// Credentials can carry runtime parameters, but the runner owns these two.
	t.Setenv(o.DSNEnv, os.Getenv(o.DSNEnv)+" application_name=unrelated_app standard_conforming_strings=off")
	ctx := context.Background()
	c, err := connectAndVerify(ctx, o, true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var applicationName, stringMode, literal string
	if err := c.QueryRow(ctx, `SELECT current_setting('application_name'), current_setting('standard_conforming_strings'), 'a\b'`).Scan(&applicationName, &stringMode, &literal); err != nil {
		t.Fatal(err)
	}
	if applicationName != "ddl-mt" || stringMode != "on" || literal != `a\b` {
		t.Fatalf("unsafe connection settings: application=%q string_mode=%q literal=%q", applicationName, stringMode, literal)
	}
	// Migration SQL can contain multiple statements; the connection must support it.
	if _, err := c.Exec(ctx, "SELECT 1; SELECT 2;"); err != nil {
		t.Fatalf("migration SQL protocol failed: %v", err)
	}
}

// TestPostgresCLIWorkflow exercises the public command dispatcher against a real
// database, including pending/applied output and the offline migration template.
func TestPostgresCLIWorkflow(t *testing.T) {
	o, _, dir := testDatabase(t)
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	config := filepath.Join(dir, "ddl-mt.json")
	putFile(t, config, fmt.Sprintf(`{"project":"test","targets":{"local":{"database":%q,"migrations_dir":"sql","dsn_env":%q}}}`, o.Database, o.DSNEnv))
	sqlDir := filepath.Join(dir, "sql")
	if err := os.Mkdir(sqlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Each call has its own output buffer, just as separate CLI processes do.
	run := func(want string, args ...string) {
		t.Helper()
		var out strings.Builder
		flags := append([]string{"--project", config, "--target", "local"}, args...)
		if err := runCLI(ctx, flags, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%v: expected %q, got %q", args, want, out.String())
		}
	}
	run("001_create_cli_table.sql", "new", "create_cli_table")
	putFile(t, filepath.Join(sqlDir, "001_create_cli_table.sql"), "CREATE TABLE cli_table(id int);")
	run("Validated 1 migration", "validate")
	run("initialized", "init")
	run("PENDING", "status")
	run("Applied:", "apply")
	run("APPLIED", "status")
	run("check passed", "check")
}

// TestPostgresBaselineRollsBackInvalidSelection proves a gapped baseline rolls
// back earlier inserts, executes no migration SQL, and remains idempotent on retry.
func TestPostgresBaselineRollsBackInvalidSelection(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "SELECT 1/0;", "SELECT 1/0;", "SELECT 1/0;")
	if err := baselineCommand(ctx, o, ms, dir, "001,003", io.Discard); err == nil {
		t.Fatal("gapped baseline succeeded")
	}
	rows, err := readLedger(ctx, c, o)
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid baseline committed partial history: %v %v", rows, err)
	}
	for i := 0; i < 2; i++ {
		if err := baselineCommand(ctx, o, ms, dir, "001-003", io.Discard); err != nil {
			t.Fatal("baseline ran SQL or was not idempotent:", err)
		}
	}
	rows, err = readLedger(ctx, c, o)
	if err != nil || len(rows) != 3 {
		t.Fatalf("unexpected baseline history: %v %v", rows, err)
	}
	for _, row := range rows {
		if !row.IsBaseline || row.Status != "applied" {
			t.Fatalf("wrong baseline record: %+v", row)
		}
	}
}

// TestPostgresRepairRejectsUnsafeHistory verifies repair cannot bypass checksums,
// alter applied history, or edit a dirty record underneath later ledger entries.
func TestPostgresRepairRejectsUnsafeHistory(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "SELECT 1;", "SELECT 2;")
	records := migrationRecords(ms)
	if err := insertRecord(ctx, c, o, records[0], nil, 0, false, "failed"); err != nil {
		t.Fatal(err)
	}
	changed := append([]migration(nil), ms...)
	changed[0].Checksum = checksumMigration("modified")
	if err := repairCommand(ctx, o, changed, "001", "applied", io.Discard); err == nil {
		t.Fatal("repair accepted changed SQL")
	}
	if err := insertRecord(ctx, c, o, records[1], nil, 0, false, "applied"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"retry", "applied"} {
		for _, v := range []string{"001", "002", "999"} {
			if err := repairCommand(ctx, o, ms, v, action, io.Discard); err == nil {
				t.Fatalf("unsafe repair accepted: version=%s action=%s", v, action)
			}
		}
	}
	rows, err := readLedger(ctx, c, o)
	if err != nil || len(rows) != 2 || rows[0].Status != "failed" || rows[1].Status != "applied" {
		t.Fatalf("rejected repair mutated history: %v %v", rows, err)
	}
}

// TestPostgresLexicalRegression executes the repaired lexical cases against
// PostgreSQL so unit expectations cannot merely mirror the custom splitter.
func TestPostgresLexicalRegression(t *testing.T) {
	o, c, dir := testDatabase(t)
	ctx := context.Background()
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	sql := `CREATE TABLE lexical_regression (value text);
CREATE FUNCTION concurrently () RETURNS int LANGUAGE SQL AS $$SELECT 1$$;
INSERT INTO lexical_regression VALUES (E'first'
'\';COMMIT;');
INSERT INTO lexical_regression VALUES ($body$
-- Target database: unrelated
-- migration:transaction=false
$body$);`
	ms := loadTestMigrations(t, dir, o, sql)
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	var literal string
	if err := c.QueryRow(ctx, "SELECT value FROM lexical_regression WHERE value LIKE 'first%'").Scan(&literal); err != nil {
		t.Fatal(err)
	}
	if literal != "first';COMMIT;" {
		t.Fatalf("SQL string contents changed: %q", literal)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// roleTestDatabase extends testDatabase with an owner role holding a private
// schema and a login that has no rights of its own. ddl-mt connects as that
// login, which is granted the owner role only when member is true. Both
// cluster-wide roles are dropped before the test database is removed.
func roleTestDatabase(t *testing.T, member bool) (options, *pgx.Conn, string) {
	t.Helper()
	o, c, dir := testDatabase(t)
	suffix := strings.TrimPrefix(o.Database, "ddl_mt_test_")
	owner, login := "ddl_mt_owner_"+suffix, "ddl_mt_login_"+suffix
	cfg, err := pgx.ParseConfig(os.Getenv("DDL_MT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	password := ""
	if cfg.Password != "" {
		password = " PASSWORD '" + strings.ReplaceAll(cfg.Password, "'", "''") + "'"
	}
	mustExec(t, c, "CREATE ROLE "+owner+" NOLOGIN")
	// NOINHERIT: membership permits SET ROLE but grants nothing until the switch.
	mustExec(t, c, "CREATE ROLE "+login+" LOGIN NOINHERIT"+password)
	t.Cleanup(func() {
		for _, statement := range []string{"DROP SCHEMA IF EXISTS app CASCADE", "DROP OWNED BY " + owner + ", " + login, "DROP ROLE " + owner + ", " + login} {
			if _, err := c.Exec(context.Background(), statement); err != nil {
				t.Error(err)
			}
		}
	})
	if member {
		mustExec(t, c, "GRANT "+owner+" TO "+login)
	}
	mustExec(t, c, "CREATE SCHEMA app AUTHORIZATION "+owner)
	// A later libpq keyword overrides the earlier one, as with dbname.
	t.Setenv(o.DSNEnv, os.Getenv(o.DSNEnv)+" user="+login)
	o.LedgerTable = "app.schema_migrations"
	o.Role = owner
	return o, c, dir
}

// relationOwner returns the owning role of a relation in the app schema.
func relationOwner(t *testing.T, c *pgx.Conn, name string) string {
	t.Helper()
	var owner string
	if err := c.QueryRow(context.Background(), "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = to_regclass($1)", "app."+name).Scan(&owner); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return owner
}

// TestPostgresRoleOption verifies that a login without rights of its own creates
// the history table and migration objects as the configured owner role in both
// connection modes, and that the same login fails loudly without the role.
func TestPostgresRoleOption(t *testing.T) {
	o, c, dir := roleTestDatabase(t, true)
	ctx := context.Background()
	without := o
	without.Role = ""
	if err := initCommand(ctx, without, io.Discard); err == nil {
		t.Fatal("login without the role initialized the ledger")
	}
	if err := initCommand(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ms := loadTestMigrations(t, dir, o, "CREATE TABLE app.first (id int);", "CREATE TABLE app.second (id int);")
	if err := applyCommand(ctx, without, ms, dir, io.Discard); err == nil {
		t.Fatal("login without the role applied migrations")
	}
	if err := baselineCommand(ctx, o, ms, dir, "001", io.Discard); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "CREATE TABLE app.first (id int)")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := statusCommand(ctx, o, ms, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Session mode covers the history read and SQL that run outside a transaction.
	o.ConnectionMode, o.AllowNonTransactional = "session", true
	ms = loadTestMigrations(t, dir, o, "CREATE TABLE app.first (id int);", "CREATE TABLE app.second (id int);",
		"CREATE TABLE app.third (id int);", "-- migration:transaction=false\nCREATE INDEX CONCURRENTLY third_idx ON app.third(id);",
		"-- migration:transaction=false\nCREATE TABLE app.fourth (id int); SELECT 1/0;")
	if err := applyCommand(ctx, o, ms, dir, io.Discard); err == nil {
		t.Fatal("expected failure")
	}
	for _, name := range []string{"schema_migrations", "second", "third", "third_idx", "fourth"} {
		if owner := relationOwner(t, c, name); owner != o.Role {
			t.Fatalf("%s is owned by %s, want %s", name, owner, o.Role)
		}
	}
	mustExec(t, c, "DROP TABLE app.fourth")
	if err := repairCommand(ctx, o, ms, "005", "retry", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := statusCommand(ctx, o, ms, false, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresRoleRequiresMembership verifies every database command refuses to
// start, before running any SQL, when the login cannot switch to the role.
func TestPostgresRoleRequiresMembership(t *testing.T) {
	o, c, dir := roleTestDatabase(t, false)
	ctx := context.Background()
	mustExec(t, c, bootstrapSQL(o))
	ms := loadTestMigrations(t, dir, o, "CREATE TABLE public.must_not_run (id int);")
	mustExec(t, c, "GRANT CREATE ON SCHEMA public TO PUBLIC")
	missing := o
	missing.Role = "ddl_mt_no_such_role"
	for _, tc := range []options{o, missing} {
		for name, run := range map[string]func() error{
			"init":   func() error { return initCommand(ctx, tc, io.Discard) },
			"status": func() error { return statusCommand(ctx, tc, ms, false, io.Discard) },
			"apply":  func() error { return applyCommand(ctx, tc, ms, dir, io.Discard) },
		} {
			if err := run(); err == nil || !strings.Contains(err.Error(), "cannot switch to role") {
				t.Fatalf("%s as %s: %v", name, tc.Role, err)
			}
		}
	}
	var exists bool
	if err := c.QueryRow(ctx, "SELECT to_regclass('public.must_not_run') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("migration ran without the role: %v %v", exists, err)
	}
}
