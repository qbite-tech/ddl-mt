package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var migrationFilenamePattern = regexp.MustCompile(`^(\d{3})_([a-z0-9_]+)\.sql$`)
var targetDatabaseComment = regexp.MustCompile(`(?im)^\s*--\s*Target database:\s*([^\r\n]+?)\s*$`)
var transactionMetadata = regexp.MustCompile(`(?im)^\s*--\s*migration:transaction=(true|false)\s*$`)

// PostgreSQL allows non-ASCII letters in dollar-quote tags.
var dollarTag = regexp.MustCompile(`^\$(?:[a-zA-Z_\x{80}-\x{10FFFF}][a-zA-Z0-9_\x{80}-\x{10FFFF}]*)?\$`)

type migration struct {
	Version, Name, Filename, Path, Contents, SQL, Checksum string
	Statements                                             []string
	NonTransactional                                       bool
}

type migrationRecord struct {
	Version, Filename, Checksum, Status string
	IsBaseline                          bool
}

type sqlStatement struct{ text, code string }

// splitSQL returns complete statements and standalone metadata comments.
// It recognizes PostgreSQL lexical boundaries; quoted bodies are opaque,
// so semicolons and BEGIN/END inside DO blocks never become transaction commands.
// This is a statement splitter, not a semantic SQL validator; PostgreSQL validates SQL.
func splitSQL(sql string) ([]sqlStatement, string, error) {
	var comments strings.Builder
	var result []sqlStatement
	var code strings.Builder
	start := 0
	for i := 0; i < len(sql); {
		if strings.HasPrefix(sql[i:], "--") {
			commentStart := i
			end := strings.IndexAny(sql[i:], "\r\n")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 1
			}
			lineStart := strings.LastIndexAny(sql[:commentStart], "\r\n") + 1
			if strings.TrimSpace(code.String()) == "" && strings.TrimSpace(sql[lineStart:commentStart]) == "" {
				comments.WriteString(sql[commentStart:i])
				comments.WriteByte('\n')
			}
			code.WriteByte(' ')
			continue
		}
		if strings.HasPrefix(sql[i:], "/*") {
			depth := 1
			i += 2
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return nil, "", errors.New("unterminated SQL comment")
			}
			code.WriteByte(' ')
			continue
		}
		if sql[i] == '\'' || sql[i] == '"' {
			end, err := scanSQLQuote(sql, i)
			if err != nil {
				return nil, "", err
			}
			i = end
			code.WriteString(" ? ")
			continue
		}
		if sql[i] == '$' && (i == 0 || !identifierByte(sql[i-1])) {
			if tag := dollarTag.FindString(sql[i:]); tag != "" {
				end := strings.Index(sql[i+len(tag):], tag)
				if end < 0 {
					return nil, "", errors.New("unterminated dollar-quoted SQL body")
				}
				i += len(tag)*2 + end
				code.WriteString(" ? ")
				continue
			}
		}
		if sql[i] == '\\' {
			return nil, "", errors.New("psql meta-commands are not supported")
		}
		if sql[i] == ';' {
			if strings.TrimSpace(code.String()) != "" {
				result = append(result, sqlStatement{sql[start : i+1], strings.TrimSpace(code.String())})
			}
			code.Reset()
			i++
			start = i
			continue
		}
		code.WriteByte(sql[i])
		i++
	}
	if strings.TrimSpace(code.String()) != "" {
		result = append(result, sqlStatement{sql[start:], strings.TrimSpace(code.String())})
	}
	return result, comments.String(), nil
}

// identifierByte recognizes ASCII identifier characters and non-ASCII bytes
// when deciding whether an E prefix or dollar quote starts a separate SQL token.
func identifierByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '$' || b >= 128
}

