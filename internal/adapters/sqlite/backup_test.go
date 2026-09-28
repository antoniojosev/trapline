package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupProducesAnOpenableCopy(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	repo := NewProjectRepository(db)

	created, key := createProject(t, repo, "venekambio")

	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, destination); err != nil {
		t.Fatalf("backing up: %v", err)
	}

	info, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("the backup file is missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the backup file is empty")
	}

	// The point of a backup is that it restores. Opening the copy as a normal
	// database and reading the data back is the only assertion that means
	// anything here.
	restored, err := Open(ctx, destination)
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer func() { _ = restored.Close() }()

	project, err := NewProjectRepository(restored).FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("reading the project from the backup: %v", err)
	}
	if project.Name != "venekambio" {
		t.Errorf("Name = %q, want %q", project.Name, "venekambio")
	}

	found, err := NewProjectRepository(restored).FindActiveKey(ctx, key.PublicKey)
	if err != nil {
		t.Fatalf("reading the key from the backup: %v", err)
	}
	if found.ProjectID != created.ID {
		t.Errorf("the restored key points at project %d, want %d", found.ProjectID, created.ID)
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, destination); err != nil {
		t.Fatalf("first backup: %v", err)
	}

	// Silently replacing an existing backup is how a good copy gets destroyed
	// by a cron job at the worst possible moment.
	if err := db.Backup(ctx, destination); err == nil {
		t.Error("the second backup overwrote the first")
	}
}

func TestBackupRejectsAnEmptyPath(t *testing.T) {
	if err := openTemp(t).Backup(context.Background(), ""); err == nil {
		t.Error("an empty destination was accepted")
	}
}

func TestBackupPathWithAQuote(t *testing.T) {
	// The destination cannot be a bound parameter, so the quoting has to be
	// right. A directory with an apostrophe is a perfectly ordinary path.
	ctx := context.Background()
	db := openTemp(t)

	dir := filepath.Join(t.TempDir(), "antonio's backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating directory: %v", err)
	}

	destination := filepath.Join(dir, "backup.db")
	if err := db.Backup(ctx, destination); err != nil {
		t.Fatalf("backing up to a quoted path: %v", err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Errorf("the backup is missing: %v", err)
	}
}

func TestEscapeSQLString(t *testing.T) {
	cases := map[string]string{
		"/plain/path": "/plain/path",
		"antonio's":   "antonio''s",
		"''":          "''''",
		"":            "",
		"a'b'c":       "a''b''c",
	}
	for input, want := range cases {
		if got := escapeSQLString(input); got != want {
			t.Errorf("escapeSQLString(%q) = %q, want %q", input, got, want)
		}
	}
}
