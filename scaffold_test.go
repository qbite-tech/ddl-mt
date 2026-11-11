package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateConfigDestinations checks default, directory, and filename output
// paths and confirms starter configurations require explicit database selection.
func TestCreateConfigDestinations(t *testing.T) {
	for _, destination := range []string{"", "nested/repo", "nested/repo/custom.json"} {
		t.Run(destination, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("DB_DSN", "not a connection string")
			args := []string{"create-config"}
			want := filepath.Join(dir, "ddl-mt.json")
			if destination != "" {
				args = append(args, "--path", destination)
				want = filepath.Join(dir, destination)
				if filepath.Ext(want) != ".json" {
					want = filepath.Join(want, "ddl-mt.json")
				}
			}
			var output bytes.Buffer
			if err := runCLI(context.Background(), args, &output); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			var project projectConfig
			if err := json.Unmarshal(contents, &project); err != nil {
				t.Fatal(err)
			}
			target, ok := project.Targets["default"]
			if !ok || project.Project != "" || target.Database != "" || target.MigrationsDir != "migrations" || target.DSNEnv != "DB_DSN" {
				t.Fatalf("unexpected starter: %+v", project)
			}
			if _, err := readProject(want); err == nil {
				t.Fatal("blank starter must require configuration before use")
			}
			if !strings.Contains(output.String(), want) {
				t.Fatalf("missing destination in output: %s", output.String())
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(want), "migrations")); !os.IsNotExist(err) {
				t.Fatal("scaffolding unexpectedly created migrations")
			}
		})
	}
}

// TestCreateConfigPreservesExistingFile verifies repeated scaffolding preserves
// existing bytes, including incomplete or invalid configuration files.
func TestCreateConfigPreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ddl-mt.json")
	original := "existing configuration, even if incomplete"
	putFile(t, path, original)
	for _, destination := range []string{dir, path} {
		var output bytes.Buffer
		err := runCLI(context.Background(), []string{"create-config", "--path", destination}, &output)
		if err != nil || !strings.Contains(output.String(), "already exists") {
			t.Fatalf("expected successful existing-file notice, got %q, %v", output.String(), err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != original {
			t.Fatalf("existing config changed: %q %v", got, err)
		}
	}
}

// TestCreateConfigPreservesDanglingSymlink ensures exclusive creation cannot
// follow an existing symlink or write to its missing destination.
func TestCreateConfigPreservesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ddl-mt.json")
	target := filepath.Join(dir, "missing.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"create-config", "--path", dir}, &output); err != nil || !strings.Contains(output.String(), "already exists") {
		t.Fatalf("expected existing-symlink notice, got %q, %v", output.String(), err)
	}
	if got, err := os.Readlink(path); err != nil || got != target {
		t.Fatal("existing symlink changed")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("symlink destination was written")
	}
}

// TestInformationalCommandsDoNotCreateConfig verifies help/version and invalid
// scaffolding arguments leave the working directory untouched.
func TestInformationalCommandsDoNotCreateConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"version"}, {"create-config", "--help"}} {
		if err := runCLI(context.Background(), args, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"create-config", "unexpected"}, {"create-config", "--path", ""}, {"create-config", "--unknown"}} {
		if err := runCLI(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected files: %v %v", entries, err)
	}
}
