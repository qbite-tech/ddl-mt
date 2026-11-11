package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// putFile creates an isolated test fixture and its parent directories, failing
// the calling test immediately on any filesystem error.
func putFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSQLBoundaries exercises comments, quoted bodies, escape strings, and
// transaction controls to keep statement splitting separate from SQL semantics.
func TestSQLBoundaries(t *testing.T) {
	valid := []string{
		"CREATE TABLE demo (id int);",
		"-- header\nBEGIN; DO $body$ BEGIN PERFORM ';COMMIT;'; END; $body$; COMMIT; -- tail",
		`/* outer /* inner */ */ START TRANSACTION; SELECT E'escaped\';COMMIT;'; COMMIT;`,
		"SELECT 'CREATE INDEX CONCURRENTLY;'; SELECT \"COMMIT;\" FROM demo;",
		"DO $$ BEGIN RAISE NOTICE 'BEGIN; COMMIT;'; END $$;",
	}
	for _, sql := range valid {
		if _, _, err := prepareMigration(sql, "example"); err != nil {
			t.Errorf("rejected %q: %v", sql, err)
		}
	}
	invalid := []string{
		"BEGIN; SELECT 1; COMMIT; SELECT 2;", "SELECT 1; COMMIT;", "SELECT 1; END;",
		"SELECT 1; ROLLBACK;", "SELECT 1; PREPARE TRANSACTION 'x';", "/* unclosed",
		"DO $x$ unclosed", "SELECT 'unclosed", "\\set ON_ERROR_STOP on\nSELECT 1;",
		"BEGIN; CREATE INDEX CONCURRENTLY idx ON demo (id); COMMIT;",
		"SET standard_conforming_strings=off; SELECT 1;",
	}
	for _, sql := range invalid {
		if _, _, err := prepareMigration(sql, "example"); err == nil {
			t.Errorf("accepted unsafe SQL %q", sql)
		}
	}
}

// TestMigrationModesAndOriginalChecksums verifies execution-mode detection, target
// comment rules, and hashing of original bytes before wrapper stripping.
func TestMigrationModesAndOriginalChecksums(t *testing.T) {
	for _, sql := range []string{"CREATE UNIQUE INDEX CONCURRENTLY idx ON demo (id);", "-- migration:transaction=false\nVACUUM demo;"} {
		_, nonAtomic, err := prepareMigration(sql, "example")
		if err != nil || !nonAtomic {
			t.Fatalf("non-atomic detection failed: %v %v", nonAtomic, err)
		}
	}
	wrapped := "-- Target database: example\nBEGIN; SELECT 1; COMMIT;"
	if _, _, err := prepareMigration(wrapped, "example"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareMigration(wrapped, "other"); err == nil {
		t.Fatal("accepted incorrect target")
	}
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "001_first.sql"), wrapped)
	migrations, err := loadMigrations(dir, "example")
	if err != nil {
		t.Fatal(err)
	}
	if migrations[0].Checksum != checksumMigration(wrapped) || strings.Contains(migrations[0].SQL, "COMMIT") {
		t.Fatal("checksum or transaction stripping changed")
	}
}

// TestMigrationSequence rejects gaps, duplicate versions, and malformed SQL names
// while ignoring documentation and other non-SQL files.
func TestMigrationSequence(t *testing.T) {
	for _, names := range [][]string{{"002_gap.sql"}, {"001_one.sql", "001_duplicate.sql"}, {"1_bad.sql"}, {"001_bad.SQL"}} {
		dir := t.TempDir()
		for _, name := range names {
			putFile(t, filepath.Join(dir, name), "SELECT 1;")
		}
		if _, err := loadMigrations(dir, "example"); err == nil {
			t.Fatalf("accepted %v", names)
		}
	}
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "001_good.sql"), "SELECT 1;")
	putFile(t, filepath.Join(dir, "README.md"), "ignored docs")
	if _, err := loadMigrations(dir, "example"); err != nil {
		t.Fatal(err)
	}
}

// TestLedgerValidation rejects drift, missing files, duplicate records, history
// gaps, and dirty execution states before further migrations can be applied.
func TestLedgerValidation(t *testing.T) {
	a := migrationRecord{Version: "001", Filename: "001_first.sql", Checksum: checksumMigration("one"), Status: "applied"}
	b := migrationRecord{Version: "002", Filename: "002_second.sql", Checksum: checksumMigration("two"), Status: "applied"}
	repo := []migrationRecord{a, b}
	if _, err := ledgerByVersion(repo, []migrationRecord{a}); err != nil {
		t.Fatal(err)
	}
	changed := a
	changed.Checksum = b.Checksum
	renamed := a
	renamed.Filename = "001_renamed.sql"
	for _, rows := range [][]migrationRecord{{b}, {changed}, {renamed}, {a, a}} {
		if _, err := ledgerByVersion(repo, rows); err == nil {
			t.Fatalf("accepted invalid ledger %+v", rows)
		}
	}
	if _, err := ledgerByVersion(nil, []migrationRecord{a}); err == nil {
		t.Fatal("accepted missing repository files")
	}
	a.Status = "applying"
	if err := requireCleanLedger([]migrationRecord{a}); err == nil {
		t.Fatal("accepted dirty ledger")
	}
}

