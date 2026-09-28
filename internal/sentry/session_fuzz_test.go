package sentry_test

import (
	"testing"

	"github.com/antoniojosev/trapline/internal/sentry"
)

// FuzzDecodeSession exercises the two session decoders against bytes nobody
// chose.
//
// They read from the public ingest endpoint, which is the same reason the
// event, transaction and envelope parsers are fuzzed (ADR 002). The property
// is the modest one that matters for a decoder on a hot path: whatever it is
// handed, it returns rather than panicking, and everything it does return is
// something the rest of the pipeline can count — no negative counters, and no
// bucket claiming a total it cannot account for.
func FuzzDecodeSession(f *testing.F) {
	f.Add([]byte(`{"sid":"s1","started":"2026-08-29T14:16:00Z","status":"crashed","errors":2,"attrs":{"release":"r@1"}}`))
	f.Add([]byte(`{"sid":"s1","started":1756476960,"status":"exited"}`))
	f.Add([]byte(`{"attrs":{"release":"r@1"},"aggregates":[{"started":"2026-08-29T14:00:00Z","exited":2,"crashed":1}]}`))
	f.Add([]byte(`{"aggregates":[{"started":"x"}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, payload []byte) {
		if session, err := sentry.DecodeSession(payload); err == nil {
			if session.SID == "" {
				t.Fatalf("a session decoded with no sid: %+v", session)
			}
			if session.Errors < 0 {
				t.Fatalf("a negative error count: %d", session.Errors)
			}
		}

		aggregates, err := sentry.DecodeSessionAggregates(payload)
		if err != nil {
			return
		}
		if len(aggregates.Buckets) > sentry.MaxSessionAggregates {
			t.Fatalf("%d buckets survived the ceiling", len(aggregates.Buckets))
		}
		for _, bucket := range aggregates.Buckets {
			if bucket.Started.IsZero() {
				t.Fatalf("a bucket with no start: %+v", bucket)
			}
			if bucket.Exited < 0 || bucket.Errored < 0 || bucket.Crashed < 0 || bucket.Abnormal < 0 {
				t.Fatalf("a negative counter: %+v", bucket)
			}
			if bucket.Total() <= 0 {
				t.Fatalf("a bucket accounting for no sessions: %+v", bucket)
			}
		}
	})
}
