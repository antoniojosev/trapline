package engine

import (
	"context"
	"time"
)

// Store is the storage port the engine needs. It is declared here, on the
// consumer side, so the engine states its own requirements and adapters
// satisfy them — never the other way round.
//
// Append takes a slice, not a single event, because batching is the engine's
// primary write strategy: grouping writes into one transaction is both how
// throughput is reached and how the single-writer store stays responsive
// (ADR 001). A one-event call is just a batch of one.
type Store interface {
	Append(ctx context.Context, events []Event) error
	// DeleteBefore removes events of one category older than cutoff, at most
	// limit rows per call. Retention deletes incrementally: a sweep must
	// never hold a long write transaction on a live store, so the caller
	// loops until the returned count is zero.
	DeleteBefore(ctx context.Context, category Category, cutoff time.Time, limit int) (deleted int64, err error)
}

// CategoryAggregates is the pre-computed rollups of everything above, and it
// is deliberately not one of Categories().
//
// Nothing is ever ingested as an aggregate: they are written as a side effect
// of storing an event, so they can neither be switched on nor rate-limited,
// and listing them beside the ingest categories would offer both. What they
// do have is a keep-window of their own, and that is the point. Aggregates
// are two orders of magnitude smaller than the events they summarise and
// answer the questions that stay interesting longest, so deleting them on the
// events' schedule would throw away the cheap history to save the expensive
// one (ADR 010).
const CategoryAggregates Category = "aggregates"

// RetentionCategories lists everything that has a keep-window: the ingest
// categories plus the aggregates derived from them.
//
// Separate from Categories() because the two answer different questions.
// "What may a project accept?" is Categories(); "what does a sweep delete?"
// is this. Merging them would let an operator enable a category that no SDK
// can send.
func RetentionCategories() []Category {
	categories := Categories()
	return append(categories, CategoryAggregates)
}

// RetentionPolicy is how long events of each category are kept.
//
// Retention is per category because the categories have wildly different
// value-per-byte: an error from three months ago is still worth reading,
// while a raw transaction from three months ago is noise once its aggregates
// exist.
type RetentionPolicy map[Category]time.Duration

// DefaultRetention is the policy a fresh installation starts with. It is
// configurable per project; these are the defaults, not limits.
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{
		CategoryError:       90 * 24 * time.Hour,
		CategoryTransaction: 7 * 24 * time.Hour,
		CategorySession:     7 * 24 * time.Hour,
		CategoryUptime:      90 * 24 * time.Hour,
		CategoryCheckIn:     90 * 24 * time.Hour,
		// Longer than anything it summarises, on purpose: a year and a bit is
		// what makes "is this worse than last Christmas" answerable at all,
		// and a bucket costs a few dozen bytes against the kilobytes of the
		// events it counted.
		CategoryAggregates: 400 * 24 * time.Hour,
	}
}
