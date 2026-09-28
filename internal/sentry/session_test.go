package sentry_test

import (
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/sentry"
)

// TestDecodeSessionReadsWhatThePythonSDKSends uses a payload copied verbatim
// from sentry_sdk 2.68.1's Session.to_json(), because what is under test is
// the shape an SDK puts on the wire and not the shape this repository would
// have chosen (ADR 002).
func TestDecodeSessionReadsWhatThePythonSDKSends(t *testing.T) {
	payload := []byte(`{
		"sid": "888c09a8-290b-422e-94ed-2d70ac755d10",
		"init": true,
		"started": "2026-08-29T14:16:00.674266Z",
		"timestamp": "2026-08-29T14:16:04.674325Z",
		"status": "crashed",
		"errors": 2,
		"attrs": {"release": "shop@1.4.2", "environment": "production"}
	}`)

	session, err := sentry.DecodeSession(payload)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if session.SID != "888c09a8-290b-422e-94ed-2d70ac755d10" {
		t.Fatalf("sid %q", session.SID)
	}
	if !session.Init {
		t.Fatal("init was dropped")
	}
	if session.Status != "crashed" {
		t.Fatalf("status %q", session.Status)
	}
	if session.Errors != 2 {
		t.Fatalf("errors %d, want 2", session.Errors)
	}
	if session.Release != "shop@1.4.2" || session.Environment != "production" {
		t.Fatalf("attrs lost: %q / %q", session.Release, session.Environment)
	}
	want := time.Date(2026, 8, 29, 14, 16, 0, 674266000, time.UTC)
	if !session.Started.Equal(want) {
		t.Fatalf("started %v, want %v", session.Started, want)
	}
}

