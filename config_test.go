package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectTargetsAndPaths verifies project-relative paths, target selection,
// and rejection of ambiguous direct/project configuration.
func TestProjectTargetsAndPaths(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "ddl-mt.json")
	putFile(t, path, `{"project":"shop","targets":{"dev":{"database":"shop-dev","migrations_dir":"db/migrations","dsn_env":"SHOP_DEV_DSN"},"prod":{"database":"shop","migrations_dir":"db/migrations","credentials_file":"secrets/prod.json","ledger_table":"public.shop_migrations"}}}`)
	o, command, _, err := parseOptions([]string{"--project", path, "--target", "dev", "validate"})
	if err != nil {
		t.Fatal(err)
	}
	if command != "validate" || o.MigrationsDir != filepath.Join(dir, "db/migrations") || o.DSNEnv != "SHOP_DEV_DSN" || o.Project != "shop" {
		t.Fatalf("wrong options %+v", o)
	}
	o, _, _, err = parseOptions([]string{"--project", path, "--target", "prod", "status"})
	if err != nil || o.ConfigPath != filepath.Join(dir, "secrets/prod.json") {
		t.Fatalf("%+v %v", o, err)
	}
	if _, _, _, err := parseOptions([]string{"--project", path, "--target", "missing", "apply"}); err == nil {
		t.Fatal("unknown target accepted")
	}
	if _, _, _, err := parseOptions([]string{"--project", path, "--target", "dev", "--database", "other", "apply"}); err == nil {
		t.Fatal("ambiguous selection accepted")
	}
	if err := runCLI(context.Background(), []string{"--project", path, "targets"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// TestTargetsOutput verifies that targets prints a header row and aligned
// columns so the target name and database name are distinguishable.
func TestTargetsOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ddl-mt.json")
	putFile(t, path, `{"project":"shop","targets":{"prod":{"database":"shop","migrations_dir":"db/migrations"},"development":{"database":"shop_dev","migrations_dir":"db/migrations"}}}`)
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"--project", path, "targets"}, &out); err != nil {
		t.Fatal(err)
	}
	want := "TARGET       DATABASE  MIGRATIONS DIR\n" +
		"development  shop_dev  db/migrations\n" +
		"prod         shop      db/migrations\n"
	if got := out.String(); got != want {
		t.Fatalf("targets output:\n%s\nwant:\n%s", got, want)
	}
}

// TestProjectDiscovery checks ancestor configuration lookup and proves it stops
// at the nearest Git repository boundary.
func TestProjectDiscovery(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "ddl-mt.json"), `{"project":"test","targets":{"local":{"database":"db","migrations_dir":"migrations"}}}`)
	child := filepath.Join(dir, "src", "nested")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)
	o, _, _, err := parseOptions([]string{"--target", "local", "validate"})
	if err != nil || o.MigrationsDir != filepath.Join(dir, "migrations") {
		t.Fatalf("%+v %v", o, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "src", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := findProject(""); err == nil {
		t.Fatal("discovery crossed a repository boundary")
	}
}

// TestInvalidProject rejects incomplete, mistyped, unsafe, and conflicting
// configuration objects before any database work can begin.
func TestInvalidProject(t *testing.T) {
	for _, contents := range []string{
		`{}`, `null`, `{"project":"../escape","targets":{}}`,
		`{"project":"x","targets":{"a":{"database":"x","migrations_dir":"m","typo":true}}}`,
		`{"project":"x","targets":{"a":{"database":"x","migrations_dir":"m","ledger_table":"public.a;DROP TABLE x"}}}`,
		`{"project":"x","targets":{"a":{"database":"x","migrations_dir":"m","dsn_env":"X","credentials_file":"x"}}}`,
		`{"project":"x","targets":{"a":{"database":"x","migrations_dir":"m","ledger_format":"ddl-mt"}}}`,
	} {
		path := filepath.Join(t.TempDir(), "ddl-mt.json")
		putFile(t, path, contents)
		if _, err := readProject(path); err == nil {
			t.Fatalf("accepted %s", contents)
		}
	}
}

// TestConnectionModes checks pooling-mode defaults, target settings, CLI
// overrides, and invalid lock timeouts or connection modes.
func TestConnectionModes(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	path := filepath.Join(t.TempDir(), "ddl-mt.json")
	putFile(t, path, `{"project":"test","targets":{"pooled":{"database":"db","migrations_dir":"migrations"},"direct":{"database":"db","migrations_dir":"migrations","connection_mode":"session"}}}`)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--database", "db", "apply"}, "transaction"},
		{[]string{"--project", path, "--target", "pooled", "apply"}, "transaction"},
		{[]string{"--project", path, "--target", "direct", "apply"}, "session"},
		{[]string{"--project", path, "--target", "direct", "--connection-mode", "transaction", "apply"}, "transaction"},
	} {
		o, _, _, err := parseOptions(tc.args)
		if err != nil || o.ConnectionMode != tc.want {
			t.Fatalf("%v: %+v %v", tc.args, o, err)
		}
	}
	for _, args := range [][]string{
		{"--database", "db", "--connection-mode", "auto", "apply"},
		{"--database", "db", "--lock-timeout", "0s", "apply"},
	} {
		if _, _, _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	putFile(t, path, `{"project":"test","targets":{"bad":{"database":"db","migrations_dir":"migrations","connection_mode":"typo"}}}`)
	if _, err := readProject(path); err == nil {
		t.Fatal("invalid target connection mode accepted")
	}
}

// TestDiscoveryRejectsBrokenNearestPaths prevents fallback to a parent's project
// when a nearer config is unreadable or its migration path is not a directory.
func TestDiscoveryRejectsBrokenNearestPaths(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "ddl-mt.json"), `{}`)
	putFile(t, filepath.Join(dir, "migrations", "example", "001_first.sql"), "SELECT 1;")
	child := filepath.Join(dir, "child")
	putFile(t, filepath.Join(child, "migrations", "example"), "not a directory")
	t.Chdir(child)
	if _, _, err := findMigrationsDirectory(options{Database: "example"}); err == nil {
		t.Error("invalid nearer migration path silently selected parent")
	}
	path := filepath.Join(child, "ddl-mt.json")
	if err := os.Symlink("ddl-mt.json", path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := findProject(""); err == nil {
		t.Error("broken nearer project silently selected parent")
	}
}

