package main

import (
	"context"
	"strings"
	"testing"
)

// TestChecksumMigrationPreservesBytes pins the SHA-256 history contract to a
// known digest and verifies that line-ending changes are treated as drift.
func TestChecksumMigrationPreservesBytes(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := checksumMigration("abc"); got != want {
		t.Fatalf("checksum changed: %s", got)
	}
	if checksumMigration("SELECT 1;\n") == checksumMigration("SELECT 1;\r\n") {
		t.Fatal("line endings were normalized before checksumming")
	}
}

// TestAuditEnvironmentPrecedence verifies explicit audit metadata overrides CI
// defaults, while absent Git metadata remains nullable rather than fabricated.
func TestAuditEnvironmentPrecedence(t *testing.T) {
	t.Setenv("MIGRATION_APPLIED_BY", " explicit-operator ")
	t.Setenv("GITHUB_ACTOR", "ci-operator")
	if got := getAppliedBy(); got != "explicit-operator" {
		t.Fatalf("wrong explicit actor: %s", got)
	}
	t.Setenv("MIGRATION_APPLIED_BY", "")
	if got := getAppliedBy(); got != "ci-operator" {
		t.Fatalf("wrong CI actor: %s", got)
	}
	t.Setenv("GIT_SHA", " explicit-sha ")
	t.Setenv("GITHUB_SHA", "ci-sha")
	root := t.TempDir()
	if got := getGitSHA(context.Background(), root); got != "explicit-sha" {
		t.Fatalf("wrong explicit SHA: %v", got)
	}
	t.Setenv("GIT_SHA", "")
	if got := getGitSHA(context.Background(), root); got != "ci-sha" {
		t.Fatalf("wrong CI SHA: %v", got)
	}
	t.Setenv("GITHUB_SHA", "")
	if got := getGitSHA(context.Background(), root); got != nil {
		t.Fatalf("invented Git revision: %v", got)
	}
}

// TestCurrentVersionPrecedence ensures release linker metadata takes precedence
// over the embedded source version and normalizes the conventional v prefix.
func TestCurrentVersionPrecedence(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })
	version = " v1.2.3 "
	if got := currentVersion(); got != "1.2.3" {
		t.Fatalf("wrong linker version: %s", got)
	}
	version = ""
	if got := currentVersion(); got != strings.TrimSpace(sourceVersion) {
		t.Fatalf("wrong development version: %s", got)
	}
}
