package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// This file holds the deliberate moves: a person resolving, ignoring or
// reopening an issue. What an *event* does to the same columns is decided and
// written by RecordEvent, inside the transaction that stores the event
// (ADR 032), because the reopen rule and the counters it depends on are one
// decision and must not be two writes.

// resolutionColumns are what a resolution is made of, in scan order.
const resolutionColumns = `status, first_release, resolved_at, resolved_in_release,
	resolve_next_release, regressions, regressed_in_release, seen_in_resolved_release_count`

// Resolve marks an issue fixed, either as of now or as of the release it is
// currently being seen in.
//
// One statement, and the decision of what the new state should be is the
// domain's: the row is read, handed to the domain, and written back inside a
// transaction. Doing the arithmetic in SQL would put the rule somewhere it
// cannot be tested without a database and where it would drift from the
// domain that owns it.
func (r *ReleaseRepository) Resolve(
	ctx context.Context, projectID, issueID int64, at time.Time, inNextRelease bool,
) (domain.Issue, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Issue{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, err := readIssueLifecycle(ctx, tx, projectID, issueID)
	if err != nil {
		return domain.Issue{}, err
	}

	resolved := issue.Resolve(at)
	if inNextRelease {
		resolved = issue.ResolveInNextRelease(at)
	}
	if err := writeIssueLifecycle(ctx, tx, resolved); err != nil {
		return domain.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Issue{}, fmt.Errorf("committing resolution: %w", err)
	}
	return resolved, nil
}

// SetStatus moves an issue to a status that is not "resolved", forgetting a
// resolution that no longer applies.
//
// Ignoring or reopening an issue that was resolved in the next release has to
// clear the pin, or a later event would be measured against a release nobody
// is waiting for any more.
func (r *ReleaseRepository) SetStatus(
	ctx context.Context, projectID, issueID int64, status domain.IssueStatus,
) error {
	if !status.ValidStatus() {
		return fmt.Errorf("%w: unknown status %q", domain.ErrInvalidIssue, status)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	issue, err := readIssueLifecycle(ctx, tx, projectID, issueID)
	if err != nil {
		return err
	}

	var updated domain.Issue
	switch status {
	case domain.StatusIgnored:
		updated = issue.Ignore()
	case domain.StatusUnresolved:
		updated = issue.Reopen()
	case domain.StatusResolved:
		// Routed through Resolve so a resolution always carries a timestamp.
		updated = issue.Resolve(time.Now().UTC())
	}
	if err := writeIssueLifecycle(ctx, tx, updated); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing status: %w", err)
	}
	return nil
}

// Resolution reads an issue's resolution columns.
func (r *ReleaseRepository) Resolution(
	ctx context.Context, projectID, issueID int64,
) (ports.IssueResolution, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+resolutionColumns+" FROM issues WHERE id = ? AND project_id = ?",
		issueID, projectID)

	issue, err := scanIssueLifecycle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.IssueResolution{}, fmt.Errorf("%w: id %d", domain.ErrIssueNotFound, issueID)
	}
	if err != nil {
		return ports.IssueResolution{}, fmt.Errorf("reading resolution: %w", err)
	}
	return ports.IssueResolution{
		Status:                     issue.Status,
		FirstRelease:               issue.FirstRelease,
		ResolvedAt:                 issue.ResolvedAt,
		ResolvedInRelease:          issue.ResolvedInRelease,
		ResolveNextRelease:         issue.ResolveNextRelease,
		Regressions:                issue.Regressions,
		RegressedInRelease:         issue.RegressedInRelease,
		SeenInResolvedReleaseCount: issue.SeenInResolvedReleaseCount,
	}, nil
}

// readIssueLifecycle reads the columns this file owns, plus the two the
// domain needs to decide with: the status and the release the issue is
// currently being seen in.
func readIssueLifecycle(ctx context.Context, tx *sql.Tx, projectID, issueID int64) (domain.Issue, error) {
	row := tx.QueryRowContext(ctx,
		"SELECT id, last_release, "+resolutionColumns+" FROM issues WHERE id = ? AND project_id = ?",
		issueID, projectID)

	var (
		issue              domain.Issue
		resolvedAt         sql.NullString
		resolveNextRelease int
		err                error
	)
	if err = row.Scan(&issue.ID, &issue.LastRelease, &issue.Status, &issue.FirstRelease,
		&resolvedAt, &issue.ResolvedInRelease, &resolveNextRelease,
		&issue.Regressions, &issue.RegressedInRelease,
		&issue.SeenInResolvedReleaseCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Issue{}, fmt.Errorf("%w: id %d", domain.ErrIssueNotFound, issueID)
		}
		return domain.Issue{}, fmt.Errorf("reading issue lifecycle: %w", err)
	}
	issue.ProjectID = projectID
	issue.ResolveNextRelease = resolveNextRelease != 0
	if issue.ResolvedAt, err = parseNullableTime(resolvedAt); err != nil {
		return domain.Issue{}, err
	}
	return issue, nil
}

// writeIssueLifecycle writes back only the columns this file owns.
//
// Only those columns, deliberately: the counters and timestamps an event
// updates belong to the write that recorded the event, and an UPDATE here
// that also set them would overwrite a concurrent one with a stale copy.
func writeIssueLifecycle(ctx context.Context, tx *sql.Tx, issue domain.Issue) error {
	nextRelease := 0
	if issue.ResolveNextRelease {
		nextRelease = 1
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE issues
		SET status = ?, first_release = ?, resolved_at = ?, resolved_in_release = ?,
		    resolve_next_release = ?, regressions = ?, regressed_in_release = ?,
		    seen_in_resolved_release_count = ?
		WHERE id = ?`,
		string(issue.Status), issue.FirstRelease, nullableTime(issue.ResolvedAt),
		issue.ResolvedInRelease, nextRelease, issue.Regressions, issue.RegressedInRelease,
		issue.SeenInResolvedReleaseCount, issue.ID)
	if err != nil {
		return fmt.Errorf("updating issue lifecycle: %w", err)
	}
	return nil
}

// scanIssueLifecycle reads the resolution columns alone, for a read-only
// caller that already has the issue.
func scanIssueLifecycle(row interface{ Scan(...any) error }) (domain.Issue, error) {
	var (
		issue              domain.Issue
		resolvedAt         sql.NullString
		resolveNextRelease int
	)
	if err := row.Scan(&issue.Status, &issue.FirstRelease, &resolvedAt,
		&issue.ResolvedInRelease, &resolveNextRelease,
		&issue.Regressions, &issue.RegressedInRelease,
		&issue.SeenInResolvedReleaseCount); err != nil {
		return domain.Issue{}, err //nolint:wrapcheck // the caller distinguishes sql.ErrNoRows.
	}
	issue.ResolveNextRelease = resolveNextRelease != 0

	var err error
	if issue.ResolvedAt, err = parseNullableTime(resolvedAt); err != nil {
		return domain.Issue{}, err
	}
	return issue, nil
}
