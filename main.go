package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
)

// main runs the CLI with interrupt and termination cancellation, reporting errors
// to stderr and returning a nonzero process exit status on failure.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ddl-mt:", err)
		os.Exit(1)
	}
}

// runCLI validates command arguments and dispatches offline or database work.
// Database commands share the configured deadline; output is written to out.
func runCLI(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" {
		printUsage(out)
		return nil
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			printUsage(out)
			return nil
		}
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(out, "ddl-mt %s — damn delightful migrations\n", currentVersion())
		return nil
	}
	if args[0] == "create-config" {
		return createConfigCommand(args[1:], out)
	}
	o, command, rest, err := parseOptions(args)
	if err != nil {
		return err
	}
	switch command {
	case "targets", "validate", "init", "status", "check", "apply":
		if len(rest) != 0 {
			return fmt.Errorf("%s does not accept arguments; put global flags before the command", command)
		}
	case "new":
		if len(rest) != 1 {
			return errors.New("usage: ddl-mt [flags] new <snake_case_name>")
		}
	case "baseline":
		if len(rest) != 2 || rest[0] != "--versions" {
			return errors.New("usage: ddl-mt [flags] baseline --versions 001-003")
		}
	case "repair":
		if len(rest) != 4 || rest[0] != "--version" || rest[2] != "--action" {
			return errors.New("usage: ddl-mt [flags] repair --version 001 --action retry|applied")
		}
	default:
		return fmt.Errorf("unknown migration command: %s", command)
	}
	if command == "targets" {
		p, err := readProject(o.ProjectPath)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(p.Targets))
		for name := range p.Targets {
			names = append(names, name)
		}
		sort.Strings(names)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TARGET\tDATABASE\tMIGRATIONS DIR")
		for _, name := range names {
			t := p.Targets[name]
			fmt.Fprintf(tw, "%s\t%s\t%s\n", name, t.Database, t.MigrationsDir)
		}
		return tw.Flush()
	}
	dir, root, err := findMigrationsDirectory(o)
	if err != nil {
		return err
	}
	if command == "new" {
		path, err := newMigration(dir, rest[0], o)
		if err == nil {
			fmt.Fprintln(out, path)
		}
		return err
	}
	migrations, err := loadTargetMigrations(dir, o)
	if err != nil {
		return err
	}
	if command == "validate" {
		for _, m := range migrations {
			if m.NonTransactional {
				fmt.Fprintf(out, "Non-transactional: %s\n", m.Filename)
			}
		}
		fmt.Fprintf(out, "Validated %d migration(s) for %s.\n", len(migrations), o.Database)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	switch command {
	case "init":
		return initCommand(ctx, o, out)
	case "status", "check":
		return statusCommand(ctx, o, migrations, command == "check", out)
	case "baseline":
		return baselineCommand(ctx, o, migrations, root, rest[1], out)
	case "apply":
		return applyCommand(ctx, o, migrations, root, out)
	case "repair":
		return repairCommand(ctx, o, migrations, rest[1], rest[3], out)
	}
	return nil
}

// printUsage writes the supported commands, global flags, and credential
// precedence without reading configuration files or contacting a database.
func printUsage(out io.Writer) {
	fmt.Fprint(out, `ddl-mt — damn delightful migrations

Usage: ddl-mt [--project <ddl-mt.json>] --target <name> [flags] <command>
       ddl-mt --database <name> [--migrations-root <path>] [flags] <command>

Commands:
  create-config [--path <path>]  Create a starter project config, offline
  targets                       List configured targets, offline
  new <snake_case_name>         Create the next migration file, offline
  validate                      Validate local SQL files, offline
  init                          Initialize the configured history table
  status                        Show PENDING, BASELINE, APPLIED, or dirty states
  check                         Fail on pending, changed, missing, or dirty history
  apply                         Apply pending migrations in order
  baseline --versions <list>    Record audited existing changes without running SQL
  repair --version NNN --action retry|applied
                                Resolve audited failed non-transactional history
  version                       Print version

Global flags must precede the command:
  --config <path>                Credentials JSON containing {"DB_DSN":"..."}
  --connection-mode <mode>      transaction (default) or session; overrides target
  --role <name>                 Role to switch to for all database work; overrides target
  --lock-timeout <duration>     Maximum migration lock wait (default 30s)
  --timeout <duration>           Database deadline including lock waits (default 5m)

Named targets resolve migration paths relative to their project file.
Credentials: explicit --config, target credentials_file or dsn_env, otherwise
DB_DSN then <user-config>/ddl-mt/<project>/<target>.json.
Direct mode uses <user-config>/ddl-mt/<database>.json and also supports
MIGRATIONS_DATABASE and MIGRATIONS_ROOT. Offline commands never read credentials.
Baseline lists accept comma-separated versions and ranges, e.g. 001-004,006.

Developed by QBite Technologies (https://qbite.tech). MIT License.
Documentation and issues: https://github.com/qbite-tech/ddl-mt
`)
}
