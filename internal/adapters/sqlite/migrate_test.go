package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// timeColumns is every column in the schema that holds a full instant.
//
// It is written down rather than discovered so that a column added without a
// thought about its layout fails a test instead of quietly storing a shape
// nothing sorts. The `hour` columns of the aggregate tables are deliberately
// absent: they are domain.HourLayout, which has no fraction and is already
// fixed width.
var timeColumns = map[string][]string{
	"projects":          {"created_at"},
	"project_keys":      {"created_at", "revoked_at"},
	"admins":            {"created_at"},
	"sessions":          {"created_at", "expires_at"},
	"api_tokens":        {"created_at", "expires_at", "last_used"},
	"issues":            {"first_seen", "last_seen", "resolved_at"},
	"events":            {"received_at", "occurred_at"},
	"releases":          {"created_at", "date_released", "first_event_at", "last_event_at"},
	"release_commits":   {"timestamp"},
	"deploys":           {"started_at", "finished_at"},
	"schema_migrations": {"applied_at"},
	// Created after migration 0010, so there is nothing for it to rewrite:
	// every row these tables have ever held was written by formatTime, in the
	// fixed-width layout ADR 033 settled on. They are listed here because the
	// inventory is what the next person reads to learn which columns are
	// instants, and a list that only covers the tables that needed migrating
	// would stop being that after the first new table.
	"alert_channels": {"created_at"},
	"alert_rules":    {"created_at"},
	"alert_state":    {"last_fired_at"},
	"notifications":  {"next_attempt_at", "created_at", "sent_at"},
}

// createdAfter0010 are the tables younger than the rewrite migration.
//
// A table created after 0010 has never held a variable-width timestamp, so
// there is nothing for the rewrite to do to it and no legacy fixture to build.
// Listing them is how the rewrite test stays honest about what it covers,
// instead of the alternative — dropping them from the inventory, which would
// also drop them from the check that notices an unconsidered timestamp column.
var createdAfter0010 = map[string]bool{
	"alert_channels": true,
	"alert_rules":    true,
	"alert_state":    true,
	"notifications":  true,
}

// laterTimeColumns is every instant added to the schema *after* migration
// 0010, and it exists so the completeness check below keeps covering the whole
// schema without the rewrite check being asked to prove something impossible.
//
// A column created after 0010 cannot hold a pre-0010 value. There is exactly
// one writer of these — formatTime — and it has had exactly one layout since
// that migration, so there is no legacy shape for 0010 to rewrite and no rows
// for it to find. Listing them here rather than in the map above says that in
// the schema itself: the inventory still names every timestamp in the
// database, and each one says which side of 0010 it was born on.
var laterTimeColumns = map[string][]string{
	// Migration 0017: when an issue last came back.
	"issues": {"regressed_at"},
	// Migration 0016: when an installation setting was last written.
	"settings": {"updated_at"},
	// Migration 0018: when a monitor was declared, when it was last heard
	// from, and when the next run is due.
	"cron_monitors": {"created_at", "last_checkin_at", "next_expected_at"},
	// Migration 0019: when a run announced itself and when it finished.
	"cron_checkins": {"started_at", "finished_at"},
	// Migration 0020: when a monitor was last checked, when it is next due,
	// and when its status last changed.
	"uptime_monitors": {"last_checked_at", "next_check_at", "last_status_change_at", "created_at"},
	// Migration 0020: when one check ran.
	"uptime_results": {"checked_at"},
	// Migration 0023: when a sampled trace's root transaction started. The
	// minute and hour buckets of 0022 are deliberately not here — they are
	// buckets, not instants, and they carry their own truncated layout.
	"traces": {"timestamp"},
	// Migración 0024: cuándo se subió un artefacto de build. Es lo que decide
	// la retención de los que no pertenecen a ninguna release (ADR 018).
	"artifacts": {"created_at"},
	// Migration 0025: when a chunk arrived and started waiting for the
	// assembly that names it.
	"artifact_chunks": {"created_at"},
}

