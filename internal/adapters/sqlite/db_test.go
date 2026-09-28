package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// openTemp opens a database in a real file, not :memory:. WAL mode,
// auto_vacuum and the busy timeout only behave like production against a
// real file, and testing storage against a fake store proves nothing.
func openTemp(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trapline.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAppliesPragmas(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	cases := map[string]struct {
		pragma string
		want   string
	}{
		"WAL journal":        {"PRAGMA journal_mode", "wal"},
		"incremental vacuum": {"PRAGMA auto_vacuum", "2"},
		"foreign keys on":    {"PRAGMA foreign_keys", "1"},
		"synchronous normal": {"PRAGMA synchronous", "1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			if err := db.QueryRowContext(ctx, tc.pragma).Scan(&got); err != nil {
				t.Fatalf("reading %s: %v", tc.pragma, err)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.pragma, got, tc.want)
			}
		})
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trapline.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	version, err := first.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("reading schema version: %v", err)
	}
	if version == 0 {
		t.Fatal("schema version is 0 after opening a fresh database")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	// Reopening must not re-run migrations or complain.
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = second.Close() }()

	reopened, err := second.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("reading schema version: %v", err)
	}
	if reopened != version {
		t.Errorf("schema version changed from %d to %d on reopen", version, reopened)
	}
}

func TestOpenRejectsDatabaseWithoutIncrementalVacuum(t *testing.T) {
	// A database created elsewhere without auto_vacuum cannot reclaim space
	// after a retention sweep without a full VACUUM. Failing at startup beats
	// discovering that when the disk is full.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := openRaw(path)
	if err != nil {
		t.Fatalf("creating legacy database: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, "PRAGMA auto_vacuum = NONE"); err != nil {
		t.Fatalf("setting auto_vacuum: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, "CREATE TABLE anything (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("closing legacy database: %v", err)
	}

	if _, err := Open(ctx, path); !errors.Is(err, ErrSchema) {
		t.Errorf("error = %v, want ErrSchema", err)
	}
}

func TestOpenFailsOnUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := Open(context.Background(), filepath.Join(dir, "trapline.db")); err == nil {
		t.Error("expected an error opening a database in a read-only directory")
	}
}
