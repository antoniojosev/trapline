package sqlite

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// orderingCases are the instants the stored layout has to order correctly, and
// they are chosen around the shape of the string rather than around the clock:
// every one of them is a place where a variable-width fraction changes the
// number of characters before the trailing 'Z'.
var orderingCases = []struct {
	name string
	at   time.Time
}{
	{"midnight, exact second", time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
	{"midnight, one nanosecond", time.Date(2026, 8, 29, 0, 0, 0, 1, time.UTC)},
	{"exact second", time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)},
	{"one millisecond", time.Date(2026, 8, 29, 10, 0, 0, 1_000_000, time.UTC)},
	{"one digit of fraction", time.Date(2026, 8, 29, 10, 0, 0, 500_000_000, time.UTC)},
	{"nine digits of fraction", time.Date(2026, 8, 29, 10, 0, 0, 123_456_789, time.UTC)},
	{"last nanosecond of the second", time.Date(2026, 8, 29, 10, 0, 0, 999_999_999, time.UTC)},
	{"the next second", time.Date(2026, 8, 29, 10, 0, 1, 0, time.UTC)},
	{"last nanosecond of the day", time.Date(2026, 8, 29, 23, 59, 59, 999_999_999, time.UTC)},
	{"the next midnight", time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)},
	{"a year boundary", time.Date(2026, 12, 31, 23, 59, 59, 999_999_999, time.UTC)},
	{"the next year", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	{"the earliest storable instant", minStorableTime},
	{"the latest storable instant", maxStorableTime},
}

// TestStoredTimeOrdersLexicographically is the property the whole layout exists
// for: for any two instants, comparing their stored text answers the same
// question as comparing the instants.
//
// SQLite compares TEXT byte by byte, so Go's own string comparison is that
// comparison — this is checking the bytes SQLite will see, not a stand-in for
// them. The database-level half is in TestMigrationRewritesExistingTimestamps.
func TestStoredTimeOrdersLexicographically(t *testing.T) {
	for _, a := range orderingCases {
		for _, b := range orderingCases {
			left, right := formatTime(a.at), formatTime(b.at)

			var wantOrder, gotOrder string
			switch {
			case a.at.Before(b.at):
				wantOrder = "<"
			case a.at.After(b.at):
				wantOrder = ">"
			default:
				wantOrder = "="
			}
			switch {
			case left < right:
				gotOrder = "<"
			case left > right:
				gotOrder = ">"
			default:
				gotOrder = "="
			}

			if wantOrder != gotOrder {
				t.Errorf("%s (%q) %s %s (%q), but chronologically it is %s",
					a.name, left, gotOrder, b.name, right, wantOrder)
			}
		}
	}
}

// TestStoredTimeOrdersRandomInstants is the same property over instants nobody
// chose, because a table of cases only proves the cases somebody thought of.
//
// The seed is fixed so a failure is reproducible; the generator deliberately
// biases towards exact seconds and short fractions, which is where the old
// layout broke and where a random nanosecond count almost never lands.
func TestStoredTimeOrdersRandomInstants(t *testing.T) {
	source := rand.New(rand.NewPCG(0x33, 0x2e5a)) //nolint:gosec // reproducibility, not secrecy

	fractions := []int{0, 0, 0, 1, 10, 1_000, 100_000_000, 500_000_000, 999_999_999}
	instant := func() time.Time {
		return time.Date(
			2020+source.IntN(10), time.Month(1+source.IntN(12)), 1+source.IntN(28),
			source.IntN(24), source.IntN(60), source.IntN(60),
			fractions[source.IntN(len(fractions))], time.UTC)
	}

	for range 20_000 {
		a, b := instant(), instant()
		left, right := formatTime(a), formatTime(b)

		if (left < right) != a.Before(b) || (left > right) != a.After(b) {
			t.Fatalf("text order disagrees with time order: %q vs %q (%s vs %s)",
				left, right, a, b)
		}
	}
}

