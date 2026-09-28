package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// SessionBucket is one row of session_hourly on its way in: which release, in
// which environment, in which hour, and the four counters to add to it.
//
// Counters to *add*, never to set. Every writer of this table is folding a
// window's verdicts into whatever is already there, and a store that replaced
// instead of adding would silently discard the counts of the flush before it.
type SessionBucket struct {
	Key    domain.HealthKey
	Counts domain.SessionCounts
}

// SessionHour is one hour of one release, as read back.
type SessionHour struct {
	Hour   string
	Counts domain.SessionCounts
}

// ReleaseSessions is one release's totals over a range.
type ReleaseSessions struct {
	Release string
	Counts  domain.SessionCounts
	// FirstHour and LastHour are the ends of the range this release actually
	// reported in, which is not the range that was asked for. They are what
	// lets a listing say "this release stopped reporting yesterday" rather
	// than showing it beside a live one with no way to tell them apart.
	FirstHour string
	LastHour  string
}

// HealthRepository stores and reads the session counters behind release
// health.
//
// There is no method here that reads or writes an individual session, and that
// absence is the design (ADR 008). Sessions are never queryable one at a time,
// which closes the door on this product drifting into session analytics — a
// different product, and the one the competitor spends its infrastructure on.
//
// Named for health rather than for sessions because `SessionRepository` in
// this codebase already means panel logins, and two unrelated things wearing
// one name is how somebody wires the wrong one at four in the morning.
type HealthRepository interface {
	// AddSessionCounts folds a flush's worth of buckets into the table, in one
	// transaction.
	//
	// One transaction for the whole flush rather than one per bucket: a flush
	// carries at most a handful of buckets — the releases seen in the last
	// minute — and the store has a single writer shared with ingestion, so
	// each extra transaction is another chance to sit between an event and its
	// row.
	AddSessionCounts(ctx context.Context, buckets []SessionBucket) error

	// ReleaseSeries reads one release's hours over a closed range of hour
	// buckets, oldest first, summed across environments.
	//
	// Absent hours are absent rather than zero: the caller knows the range it
	// asked for and fills the gaps, and a store that invented rows would be
	// inventing sessions.
	ReleaseSeries(
		ctx context.Context, projectID int64, release string, from, to string,
	) ([]SessionHour, error)

	// ReleaseTotals reads every release of a project over a range, busiest
	// first.
	ReleaseTotals(ctx context.Context, projectID int64, from, to string, limit int) ([]ReleaseSessions, error)

	// HasSessions reports whether any project accepts the session category. It
	// is the question the scheduler asks before the flush job exists at all
	// (ADR 014).
	HasSessions(ctx context.Context) (bool, error)

	// PruneSessionsBefore deletes buckets older than cutoff for one project,
	// at most limit rows per call.
	//
	// Batched for the reason every sweep in this product is: one long delete
	// transaction stalls the endpoint the product exists to keep answering.
	PruneSessionsBefore(ctx context.Context, projectID int64, cutoff time.Time, limit int) (int64, error)
}
