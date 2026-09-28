package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/compression"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.IssueRepository = (*IssueRepository)(nil)

// DefaultEventListLimit bounds a detail view's event list.
const DefaultEventListLimit = 50

// IssueRepository is the SQLite implementation of ports.IssueRepository.
type IssueRepository struct {
	db *DB
	// alerts is who turns "this issue is new" into rows in the outbox, in
	// this repository's own transaction. Optional: an assembly without
	// alerting records events exactly as it did before alerting existed.
	alerts *AlertRepository
	// origin is the installation's public address, needed to put a link to
	// the issue in the notification. A notification without one is an
	// interruption; with one it is the first click of the investigation.
	origin domain.Origin
}

// NewIssueRepository wires the repository to an open database.
func NewIssueRepository(db *DB) *IssueRepository {
	return &IssueRepository{db: db}
}

// WithAlerts makes recorded events offer themselves to the alert rules.
//
// The collaboration is between two adapters rather than through a use case on
// purpose, and it is the only one in the repository. What it buys is the
// guarantee the whole design rests on: the notification row and the issue it
// is about are written by one transaction and committed together, so there is
// no window in which an issue exists and its notification does not (ADR 015).
// Routing that through a use case would mean two transactions and exactly that
// window — the one a restart lands in.
func (r *IssueRepository) WithAlerts(alerts *AlertRepository, origin domain.Origin) *IssueRepository {
	r.alerts, r.origin = alerts, origin
	return r
}

