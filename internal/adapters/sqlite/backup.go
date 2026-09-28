package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Backup writes a consistent copy of the database to path.
//
// This exists because the obvious thing is wrong. "Backup is copying one
// file" is true of SQLite in general but not of a live WAL database: `cp`
// while the server is running can capture the main file and its -wal out of
// step and produce a copy that will not open. VACUUM INTO takes the same
// snapshot the database itself would, without blocking writers for the
// duration and without needing the server stopped.
//
// The copy also arrives defragmented and without the WAL, which is what you
// want from an archive.
func (db *DB) Backup(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("%w: backup path is required", ErrSchema)
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving backup path: %w", err)
	}

	// VACUUM INTO refuses to overwrite, which is the right default for a
	// backup — but it fails with a message about the file existing rather
	// than about what the operator did, so say it plainly here.
	if _, err := os.Stat(absolute); err == nil {
		return fmt.Errorf("%w: %s already exists; backups never overwrite", ErrSchema, absolute)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking backup path: %w", err)
	}

	// The destination is a filename, not user input in a query: it cannot be
	// bound as a parameter because VACUUM INTO takes a literal. It is quoted
	// with SQLite's own escaping (a doubled single quote) rather than
	// concatenated raw.
	quoted := "'" + escapeSQLString(absolute) + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("backing up to %s: %w", absolute, err)
	}
	return nil
}

// escapeSQLString escapes a string literal for SQLite by doubling single
// quotes, which is the only escape its literal syntax has.
func escapeSQLString(value string) string {
	escaped := make([]byte, 0, len(value))
	for i := range len(value) {
		if value[i] == '\'' {
			escaped = append(escaped, '\'', '\'')
			continue
		}
		escaped = append(escaped, value[i])
	}
	return string(escaped)
}
