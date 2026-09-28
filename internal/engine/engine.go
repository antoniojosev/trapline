// Package engine is the generic event engine: ingest with batching and
// backpressure, storage, windowed aggregation with downsampling, and
// retention by category.
//
// Everything this product records is an event with dimensions — errors,
// transactions, sessions, uptime results and cron check-ins — so each
// feature is a view over this one primitive.
//
// The boundary is deliberate and enforced by a test (internal/arch): this
// package imports nothing from the rest of the repository, and nothing
// domain-specific lives here. Envelope parsing, grouping, symbolication and
// alert rules belong outside. That discipline is what keeps the eventual
// extraction into a standalone Apache-2.0 library cheap (ADR 004); relax it
// and the extraction stops being cheap, which is the whole reason the
// boundary exists.
package engine

import (
	"fmt"
	"time"
)

// Category classifies an event for the purposes that differ per kind of
// data: rate limiting, sampling, retention and aggregation. It maps onto the
// categories the ingest protocol names, so backpressure can be expressed in
// the terms an SDK already understands (ADR 005).
type Category string

// The categories v1 records.
const (
	CategoryError       Category = "error"
	CategoryTransaction Category = "transaction"
	CategorySession     Category = "session"
	CategoryUptime      Category = "uptime"
	CategoryCheckIn     Category = "check_in"
)

// Categories lists every known category. Callers that need to iterate over
// all of them (config defaults, retention sweeps) use this instead of
// hard-coding a list that silently goes stale.
func Categories() []Category {
	return []Category{
		CategoryError,
		CategoryTransaction,
		CategorySession,
		CategoryUptime,
		CategoryCheckIn,
	}
}

// Valid reports whether c is a known category.
func (c Category) Valid() bool {
	for _, known := range Categories() {
		if c == known {
			return true
		}
	}
	return false
}

// Event is the engine's single unit of storage.
//
// Payload holds the complete original record, compressed by the storage
// adapter. Dimensions hold the few values that must be queryable, and they
// are the only thing a listing, search or dashboard query ever reads: the
// rule the whole design rests on is that no query scans payloads (ADR 001).
type Event struct {
	Category   Category
	ProjectID  int64
	Timestamp  time.Time
	Dimensions map[string]string
	Payload    []byte
}

// Validate checks the invariants the storage layer depends on.
//
// The receiver is a pointer because an Event is a heavy struct and this runs
// once per event in the ingest loop; copying it there is measurable waste.
func (e *Event) Validate() error {
	if !e.Category.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownCategory, e.Category)
	}
	if e.ProjectID <= 0 {
		return fmt.Errorf("%w: project id must be positive", ErrInvalidEvent)
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidEvent)
	}
	return nil
}
