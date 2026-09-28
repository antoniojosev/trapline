package sqlite

import (
	"database/sql"
	"fmt"
	"time"
)

// timeLayout is how every instant is written to this database: RFC 3339 in
// UTC with a fraction that is **always present and always nine digits wide**.
//
// Timestamps are TEXT so a human reading a row can tell what it says and so
// SQLite can range-scan an index on the column. Both of those still hold. What
// does not hold, and what this layout exists to fix, is the claim this constant
// used to carry: that RFC3339Nano "sorts lexicographically in the same order it
// sorts chronologically". It does not. RFC3339Nano omits trailing zeros in the
// fraction, so an instant that lands on an exact second is written with no
// fraction at all — and '.' (0x2E) sorts before 'Z' (0x5A):
//
//	chronological:  10:00:00Z      10:00:00.001Z  10:00:00.5Z  10:00:01Z
//	as SQLite sorts: 10:00:00.001Z 10:00:00.5Z    10:00:00Z    10:00:01Z
//
// Exact seconds are not an edge case; they are what any SDK that rounds sends.
// `ORDER BY last_seen`, the keyset cursor and the retention `WHERE` all compare
// this text, so the wrong order there meant an event half a second *after* the
// retention cutoff being read as before it and deleted (ADR 033).
//
// A fixed width removes the possibility rather than narrowing it: with the same
// number of characters in every value, byte-by-byte comparison of two of these
// strings is comparison of the instants they name, for every pair.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// legacyTimeLayout is what rows written before migration 0010 look like.
//
// parseTime still accepts it, and must: a binary opens the database and reads
// from it in the same call that migrates it, and a database restored from an
// old backup is a database whose rows have not been rewritten yet. It is only
// ever read, never written — formatTime has one layout.
const legacyTimeLayout = time.RFC3339Nano

// The window this layout can represent without losing its fixed width. Go
// writes a five-digit year for anything past 9999 and a leading '-' before
// year 1, and either of those sorts wrongly against every other row — the
// exact failure this file exists to prevent, reintroduced from the other end.
var (
	minStorableTime = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	maxStorableTime = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
)

// formatTime renders an instant for storage.
//
// Instants outside the representable window are clamped to its edge rather
// than written out. Only a broken clock or a hostile payload gets here — the
// protocol's own timestamp field is a number, and a large enough one lands
// thousands of years out — and a clamped row that sorts correctly is a far
// smaller lie than one string that silently reorders the whole table around
// it.
func formatTime(t time.Time) string {
	utc := t.UTC()
	switch {
	case utc.Before(minStorableTime):
		utc = minStorableTime
	case utc.After(maxStorableTime):
		utc = maxStorableTime
	}
	return utc.Format(timeLayout)
}

// parseTime reads a stored instant, in either the current layout or the one
// used before migration 0010.
//
// Anything else is an error naming both, because a timestamp column holding
// something that is not a timestamp is a corrupt database and the message has
// to be enough to tell which of the two shapes was expected.
func parseTime(raw string) (time.Time, error) {
	if parsed, err := time.Parse(timeLayout, raw); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse(legacyTimeLayout, raw); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf(
		"%w: timestamp %q is neither %q nor the pre-0010 RFC 3339 with a variable fraction",
		ErrSchema, raw, timeLayout)
}

// nullableTime maps an optional timestamp to a value the driver stores as
// NULL when absent.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// parseNullableTime reads an optional timestamp column.
func parseNullableTime(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid {
		return nil, nil
	}
	parsed, err := parseTime(raw.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