// RecordEvent files an event into its issue.
//
// Read, decide, write — all inside one transaction, and the decision is the
// domain's: the row is read, handed to domain.Issue.Observe, and written back.
// An upsert would be one statement shorter and would put that rule into SQL,
// where it could not be tested without a database and would drift from the
// domain that owns it.
//
// The whole of what an event does now happens here, releases included: the
// release row, the counters, the resolution columns, the event, its tags and
// the hourly buckets. That is one transaction and one commit per event rather
// than three, and it is the only arrangement in which the reopen rule is ever
// true of the stored row — see ADR 032. The transaction is not optional: an
// event that reached storage without updating its issue's counters would make
// the numbers lie, and the numbers are the product.
func (r *IssueRepository) RecordEvent(
	ctx context.Context, input *ports.RecordEventInput,
) (ports.RecordEventResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.RecordEventResult{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The release is recorded before the issue is judged, because the rank
	// that orders identifiers which are not versions — a git sha, a build
	// number — is the release row's own id. An event from a build nobody has
	// seen has to become a known release first, or the comparison that
	// decides whether it reopens anything would be run against a version the
	// table does not contain yet.
	if err := noteEventRelease(ctx, tx, input); err != nil {
		return ports.RecordEventResult{}, err
	}

	existing, found, err := findIssueForUpdate(ctx, tx, input.ProjectID, input.Fingerprint)
	if err != nil {
		return ports.RecordEventResult{}, err
	}

	var result ports.RecordEventResult
	switch {
	case found:
		observation := input.Observation
		// The order is read only when the issue is pinned to a release, which
		// is the one case whose verdict depends on it. Two rows, and only on
		// the rare path: the common event — an open issue seeing another
		// occurrence — pays nothing for this.
		if existing.Status == domain.StatusResolved && existing.ResolveNextRelease {
			if observation.ReleaseOrder, err = releaseRanks(ctx, tx, input.ProjectID,
				observation.Release, existing.ResolvedInRelease); err != nil {
				return ports.RecordEventResult{}, err
			}
		}
		updated, regressed := existing.Observe(observation)
		// Whether this event moved the issue's lifecycle at all. Almost never:
		// the overwhelmingly common event is another occurrence of something
		// already open, and for it the narrow UPDATE below leaves two indexed
		// columns out of the statement entirely — which is what keeps SQLite
		// from rewriting both release indexes once per ingested event.
		moved := updated.Status != existing.Status ||
			updated.FirstRelease != existing.FirstRelease ||
			updated.ResolvedAt != existing.ResolvedAt ||
			updated.ResolvedInRelease != existing.ResolvedInRelease ||
			updated.ResolveNextRelease != existing.ResolveNextRelease ||
			updated.Regressions != existing.Regressions ||
			updated.RegressedInRelease != existing.RegressedInRelease ||
			updated.SeenInResolvedReleaseCount != existing.SeenInResolvedReleaseCount
		// When it came back, and not only that it did. The counter and the
		// release were enough for an issue page, which only ever asks "has
		// this happened"; they are not enough for anything asked about a
		// period, and the weekly digest is exactly that question (migration
		// 0017). The occurrence time and not the ingest time, for the same
		// reason last_seen uses it: a late event describes when it happened.
		var regressedAt *time.Time
		if regressed {
			at := observation.At.UTC()
			regressedAt = &at
		}
		if err := updateIssue(ctx, tx, updated, input.Message, moved, regressedAt); err != nil {
			return ports.RecordEventResult{}, err
		}
		result = ports.RecordEventResult{Issue: updated, Regressed: regressed}

	default:
		created, err := domain.NewIssue(input.ProjectID, input.Fingerprint, input.Observation)
		if err != nil {
			return ports.RecordEventResult{}, err
		}
		id, err := insertIssue(ctx, tx, created, input.Message)
		if err != nil {
			return ports.RecordEventResult{}, err
		}
		created.ID = id
		result = ports.RecordEventResult{Issue: created, New: true}
	}

	if err := insertEvent(ctx, tx, &result.Issue, input); err != nil {
		return ports.RecordEventResult{}, err
	}
	if err := upsertTags(ctx, tx, result.Issue.ID, input.Tags); err != nil {
		return ports.RecordEventResult{}, err
	}
	if err := upsertAggregates(ctx, tx, result.Issue.ID, input); err != nil {
		return ports.RecordEventResult{}, err
	}

	// Last, and inside the same transaction: what this event did to its issue
	// is now decided, and if it is worth telling somebody about, the row that
	// will tell them is committed together with the fact itself.
	r.offerToAlerts(ctx, tx, &result, input)

	if err := tx.Commit(); err != nil {
		return ports.RecordEventResult{}, fmt.Errorf("committing event: %w", err)
	}
	return result, nil
}

// offerToAlerts hands a new issue or a regression to the alert rules.
//
// It cannot fail the event. An installation whose Slack channel was
// misconfigured must still record its errors — losing an error report to
// protect a notification is the wrong way round for a product whose job is not
// to lose error reports — so the work happens inside a savepoint and a failure
// rolls back the notifications alone, leaving the event to commit. The failure
// is logged, and the notification simply never existed, which is the same
// outcome as having no rule.
func (r *IssueRepository) offerToAlerts(
	ctx context.Context, tx *sql.Tx, result *ports.RecordEventResult, input *ports.RecordEventInput,
) {
	if r.alerts == nil {
		return
	}
	var kind domain.TriggerKind
	switch {
	case result.Regressed:
		// A regression outranks newness, and an issue cannot be both.
		kind = domain.TriggerRegression
	case result.New:
		kind = domain.TriggerNewIssue
	default:
		return
	}

	event := domain.AlertEvent{
		Kind:      kind,
		ProjectID: input.ProjectID,
		IssueID:   result.Issue.ID,
		Title:     result.Issue.Title,
		Culprit:   result.Issue.Culprit,
		Level:     result.Issue.Level,
		Release:   result.Issue.LastRelease,
		// The environment comes from the event rather than the issue: an issue
		// has no environment, it has occurrences in several, and the one that
		// matters in a notification is the one that just happened.
		Environment: input.Environment,
		Count:       result.Issue.Times,
		// The server's clock, not the event's. The payload's timestamp decides
		// when something happened; this decides when a notification is due,
		// and an SDK with a wrong clock must not be able to schedule a message
		// for next Tuesday.
		At: input.ReceivedAt,
	}

	if _, err := tx.ExecContext(ctx, "SAVEPOINT alerts"); err != nil {
		slog.Error("could not open a savepoint for alerting", "error", err)
		return
	}
	if _, err := r.alerts.enqueueTx(ctx, tx, &event, r.origin.IssueURL(input.ProjectID, result.Issue.ID)); err != nil {
		slog.Error("could not queue a notification; the event is stored regardless",
			"project_id", input.ProjectID, "issue_id", result.Issue.ID, "error", err)
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO alerts"); rollbackErr != nil {
			slog.Error("could not roll back the alerting savepoint", "error", rollbackErr)
		}
	}
	if _, err := tx.ExecContext(ctx, "RELEASE alerts"); err != nil {
		slog.Error("could not release the alerting savepoint", "error", err)
	}
}