// TestRFC3339NanoDoesNotOrder is the bug this layout replaced, kept as a test
// so nobody re-derives the comment that used to justify it. It asserts the
// wrong behaviour on purpose: if a future Go stopped trimming the fraction,
// this failing is the notification.
func TestRFC3339NanoDoesNotOrder(t *testing.T) {
	exact := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	later := exact.Add(500 * time.Millisecond)

	if !exact.Before(later) {
		t.Fatal("the fixture is wrong")
	}
	if exact.Format(time.RFC3339Nano) < later.Format(time.RFC3339Nano) {
		t.Error("RFC3339Nano now orders correctly; the reason for this layout may have changed")
	}
	if formatTime(exact) >= formatTime(later) {
		t.Errorf("the stored layout has the same bug: %q vs %q", formatTime(exact), formatTime(later))
	}
}

func TestFormatTimeIsFixedWidth(t *testing.T) {
	const want = len("2026-08-29T10:00:00.000000000Z")

	for _, tc := range orderingCases {
		got := formatTime(tc.at)
		if len(got) != want {
			t.Errorf("%s formatted as %q, %d characters, want %d", tc.name, got, len(got), want)
		}
		if !strings.HasSuffix(got, "Z") || !strings.Contains(got, ".") {
			t.Errorf("%s formatted as %q, want a UTC instant with a fraction", tc.name, got)
		}
	}
}

// A timestamp outside four-digit years would be written with five digits or a
// leading '-', either of which sorts wrongly against every other row. The
// protocol lets a client send one as a number, so this is reachable input and
// not a hypothetical.
func TestFormatTimeClampsUnstorableYears(t *testing.T) {
	cases := map[string]struct {
		at   time.Time
		want string
	}{
		"far future": {time.Date(30000, 1, 1, 0, 0, 0, 0, time.UTC), formatTime(maxStorableTime)},
		"before AD":  {time.Date(-44, 3, 15, 0, 0, 0, 0, time.UTC), formatTime(minStorableTime)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := formatTime(tc.at)
			if got != tc.want {
				t.Errorf("formatTime = %q, want %q", got, tc.want)
			}
			if len(got) != len(formatTime(time.Now())) {
				t.Errorf("formatTime = %q, which is not the fixed width", got)
			}
		})
	}
}

// parseTime has to read rows written before migration 0010, because a binary
// opens and migrates a database in the same call and a restored backup can be
// older than the schema.
func TestParseTimeReadsBothLayouts(t *testing.T) {
	want := time.Date(2026, 8, 29, 10, 0, 0, 500_000_000, time.UTC)

	cases := map[string]string{
		"current layout":         "2026-08-29T10:00:00.500000000Z",
		"legacy, short fraction": "2026-08-29T10:00:00.5Z",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseTime(raw)
			if err != nil {
				t.Fatalf("parsing %q: %v", raw, err)
			}
			if !got.Equal(want) {
				t.Errorf("parsed %q as %s, want %s", raw, got, want)
			}
		})
	}

	// And the case that used to be written with no fraction at all.
	got, err := parseTime("2026-08-29T10:00:00Z")
	if err != nil {
		t.Fatalf("parsing a legacy exact second: %v", err)
	}
	if !got.Equal(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("parsed a legacy exact second as %s", got)
	}
}

func TestParseTimeRejectsWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{"", "not a time", "2026-08-29", "1756468800", "2026-13-01T00:00:00Z"} {
		if _, err := parseTime(raw); err == nil {
			t.Errorf("parsing %q succeeded, want an error", raw)
		} else if !errors.Is(err, ErrSchema) {
			t.Errorf("parsing %q returned %v, want it to be an ErrSchema", raw, err)
		} else if !strings.Contains(err.Error(), raw) && raw != "" {
			t.Errorf("parsing %q returned %v, which does not name the value", raw, err)
		}
	}
}
