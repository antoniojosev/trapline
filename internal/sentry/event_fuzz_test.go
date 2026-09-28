package sentry_test

import (
	"testing"
	"unicode/utf8"

	"github.com/antoniojosev/trapline/internal/sentry"
)

// FuzzDecodeEvent exercises the event decoder against bytes nobody chose.
//
// This was the last of the four decoders on the public ingest path without a
// fuzz target, and it is the one that reads the most: the transaction, session
// and check-in payloads are small and regular, while an event carries an
// exception chain, a stacktrace, breadcrumbs, tags, contexts and extras, in
// several shapes each, because the protocol really is that polymorphic
// (event.go). Every one of those shapes is a branch that runs on bytes chosen
// by whoever has a DSN — which is everyone, since the key ships inside browser
// bundles (ADR 002).
//
// The properties are the ones the rest of the pipeline depends on, not merely
// "it did not panic":
//
//   - What comes back is storable. Grouping, the issue title and the FTS row
//     are all built from these fields, and a decoder that can emit a string
//     the database cannot hold turns a hostile event into a failed write on
//     the ingest path — which is a refusal for every other project on the
//     same transaction.
//   - Truncation is honoured. The ceilings in this package exist because the
//     payload chooses their input; one that only applies on the paths the unit
//     tests happen to take is not a ceiling.
//   - The stacktrace it hands grouping is the one it decoded. A frame invented
//     by the decoder is a fingerprint invented by the decoder, and identity is
//     the one thing in this product that must never be a function of anything
//     but the event (ADR 003).
func FuzzDecodeEvent(f *testing.F) {
	f.Add([]byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"2026-08-24T10:00:00Z",` +
		`"level":"error","platform":"python",` +
		`"exception":{"values":[{"type":"ValueError","value":"nope",` +
		`"stacktrace":{"frames":[{"abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}]}}]}}`))
	// A timestamp as a float, which is what several SDKs send.
	f.Add([]byte(`{"timestamp":1756476960.25,"message":"plain string message"}`))
	// A message as an object, tags as a list of pairs, breadcrumbs wrapped.
	f.Add([]byte(`{"message":{"formatted":"a","message":"b %s","params":["c"]},` +
		`"tags":[["a","1"],["b","2"]],"breadcrumbs":{"values":[{"type":"log","message":"x"}]}}`))
	f.Add([]byte(`{"debug_meta":{"images":[{"type":"sourcemap","code_file":"app.js","debug_id":"abc"}]}}`))
	f.Add([]byte(`{"exception":{"values":[]},"level":"not a level","timestamp":"not a time"}`))
	f.Add([]byte(`{"extra":{"password":"hunter2","n":-0.0},"contexts":{"trace":{"trace_id":1}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, payload []byte) {
		event, err := sentry.DecodeEvent(payload)
		if err != nil {
			return
		}

		// Anything that reaches storage has to be text SQLite can hold. The
		// decoder normalises several fields out of arbitrary JSON, and a
		// lone surrogate or an unterminated sequence surviving that would
		// fail the write rather than this decode.
		for name, value := range map[string]string{
			"event_id":    event.EventID,
			"platform":    event.Platform,
			"release":     event.Release,
			"environment": event.Environment,
			"server_name": event.ServerName,
			"title":       event.Title(),
		} {
			if !utf8.ValidString(value) {
				t.Fatalf("%s is not valid UTF-8: %q", name, value)
			}
		}
		for key, value := range event.Tags {
			if !utf8.ValidString(key) || !utf8.ValidString(value) {
				t.Fatalf("tag %q=%q is not valid UTF-8", key, value)
			}
		}

		// A decoded level is either empty or one this product knows. The
		// listing filters and the alert rules switch on it, so an
		// attacker-chosen value reaching them would be a value nothing else
		// in the system has a branch for.
		if event.Level != "" && !event.Level.Valid() {
			t.Fatalf("level %q survived decoding and is not a known level", event.Level)
		}

		// A stored timestamp must be a time, not the zero value and not a
		// year the database sorts before everything else: the listing, the
		// keyset cursor and the retention sweep all compare these as text
		// (ADR 033).
		if event.Timestamp.IsZero() {
			t.Fatal("a decoded event has no timestamp; storable() should have chosen one")
		}

		// The stacktrace grouping sees must be the one that was sent. Frames
		// may be dropped by a ceiling; they may not be invented.
		if trace := event.BestStacktrace(); trace != nil {
			for _, frame := range trace.Frames {
				if !utf8.ValidString(frame.Path()) || !utf8.ValidString(frame.Function) {
					t.Fatalf("frame %+v carries invalid UTF-8", frame)
				}
			}
		}

		// Re-encoding what was decoded is what the detail page and the issue
		// bundle read back, so a payload that decodes and cannot be stored is
		// a 500 on the ingest path rather than a rejected event.
		if _, err := sentry.EncodeEvent(&event); err != nil {
			t.Fatalf("an event decoded but could not be re-encoded: %v", err)
		}
	})
}
