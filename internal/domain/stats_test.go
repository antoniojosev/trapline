package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var statsNow = time.Date(2026, 8, 29, 14, 37, 12, 0, time.UTC)

func TestHourBucketTruncatesAndNormalisesToUTC(t *testing.T) {
	// A bucket has to be the same string whichever zone the caller's clock is
	// in, or the same hour lands in two rows and every count is wrong by
	// however many timezones the fleet spans.
	madrid := time.FixedZone("CEST", 2*60*60)
	local := time.Date(2026, 8, 29, 16, 59, 59, 0, madrid)

	if got, want := HourBucket(local), "2026-08-29T14"; got != want {
		t.Errorf("HourBucket(%s) = %q, want %q", local, got, want)
	}
	if got := HourBucket(statsNow); got != "2026-08-29T14" {
		t.Errorf("HourBucket = %q", got)
	}
}

func TestHourBucketRoundTrips(t *testing.T) {
	parsed, err := ParseHourBucket(HourBucket(statsNow))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if want := statsNow.Truncate(time.Hour); !parsed.Equal(want) {
		t.Errorf("parsed = %s, want %s", parsed, want)
	}
}

func TestParseHourBucketRejectsNonsense(t *testing.T) {
	for _, raw := range []string{"", "2026-08-29", "2026-08-29T14:00:00Z", "yesterday"} {
		if _, err := ParseHourBucket(raw); !errors.Is(err, ErrInvalidRange) {
			t.Errorf("ParseHourBucket(%q) error = %v, want ErrInvalidRange", raw, err)
		}
	}
}

func TestNewRangeFillsInBothEnds(t *testing.T) {
	window, err := NewRange(time.Time{}, time.Time{}, statsNow)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	// A day, inclusive of both ends: 24 buckets, not 25. The off-by-one here
	// is the difference between a chart that shows a day and one that shows a
	// day and an hour, which is exactly the sort of thing nobody notices.
	if got := window.Hours(); got != DefaultRangeHours {
		t.Errorf("hours = %d, want %d", got, DefaultRangeHours)
	}
	if got, want := window.LastBucket(), "2026-08-29T14"; got != want {
		t.Errorf("last bucket = %q, want %q", got, want)
	}
	if got, want := window.FirstBucket(), "2026-08-28T15"; got != want {
		t.Errorf("first bucket = %q, want %q", got, want)
	}
}

func TestNewRangeDefaultsOnlyTheEndThatIsMissing(t *testing.T) {
	from := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	window, err := NewRange(from, time.Time{}, statsNow)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	if got := window.Hours(); got != 5 {
		t.Errorf("hours = %d, want 5 (10:00 through 14:00 inclusive)", got)
	}
}

func TestNewRangeTruncatesToTheHourItCanAnswer(t *testing.T) {
	from := time.Date(2026, 8, 29, 10, 59, 59, 0, time.UTC)
	to := time.Date(2026, 8, 29, 12, 0, 1, 0, time.UTC)
	window, err := NewRange(from, to, statsNow)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	if got, want := window.FirstBucket(), "2026-08-29T10"; got != want {
		t.Errorf("first = %q, want %q", got, want)
	}
	if got, want := window.LastBucket(), "2026-08-29T12"; got != want {
		t.Errorf("last = %q, want %q", got, want)
	}
}

func TestNewRangeRejectsBackwardsAndOversized(t *testing.T) {
	backwards := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	if _, err := NewRange(statsNow, backwards, statsNow); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("a backwards range was accepted: %v", err)
	}

	// Past the aggregates' own keep-window there is nothing left to return, so
	// asking is asking for a response full of zeroes.
	tooFar := statsNow.Add(-(MaxRangeHours + 1) * time.Hour)
	if _, err := NewRange(tooFar, statsNow, statsNow); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("an oversized range was accepted: %v", err)
	}
	atTheLimit := statsNow.Add(-(MaxRangeHours - 1) * time.Hour)
	if _, err := NewRange(atTheLimit, statsNow, statsNow); err != nil {
		t.Errorf("the largest legal range was refused: %v", err)
	}
}

func TestBucketsCoverEveryHourIncludingTheQuietOnes(t *testing.T) {
	from := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	window, err := NewRange(from, statsNow, statsNow)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	buckets := window.Buckets()
	want := []string{"2026-08-29T12", "2026-08-29T13", "2026-08-29T14"}
	if len(buckets) != len(want) {
		t.Fatalf("buckets = %v, want %v", buckets, want)
	}
	for index := range want {
		if buckets[index] != want[index] {
			t.Errorf("bucket %d = %q, want %q", index, buckets[index], want[index])
		}
	}
}

func TestAnEmptyRangeHasNoBuckets(t *testing.T) {
	inverted := Range{From: statsNow, To: statsNow.Add(-2 * time.Hour)}
	if got := inverted.Hours(); got != 0 {
		t.Errorf("hours = %d, want 0", got)
	}
	if got := inverted.Buckets(); got != nil {
		t.Errorf("buckets = %v, want none", got)
	}
}

func TestParseWindow(t *testing.T) {
	cases := map[string]Window{"": Window24h, "24h": Window24h, "14d": Window14d}
	// Whitespace is trimmed, because a value pasted from a URL or a form
	// arrives with it and refusing that is a riddle, not a validation.
	cases["\t14d "] = Window14d
	for raw, want := range cases {
		got, err := ParseWindow(raw)
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("ParseWindow(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := ParseWindow("7d"); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("an unnamed window was accepted: %v", err)
	}
}

func TestWindowRangeCoversExactlyItsHours(t *testing.T) {
	for _, window := range []Window{Window24h, Window14d} {
		span := window.Range(statsNow)
		want := int(window.Duration() / time.Hour)
		if got := span.Hours(); got != want {
			t.Errorf("%s covers %d hours, want %d", window, got, want)
		}
		if got, expected := span.LastBucket(), HourBucket(statsNow); got != expected {
			t.Errorf("%s ends at %q, want the current hour %q", window, got, expected)
		}
	}
}

func TestParseDimension(t *testing.T) {
	for _, want := range Dimensions() {
		got, err := ParseDimension(string(want))
		if err != nil {
			t.Fatalf("ParseDimension(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("ParseDimension(%q) = %q", want, got)
		}
	}
	// The message has to name the alternatives: a breakdown by "version" is a
	// reasonable guess, and the answer should say so rather than send someone
	// to the source.
	_, err := ParseDimension("version")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("error = %v, want ErrInvalidRange", err)
	}
	if got := err.Error(); !strings.Contains(got, "release") || !strings.Contains(got, "environment") {
		t.Errorf("error = %q, want it to name the dimensions that exist", got)
	}
}