// TestTimeColumnsInventoryIsComplete fails when a migration adds a timestamp
// column that the inventory above does not know about.
//
// Without it, the next column to be added is stored in whatever shape its
// author assumed and nothing notices until an ORDER BY on it is wrong. The
// probe is the schema itself: any TEXT column whose name reads like an instant.
func TestTimeColumnsInventoryIsComplete(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx,
		"SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '%_fts%'")
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating tables: %v", err)
	}

	// Names that end in a suffix a timestamp uses, minus the ones that are a
	// bucket rather than an instant.
	looksLikeAnInstant := func(column string) bool {
		for _, suffix := range []string{"_at", "_seen", "_used", "timestamp"} {
			if strings.HasSuffix(column, suffix) {
				return true
			}
		}
		return false
	}

	for _, table := range tables {
		known := map[string]bool{}
		for _, column := range timeColumns[table] {
			known[column] = true
		}
		for _, column := range laterTimeColumns[table] {
			known[column] = true
		}

		columns, err := db.QueryContext(ctx, "SELECT name, type FROM pragma_table_info(?)", table)
		if err != nil {
			t.Fatalf("reading columns of %s: %v", table, err)
		}
		for columns.Next() {
			var name, columnType string
			if err := columns.Scan(&name, &columnType); err != nil {
				t.Fatalf("scanning column of %s: %v", table, err)
			}
			if columnType == "TEXT" && looksLikeAnInstant(name) && !known[name] {
				t.Errorf("%s.%s looks like a stored instant that the inventory does not know about.\n"+
					"If the column predates migration 0010, add it to that migration and to timeColumns.\n"+
					"If it was added after 0010, add it to laterTimeColumns — and make sure formatTime "+
					"is what writes it.\n"+
					"If it is not an instant at all, rename it.", table, name)
			}
		}
		_ = columns.Close()
		if err := columns.Err(); err != nil {
			t.Fatalf("iterating columns of %s: %v", table, err)
		}
	}
}

// legacyStamps are the values a pre-0010 database holds, and what each one has
// to become. Every shape RFC3339Nano can produce is here, because the width is
// exactly what varied.
var legacyStamps = []struct {
	legacy string
	want   string
}{
	{"2026-08-29T10:00:00Z", "2026-08-29T10:00:00.000000000Z"},
	{"2026-08-29T10:00:00.5Z", "2026-08-29T10:00:00.500000000Z"},
	{"2026-08-29T10:00:00.001Z", "2026-08-29T10:00:00.001000000Z"},
	{"2026-08-29T10:00:00.123456789Z", "2026-08-29T10:00:00.123456789Z"},
	{"2026-08-29T10:00:00.000000001Z", "2026-08-29T10:00:00.000000001Z"},
	{"2026-08-29T23:59:59.999999999Z", "2026-08-29T23:59:59.999999999Z"},
}

func legacy(i int) string { return legacyStamps[i%len(legacyStamps)].legacy }
func expected(i int) string {
	return legacyStamps[i%len(legacyStamps)].want
}

// TestMigrationRewritesExistingTimestamps builds a database in the old shape,
// re-runs migration 0010 over it and checks that every column came out fixed
// width — because a migration that only works on an empty database is a
// migration nobody has run.
func TestMigrationRewritesExistingTimestamps(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	seedLegacyRows(t, db)
	rerunTimestampMigration(t, db)

	for table, columns := range timeColumns {
		if createdAfter0010[table] {
			// Nothing to rewrite and nothing to fixture: this table did not
			// exist when 0010 ran, and every row it has ever held was written
			// in the current layout. It is still in the inventory above,
			// which is the list of what is an instant.
			continue
		}
		for _, column := range columns {
			//nolint:gosec // table and column come from the inventory above, not from input
			rows, err := db.QueryContext(ctx,
				fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL", column, table, column))
			if err != nil {
				t.Fatalf("reading %s.%s: %v", table, column, err)
			}
			seen := 0
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					t.Fatalf("scanning %s.%s: %v", table, column, err)
				}
				seen++
				if len(raw) != len("2026-08-29T10:00:00.000000000Z") {
					t.Errorf("%s.%s holds %q, which is not the fixed-width layout", table, column, raw)
				}
				if _, err := time.Parse(timeLayout, raw); err != nil {
					t.Errorf("%s.%s holds %q, which does not parse as the current layout: %v",
						table, column, raw, err)
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("iterating %s.%s: %v", table, column, err)
			}
			if seen == 0 {
				t.Errorf("%s.%s had no rows to check; the fixture does not cover it", table, column)
			}
		}
	}
}

