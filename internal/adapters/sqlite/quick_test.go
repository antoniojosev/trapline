package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestQuickCheckOnAHealthyDatabase(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	report, err := QuickCheck(ctx, db.Path())
	if err != nil {
		t.Fatalf("QuickCheck: %v", err)
	}
	if report.JournalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", report.JournalMode)
	}
	if report.AutoVacuum != 2 {
		t.Errorf("auto_vacuum = %d, want 2", report.AutoVacuum)
	}
	if report.SchemaVersion == 0 {
		t.Error("schema version = 0, want the migrations to be reported")
	}
}

// The healthcheck also has to work on an installation that is stopped — an
// operator running `doctor --quick` by hand before starting anything is the
// normal case, and a check that only passes while the server holds the WAL
// files would say "broken" about a database that is fine.
func TestQuickCheckOnAClosedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trapline.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing database: %v", err)
	}

	if _, err := QuickCheck(ctx, path); err != nil {
		t.Fatalf("QuickCheck on a closed database: %v", err)
	}
}

// The property this test protects is the one that makes the healthcheck worth
// trusting: pointed at a path that is not there, it fails and leaves the
// filesystem alone. The alternative — creating an empty database and calling
// the container healthy — would hide a broken volume mount indefinitely.
func TestQuickCheckCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.db")

	if _, err := QuickCheck(context.Background(), path); !errors.Is(err, ErrSchema) {
		t.Errorf("error = %v, want ErrSchema", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the check created %d file(s): %v", len(entries), entries)
	}
}

func TestQuickCheckRejectsADirectory(t *testing.T) {
	if _, err := QuickCheck(context.Background(), t.TempDir()); !errors.Is(err, ErrSchema) {
		t.Errorf("error = %v, want ErrSchema", err)
	}
}

func TestQuickCheckRejectsAForeignDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "foreign.db")

	foreign, err := openRaw(path)
	if err != nil {
		t.Fatalf("creating the database: %v", err)
	}
	if _, err := foreign.ExecContext(ctx, "CREATE TABLE anything (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating a table: %v", err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	// Not an trapline database at all: no schema_migrations, no WAL, no
	// incremental vacuum. Every one of those is a reason to be unhealthy.
	if _, err := QuickCheck(ctx, path); !errors.Is(err, ErrSchema) {
		t.Errorf("error = %v, want ErrSchema", err)
	}
}

// An trapline database whose migrations never ran is the shape a bug would
// produce, so the report names it rather than passing on the pragmas alone.
func TestQuickCheckRejectsAnUnmigratedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "empty.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations"); err != nil {
		t.Fatalf("emptying schema_migrations: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	_, err = QuickCheck(ctx, path)
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("error = %v, want ErrSchema", err)
	}
}
