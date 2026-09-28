package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

func TestAProjectIsFoundByItsSlug(t *testing.T) {
	repo, ctx := newRepo(t)
	created, _ := createProject(t, repo, "Mi App")

	if created.Slug != "mi-app" {
		t.Fatalf("slug = %q, want mi-app", created.Slug)
	}
	found, err := repo.FindBySlug(ctx, "mi-app")
	if err != nil {
		t.Fatalf("FindBySlug: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("found project %d, want %d", found.ID, created.ID)
	}
}

func TestAnUnknownSlugIsNotFound(t *testing.T) {
	repo, ctx := newRepo(t)
	_, err := repo.FindBySlug(ctx, "nadie")
	if !errors.Is(err, domain.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

// TestTwoProjectsWithTheSameDerivedSlugBothGetOne is the case that decides
// whether the column can be unique at all. Two names that derive the same slug
// is not exotic — "Mi App" and "mi-app" are the same string to this rule — and
// refusing the second project would make an internal naming decision visible
// as a failure to create something.
func TestTwoProjectsWithTheSameDerivedSlugBothGetOne(t *testing.T) {
	repo, _ := newRepo(t)

	first, _ := createProject(t, repo, "Mi App")
	second, _ := createProject(t, repo, "mi app")
	third, _ := createProject(t, repo, "MI-APP")

	slugs := []string{first.Slug, second.Slug, third.Slug}
	if slugs[0] != "mi-app" {
		t.Errorf("the first project should keep the plain slug, got %q", slugs[0])
	}
	seen := map[string]bool{}
	for _, slug := range slugs {
		if slug == "" {
			t.Fatal("a project was stored without a slug")
		}
		if seen[slug] {
			t.Fatalf("two projects share the slug %q", slug)
		}
		seen[slug] = true
	}
}

// TestASlugIsNeverOnlyDigits pins the rule that keeps the two ways of
// addressing a project from colliding: a numeric path segment is read as an id
// first (ADR 013), so a slug made of digits could never be resolved as one and
// would quietly address a different project.
func TestASlugIsNeverOnlyDigits(t *testing.T) {
	repo, _ := newRepo(t)
	created, _ := createProject(t, repo, "2026")
	if strings.Trim(created.Slug, "0123456789") == "" {
		t.Fatalf("slug = %q, which is only digits", created.Slug)
	}
}

// TestTheSlugBackfillCannotViolateItsOwnIndex runs the migration's backfill
// over the names most likely to break it.
//
// The statements are the ones that ship: the test reads the migration file
// rather than a copy, so a change to the SQL is a change to what is tested. It
// is worth this much care because a backfill that produces two equal slugs
// does not fail in a test — it fails while somebody is upgrading, with the
// unique index rejecting the migration and the database half moved.
func TestTheSlugBackfillCannotViolateItsOwnIndex(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// Back to the state the migration starts from: the column exists and holds
	// nothing, and the index it ends with is not there yet.
	if _, err := db.ExecContext(ctx, "DROP INDEX idx_projects_slug"); err != nil {
		t.Fatalf("dropping the index: %v", err)
	}

	// Names chosen to collide with each other and with the fallback: two that
	// derive the same slug, one that derives nothing, one that derives only
	// digits, one in a script the SQL derivation does not know, and one whose
	// derivation is exactly the fallback another row will want.
	names := []string{
		"Mi App", "mi app", "···", "2026", "日本語", "Project 3", "Diseño",
	}
	for _, name := range names {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO projects (name, slug, created_at) VALUES (?, '', ?)",
			name, formatTime(testNow)); err != nil {
			t.Fatalf("inserting %q: %v", name, err)
		}
	}

	for _, statement := range backfillStatements(t) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("running %q: %v", firstLine(statement), err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT name, slug FROM projects ORDER BY id")
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]string{}
	for rows.Next() {
		var name, slug string
		if err := rows.Scan(&name, &slug); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		if slug == "" {
			t.Errorf("%q was left without a slug", name)
		}
		if strings.Trim(slug, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Errorf("%q derived %q, which is not URL-safe", name, slug)
		}
		if other, clash := seen[slug]; clash {
			t.Fatalf("%q and %q both derived %q: the unique index would reject the migration",
				other, name, slug)
		}
		seen[slug] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	// The names the derivation *can* handle keep a readable slug rather than
	// falling back; a migration that renamed every project to project-<id>
	// would satisfy the index and help nobody.
	if seen["mi-app"] != "Mi App" {
		t.Errorf("mi-app belongs to %q, want the first of the two colliding names", seen["mi-app"])
	}
	if seen["diseno"] != "Diseño" {
		t.Errorf("the accented name derived %v, want diseno", seen)
	}

	// And the index the migration ends with must actually take.
	if _, err := db.ExecContext(ctx,
		"CREATE UNIQUE INDEX idx_projects_slug ON projects (slug)"); err != nil {
		t.Fatalf("the backfill left a state the unique index rejects: %v", err)
	}
}

// backfillStatements returns the migration's statements, minus the two that
// only make sense once: adding the column and creating the index.
func backfillStatements(t *testing.T) []string {
	t.Helper()

	raw, err := migrationFS.ReadFile("migrations/0012_project_slug.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	var statements []string
	for _, statement := range strings.Split(string(raw), ";") {
		trimmed := strings.TrimSpace(statement)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "ALTER TABLE") || strings.Contains(upper, "CREATE UNIQUE INDEX") {
			continue
		}
		statements = append(statements, trimmed)
	}
	if len(statements) == 0 {
		t.Fatal("the migration has no backfill left, which cannot be right")
	}
	return statements
}

func firstLine(statement string) string {
	for _, line := range strings.Split(statement, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return trimmed
		}
	}
	return statement
}
