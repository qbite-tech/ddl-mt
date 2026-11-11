package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var databaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
var ledgerNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`)

type targetConfig struct {
	ConnectionMode        string `json:"connection_mode,omitempty"`
	Database              string `json:"database"`
	MigrationsDir         string `json:"migrations_dir"`
	DSNEnv                string `json:"dsn_env,omitempty"`
	CredentialsFile       string `json:"credentials_file,omitempty"`
	LedgerTable           string `json:"ledger_table,omitempty"`
	LockKey               *int64 `json:"lock_key,omitempty"`
	AllowNonTransactional bool   `json:"allow_non_transactional,omitempty"`
}

type projectConfig struct {
	Project string                  `json:"project"`
	Targets map[string]targetConfig `json:"targets"`
}

type options struct {
	ConnectionMode                                      string
	LockTimeout                                         time.Duration
	Database, MigrationsRoot, MigrationsDir, ConfigPath string
	Project, ProjectPath, Target, DSNEnv                string
	LedgerTable                                         string
	LockKey                                             *int64
	AllowNonTransactional                               bool
	Timeout                                             time.Duration
}

// parseOptions resolves global flags, environment defaults, and an explicitly
// selected project target. It returns the command and its unparsed arguments;
// credential files are not opened here.
func parseOptions(args []string) (options, string, []string, error) {
	var o options
	f := flag.NewFlagSet("ddl-mt", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.ProjectPath, "project", "", "project JSON file (default: discover ddl-mt.json)")
	f.StringVar(&o.Target, "target", "", "named project target")
	f.StringVar(&o.Database, "database", strings.TrimSpace(os.Getenv("MIGRATIONS_DATABASE")), "database for direct mode")
	f.StringVar(&o.MigrationsRoot, "migrations-root", strings.TrimSpace(os.Getenv("MIGRATIONS_ROOT")), "parent of database directories")
	f.StringVar(&o.ConfigPath, "config", "", "credentials JSON file containing DB_DSN")
	f.StringVar(&o.ConnectionMode, "connection-mode", "", "transaction (default) or session; session requires a direct or session-pooled endpoint")
	f.DurationVar(&o.LockTimeout, "lock-timeout", 30*time.Second, "maximum migration lock wait")
	f.DurationVar(&o.Timeout, "timeout", 5*time.Minute, "database command deadline, including lock waits")
	if err := f.Parse(args); err != nil {
		return o, "", nil, err
	}
	remaining := f.Args()
	if len(remaining) == 0 {
		return o, "", nil, errors.New("a command is required; run ddl-mt --help")
	}
	command := remaining[0]
	if err := validateConnectionMode(o.ConnectionMode); err != nil {
		return o, "", nil, err
	}
	if o.LockTimeout <= 0 {
		return o, "", nil, errors.New("--lock-timeout must be positive")
	}
	if o.Timeout <= 0 {
		return o, "", nil, errors.New("--timeout must be positive")
	}
	if o.Target != "" || o.ProjectPath != "" || command == "targets" {
		if o.Database != "" || o.MigrationsRoot != "" {
			return o, "", nil, errors.New("project targets cannot be combined with --database/--migrations-root or MIGRATIONS_DATABASE/MIGRATIONS_ROOT")
		}
		path, err := findProject(o.ProjectPath)
		if err != nil {
			return o, "", nil, err
		}
		o.ProjectPath = path
		p, err := readProject(path)
		if err != nil {
			return o, "", nil, err
		}
		o.Project = p.Project
		if command == "targets" {
			return o, command, remaining[1:], nil
		}
		target, ok := p.Targets[o.Target]
		if !ok {
			return o, "", nil, fmt.Errorf("select a configured --target; run ddl-mt --project %s targets", path)
		}
		o.Database, o.DSNEnv = target.Database, target.DSNEnv
		o.MigrationsDir = relativeTo(filepath.Dir(path), target.MigrationsDir)
		o.LedgerTable, o.LockKey = target.LedgerTable, target.LockKey
		o.AllowNonTransactional = target.AllowNonTransactional
		if o.ConnectionMode == "" {
			o.ConnectionMode = target.ConnectionMode
		}
		if o.ConfigPath == "" && target.CredentialsFile != "" {
			o.ConfigPath = relativeTo(filepath.Dir(path), target.CredentialsFile)
		}
	} else if !databaseNamePattern.MatchString(o.Database) {
		return o, "", nil, errors.New("select --target from ddl-mt.json, or --database with a lowercase PostgreSQL database name")
	}
	if o.ConnectionMode == "" {
		o.ConnectionMode = "transaction"
	}
	if err := validateConnectionMode(o.ConnectionMode); err != nil {
		return o, "", nil, err
	}
	if o.LedgerTable == "" {
		o.LedgerTable = "public.schema_migrations"
	}
	return o, command, remaining[1:], nil
}

// relativeTo resolves a configured path against base while preserving absolute
// paths. Neither environment variables nor home-directory shortcuts are expanded.
func relativeTo(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// findProject resolves an explicit path or searches ancestors for ddl-mt.json.
// Discovery stops at a Git boundary and reports unreadable nearer candidates
// instead of silently selecting a different project.
func findProject(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(current, "ddl-mt.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect project config: %w", err)
		}
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", errors.New("could not find ddl-mt.json; pass --project <path>")
}

// readProject loads and validates a project object with unambiguous JSON keys.
// It validates every target and reports configuration errors without file contents.
func readProject(path string) (projectConfig, error) {
	var p projectConfig
	f, err := os.Open(path)
	if err != nil {
		return p, fmt.Errorf("open project: %w", err)
	}
	defer f.Close()
	if err := decodeJSONObject(f, &p); err != nil {
		return p, errors.New("invalid project JSON; expected one object with unique keys and valid field names and types (contents omitted)")
	}
	if !aliasPattern.MatchString(p.Project) || len(p.Targets) == 0 {
		return p, errors.New("project requires a simple project name and at least one target")
	}
	for name, target := range p.Targets {
		if !aliasPattern.MatchString(name) {
			return p, fmt.Errorf("invalid target name %q", name)
		}
		if strings.TrimSpace(target.Database) == "" {
			return p, fmt.Errorf("target %s requires a database", name)
		}
		if strings.TrimSpace(target.MigrationsDir) == "" {
			return p, fmt.Errorf("target %s requires migrations_dir", name)
		}
		// These values reach terminals, generated SQL comments, and file paths.
		for field, value := range map[string]string{"database": target.Database, "migrations_dir": target.MigrationsDir, "dsn_env": target.DSNEnv, "credentials_file": target.CredentialsFile} {
			if containsControl(value) {
				return p, fmt.Errorf("target %s %s must not contain control characters", name, field)
			}
		}
		if target.LedgerTable != "" && !ledgerNamePattern.MatchString(target.LedgerTable) {
			return p, fmt.Errorf("target %s ledger_table must be a lowercase schema.table", name)
		}
		if err := validateConnectionMode(target.ConnectionMode); err != nil {
			return p, fmt.Errorf("target %s: %w", name, err)
		}
		if target.DSNEnv != "" && target.CredentialsFile != "" {
			return p, fmt.Errorf("target %s must choose dsn_env or credentials_file", name)
		}
	}
	return p, nil
}

// containsControl reports whether value holds control characters, including
// NUL, newlines, and terminal escape introducers, or invalid UTF-8.
func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return true
		}
	}
	return false
}

// findMigrationsDirectory resolves the selected SQL directory and repository root
// used for Git audit metadata. Direct-mode discovery stops at Git boundaries and
// does not bypass an invalid nearer migration path.
func findMigrationsDirectory(o options) (string, string, error) {
	if o.MigrationsDir != "" {
		return o.MigrationsDir, filepath.Dir(o.ProjectPath), requireDirectory(o.MigrationsDir)
	}
	if o.MigrationsRoot != "" {
		root, err := filepath.Abs(o.MigrationsRoot)
		if err != nil {
			return "", "", err
		}
		dir := filepath.Join(root, o.Database)
		return dir, filepath.Dir(root), requireDirectory(dir)
	}
	current, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for {
		dir := filepath.Join(current, "migrations", o.Database)
		if err := requireDirectory(dir); err == nil {
			return dir, current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", "", fmt.Errorf("could not find migrations/%s; set --migrations-root", o.Database)
}

// requireDirectory verifies that path is an accessible directory, preserving
// filesystem errors so discovery can distinguish absence from other failures.
func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("migration directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("migration path is not a directory: %s", path)
	}
	return nil
}

// validateConnectionMode accepts transaction or session mode, plus the empty
// value used before defaults are resolved. It does not inspect a database endpoint.
func validateConnectionMode(mode string) error {
	switch mode {
	case "", "transaction", "session":
		return nil
	default:
		return errors.New("connection_mode must be transaction or session")
	}
}
