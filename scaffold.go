package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Required names stay blank so a starter file cannot select a database.
const projectPlaceholder = `{
  "project": "",
  "targets": {
    "default": {
      "database": "",
      "migrations_dir": "migrations",
      "dsn_env": "DB_DSN",
      "connection_mode": "transaction"
    }
  }
}
`

// createConfigCommand writes an offline starter JSON file at the requested path.
// It creates missing parent directories, preserves existing files and symlinks,
// and leaves database selection blank until the operator supplies it.
func createConfigCommand(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("create-config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	destination := flags.String("path", ".", "destination directory or JSON filename")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *destination == "" {
		return errors.New("usage: ddl-mt create-config [--path <directory-or-file.json>]")
	}
	path, err := filepath.Abs(*destination)
	if err != nil {
		return fmt.Errorf("resolve config destination: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect config destination: %w", err)
	}
	if (err == nil && info.IsDir()) || (errors.Is(err, os.ErrNotExist) && filepath.Ext(path) != ".json") {
		path = filepath.Join(path, "ddl-mt.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// Exclusive creation also protects existing symlinks and concurrent creators.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		fmt.Fprintf(out, "Config already exists: %s. Existing file preserved.\n", path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	_, writeErr := file.WriteString(projectPlaceholder)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(out, "Created %s.\nFill in project and database before running validate.\nTo use a history table other than public.schema_migrations, set ledger_table.\n", path)
	return nil
}