// noteEventRelease records the release an event named, creating it when this
// is the first anyone has heard of it.
//
// A version this product could never address again — one with a slash, one
// longer than the column allows — is not worth failing an event over. The
// event is still stored, still counted and still shows the string it carried;
// what it does not get is a row in `releases`. Refusing the event instead
// would lose an error report to protect a listing, which is the wrong way
// round for a product whose whole job is not to lose error reports.
func noteEventRelease(ctx context.Context, tx *sql.Tx, input *ports.RecordEventInput) error {
	if input.Observation.Release == "" {
		return nil
	}
	version, err := domain.CleanVersion(input.Observation.Release)
	if err != nil {
		slog.Warn("an event named a release that cannot be recorded",
			"project_id", input.ProjectID, "release", input.Observation.Release, "error", err)
		return nil
	}
	return ensureRelease(ctx, tx, input.ProjectID, version, input.Observation.At)
}

// issueForUpdateColumns is everything an event can change about an issue: the
// identity and counters it always touches, and the resolution state it only
// touches when the event lands on something somebody had already declared
// fixed.
const issueForUpdateColumns = `id, project_id, fingerprint, grouping_version, title, culprit,
	level, status, first_seen, last_seen, times, last_release,
	first_release, resolved_at, resolved_in_release, resolve_next_release,
	regressions, regressed_in_release, seen_in_resolved_release_count`

func findIssueForUpdate(ctx context.Context, tx *sql.Tx, projectID int64, fingerprint string) (domain.Issue, bool, error) {
	row := tx.QueryRowContext(ctx,
		"SELECT "+issueForUpdateColumns+" FROM issues WHERE project_id = ? AND fingerprint = ?",
		projectID, fingerprint)

	issue, err := scanIssueForUpdate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Issue{}, false, nil
	}
	if err != nil {
		return domain.Issue{}, false, fmt.Errorf("reading issue: %w", err)
	}
	return issue, true, nil
}

// scanIssueForUpdate reads the whole of an issue, resolution included.
//
// A scanner of its own rather than a wider scanIssue: every listing reads the
// narrow row, and making them all carry seven more columns would charge every
// page of the main screen for what one write needs.
func scanIssueForUpdate(row scanner) (domain.Issue, error) {
	var (
		issue              domain.Issue
		level              string
		status             string
		firstSeen          string
		lastSeen           string
		resolvedAt         sql.NullString
		resolveNextRelease int
	)
	if err := row.Scan(&issue.ID, &issue.ProjectID, &issue.Fingerprint, &issue.GroupingVersion,
		&issue.Title, &issue.Culprit, &level, &status, &firstSeen, &lastSeen,
		&issue.Times, &issue.LastRelease,
		&issue.FirstRelease, &resolvedAt, &issue.ResolvedInRelease, &resolveNextRelease,
		&issue.Regressions, &issue.RegressedInRelease,
		&issue.SeenInResolvedReleaseCount); err != nil {
		return domain.Issue{}, err //nolint:wrapcheck // the caller distinguishes sql.ErrNoRows.
	}

	issue.Level = domain.Level(level)
	issue.Status = domain.IssueStatus(status)
	issue.ResolveNextRelease = resolveNextRelease != 0

	var err error
	if issue.FirstSeen, err = parseTime(firstSeen); err != nil {
		return domain.Issue{}, err
	}
	if issue.LastSeen, err = parseTime(lastSeen); err != nil {
		return domain.Issue{}, err
	}
	if issue.ResolvedAt, err = parseNullableTime(resolvedAt); err != nil {
		return domain.Issue{}, err
	}
	return issue, nil
}

// insertIssue writes a new issue, sample message included.
//
// The sample message is passed alongside the domain issue rather than living
// on it: it exists to feed the search index (ADR 011) and no domain rule reads
// it, so putting it on the entity would be storage borrowing the domain to
// carry its own baggage.
func insertIssue(ctx context.Context, tx *sql.Tx, issue domain.Issue, sampleMessage string) (int64, error) {
	// first_release is written here rather than filled in afterwards: the
	// event that creates an issue is by definition the one that names the
	// release it was born in, and a second statement to say so could only
	// ever say the same thing later.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO issues (project_id, fingerprint, grouping_version, title, culprit,
		                    level, status, first_seen, last_seen, times, last_release,
		                    first_release, sample_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issue.ProjectID, issue.Fingerprint, issue.GroupingVersion, issue.Title, issue.Culprit,
		string(issue.Level), string(issue.Status), formatTime(issue.FirstSeen),
		formatTime(issue.LastSeen), issue.Times, issue.LastRelease,
		issue.FirstRelease, sampleMessage)
	if err != nil {
		return 0, fmt.Errorf("inserting issue: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading assigned issue id: %w", err)
	}
	return id, nil
}

