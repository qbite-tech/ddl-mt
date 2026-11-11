package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// getDSN selects credentials from an explicit file, a target environment variable,
// DB_DSN, or the scoped user-config file, in that order. Selected sources never
// fall back on error; returned strings are trimmed but otherwise left literal.
func getDSN(config options) (string, error) {
	path := config.ConfigPath
	if path == "" {
		env := config.DSNEnv
		if env == "" {
			env = "DB_DSN"
		}
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			return value, nil
		}
		if config.DSNEnv != "" {
			return "", fmt.Errorf("environment variable %s must be set", env)
		}
		root, err := os.UserConfigDir()
		if err != nil {
			return "", errors.New("cannot locate user config directory; set DB_DSN or pass --config")
		}
		path = filepath.Join(root, "ddl-mt", config.Database+".json")
		if config.Project != "" {
			path = filepath.Join(root, "ddl-mt", config.Project, config.Target+".json")
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open database config: %w; supply a JSON file containing DB_DSN or set the DB_DSN environment variable", err)
	}
	defer file.Close()
	var values struct {
		DSN string `json:"DB_DSN"`
	}
	if err := decodeJSONObject(file, &values); err != nil {
		// JSON errors can contain credentials, including duplicate/unknown keys.
		return "", fmt.Errorf("invalid database config %s: expected one JSON object with a unique DB_DSN string", path)
	}
	value := strings.TrimSpace(values.DSN)
	if value == "" {
		return "", fmt.Errorf("database config %s must contain a non-empty DB_DSN string", path)
	}
	return value, nil
}

// connect opens a dedicated PostgreSQL connection with a bounded connect timeout,
// simple-query execution, and fixed application/string settings. Returned errors
// classify failures without exposing the connection string or raw server messages.
func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database connection string (credentials omitted)")
	}
	config.ConnectTimeout = 10 * time.Second
	config.RuntimeParams["application_name"] = "ddl-mt"
	config.RuntimeParams["standard_conforming_strings"] = "on"
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, connectionFailure(err)
	}
	return connection, nil
}

// connectAndVerify resolves credentials and checks the connected database name.
// Write callers also require a non-read-only connection. Rejected connections are
// closed; the caller owns and must close a successfully returned connection.
func connectAndVerify(ctx context.Context, config options, writable bool) (*pgx.Conn, error) {
	dsn, err := getDSN(config)
	if err != nil {
		return nil, err
	}
	connection, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	var actual string
	var readOnly bool
	if err := connection.QueryRow(ctx, `
		SELECT current_database(), current_setting('transaction_read_only')::boolean
	`).Scan(&actual, &readOnly); err != nil {
		connection.Close(context.Background())
		return nil, fmt.Errorf("verify target database: %w", err)
	}
	if actual != config.Database {
		connection.Close(context.Background())
		return nil, fmt.Errorf("refusing target database %s: selected migration sequence is %s", actual, config.Database)
	}
	if writable && readOnly {
		connection.Close(context.Background())
		return nil, errors.New("database connection is read-only; a write-capable DB_DSN is required")
	}
	return connection, nil
}
