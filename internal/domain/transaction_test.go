package domain

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestMinuteBucketRoundTrip(t *testing.T) {
	at := time.Date(2026, time.August, 29, 14, 32, 47, 123456789, time.UTC)

	bucket := MinuteBucket(at)
	if bucket != "2026-08-29T14:32" {
		t.Fatalf("bucket %q, want 2026-08-29T14:32", bucket)
	}

	parsed, err := ParseMinuteBucket(bucket)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if !parsed.Equal(at.Truncate(time.Minute)) {
		t.Errorf("parsed %s, want %s", parsed, at.Truncate(time.Minute))
	}
}

// TestMinuteBucketsSortChronologically is the property the layout exists for:
// a range over these buckets is an index scan, and it is only correct if text
// order and clock order are the same order (ADR 033).
func TestMinuteBucketsSortChronologically(t *testing.T) {
	base := time.Date(2026, time.December, 31, 23, 58, 0, 0, time.UTC)
	previous := ""
	for step := range 5 {
		bucket := MinuteBucket(base.Add(time.Duration(step) * time.Minute))
		if previous != "" && previous >= bucket {
			t.Errorf("%q does not sort before %q", previous, bucket)
		}
		previous = bucket
	}
}

func TestParseMinuteBucketRejectsWhatIsNotOne(t *testing.T) {
	for _, raw := range []string{"", "2026-08-29", "2026-08-29T14", "not a bucket"} {
		if _, err := ParseMinuteBucket(raw); err == nil {
			t.Errorf("ParseMinuteBucket(%q) accepted", raw)
		}
	}
}

func TestFailedCountsEverythingThatIsNotSuccess(t *testing.T) {
	// The successes are a closed list and the failures are open. A status this
	// build has never heard of is far more likely to be a new way of failing
	// than a new way of succeeding, and getting it backwards makes a failure
	// rate under-report — the direction nobody notices.
	notFailures := []string{"", "ok", "OK", " ok ", "cancelled", "unknown"}
	failures := []string{
		"internal_error", "deadline_exceeded", "unavailable", "not_found",
		"permission_denied", "resource_exhausted", "something_new_in_2031",
	}

	for _, status := range notFailures {
		if TransactionStatus(status).Failed() {
			t.Errorf("%q counted as a failure", status)
		}
	}
	for _, status := range failures {
		if !TransactionStatus(status).Failed() {
			t.Errorf("%q did not count as a failure", status)
		}
	}
}

func TestTransactionNameIsBoundedAndNeverEmpty(t *testing.T) {
	if got := TransactionName("  GET /api/checkout  "); got != "GET /api/checkout" {
		t.Errorf("name %q, want the trimmed form", got)
	}
	if got := TransactionName("   "); got != UnnamedTransaction {
		t.Errorf("an empty name became %q, want %q", got, UnnamedTransaction)
	}

	long := strings.Repeat("x", MaxTransactionNameLength*2)
	if got := TransactionName(long); len(got) != MaxTransactionNameLength {
		t.Errorf("a %d-character name became %d, want %d",
			len(long), len(got), MaxTransactionNameLength)
	}
}

// TestSampleTraceIsDeterministicPerTrace is the property waterfalls depend on.
//
// Every transaction of one request has to reach the same verdict, in this
// process and in the next one. A coin flip per transaction would store some
// and drop others, and the resulting waterfall would have holes in it that
// look exactly like instrumentation somebody forgot to add (ADR 021).
func TestSampleTraceIsDeterministicPerTrace(t *testing.T) {
	const traceID = "4c79f60c11214eb38604f4ae0781bfb2"
	first := SampleTrace(traceID, 0.5)
	for range 100 {
		if SampleTrace(traceID, 0.5) != first {
			t.Fatal("the same trace id reached two different verdicts")
		}
	}
}