// TestDuplicateProjectKeysRejectAmbiguousTargets prevents JSON's last-value-wins
// behavior from silently changing a selected database or target definition.
func TestDuplicateProjectKeysRejectAmbiguousTargets(t *testing.T) {
	for _, contents := range []string{
		`{"project":"first","project":"second","targets":{"prod":{"database":"db","migrations_dir":"m"}}}`,
		`{"project":"test","targets":{"prod":{"database":"first","database":"second","migrations_dir":"m"}}}`,
		`{"project":"test","targets":{"prod":{"database":"first","migrations_dir":"m"},"prod":{"database":"second","migrations_dir":"m"}}}`,
	} {
		path := filepath.Join(t.TempDir(), "ddl-mt.json")
		putFile(t, path, contents)
		if _, err := readProject(path); err == nil {
			t.Errorf("ambiguous project accepted: %s", contents)
		}
	}
}

// TestRoleOption checks that a target role reaches the options, that --role
// overrides it and works in direct mode, and that unusable names are rejected.
func TestRoleOption(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	path := filepath.Join(t.TempDir(), "ddl-mt.json")
	putFile(t, path, `{"project":"test","targets":{"plain":{"database":"db","migrations_dir":"migrations"},"owned":{"database":"db","migrations_dir":"migrations","role":"app_owner"}}}`)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--database", "db", "apply"}, ""},
		{[]string{"--database", "db", "--role", "app_owner", "apply"}, "app_owner"},
		{[]string{"--project", path, "--target", "plain", "apply"}, ""},
		{[]string{"--project", path, "--target", "owned", "apply"}, "app_owner"},
		{[]string{"--project", path, "--target", "owned", "--role", "Other Owner", "apply"}, "Other Owner"},
	} {
		o, _, _, err := parseOptions(tc.args)
		if err != nil || o.Role != tc.want {
			t.Fatalf("%v: %+v %v", tc.args, o, err)
		}
	}
	for _, role := range []string{" padded", "line\nbreak", strings.Repeat("r", 64)} {
		if _, _, _, err := parseOptions([]string{"--database", "db", "--role", role, "apply"}); err == nil {
			t.Fatalf("accepted role %q", role)
		}
	}
	putFile(t, path, `{"project":"test","targets":{"bad":{"database":"db","migrations_dir":"migrations","role":" padded"}}}`)
	if _, err := readProject(path); err == nil {
		t.Fatal("invalid target role accepted")
	}
}