// TestASessionWithoutAStartedFallsBackToItsUpdateTime: putting it in the hour
// it was last heard from is wrong by at most the session's length, and is far
// better than dropping it.
func TestASessionWithoutAStartedFallsBackToItsUpdateTime(t *testing.T) {
	session, err := sentry.DecodeSession([]byte(
		`{"sid":"s1","timestamp":"2026-08-29T14:16:04Z","status":"exited","attrs":{"release":"r@1"}}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	want := time.Date(2026, 8, 29, 14, 16, 4, 0, time.UTC)
	if !session.Started.Equal(want) {
		t.Fatalf("started %v, want the update's own time %v", session.Started, want)
	}
}

// TestReleaseAndEnvironmentAreReadFromBothPlaces: reading only `attrs` would
// give a release with no numbers for any SDK that puts them at the top level,
// which is a silent failure and the expensive kind.
func TestReleaseAndEnvironmentAreReadFromBothPlaces(t *testing.T) {
	session, err := sentry.DecodeSession([]byte(
		`{"sid":"s1","started":"2026-08-29T14:16:00Z","release":"top@1","environment":"staging"}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if session.Release != "top@1" || session.Environment != "staging" {
		t.Fatalf("got %q / %q", session.Release, session.Environment)
	}
}

func TestDecodeSessionRefusesWhatCannotBeCounted(t *testing.T) {
	for _, test := range []struct{ name, payload string }{
		{"not json", `{`},
		{"no sid", `{"started":"2026-08-29T14:16:00Z","attrs":{"release":"r@1"}}`},
		{"empty sid", `{"sid":"","attrs":{"release":"r@1"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := sentry.DecodeSession([]byte(test.payload)); !errors.Is(err, sentry.ErrInvalidSession) {
				t.Fatalf("got %v, want ErrInvalidSession", err)
			}
		})
	}
}

// TestDecodeSessionAggregatesReadsWhatTheFlusherSends uses the shape
// sentry_sdk's SessionFlusher.add_aggregate_session builds, which is the item
// an SDK in request mode sends instead of individual sessions.
func TestDecodeSessionAggregatesReadsWhatTheFlusherSends(t *testing.T) {
	payload := []byte(`{
		"attrs": {"release": "api@2.0.0", "environment": "production"},
		"aggregates": [
			{"started": "2026-08-29T14:16:00.000000Z", "exited": 40, "errored": 3, "crashed": 2},
			{"started": "2026-08-29T14:17:00.000000Z", "exited": 10, "abnormal": 1}
		]
	}`)

	aggregates, err := sentry.DecodeSessionAggregates(payload)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if aggregates.Release != "api@2.0.0" || aggregates.Environment != "production" {
		t.Fatalf("attrs lost: %q / %q", aggregates.Release, aggregates.Environment)
	}
	if len(aggregates.Buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(aggregates.Buckets))
	}
	if total := aggregates.Buckets[0].Total(); total != 45 {
		t.Fatalf("first bucket totals %d, want 45", total)
	}
	if aggregates.Buckets[0].Crashed != 2 || aggregates.Buckets[1].Abnormal != 1 {
		t.Fatalf("counters lost: %+v", aggregates.Buckets)
	}
}

// TestAnAggregateWithoutAStartIsSkipped: attributing a bucket to the hour the
// request happened to arrive in would put yesterday's crashes on today's
// release, which is the one mistake that would make this number actively
// misleading rather than merely absent.
func TestAnAggregateWithoutAStartIsSkipped(t *testing.T) {
	aggregates, err := sentry.DecodeSessionAggregates([]byte(
		`{"attrs":{"release":"r@1"},"aggregates":[{"exited":9},{"started":"2026-08-29T14:00:00Z","exited":1}]}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(aggregates.Buckets) != 1 {
		t.Fatalf("got %d buckets, want the one that said when", len(aggregates.Buckets))
	}
}

// TestAnEmptyAggregateBucketIsSkipped keeps a flush from writing rows of
// zeroes, which would make "this release reported nothing" and "this release
// reported nothing bad" look identical.
func TestAnEmptyAggregateBucketIsSkipped(t *testing.T) {
	aggregates, err := sentry.DecodeSessionAggregates([]byte(
		`{"attrs":{"release":"r@1"},"aggregates":[{"started":"2026-08-29T14:00:00Z"}]}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(aggregates.Buckets) != 0 {
		t.Fatalf("an empty bucket survived: %+v", aggregates.Buckets)
	}
}

// TestAnOverlongAggregateIsTruncatedNotRefused: the payload arrives on a
// public endpoint, so its length is chosen by whoever is talking to us. A
// truncated aggregate still counts most of what was sent; a rejected one
// counts none of it.
func TestAnOverlongAggregateIsTruncatedNotRefused(t *testing.T) {
	var payload []byte
	payload = append(payload, `{"attrs":{"release":"r@1"},"aggregates":[`...)
	for index := range sentry.MaxSessionAggregates + 50 {
		if index > 0 {
			payload = append(payload, ',')
		}
		payload = append(payload, `{"started":"2026-08-29T14:00:00Z","exited":1}`...)
	}
	payload = append(payload, `]}`...)

	aggregates, err := sentry.DecodeSessionAggregates(payload)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !aggregates.Truncated {
		t.Fatal("an overlong aggregate did not report itself truncated")
	}
	if len(aggregates.Buckets) != sentry.MaxSessionAggregates {
		t.Fatalf("kept %d buckets, want %d", len(aggregates.Buckets), sentry.MaxSessionAggregates)
	}
}

// TestNonsenseCountsBecomeZero: a negative count would let a payload subtract
// sessions that really were reported, and a NaN converted to an int64 is
// undefined behaviour the specification declines to define.
func TestNonsenseCountsBecomeZero(t *testing.T) {
	aggregates, err := sentry.DecodeSessionAggregates([]byte(
		`{"attrs":{"release":"r@1"},"aggregates":[{"started":"2026-08-29T14:00:00Z","exited":-40,"crashed":3}]}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(aggregates.Buckets) != 1 || aggregates.Buckets[0].Exited != 0 {
		t.Fatalf("a negative count survived: %+v", aggregates.Buckets)
	}
	if aggregates.Buckets[0].Total() != 3 {
		t.Fatalf("total %d, want 3", aggregates.Buckets[0].Total())
	}
}

func TestDecodeSessionAggregatesRefusesNonsense(t *testing.T) {
	if _, err := sentry.DecodeSessionAggregates([]byte(`[`)); !errors.Is(err, sentry.ErrInvalidSession) {
		t.Fatalf("got %v, want ErrInvalidSession", err)
	}
}