// The two shapes of the write-back. They differ only in whether the
// resolution columns are named, and that difference is not cosmetic: SQLite
// decides which indexes a statement has to maintain from the columns in its
// SET clause, so naming `first_release` and `regressed_in_release` costs a
// rewrite of both release indexes — on every ingested event, to store the
// values they already held.
const (
	updateIssueCounters = `
		UPDATE issues
		SET title = ?, culprit = ?, level = ?, status = ?,
		    first_seen = ?, last_seen = ?, times = ?, last_release = ?,
		    sample_message = ?
		WHERE id = ?`

	updateIssueWithResolution = `
		UPDATE issues
		SET title = ?, culprit = ?, level = ?, status = ?,
		    first_seen = ?, last_seen = ?, times = ?, last_release = ?,
		    sample_message = ?,
		    first_release = ?, resolved_at = ?, resolved_in_release = ?,
		    resolve_next_release = ?, regressions = ?, regressed_in_release = ?,
		    seen_in_resolved_release_count = ?,
		    -- COALESCE, so this statement can be used by both events that move
		    -- the resolution: the one that reopens an issue passes the moment
		    -- it happened, and the one that merely counted an occurrence from
		    -- the already-resolved build passes NULL and leaves the previous
		    -- regression's timestamp alone (migration 0017).
		    regressed_at = COALESCE(?, regressed_at)
		WHERE id = ?`
)

// updateIssue writes back an issue that has just observed an event.
//
// When the event moved the issue's lifecycle — it reopened it, or it was one
// more from the build already known to be broken — the resolution columns go
// back in the same statement as the counters. That is the point of ADR 032:
// the verdict and the count it was derived from are one decision, and writing
// them separately left a window in which the stored row said "unresolved"
// about an event that was about to be judged expected.
//
// sample_message is set on every event, so an issue's indexed text follows the
// most recent occurrence rather than the first one — which is what makes
// searching for a message that only started appearing yesterday find anything.
// The trigger behind the search index is narrowed to fire only when one of the
// indexed columns actually changes value, so the common case of the same error
// arriving again re-indexes nothing (migration 0007).
func updateIssue(
	ctx context.Context, tx *sql.Tx, issue domain.Issue, sampleMessage string,
	resolutionMoved bool, regressedAt *time.Time,
) error {
	args := []any{
		issue.Title, issue.Culprit, string(issue.Level), string(issue.Status),
		formatTime(issue.FirstSeen), formatTime(issue.LastSeen), issue.Times,
		issue.LastRelease, sampleMessage,
	}
	statement := updateIssueCounters
	if resolutionMoved {
		nextRelease := 0
		if issue.ResolveNextRelease {
			nextRelease = 1
		}
		statement = updateIssueWithResolution
		args = append(args, issue.FirstRelease, nullableTime(issue.ResolvedAt),
			issue.ResolvedInRelease, nextRelease, issue.Regressions,
			issue.RegressedInRelease, issue.SeenInResolvedReleaseCount,
			nullableTime(regressedAt))
	}
	args = append(args, issue.ID)

	if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
		return fmt.Errorf("updating issue: %w", err)
	}
	return nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, issue *domain.Issue, input *ports.RecordEventInput) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events (issue_id, project_id, event_id, received_at, occurred_at,
		                    level, release, environment, message, payload, payload_codec)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issue.ID, input.ProjectID, input.EventID,
		formatTime(input.ReceivedAt), formatTime(input.Observation.At),
		string(domain.ValidLevel(input.Observation.Level)),
		input.Observation.Release, input.Environment, input.Message,
		compression.Compress(input.Payload), compression.Codec)
	if err != nil {
		return fmt.Errorf("inserting event: %w", err)
	}
	return nil
}