// TestVersionSelection verifies inclusive ranges, deduplication, and rejection
// of malformed, descending, or unknown baseline versions.
func TestVersionSelection(t *testing.T) {
	known := map[string]struct{}{"001": {}, "002": {}, "003": {}}
	got, err := parseVersionSelection("001-003,002", known)
	if err != nil || strings.Join(got, ",") != "001,002,003" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "000", "004", "003-001", "1", "001,"} {
		if _, err := parseVersionSelection(bad, known); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// TestNewMigration checks sequential file creation and rejects unsafe names
// without overwriting earlier migration contents.
func TestNewMigration(t *testing.T) {
	dir := t.TempDir()
	path, err := newMigration(dir, "create_widgets", options{Database: "example"})
	if err != nil || filepath.Base(path) != "001_create_widgets.sql" {
		t.Fatalf("%s %v", path, err)
	}
	putFile(t, path, "CREATE TABLE widgets (id int);")
	next, err := newMigration(dir, "add_name", options{Database: "example"})
	if err != nil || filepath.Base(next) != "002_add_name.sql" {
		t.Fatalf("%s %v", next, err)
	}
	if _, err := newMigration(dir, "../escape", options{}); err == nil {
		t.Fatal("accepted unsafe name")
	}
}

// TestOfflineValidateIgnoresCredentials proves local SQL validation succeeds even
// when the explicitly selected credential file does not exist.
func TestOfflineValidateIgnoresCredentials(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "example", "001_first.sql"), "SELECT 1;")
	if err := runCLI(context.Background(), []string{"--database", "example", "--migrations-root", dir, "--config", "/missing/credentials", "validate"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentIndexClassification distinguishes command options from SQL names
// so ordinary DDL cannot accidentally require non-transactional execution.
func TestConcurrentIndexClassification(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		nonAtomic bool
	}{
		{"function named concurrently", "CREATE FUNCTION concurrently () RETURNS int LANGUAGE SQL AS $$SELECT 1$$;", false},
		{"quoted name", `CREATE TABLE "concurrently" (id int);`, false},
		{"create index", "CREATE UNIQUE INDEX CONCURRENTLY idx ON demo(id);", true},
		{"drop index", "DROP INDEX CONCURRENTLY idx;", true},
		{"reindex keyword", "REINDEX INDEX CONCURRENTLY idx;", true},
		{"reindex option", "REINDEX(CONCURRENTLY) INDEX idx;", true},
		{"reindex disabled option", "REINDEX (CONCURRENTLY false) INDEX idx;", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := prepareMigration(tc.sql, "example")
			if err != nil || got != tc.nonAtomic {
				t.Fatalf("non-transactional=%v, want %v; error=%v", got, tc.nonAtomic, err)
			}
		})
	}
}

// TestMigrationMetadataIgnoresQuotedBodies ensures only standalone SQL comments
// outside statements can select a target or execution mode.
func TestMigrationMetadataIgnoresQuotedBodies(t *testing.T) {
	for _, body := range []string{
		"SELECT $body$\n-- Target database: unrelated\n-- migration:transaction=false\n$body$;",
		"SELECT '\n-- Target database: unrelated\n-- migration:transaction=false\n';",
		"/*\n-- Target database: unrelated\n-- migration:transaction=false\n*/\nSELECT 1;",
	} {
		if _, nonAtomic, err := prepareMigration(body, "example"); err != nil || nonAtomic {
			t.Fatalf("embedded metadata affected migration: non-transactional=%v error=%v", nonAtomic, err)
		}
	}
}

// TestEscapeStringContinuation preserves PostgreSQL's E-string escape semantics
// across newline-separated string segments, including SQL-looking string values.
func TestEscapeStringContinuation(t *testing.T) {
	sql := `SELECT E'first'
'\';COMMIT;'; SELECT 2;`
	statements, _, err := prepareMigration(sql, "example")
	if err != nil || len(statements) != 2 {
		t.Fatalf("continued string split incorrectly: %d statements, %v", len(statements), err)
	}
}

// TestSQLTriviaBoundaries exercises newline, nested-comment, and quoted-value
// boundaries that can otherwise expose string contents as executable statements.
func TestSQLTriviaBoundaries(t *testing.T) {
	for _, sql := range []string{
		"-- leading comment\rSELECT 1;",
		"SELECT E'first'\n-- continuation\n'\\n'; SELECT 2;",
		"SELECT 'first' /* between strings\n /* nested */ */ 'second';",
		"SELECT 'first'\n/* after newline */ 'second';",
		"SELECT 'value' /* trailing comment */;",
	} {
		if _, _, err := prepareMigration(sql, "example"); err != nil {
			t.Errorf("rejected trivia boundary: %q: %v", sql, err)
		}
	}
	for _, sql := range []string{"SELECT 'value' /* unterminated", "SELECT E'escape\\", "SELECT E'first'\n'\\"} {
		if _, _, err := prepareMigration(sql, "example"); err == nil {
			t.Errorf("accepted unterminated quote/comment: %q", sql)
		}
	}
}

// FuzzPrepareMigration checks arbitrary SQL cannot panic the validator and that
// every accepted execution unit remains one complete statement when reparsed.
// Seed cases run in the ordinary suite; mutation runs require go test -fuzz.
func FuzzPrepareMigration(f *testing.F) {
	for _, seed := range []string{
		"SELECT 1;", "BEGIN; SELECT 1; COMMIT;", "DO $$ BEGIN PERFORM ';'; END $$;",
		"SELECT E'first'\n'\\n';", "/* nested /* comment */ */ SELECT 'value';",
		"-- migration:transaction=false\nCREATE INDEX CONCURRENTLY idx ON demo(id);",
		"-- Target database: example\nSELECT 1;", "SELECT E'unterminated\\",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		statements, _, err := prepareMigration(sql, "example")
		if err != nil {
			return
		}
		if len(statements) == 0 {
			t.Fatal("accepted an empty migration")
		}
		for _, statement := range statements {
			parts, _, err := splitSQL(statement)
			if err != nil || len(parts) != 1 {
				t.Fatalf("unstable statement boundary: %q: parts=%d error=%v", statement, len(parts), err)
			}
		}
	})
}
