package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one versioned schema step, embedded in the binary so an
// installation never needs files alongside it.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate brings the database to the newest embedded schema version.
//
// Migrations only ever move forward: there are no down migrations. A rollback
// of a schema change on a live single-file store is a restore from backup,
// not a reverse script — pretending otherwise produces down migrations nobody
// has ever run and that do not work when finally needed.
func (db *DB) migrate(ctx context.Context) error {
	if err := db.ensureMigrationTable(ctx); err != nil {
		return err
	}

	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return err
	}

	available, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range available {
		if applied[m.version] {
			continue
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ensureMigrationTable(ctx context.Context) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		) STRICT`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	return nil
}

func (db *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating applied migrations: %w", err)
	}
	return applied, nil
}

// applyMigration runs one migration and records it in the same transaction,
// so a crash mid-migration cannot leave the schema changed but unrecorded.
func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("applying migration %d (%s): %w", m.version, m.name, err)
	}
	// formatTime, not a layout of its own: applied_at is a stored instant like
	// every other one, and the fixed-width layout is what makes text order and
	// chronological order the same thing (ADR 033). Migration 0010 rewrites
	// the rows written before this line said so; this row is already right.
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		m.version, m.name, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("recording migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %d: %w", m.version, err)
	}
	return nil
}

// SchemaVersion is the highest applied migration version, or 0 on an empty
// database. Exposed so `doctor` can report it.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}
	return int(version.Int64), nil
}

// loadMigrations reads the embedded migrations in version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	seen := map[int]string{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, duplicate := seen[version]; duplicate {
			// Two files claiming one version means the applied order is
			// undefined, which is a corrupt schema waiting to happen.
			return nil, fmt.Errorf("%w: migrations %q and %q share version %d",
				ErrSchema, previous, entry.Name(), version)
		}
		seen[version] = entry.Name()

		body, err := migrationFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	if len(migrations) == 0 {
		return nil, fmt.Errorf("%w: no migrations embedded in the binary", ErrSchema)
	}
	return migrations, nil
}

// parseMigrationName reads "0001_init.sql" as version 1, name "init".
func parseMigrationName(filename string) (version int, name string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	rawVersion, rawName, found := strings.Cut(base, "_")
	if !found {
		return 0, "", fmt.Errorf("%w: migration %q is not named <version>_<name>.sql", ErrSchema, filename)
	}
	version, err = strconv.Atoi(rawVersion)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("%w: migration %q has no positive version prefix", ErrSchema, filename)
	}
	if rawName == "" {
		return 0, "", errors.New("migration name is empty")
	}
	return version, rawName, nil
}