// upsertTags accumulates an issue's tag counts.
//
// Aggregated here rather than derived from the event table, because deriving
// them means a GROUP BY over every event of an issue — the exact scan the
// design exists to avoid (ADR 001).
func upsertTags(ctx context.Context, tx *sql.Tx, issueID int64, tags map[string]string) error {
	for key, value := range tags {
		if key == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO issue_tags (issue_id, key, value, count) VALUES (?, ?, ?, 1)
			ON CONFLICT (issue_id, key, value) DO UPDATE SET count = count + 1`,
			issueID, key, value)
		if err != nil {
			return fmt.Errorf("recording tag %q: %w", key, err)
		}
	}
	return nil
}

// upsertAggregates records the event in the hourly rollups (ADR 010).
//
// In this transaction, not a queue and not a background pass. A count that can
// disagree with the events it counts is a dashboard nobody trusts twice, and
// the alternative — deriving the numbers with a GROUP BY over events when
// somebody opens the page — breaks the rule the whole storage design rests on
// (ADR 001) and stops working entirely the day retention deletes the events.
// The buckets outlive the payloads on purpose.
//
// The hour comes from when the event happened, not from when it arrived. A
// mobile client that was offline for an hour must land in the hour it crashed,
// or the chart shows a spike at the moment the network came back.
func upsertAggregates(ctx context.Context, tx *sql.Tx, issueID int64, input *ports.RecordEventInput) error {
	hour := domain.HourBucket(input.Observation.At)
	level := string(domain.ValidLevel(input.Observation.Level))

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO issue_hourly (issue_id, project_id, hour, count) VALUES (?, ?, ?, 1)
		ON CONFLICT (issue_id, hour) DO UPDATE SET count = count + 1`,
		issueID, input.ProjectID, hour); err != nil {
		return fmt.Errorf("recording the issue's hour: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO project_hourly (project_id, hour, level, count) VALUES (?, ?, ?, 1)
		ON CONFLICT (project_id, hour, level) DO UPDATE SET count = count + 1`,
		input.ProjectID, hour, level); err != nil {
		return fmt.Errorf("recording the project's hour: %w", err)
	}

	// An event with no release and no environment writes no dimension rows at
	// all, rather than a row under "". A breakdown listing an empty string as
	// its biggest bucket tells nobody anything, and the absence is already the
	// answer: those events did not say.
	dimensions := [...]struct {
		dimension domain.Dimension
		value     string
	}{
		{domain.DimensionRelease, input.Observation.Release},
		{domain.DimensionEnvironment, input.Environment},
	}
	for _, dimension := range dimensions {
		if dimension.value == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_hourly_dims (project_id, hour, dim, value, count)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT (project_id, dim, hour, value) DO UPDATE SET count = count + 1`,
			input.ProjectID, hour, string(dimension.dimension), dimension.value); err != nil {
			return fmt.Errorf("recording the %s dimension: %w", dimension.dimension, err)
		}
	}
	return nil
}

// DefaultIssuePageSize is how many issues a listing returns when the caller
// does not say.
const DefaultIssuePageSize = 50

// MaxIssuePageSize bounds what a caller may ask for. A page nobody can read is
// a query somebody's dashboard runs in a loop.
const MaxIssuePageSize = 200

