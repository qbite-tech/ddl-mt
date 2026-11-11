package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIRejectsInvalidCommandsBeforeIO verifies argument errors do not get
// obscured by filesystem or credential errors and cannot initiate database work.
func TestCLIRejectsInvalidCommandsBeforeIO(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"unknown"}, "unknown migration command"},
		{[]string{"apply", "--timeout", "1m"}, "does not accept arguments"},
		{[]string{"baseline"}, "baseline --versions"},
		{[]string{"repair", "--version", "001"}, "repair --version"},
		{[]string{"new"}, "new <snake_case_name>"},
		{[]string{"status", "extra"}, "does not accept arguments"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			args := append([]string{"--database", "example", "--migrations-root", filepath.Join(t.TempDir(), "missing")}, tc.args...)
			err := runCLI(context.Background(), args, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

// TestCLITargetsAreSortedAndOffline makes target discovery reproducible and
// proves listing targets does not require SQL directories or credentials.
func TestCLITargetsAreSortedAndOffline(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE", "")
	t.Setenv("MIGRATIONS_ROOT", "")
	path := filepath.Join(t.TempDir(), "ddl-mt.json")
	putFile(t, path, `{"project":"test","targets":{"z":{"database":"last","migrations_dir":"missing"},"a":{"database":"first","migrations_dir":"missing"}}}`)
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"--project", path, "targets"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "TARGET  DATABASE  MIGRATIONS DIR\na       first     missing\nz       last      missing\n" {
		t.Fatalf("unexpected listing: %q", out.String())
	}
	if err := runCLI(context.Background(), []string{"--project", path, "--connection-mode", "typo", "targets"}, &out); err == nil {
		t.Fatal("targets ignored invalid global flag")
	}
}

// TestDirectMigrationDiscovery documents ancestor lookup, explicit-root selection,
// and repository boundaries for projects using the configuration-free mode.
func TestDirectMigrationDiscovery(t *testing.T) {
	root := t.TempDir()
	expected := filepath.Join(root, "migrations", "example")
	if err := os.MkdirAll(expected, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	got, repo, err := findMigrationsDirectory(options{Database: "example"})
	if err != nil || got != expected || repo != root {
		t.Fatalf("ancestor discovery: %q %q %v", got, repo, err)
	}
	if err := os.Mkdir(filepath.Join(root, "src", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := findMigrationsDirectory(options{Database: "example"}); err == nil {
		t.Fatal("crossed repository boundary")
	}
	got, _, err = findMigrationsDirectory(options{Database: "example", MigrationsRoot: filepath.Join(root, "migrations")})
	if err != nil || got != expected {
		t.Fatalf("explicit directory did not override discovery: %q %v", got, err)
	}
}
