// Package sqlite is the storage adapter. It is the only package in the
// repository that knows SQL exists.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	// The pure-Go driver: no CGO, so the binary stays static and
	// cross-compilable (ADR 009).
	_ "modernc.org/sqlite"
)

// DB is an open database with the product's pragmas applied and its schema
// migrated.
type DB struct {
	*sql.DB
	path string
}

// Open opens (creating if absent) the database at path and brings it to the
// current schema version.
//
// The pragmas are not tuning knobs, they are part of the design:
//
//   - journal_mode=WAL: readers never block the single writer (ADR 001).
//   - auto_vacuum=INCREMENTAL: must be set before anything writes to the
//     database, including the journal_mode change. On an already written
//     database this pragma is a silent no-op, and switching later needs a
//     full VACUUM — exactly what retention sweeps must never do on a live
//     store. Getting this wrong is unrecoverable without downtime, so Open
//     verifies it took effect rather than assuming.
//   - foreign_keys=ON: SQLite defaults this off, and the schema relies on
//     ON DELETE CASCADE.
//   - busy_timeout: with one writer, a concurrent write waits rather than
//     failing immediately.
func Open(ctx context.Context, path string) (*DB, error) {
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// One writer: SQLite serialises writes anyway, and letting the pool open
	// several connections only converts waiting into SQLITE_BUSY errors.
	handle.SetMaxOpenConns(1)

	db := &DB{DB: handle, path: path}

	if err := db.applyPragmas(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	// Before migrating, not after: migration 0007 creates the search index, so
	// a driver without the module would fail there with "no such module:
	// fts5" halfway through a schema step. Checked here, the message names the
	// build rather than the migration (ADR 011).
	if err := db.verifyFTS5(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	if err := db.migrate(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	// Verified after migrating, not before: auto_vacuum is only persisted to
	// the file header once a table exists, so on a brand new database the
	// check is meaningless until the schema has been created.
	if err := db.verifyAutoVacuum(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	return db, nil
}

// Path is the file backing this database.
func (db *DB) Path() string { return db.path }

func (db *DB) applyPragmas(ctx context.Context) error {
	pragmas := []string{
		// auto_vacuum comes first, and the order is not cosmetic: setting
		// journal_mode writes to the database, and once the file header has
		// been written auto_vacuum can no longer be changed without a full
		// VACUUM. Putting WAL first silently produces a database with
		// auto_vacuum disabled forever.
		"PRAGMA auto_vacuum = INCREMENTAL",
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		// NORMAL trades an fsync per commit for a small window of risk on OS
		// crash. With WAL, a process crash is still safe; the exposure is
		// power loss losing the last commits. For an event store fed by
		// clients that retry, that trade is the right one.
		"PRAGMA synchronous = NORMAL",
	}
	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("applying %q: %w", pragma, err)
		}
	}
	return nil
}

// verifyFTS5 makes sure this build's driver carries the full-text module, and
// the tokenizer the index is actually built with.
//
// modernc.org/sqlite compiles both in today — verified against the module, on
// SQLite 3.53.3 — but "today" is the operative word: a driver upgrade that
// dropped either would otherwise turn every search into a query that finds
// nothing, silently and forever, because a search box that returns no results
// looks exactly like a search box with no matches. Failing at startup with a
// sentence somebody can act on is the only version of this that is honest.
//
// The probe names `trigram` rather than settling for FTS5 in general, because
// the two are separable: the module could be present and the tokenizer absent,
// and then migration 0007 would be the thing that failed, with a message about
// a CREATE statement rather than about this build.
//
// It builds a real virtual table rather than reading a compile-time option,
// because the compile-time option is what the driver claims and the table is
// what it can do. It goes in the temp schema so the check leaves no trace in
// the file, and it is dropped immediately: with a single connection in the
// pool, a leftover temp table would outlive the check.
func (db *DB) verifyFTS5(ctx context.Context) error {
	const probe = "trapline_fts5_probe"
	if _, err := db.ExecContext(ctx,
		"CREATE VIRTUAL TABLE temp."+probe+" USING fts5(probe, tokenize='trigram remove_diacritics 1')"); err != nil {
		return fmt.Errorf(
			"%w: this build's SQLite driver has no FTS5 module with the trigram tokenizer, so issue "+
				"search cannot work; the driver must be built with FTS5 enabled (ADR 011): %w",
			ErrSchema, err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE temp."+probe); err != nil {
		return fmt.Errorf("dropping the FTS5 probe table: %w", err)
	}
	return nil
}

// verifyAutoVacuum makes sure incremental auto-vacuum is actually on. If a
// database was created without it, the pragma above silently did nothing,
// and reclaiming space after a retention sweep would need a full VACUUM.
// Better to fail loudly at startup than to discover it under load.
func (db *DB) verifyAutoVacuum(ctx context.Context) error {
	const incremental = 2
	var mode int
	if err := db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return fmt.Errorf("reading auto_vacuum: %w", err)
	}
	if mode != incremental {
		return fmt.Errorf(
			"%w: auto_vacuum is %d, want %d (incremental); this database was created "+
				"without it and changing it now requires a full VACUUM while offline",
			ErrSchema, mode, incremental)
	}
	return nil
}