// List returns one page of issues for the main screen.
func (r *IssueRepository) List(ctx context.Context, filter ports.IssueFilter) (ports.IssuePage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultIssuePageSize
	}
	if limit > MaxIssuePageSize {
		limit = MaxIssuePageSize
	}

	// The clauses are fixed strings chosen here; every value the caller
	// supplied stays a bound parameter.
	query := `
		SELECT id, project_id, fingerprint, grouping_version, title, culprit,
		       level, status, first_seen, last_seen, times, last_release
		FROM issues
		WHERE project_id = ?`
	args := []any{filter.ProjectID}

	if filter.Status != "" {
		query += " AND status = ?"
		args = append(args, string(filter.Status))
	}
	if trimmed := strings.TrimSpace(filter.Query); trimmed != "" {
		match, err := ftsMatch(trimmed)
		if err != nil {
			// Everything the caller typed is too short for the index to look
			// up. Returned as an error rather than as an empty page, because
			// an empty page is indistinguishable from "nothing matched" and
			// would send somebody looking for a bug in their own data.
			return ports.IssuePage{}, err
		}
		// The index is on title, culprit and the last event's message, and on
		// nothing else — never on payloads (ADR 011). A subquery rather than a
		// join, because issues_fts is an external-content table whose rowid is
		// issues.id, so the match is a lookup that hands back primary keys.
		query += " AND issues.id IN (SELECT rowid FROM issues_fts WHERE issues_fts MATCH ?)"
		args = append(args, match)
	}

	// Tag filters are ANDed as separate EXISTS clauses rather than one join.
	// A join would multiply rows per matching tag and need a DISTINCT; EXISTS
	// stops at the first match and uses the tag index directly.
	for key, value := range filter.Tags {
		query += ` AND EXISTS (
			SELECT 1 FROM issue_tags t
			WHERE t.issue_id = issues.id AND t.key = ? AND t.value = ?
		)`
		args = append(args, key, value)
	}

	if filter.Cursor != "" {
		lastSeen, id, err := decodeCursor(filter.Cursor)
		if err != nil {
			return ports.IssuePage{}, err
		}
		// Keyset, matching the ORDER BY exactly. The id breaks ties so two
		// issues last seen in the same instant cannot hide each other between
		// pages.
		query += " AND (last_seen < ? OR (last_seen = ? AND id < ?))"
		args = append(args, lastSeen, lastSeen, id)
	}

	// One row past the page, to learn whether there is a next one without a
	// second COUNT over the same predicate.
	query += " ORDER BY last_seen DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ports.IssuePage{}, fmt.Errorf("listing issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var issues []domain.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return ports.IssuePage{}, fmt.Errorf("scanning issue: %w", err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return ports.IssuePage{}, fmt.Errorf("iterating issues: %w", err)
	}

	page := ports.IssuePage{Issues: issues}
	if len(issues) > limit {
		page.Issues = issues[:limit]
		last := page.Issues[limit-1]
		page.NextCursor = encodeCursor(last.LastSeen, last.ID)
	}

	if page.Counts, err = r.CountsByStatus(ctx, filter.ProjectID); err != nil {
		return ports.IssuePage{}, err
	}
	return page, nil
}