// prepareMigration validates standalone migration metadata and transaction
// boundaries, then returns executable statements and the non-transactional flag.
// It strips an optional outer wrapper without changing the file used for hashing.
func prepareMigration(contents, database string) ([]string, bool, error) {
	statements, comments, err := splitSQL(contents)
	if err != nil {
		return nil, false, err
	}
	if len(statements) == 0 {
		return nil, false, errors.New("migration has no SQL statements")
	}
	targets := targetDatabaseComment.FindAllStringSubmatch(comments, -1)
	if len(targets) > 1 {
		return nil, false, errors.New("expected at most one Target database comment")
	}
	if len(targets) == 1 && targets[0][1] != database {
		return nil, false, fmt.Errorf("targets database %s, not %s", targets[0][1], database)
	}
	meta := transactionMetadata.FindAllStringSubmatch(comments, -1)
	if len(meta) > 1 {
		return nil, false, errors.New("multiple migration:transaction declarations")
	}
	nonAtomic := len(meta) == 1 && strings.EqualFold(meta[0][1], "false")
	first := strings.ToUpper(strings.Join(strings.Fields(statements[0].code), " "))
	last := strings.ToUpper(strings.Join(strings.Fields(statements[len(statements)-1].code), " "))
	wrapped := first == "BEGIN" || first == "START TRANSACTION"
	if wrapped {
		if nonAtomic {
			return nil, false, errors.New("non-transactional migration cannot have a transaction wrapper")
		}
		if last != "COMMIT" || len(statements) < 3 {
			return nil, false, errors.New("transaction wrapper must end with COMMIT and contain SQL")
		}
		statements = statements[1 : len(statements)-1]
	}
	result := make([]string, 0, len(statements))
	for _, stmt := range statements {
		words := strings.Fields(strings.ToUpper(stmt.code))
		switch words[0] {
		case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE":
			return nil, false, errors.New("embedded transaction control is not supported")
		case "PREPARE":
			if len(words) > 1 && words[1] == "TRANSACTION" {
				return nil, false, errors.New("prepared transactions are not supported")
			}
		case "SET", "RESET", "DISCARD":
			// Keep the session's lexical settings stable for statement splitting.
			if strings.Contains(strings.ToLower(stmt.text), "standard_conforming_strings") || words[0] == "DISCARD" || (len(words) > 1 && words[1] == "ALL") {
				return nil, false, errors.New("changing SQL string parsing settings is not supported")
			}
		}
		if isConcurrentIndexStatement(stmt.code) {
			if wrapped || (len(meta) == 1 && !nonAtomic) {
				return nil, false, errors.New("concurrent index operations are non-transactional")
			}
			nonAtomic = true
		}
		result = append(result, stmt.text)
	}
	return result, nonAtomic, nil
}

// loadMigrations loads a directory using the default SQL format and expected
// database name. Production target-specific callers use loadTargetMigrations.
func loadMigrations(directory, database string) ([]migration, error) {
	return loadTargetMigrations(directory, options{Database: database})
}

// loadTargetMigrations reads regular SQL files, validates their contents and
// contiguous versions, and preserves original bytes for checksums. Documentation
// and other non-SQL files are ignored; malformed SQL filenames fail.
func loadTargetMigrations(directory string, o options) ([]migration, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var migrations []migration
	for _, entry := range entries {
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".sql") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("migration must be a regular file: %s", entry.Name())
		}
		record, name, ok := parseMigrationFilename(entry.Name())
		if !ok {
			return nil, fmt.Errorf("invalid migration filename %s; expected NNN_snake_case.sql", entry.Name())
		}
		path := filepath.Join(directory, entry.Name())
		bytes, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		contents := string(bytes)
		statements, nonAtomic, err := prepareMigration(contents, o.Database)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{Version: record.Version, Name: name, Filename: entry.Name(), Path: path, Contents: contents, Checksum: checksumMigration(contents), SQL: strings.Join(statements, "\n"), Statements: statements, NonTransactional: nonAtomic})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	for i, m := range migrations {
		if expected := fmt.Sprintf("%03d", i+1); m.Version != expected {
			return nil, fmt.Errorf("migration sequence has a gap or duplicate: expected %s, found %s", expected, m.Version)
		}
	}
	return migrations, nil
}