func TestSampleTraceHonoursTheRate(t *testing.T) {
	const traces = 20000

	for _, rate := range []float64{0.01, 0.1, 0.5, 0.9} {
		kept := 0
		for index := range traces {
			// Trace ids as the SDKs write them: 32 hex characters.
			if SampleTrace(fmt.Sprintf("%032x", index), rate) {
				kept++
			}
		}
		observed := float64(kept) / float64(traces)
		// Three percentage points of slack over twenty thousand draws. The
		// hash is not a random number generator and a tighter bound would be
		// asserting something about FNV rather than about the sampler.
		if math.Abs(observed-rate) > 0.03 {
			t.Errorf("rate %.2f kept %.4f of traces", rate, observed)
		}
	}
}

func TestSampleTraceAtTheExtremes(t *testing.T) {
	const traceID = "4c79f60c11214eb38604f4ae0781bfb2"

	if SampleTrace(traceID, 0) {
		t.Error("a rate of zero stored a trace")
	}
	if !SampleTrace(traceID, 1) {
		t.Error("a rate of one dropped a trace")
	}
	if SampleTrace("", 1) {
		// Nothing to hash means nothing to correlate, and a stored trace with
		// no id is a row with no reader.
		t.Error("a trace with no id was stored")
	}
	for _, rate := range []float64{-1, 2, math.NaN()} {
		if SampleTrace(traceID, rate) {
			t.Errorf("a rate of %v stored a trace", rate)
		}
	}
}

func TestValidTracesSampleRate(t *testing.T) {
	for _, rate := range []float64{0, 0.5, 1} {
		if !ValidTracesSampleRate(rate) {
			t.Errorf("%v was refused", rate)
		}
	}
	for _, rate := range []float64{-0.1, 1.1, math.NaN()} {
		if ValidTracesSampleRate(rate) {
			t.Errorf("%v was accepted", rate)
		}
	}
}

