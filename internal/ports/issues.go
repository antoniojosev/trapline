package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// StoredEvent is one recorded occurrence, as read back from storage.
type StoredEvent struct {
	ID          int64
	IssueID     int64
	EventID     string
	ReceivedAt  time.Time
	OccurredAt  time.Time
	Level       domain.Level
	Release     string
	Environment string
	Message     string
	// Payload is the complete original event, decompressed. It is only ever
	// read for a detail view or an issue bundle — never for a listing, a
	// search or a dashboard (ADR 001).
	Payload []byte
}

// RecordEventInput is one event on its way into storage.
type RecordEventInput struct {
	ProjectID   int64
	Fingerprint string
	Observation domain.Observation
	EventID     string
	ReceivedAt  time.Time
	Environment string
	Message     string
	Tags        map[string]string
	Payload     []byte
}

// RecordEventResult says what the event did to its issue.
type RecordEventResult struct {
	Issue domain.Issue
	// New is true when this event created the issue.
	New bool
	// Regressed is true when this event reopened a resolved issue — the
	// single most valuable signal the product produces.
	Regressed bool
}

// IssueFilter selects issues for a listing.
type IssueFilter struct {
	ProjectID int64
	// Status is optional; empty means every status.
	Status domain.IssueStatus
	// Query matches the title or the culprit, case-insensitively.
	Query string
	// Tags narrows to issues carrying every one of these tag values. Because
	// environment, release and level are recorded as tags on ingest, this is
	// also how "what is broken in production" is asked.
	Tags  map[string]string
	Limit int
	// Cursor continues a previous page. Keyset rather than an offset: the
	// list is ordered by last-seen and events keep arriving, so an offset
	// would skip and repeat rows between one page and the next.
	Cursor string
}

// IssuePage is one page of a listing.
type IssuePage struct {
	Issues []domain.Issue
	// NextCursor is empty when there is nothing more.
	NextCursor string
	// Counts is how many issues each status holds for the whole project,
	// ignoring the filter. A filter bar that cannot say "4 unresolved" is a
	// filter bar someone has to click to learn anything from.
	Counts map[domain.IssueStatus]int64
}

// IssueRepository stores issues and their events.
type IssueRepository interface {
	// RecordEvent files an event into its issue, creating the issue if this
	// is the first occurrence. It is one transaction: an event that reached
	// storage without updating its issue's counters would make the numbers
	// lie, and the numbers are what the product is for.
	// The input is a pointer because it is large and this is called once per
	// ingested event. Implementations must treat it as read-only; it belongs
	// to the caller.
	RecordEvent(ctx context.Context, input *RecordEventInput) (RecordEventResult, error)
	List(ctx context.Context, filter IssueFilter) (IssuePage, error)
	// CountsByStatus reports how many issues a project holds in each status.
	CountsByStatus(ctx context.Context, projectID int64) (map[domain.IssueStatus]int64, error)
	FindByID(ctx context.Context, projectID, issueID int64) (domain.Issue, error)
	SetStatus(ctx context.Context, projectID, issueID int64, status domain.IssueStatus) error
	// LatestEvents returns an issue's most recent occurrences.
	LatestEvents(ctx context.Context, issueID int64, limit int) ([]StoredEvent, error)
	// Tags returns an issue's aggregated tag values, most common first.
	Tags(ctx context.Context, issueID int64) (map[string][]TagCount, error)
	// DeleteEventsBefore removes events older than cutoff, at most limit per
	// call so a sweep never holds a long write transaction on a live store.
	DeleteEventsBefore(ctx context.Context, projectID int64, cutoff time.Time, limit int) (int64, error)
}

// TagCount is one tag value and how often it appeared.
//
// The JSON tags matter even though this is a port type: it is returned
// verbatim in the issue-detail response, and without them it serialised as
// PascalCase in the middle of a body that is snake_case everywhere else. A
// client should not have to remember which corner of one endpoint changes
// convention.
type TagCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}