// TestMigrationPreservesTheInstant checks the values themselves, not only the
// shape: a rewrite that produced well-formed but wrong timestamps would pass
// every width check and silently move every event in the store.
func TestMigrationPreservesTheInstant(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	seedLegacyRows(t, db)
	rerunTimestampMigration(t, db)

	for i := range legacyStamps {
		var got string
		if err := db.QueryRowContext(ctx,
			"SELECT occurred_at FROM events WHERE id = ?", i+1).Scan(&got); err != nil {
			t.Fatalf("reading event %d: %v", i+1, err)
		}
		if got != expected(i) {
			t.Errorf("%q was rewritten as %q, want %q", legacy(i), got, expected(i))
		}

		before, err := time.Parse(legacyTimeLayout, legacy(i))
		if err != nil {
			t.Fatalf("the fixture is not a legacy timestamp: %v", err)
		}
		after, err := parseTime(got)
		if err != nil {
			t.Fatalf("the rewritten value does not parse: %v", err)
		}
		if !before.Equal(after) {
			t.Errorf("%q became %q, a different instant (%s vs %s)", legacy(i), got, before, after)
		}
	}
}

// TestMigrationIsIdempotent runs 0010 twice. It has to be safe: a restore that
// replays migrations, or an operator rerunning one by hand, must not shorten
// or double the fractions it already fixed.
func TestMigrationIsIdempotent(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	seedLegacyRows(t, db)
	rerunTimestampMigration(t, db)

	var once string
	if err := db.QueryRowContext(ctx, "SELECT occurred_at FROM events WHERE id = 1").Scan(&once); err != nil {
		t.Fatalf("reading: %v", err)
	}

	rerunTimestampMigration(t, db)

	var twice string
	if err := db.QueryRowContext(ctx, "SELECT occurred_at FROM events WHERE id = 1").Scan(&twice); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if once != twice {
		t.Errorf("a second run changed %q into %q", once, twice)
	}
}

// TestMigratedDatabaseSortsChronologically is the point of the whole exercise
// against the real engine: after the rewrite, SQLite's own ORDER BY on the
// column agrees with the clock. Before it, the exact second sorted last.
func TestMigratedDatabaseSortsChronologically(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	seedLegacyRows(t, db)

	ordered := func() []string {
		rows, err := db.QueryContext(ctx, "SELECT occurred_at FROM events ORDER BY occurred_at")
		if err != nil {
			t.Fatalf("ordering events: %v", err)
		}
		defer func() { _ = rows.Close() }()

		var got []string
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatalf("scanning: %v", err)
			}
			got = append(got, raw)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating: %v", err)
		}
		return got
	}

	// The bug, stated as a fact about the database that shipped: the exact
	// second is the earliest instant in the fixture and SQLite put it fourth.
	if before := ordered(); before[0] == legacyStamps[0].legacy {
		t.Error("the legacy fixture already sorts correctly; it no longer reproduces the bug")
	}

	rerunTimestampMigration(t, db)

	after := ordered()
	for i := 1; i < len(after); i++ {
		previous, err := parseTime(after[i-1])
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		current, err := parseTime(after[i])
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		if previous.After(current) {
			t.Errorf("SQLite ordered %q before %q, which is backwards", after[i-1], after[i])
		}
	}
}

// TestMigratedDatabaseIsStillReadable goes through the repositories rather than
// raw SQL: a migration that leaves the file consistent but the adapters unable
// to read it has not migrated anything.
func TestMigratedDatabaseIsStillReadable(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	seedLegacyRows(t, db)
	rerunTimestampMigration(t, db)

	issues := NewIssueRepository(db)
	page, err := issues.List(ctx, ports.IssueFilter{ProjectID: 1})
	if err != nil {
		t.Fatalf("listing issues over a migrated database: %v", err)
	}
	if len(page.Issues) == 0 {
		t.Fatal("the migrated database lists no issues")
	}
	// Ordered newest first, and the fixture's newest is the last nanosecond of
	// the day rather than the exact second the old layout floated to the top.
	if want := time.Date(2026, 8, 29, 23, 59, 59, 999999999, time.UTC); !page.Issues[0].LastSeen.Equal(want) {
		t.Errorf("first issue last seen %s, want %s", page.Issues[0].LastSeen, want)
	}

	tokens := NewTokenRepository(db)
	stored, err := tokens.List(ctx)
	if err != nil {
		t.Fatalf("listing tokens over a migrated database: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("the migrated database lists no tokens")
	}

	releases := NewReleaseRepository(db)
	found, err := releases.List(ctx, 1, 10)
	if err != nil {
		t.Fatalf("listing releases over a migrated database: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the migrated database lists no releases")
	}
}

// rerunTimestampMigration forgets that 0010 was applied and applies it again,
// which is how a test built on an already-migrated database can exercise the
// rewrite against rows in the old shape.
func rerunTimestampMigration(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 10"); err != nil {
		t.Fatalf("forgetting migration 10: %v", err)
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatalf("re-running the migration: %v", err)
	}
}

