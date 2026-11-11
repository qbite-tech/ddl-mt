package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedCredentialConfigDir redirects XDG configuration into a temporary
// directory. Platforms that cannot isolate this path are skipped before any
// fixture can be written to the developer's real configuration directory.
func isolatedCredentialConfigDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	resolved, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != root {
		t.Skip("default config path test requires an XDG-based platform")
	}
	return root
}

// TestCredentialIsolationAndPrecedence verifies selected-source precedence and
// separate default credential files for repositories sharing a database name.
func TestCredentialIsolationAndPrecedence(t *testing.T) {
	isolatedCredentialConfigDir(t)
	t.Setenv("DB_DSN", "postgresql://ambient/db")
	t.Setenv("SELECTED_DSN", "")
	if _, err := getDSN(options{DSNEnv: "SELECTED_DSN"}); err == nil {
		t.Fatal("missing selected secret fell back to ambient credentials")
	}
	t.Setenv("SELECTED_DSN", "postgresql://selected/db")
	if got, err := getDSN(options{DSNEnv: "SELECTED_DSN"}); err != nil || got != "postgresql://selected/db" {
		t.Fatalf("%s %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	putFile(t, path, `{"DB_DSN":"postgresql://explicit/db"}`)
	if got, err := getDSN(options{ConfigPath: path, DSNEnv: "SELECTED_DSN"}); err != nil || got != "postgresql://explicit/db" {
		t.Fatalf("%s %v", got, err)
	}
	t.Setenv("DB_DSN", "")
	root, _ := os.UserConfigDir()
	for _, project := range []string{"shop", "billing"} {
		putFile(t, filepath.Join(root, "ddl-mt", project, "prod.json"), `{"DB_DSN":"postgresql://`+project+`/same_db"}`)
		got, err := getDSN(options{Project: project, Target: "prod", Database: "same_db"})
		if err != nil || got != "postgresql://"+project+"/same_db" {
			t.Fatalf("credentials collided: %s %v", got, err)
		}
	}
}

// TestCredentialErrorsDoNotLeakSecrets exercises malformed credential shapes
// and DSN parsing errors, asserting secrets never appear in returned diagnostics.
func TestCredentialErrorsDoNotLeakSecrets(t *testing.T) {
	t.Setenv("DB_DSN", "postgresql://ambient/db")
	t.Setenv("DDL_MT_SELECTED_DSN", "postgresql://selected/db")
	for _, contents := range []string{
		``, `null`, `{}`, `[]`, `{"DB_DSN":null}`, `{"DB_DSN":true}`,
		`{"DB_DSN":123}`, `{"DB_DSN":[]}`, `{"DB_DSN":""}`, `{"DB_DSN":"  "}`,
		`{"secret-password":"x"}`, `{"DB_DSN":"valid","unknown":"secret-password"}`,
		`{"DB_DSN":"secret-password"`, `{"DB_DSN":"secret-password"} trailing`,
		`{"DB_DSN":"valid"} {"DB_DSN":"secret-password"}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		putFile(t, path, contents)
		_, err := getDSN(options{ConfigPath: path, DSNEnv: "DDL_MT_SELECTED_DSN"})
		if err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	_, err := connect(context.Background(), "postgresql://user:secret-password@host:invalid/db")
	if err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("unsafe connection error: %v", err)
	}
}

// TestCredentialEnvironmentValues checks whitespace handling and proves missing
// or blank target secrets cannot fall back to ambient database credentials.
func TestCredentialEnvironmentValues(t *testing.T) {
	for _, test := range []struct {
		name, selected, general, want string
		named                         bool
	}{
		{name: "general env is trimmed", general: " \npostgresql://general/db\t ", want: "postgresql://general/db"},
		{name: "selected env is trimmed", named: true, selected: " \npostgresql://selected/db\t ", general: "postgresql://general/db", want: "postgresql://selected/db"},
		{name: "missing selected env never falls back", named: true, general: "postgresql://general/db"},
		{name: "blank selected env never falls back", named: true, selected: " \n\t ", general: "postgresql://general/db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DB_DSN", test.general)
			t.Setenv("DDL_MT_SELECTED_DSN", test.selected)
			o := options{Database: "example"}
			if test.named {
				o.DSNEnv = "DDL_MT_SELECTED_DSN"
			}
			got, err := getDSN(o)
			if test.want == "" {
				if err == nil || got != "" {
					t.Fatal("missing selected credentials fell back to another source")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("wrong credential selection or whitespace handling: %v", err)
			}
		})
	}
}

// TestCredentialFileReadFailuresDoNotFallback verifies an unreadable selected
// file cannot silently select an environment-provided connection instead.
func TestCredentialFileReadFailuresDoNotFallback(t *testing.T) {
	t.Setenv("DB_DSN", "postgresql://ambient/db")
	t.Setenv("DDL_MT_SELECTED_DSN", "postgresql://selected/db")
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "missing.json"), dir} {
		got, err := getDSN(options{ConfigPath: path, DSNEnv: "DDL_MT_SELECTED_DSN"})
		if err == nil || got != "" {
			t.Fatal("unreadable selected file fell back to another source")
		}
	}
}

// TestCredentialDefaultFileFallback checks default-file lookup, environment
// precedence, and rejection of unrelated application credential variables.
func TestCredentialDefaultFileFallback(t *testing.T) {
	root := isolatedCredentialConfigDir(t)
	path := filepath.Join(root, "ddl-mt", "example.json")
	putFile(t, path, "{\"DB_DSN\":\"  postgresql://default/db  \"}\n \t")
	for _, env := range []string{"", " \n\t "} {
		t.Setenv("DB_DSN", env)
		got, err := getDSN(options{Database: "example"})
		if err != nil || got != "postgresql://default/db" {
			t.Fatalf("default-file fallback or trimming failed: %v", err)
		}
	}
	// A higher-priority source must not attempt to parse an invalid default file.
	putFile(t, path, "invalid JSON")
	t.Setenv("DB_DSN", "postgresql://environment/db")
	got, err := getDSN(options{Database: "example"})
	if err != nil || got != "postgresql://environment/db" {
		t.Fatalf("environment did not override default file: %v", err)
	}
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "postgresql://unrelated/db")
	t.Setenv("MIGRATION_DB_DSN", "postgresql://unrelated/db")
	t.Setenv("MIGRATION_CHECK_DSN", "postgresql://unrelated/db")
	if got, err := getDSN(options{Database: "missing"}); err == nil || got != "" {
		t.Fatal("missing credentials used an unrelated application variable")
	}
}

// TestProjectCredentialFilePrecedence checks that project credential paths are
// project-relative while explicit overrides resolve from the working directory.
func TestProjectCredentialFilePrecedence(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	t.Setenv("DB_DSN", "postgresql://ambient/db")
	dir := t.TempDir()
	project := filepath.Join(dir, "ddl-mt.json")
	putFile(t, project, `{"project":"shop","targets":{"prod":{"database":"shop","migrations_dir":"migrations","credentials_file":"secrets/prod.json"}}}`)
	putFile(t, filepath.Join(dir, "secrets", "prod.json"), `{"DB_DSN":"postgresql://project/db"}`)
	other := t.TempDir()
	putFile(t, filepath.Join(other, "override.json"), `{"DB_DSN":"postgresql://override/db"}`)
	t.Chdir(other)
	for _, test := range []struct {
		flags []string
		want  string
	}{
		{want: "postgresql://project/db"},
		{flags: []string{"--config", "override.json"}, want: "postgresql://override/db"},
	} {
		args := append([]string{"--project", project, "--target", "prod"}, test.flags...)
		o, _, _, err := parseOptions(append(args, "status"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := getDSN(o)
		if err != nil || got != test.want {
			t.Fatalf("project or explicit credential path resolved incorrectly: %v", err)
		}
	}
}

// TestCredentialFileDoesNotExpandVariables verifies credentials remain literal
// JSON values and never undergo environment or shell expansion.
func TestCredentialFileDoesNotExpandVariables(t *testing.T) {
	t.Setenv("DDL_MT_TEST_SECRET", "postgresql://expanded/db")
	path := filepath.Join(t.TempDir(), "credentials.json")
	putFile(t, path, `{"DB_DSN":"${DDL_MT_TEST_SECRET}"}`)
	got, err := getDSN(options{ConfigPath: path})
	if err != nil || got != "${DDL_MT_TEST_SECRET}" {
		t.Fatalf("credential file must remain literal JSON: %v", err)
	}
}

// TestConnectFailureDoesNotLeakCredentials uses cancellation to exercise a failed
// connection without depending on network availability or exposing passwords.
func TestConnectFailureDoesNotLeakCredentials(t *testing.T) {
	// Cancellation exercises the connection-failure path without a network dependency.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := connect(ctx, "host=127.0.0.1 port=1 user=test password=secret-password dbname=example sslmode=disable")
	if conn != nil {
		conn.Close(context.Background())
		t.Fatal("failed connection returned a usable connection")
	}
	if err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("connection failure exposed credentials: %v", err)
	}
	if !strings.Contains(err.Error(), "connect") {
		t.Fatalf("expected connection error, got: %v", err)
	}
}

// TestVerifyDoesNotConnectWithoutValidCredentials verifies missing or malformed
// selected credentials fail safely before a usable database connection is returned.
func TestVerifyDoesNotConnectWithoutValidCredentials(t *testing.T) {
	for _, value := range []string{"", "postgresql://user:secret-password@host:invalid/db"} {
		t.Setenv("DDL_MT_SELECTED_DSN", value)
		conn, err := connectAndVerify(context.Background(), options{Database: "example", DSNEnv: "DDL_MT_SELECTED_DSN"}, true)
		if conn != nil {
			conn.Close(context.Background())
			t.Fatal("invalid credentials returned a connection")
		}
		if err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Fatalf("unexpected verification error: %v", err)
		}
	}
}

// TestDuplicateCredentialKeysRejectAmbiguousSecrets rejects repeated DSN keys
// without exposing either credential or falling back to the environment.
func TestDuplicateCredentialKeysRejectAmbiguousSecrets(t *testing.T) {
	t.Setenv("DB_DSN", "postgresql://ambient/db")
	path := filepath.Join(t.TempDir(), "credentials.json")
	putFile(t, path, `{"DB_DSN":"postgresql://first:secret-password@host/db","DB_DSN":"postgresql://second/db"}`)
	got, err := getDSN(options{ConfigPath: path})
	if err == nil || got != "" {
		t.Fatal("duplicate credentials selected a connection string")
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Fatal("error leaked credentials")
	}
}