// newMigration exclusively creates the next numbered SQL template after checking
// the existing sequence and requested name. It never overwrites an existing file;
// the caller must supply an existing migration directory.
func newMigration(directory, name string, o options) (string, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`).MatchString(name) {
		return "", errors.New("migration name must be snake_case")
	}
	migrations, err := loadTargetMigrations(directory, o)
	if err != nil {
		return "", err
	}
	if len(migrations) >= 999 {
		return "", errors.New("three-digit migration sequence is full")
	}
	path := filepath.Join(directory, fmt.Sprintf("%03d_%s.sql", len(migrations)+1, name))
	body := "-- " + name + "\n-- Add SQL below. ddl-mt owns the transaction.\n"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(body)
	closeErr := f.Close()
	return path, errors.Join(writeErr, closeErr)
}

// scanSQLQuote finds a quoted token's end, retaining E-string escape rules for
// newline-separated continuation segments. Its result is an exclusive offset.
func scanSQLQuote(sql string, start int) (int, error) {
	quote := sql[start]
	escape := quote == '\'' && start > 0 && (sql[start-1] == 'E' || sql[start-1] == 'e') && (start == 1 || !identifierByte(sql[start-2]))
	for i := start + 1; i < len(sql); {
		if escape && sql[i] == '\\' {
			i += 2
			continue
		}
		if sql[i] != quote {
			i++
			continue
		}
		if i+1 < len(sql) && sql[i+1] == quote {
			i += 2
			continue
		}
		end := i + 1
		if quote == '\'' {
			next, newline, err := skipSQLTrivia(sql, end)
			if err != nil {
				return 0, err
			}
			if newline && next < len(sql) && sql[next] == '\'' {
				i = next + 1
				continue
			}
		}
		return end, nil
	}
	return 0, errors.New("unterminated SQL quote")
}

// skipSQLTrivia skips SQL whitespace and comments while remembering whether a
// newline occurred, which PostgreSQL requires for continued string literals.
func skipSQLTrivia(sql string, start int) (int, bool, error) {
	newline := false
	i := start
	for i < len(sql) {
		switch {
		case strings.ContainsRune(" \t\r\n\f", rune(sql[i])):
			newline = newline || sql[i] == '\n' || sql[i] == '\r'
			i++
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
		case strings.HasPrefix(sql[i:], "/*"):
			depth := 1
			i += 2
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					newline = newline || sql[i] == '\n' || sql[i] == '\r'
					i++
				}
			}
			if depth != 0 {
				return 0, false, errors.New("unterminated SQL comment")
			}
		default:
			return i, newline, nil
		}
	}
	return i, newline, nil
}

var concurrentIndexStatement = regexp.MustCompile(`(?i)^(?:CREATE\s+(?:UNIQUE\s+)?INDEX|DROP\s+INDEX)\s+CONCURRENTLY\b`)
var reindexStatement = regexp.MustCompile(`(?i)^REINDEX(?:\s*\(([^)]*)\))?\s+(?:INDEX|TABLE|SCHEMA|DATABASE|SYSTEM)\s+(CONCURRENTLY\b)?`)

// isConcurrentIndexStatement recognizes concurrent index command options rather
// than occurrences in table/column names, comments, or quoted SQL values.
func isConcurrentIndexStatement(code string) bool {
	if concurrentIndexStatement.MatchString(code) {
		return true
	}
	matches := reindexStatement.FindStringSubmatch(code)
	if matches == nil {
		return false
	}
	if matches[2] != "" {
		return true
	}
	for _, option := range strings.Split(strings.ToUpper(matches[1]), ",") {
		words := strings.Fields(option)
		if len(words) > 0 && words[0] == "CONCURRENTLY" {
			return len(words) == 1 || (words[1] != "FALSE" && words[1] != "OFF" && words[1] != "0")
		}
	}
	return false
}