// CountsByStatus reports how many issues a project holds in each status.
//
// Deliberately ignoring the rest of the filter: the numbers exist to label the
// filter buttons, and a count that changed depending on which button was
// already pressed would tell nobody anything.
func (r *IssueRepository) CountsByStatus(ctx context.Context, projectID int64) (map[domain.IssueStatus]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT status, COUNT(*) FROM issues WHERE project_id = ? GROUP BY status", projectID)
	if err != nil {
		return nil, fmt.Errorf("counting issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Every known status is present, including the ones at zero: a filter
	// button that disappears when its count is nought is a button that moves
	// under the cursor.
	counts := map[domain.IssueStatus]int64{
		domain.StatusUnresolved: 0,
		domain.StatusResolved:   0,
		domain.StatusIgnored:    0,
	}
	for rows.Next() {
		var (
			status string
			count  int64
		)
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scanning count: %w", err)
		}
		counts[domain.IssueStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating counts: %w", err)
	}
	return counts, nil
}

// encodeCursor packs the sort key of the last row on a page.
//
// Opaque on purpose: it is this repository's ordering, and a client that
// learned to build one would be coupled to a decision meant to stay
// changeable.
func encodeCursor(lastSeen time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(formatTime(lastSeen) + "|" + strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (lastSeen string, id int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, fmt.Errorf("%w: unreadable cursor", domain.ErrInvalidIssue)
	}
	lastSeen, rawID, found := strings.Cut(string(raw), "|")
	if !found {
		return "", 0, fmt.Errorf("%w: malformed cursor", domain.ErrInvalidIssue)
	}
	id, err = strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: malformed cursor", domain.ErrInvalidIssue)
	}
	// Parsed to reject a cursor whose timestamp is not a timestamp, rather
	// than passing it to SQLite and comparing text that means nothing.
	if _, err := parseTime(lastSeen); err != nil {
		return "", 0, fmt.Errorf("%w: malformed cursor", domain.ErrInvalidIssue)
	}
	return lastSeen, id, nil
}

// FindByID returns one issue.
//
// The project id is part of the lookup rather than checked afterwards: an
// issue belongs to a project, and a query that can return another project's
// issue is one refactor away from being an authorisation bug.
func (r *IssueRepository) FindByID(ctx context.Context, projectID, issueID int64) (domain.Issue, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, fingerprint, grouping_version, title, culprit,
		       level, status, first_seen, last_seen, times, last_release
		FROM issues
		WHERE id = ? AND project_id = ?`, issueID, projectID)

	issue, err := scanIssue(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Issue{}, fmt.Errorf("%w: id %d", domain.ErrIssueNotFound, issueID)
	}
	if err != nil {
		return domain.Issue{}, fmt.Errorf("reading issue: %w", err)
	}
	return issue, nil
}

// SetStatus resolves, ignores or reopens an issue.
func (r *IssueRepository) SetStatus(ctx context.Context, projectID, issueID int64, status domain.IssueStatus) error {
	if !status.ValidStatus() {
		return fmt.Errorf("%w: unknown status %q", domain.ErrInvalidIssue, status)
	}
	result, err := r.db.ExecContext(ctx,
		"UPDATE issues SET status = ? WHERE id = ? AND project_id = ?",
		string(status), issueID, projectID)
	if err != nil {
		return fmt.Errorf("updating issue status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading update result: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: id %d", domain.ErrIssueNotFound, issueID)
	}
	return nil
}

// LatestEvents returns an issue's most recent occurrences, payloads included.
func (r *IssueRepository) LatestEvents(ctx context.Context, issueID int64, limit int) ([]ports.StoredEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = DefaultEventListLimit
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, issue_id, event_id, received_at, occurred_at, level,
		       release, environment, message, payload, payload_codec
		FROM events
		WHERE issue_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []ports.StoredEvent
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating events: %w", err)
	}
	return events, nil
}

// Tags returns an issue's aggregated tags, most common value first.
func (r *IssueRepository) Tags(ctx context.Context, issueID int64) (map[string][]ports.TagCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT key, value, count FROM issue_tags
		WHERE issue_id = ?
		ORDER BY key, count DESC, value`, issueID)
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tags := map[string][]ports.TagCount{}
	for rows.Next() {
		var (
			key   string
			count ports.TagCount
		)
		if err := rows.Scan(&key, &count.Value, &count.Count); err != nil {
			return nil, fmt.Errorf("scanning tag: %w", err)
		}
		tags[key] = append(tags[key], count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tags: %w", err)
	}
	return tags, nil
}

// DeleteEventsBefore removes old events in bounded batches.
//
// Batched because a retention sweep on a live single-writer store must never
// hold one long write transaction: doing so would stall ingestion, and an
// error tracker that stops accepting events while tidying up has failed at the
// only moment that matters (ADR 001).
func (r *IssueRepository) DeleteEventsBefore(
	ctx context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE id IN (
			SELECT id FROM events
			WHERE project_id = ? AND received_at < ?
			LIMIT ?
		)`, projectID, formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("deleting old events: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading delete result: %w", err)
	}
	return deleted, nil
}

func scanIssue(row scanner) (domain.Issue, error) {
	var (
		issue     domain.Issue
		level     string
		status    string
		firstSeen string
		lastSeen  string
	)
	if err := row.Scan(&issue.ID, &issue.ProjectID, &issue.Fingerprint, &issue.GroupingVersion,
		&issue.Title, &issue.Culprit, &level, &status, &firstSeen, &lastSeen,
		&issue.Times, &issue.LastRelease); err != nil {
		return domain.Issue{}, err
	}

	issue.Level = domain.Level(level)
	issue.Status = domain.IssueStatus(status)

	var err error
	if issue.FirstSeen, err = parseTime(firstSeen); err != nil {
		return domain.Issue{}, err
	}
	if issue.LastSeen, err = parseTime(lastSeen); err != nil {
		return domain.Issue{}, err
	}
	return issue, nil
}

func scanEvent(row scanner) (ports.StoredEvent, error) {
	var (
		event      ports.StoredEvent
		level      string
		receivedAt string
		occurredAt string
		payload    []byte
		codec      string
	)
	if err := row.Scan(&event.ID, &event.IssueID, &event.EventID, &receivedAt, &occurredAt,
		&level, &event.Release, &event.Environment, &event.Message, &payload, &codec); err != nil {
		return ports.StoredEvent{}, fmt.Errorf("scanning event: %w", err)
	}

	event.Level = domain.Level(level)

	var err error
	if event.ReceivedAt, err = parseTime(receivedAt); err != nil {
		return ports.StoredEvent{}, err
	}
	if event.OccurredAt, err = parseTime(occurredAt); err != nil {
		return ports.StoredEvent{}, err
	}

	// The codec is read from the row rather than assumed, so a future change
	// does not require rewriting every stored payload.
	if codec != compression.Codec {
		return ports.StoredEvent{}, fmt.Errorf("%w: event %d uses unknown payload codec %q",
			ErrSchema, event.ID, codec)
	}
	if event.Payload, err = compression.Decompress(payload); err != nil {
		return ports.StoredEvent{}, err
	}
	return event, nil
}