func TestParseResolution(t *testing.T) {
	for raw, want := range map[string]Resolution{
		"":       ResolutionMinute,
		"minute": ResolutionMinute,
		"hour":   ResolutionHour,
	} {
		got, err := ParseResolution(raw)
		if err != nil {
			t.Fatalf("ParseResolution(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("ParseResolution(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := ParseResolution("fortnight"); err == nil {
		t.Error("an unknown resolution was accepted")
	}
	if len(Resolutions()) != 2 {
		t.Errorf("Resolutions() lists %d, want 2", len(Resolutions()))
	}
}

func TestResolutionBuckets(t *testing.T) {
	from := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	window := Range{From: from, To: from.Add(time.Hour)}

	hours := ResolutionHour.Buckets(window)
	if len(hours) != 2 {
		t.Fatalf("two hours produced %d buckets: %v", len(hours), hours)
	}
	if hours[0] != "2026-08-29T10" || hours[1] != "2026-08-29T11" {
		t.Errorf("hour buckets %v", hours)
	}

	// The range is a closed interval of whole hours, so at minute resolution
	// the last hour has to contribute all sixty of its minutes and not just
	// the first — otherwise a chart's axis stops an hour short of the data.
	minutes := ResolutionMinute.Buckets(window)
	if len(minutes) != 120 {
		t.Fatalf("two hours produced %d minutes, want 120", len(minutes))
	}
	if minutes[0] != "2026-08-29T10:00" || minutes[119] != "2026-08-29T11:59" {
		t.Errorf("minute buckets run %s..%s", minutes[0], minutes[119])
	}

	if ResolutionHour.Step() != time.Hour || ResolutionMinute.Step() != time.Minute {
		t.Error("a resolution's step is not its own unit")
	}
	if ResolutionHour.Bucket(from) != HourBucket(from) {
		t.Error("the hour resolution does not render an hour bucket")
	}
}

func TestResolutionBucketsAreBounded(t *testing.T) {
	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	// A year at minute resolution is half a million points. The cap is the
	// minute retention, because past it the buckets do not exist and a
	// response full of empty ones is a chart nobody can draw.
	window := Range{From: from, To: from.AddDate(1, 0, 0)}
	if got := len(ResolutionMinute.Buckets(window)); got != MaxSeriesPoints {
		t.Errorf("a year of minutes produced %d points, want the cap of %d", got, MaxSeriesPoints)
	}

	inverted := Range{From: from, To: from.Add(-time.Hour)}
	if got := ResolutionMinute.Buckets(inverted); got != nil {
		t.Errorf("an inverted range produced %d buckets", len(got))
	}
}

func TestParseTransactionSort(t *testing.T) {
	for raw, want := range map[string]TransactionSort{
		"":      SortP95,
		"p95":   SortP95,
		"count": SortCount,
		"fail":  SortFail,
	} {
		got, err := ParseTransactionSort(raw)
		if err != nil {
			t.Fatalf("ParseTransactionSort(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("ParseTransactionSort(%q) = %q, want %q", raw, got, want)
		}
	}

	_, err := ParseTransactionSort("alphabetical")
	if err == nil {
		t.Fatal("an unknown sort was accepted")
	}
	// The message has to name the alternatives: a client that got it wrong
	// should not have to go looking.
	for _, known := range TransactionSorts() {
		if !strings.Contains(err.Error(), string(known)) {
			t.Errorf("the error does not name %q: %v", known, err)
		}
	}
}

func TestProjectConfigTracesSampleRate(t *testing.T) {
	var unset ProjectConfig
	if unset.TracesSampleRate() != DefaultTracesSampleRate {
		t.Errorf("an unset rate resolved to %v, want the default", unset.TracesSampleRate())
	}

	// Zero is a decision — aggregate everything, keep no waterfalls — and
	// must not be read as an absence.
	zero := 0.0
	chosen := ProjectConfig{TracesSampleRateSetting: &zero}
	if chosen.TracesSampleRate() != 0 {
		t.Errorf("a stored zero resolved to %v, want 0", chosen.TracesSampleRate())
	}

	// A stored value that is not a rate falls back rather than being clamped:
	// 7 is a typo or a bad migration, and honouring it as 1.0 would fill a
	// disk on the strength of one.
	broken := 7.0
	corrupt := ProjectConfig{TracesSampleRateSetting: &broken}
	if corrupt.TracesSampleRate() != DefaultTracesSampleRate {
		t.Errorf("a stored 7 resolved to %v, want the default", corrupt.TracesSampleRate())
	}
}

func TestProjectConfigTracesSampleRateSurvivesStorage(t *testing.T) {
	rate := 0.25
	encoded, err := EncodeProjectConfig(ProjectConfig{TracesSampleRateSetting: &rate})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	decoded := DecodeProjectConfig(encoded)
	if decoded.TracesSampleRateSetting == nil || *decoded.TracesSampleRateSetting != rate {
		t.Errorf("the rate did not survive: %v", decoded.TracesSampleRateSetting)
	}

	// And an unset one comes back unset rather than as a zero somebody chose.
	plain := DecodeProjectConfig(`{"enabled_categories":["error"]}`)
	if plain.TracesSampleRateSetting != nil {
		t.Errorf("an absent rate came back as %v", *plain.TracesSampleRateSetting)
	}
}

// TestSampleTraceSpreadsIdsThatShareAPrefix is a regression test for a bug the
// HTTP suite found and the statistical test above did not.
//
// FNV-1a folds each byte into the low end of its accumulator, so its high bits
// are dominated by the bytes it saw first. Taking the top 53 bits of a raw FNV
// sum put four hundred trace ids that differ only in their last characters
// — a sequential fixture, a generator with a fixed epoch — all on the same side
// of a 10% threshold: nothing at all was sampled. The failure was invisible in
// a twenty-thousand-id sample, which is exactly why it survived the first
// test.
func TestSampleTraceSpreadsIdsThatShareAPrefix(t *testing.T) {
	const traces = 400

	kept := 0
	for index := range traces {
		if SampleTrace(fmt.Sprintf("%032x", index), 0.1) {
			kept++
		}
	}
	if kept == 0 || kept == traces {
		t.Fatalf("%d of %d prefix-sharing ids sampled at 0.1: the hash is not mixing", kept, traces)
	}
	if kept < traces/40 || kept > traces/4 {
		t.Errorf("%d of %d sampled at 0.1, want roughly a tenth", kept, traces)
	}
}
