package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var versionRangePattern = regexp.MustCompile(`^(\d{3})-(\d{3})$`)
var versionPattern = regexp.MustCompile(`^\d{3}$`)

// checksumMigration returns the lowercase SHA-256 digest of the exact file bytes
// represented by contents, including comments, wrappers, and line endings.
func checksumMigration(contents string) string {
	checksum := sha256.Sum256([]byte(contents))
	return fmt.Sprintf("%x", checksum)
}

// parseMigrationFilename extracts a three-digit version and SQL migration name.
// The boolean result is false for names outside the established filename format.
func parseMigrationFilename(filename string) (migrationRecord, string, bool) {
	matches := migrationFilenamePattern.FindStringSubmatch(filename)
	if matches == nil {
		return migrationRecord{}, "", false
	}
	return migrationRecord{Version: matches[1], Filename: filename}, matches[2], true
}

// migrationRecords projects loaded files into ledger-comparison records without
// assigning execution status or baseline flags.
func migrationRecords(migrations []migration) []migrationRecord {
	records := make([]migrationRecord, 0, len(migrations))
	for _, item := range migrations {
		records = append(records, migrationRecord{Version: item.Version, Filename: item.Filename, Checksum: item.Checksum})
	}
	return records
}

// parseVersionSelection expands comma-separated three-digit versions and inclusive
// ranges, removing duplicates in first-seen order. Every selected version must
// appear in known; malformed, descending, and unknown selections fail.
func parseVersionSelection(value string, known map[string]struct{}) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("--versions requires a value")
	}
	selected := make([]string, 0)
	seen := make(map[string]struct{})
	for _, rawItem := range strings.Split(value, ",") {
		item := strings.TrimSpace(rawItem)
		if matches := versionRangePattern.FindStringSubmatch(item); matches != nil {
			start, _ := strconv.Atoi(matches[1])
			end, _ := strconv.Atoi(matches[2])
			if start > end {
				return nil, fmt.Errorf("invalid migration range: %s", item)
			}
			for version := start; version <= end; version++ {
				formatted := fmt.Sprintf("%03d", version)
				if _, exists := seen[formatted]; !exists {
					selected = append(selected, formatted)
					seen[formatted] = struct{}{}
				}
			}
			continue
		}
		if !versionPattern.MatchString(item) {
			return nil, fmt.Errorf("invalid migration version: %s", item)
		}
		if _, exists := seen[item]; !exists {
			selected = append(selected, item)
			seen[item] = struct{}{}
		}
	}
	for _, version := range selected {
		if _, exists := known[version]; !exists {
			return nil, fmt.Errorf("unknown migration version: %s", version)
		}
	}
	return selected, nil
}

// getAppliedBy selects the first nonblank explicit, CI, or operating-system actor
// for history records, falling back to unknown when none is available.
func getAppliedBy() string {
	for _, name := range []string{"MIGRATION_APPLIED_BY", "GITHUB_ACTOR", "USER"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return "unknown"
}

// getGitSHA prefers explicit or CI revision metadata, then queries HEAD at the
// application repository root. Missing Git metadata returns nil for a SQL NULL.
// The Git lookup is bounded; callers resolve it once before taking database locks.
func getGitSHA(ctx context.Context, repositoryRoot string) any {
	for _, name := range []string{"GIT_SHA", "GITHUB_SHA"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", repositoryRoot, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		return nil
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return nil
	}
	return value
}