// seedLegacyRows fills every table that has a timestamp column with values in
// the pre-0010 shape, written with raw SQL precisely because formatTime can no
// longer produce them.
func seedLegacyRows(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seeding (%s): %v", query, err)
		}
	}

	exec("INSERT INTO projects (id, name, created_at) VALUES (1, 'venekambio', ?)", legacy(0))
	exec("INSERT INTO project_keys (public_key, project_id, created_at, revoked_at) VALUES ('k1', 1, ?, NULL)", legacy(1))
	exec("INSERT INTO project_keys (public_key, project_id, created_at, revoked_at) VALUES ('k2', 1, ?, ?)", legacy(0), legacy(2))
	exec("INSERT INTO admins (id, username, password_hash, created_at) VALUES (1, 'antonio', 'x', ?)", legacy(0))
	exec("INSERT INTO sessions (token_hash, admin_id, created_at, expires_at) VALUES ('s1', 1, ?, ?)", legacy(0), legacy(3))
	exec(`INSERT INTO api_tokens (token_hash, name, scopes, created_at, expires_at, last_used)
	      VALUES ('t1', 'cli', 'projects:read', ?, ?, ?)`, legacy(0), legacy(4), legacy(1))
	exec(`INSERT INTO api_tokens (token_hash, name, scopes, created_at, expires_at, last_used)
	      VALUES ('t2', 'agent', 'projects:read', ?, NULL, NULL)`, legacy(2))

	for i := range legacyStamps {
		exec(`INSERT INTO issues (id, project_id, fingerprint, grouping_version, title, level,
		                          status, first_seen, last_seen, times, resolved_at)
		      VALUES (?, 1, ?, ?, 'ValueError', 'error', 'unresolved', ?, ?, 1, ?)`,
			i+1, fmt.Sprintf("fp-%d", i), domain.GroupingVersion, legacy(0), legacy(i), legacy(i))
		exec(`INSERT INTO events (id, issue_id, project_id, received_at, occurred_at, level, payload)
		      VALUES (?, ?, 1, ?, ?, 'error', X'00')`, i+1, i+1, legacy(i), legacy(i))
	}

	exec(`INSERT INTO releases (id, project_id, version, created_at, date_released, first_event_at, last_event_at)
	      VALUES (1, 1, '1.0.0', ?, ?, ?, ?)`, legacy(0), legacy(1), legacy(2), legacy(3))
	exec(`INSERT INTO releases (id, project_id, version, created_at, date_released, first_event_at, last_event_at)
	      VALUES (2, 1, '1.0.1', ?, NULL, NULL, NULL)`, legacy(4))
	exec(`INSERT INTO release_commits (release_id, sha, ordinal, timestamp) VALUES (1, 'abc', 0, ?)`, legacy(1))
	exec(`INSERT INTO release_commits (release_id, sha, ordinal, timestamp) VALUES (1, 'def', 1, NULL)`)
	exec(`INSERT INTO deploys (release_id, environment, started_at, finished_at) VALUES (1, 'production', ?, ?)`,
		legacy(0), legacy(5))

	// schema_migrations was written by the migrator with the current layout,
	// so it has to be pushed back into the old one for the rewrite to have
	// anything to do.
	exec("UPDATE schema_migrations SET applied_at = ?", legacy(0))

	var events int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		t.Fatalf("counting the fixture: %v", err)
	}
	if events != len(legacyStamps) {
		t.Fatalf("seeded %d events, want %d", events, len(legacyStamps))
	}
}
