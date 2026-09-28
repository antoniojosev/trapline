package envelope

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, raw string) *Envelope {
	t.Helper()
	parsed, err := Parse(strings.NewReader(raw), Limits{})
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return parsed
}

func TestParseMinimalEnvelope(t *testing.T) {
	envelope := parse(t, `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event","length":13}
{"hello":"x"}
`)

	if envelope.Header.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" {
		t.Errorf("EventID = %q", envelope.Header.EventID)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(envelope.Items))
	}
	if envelope.Items[0].Type != "event" {
		t.Errorf("Type = %q, want event", envelope.Items[0].Type)
	}
	if got := string(envelope.Items[0].Payload); got != `{"hello":"x"}` {
		t.Errorf("Payload = %q", got)
	}
}

func TestParseWithoutDeclaredLength(t *testing.T) {
	// Length is optional; without it the payload runs to the next newline.
	envelope := parse(t, `{"dsn":"https://k@example.com/1"}
{"type":"session"}
{"sid":"abc"}
`)

	if envelope.Header.DSN != "https://k@example.com/1" {
		t.Errorf("DSN = %q", envelope.Header.DSN)
	}
	if len(envelope.Items) != 1 || string(envelope.Items[0].Payload) != `{"sid":"abc"}` {
		t.Errorf("items = %+v", envelope.Items)
	}
}

func TestLengthAllowsNewlinesInsideAPayload(t *testing.T) {
	// This is the entire reason the length field exists: without it, a payload
	// containing a newline would be read as two items.
	body := "line one\nline two"
	raw := "{}\n" +
		`{"type":"attachment","length":17,"filename":"log.txt"}` + "\n" +
		body + "\n"

	envelope := parse(t, raw)
	if len(envelope.Items) != 1 {
		t.Fatalf("got %d items, want 1 — the newline inside the payload split it", len(envelope.Items))
	}
	if got := string(envelope.Items[0].Payload); got != body {
		t.Errorf("Payload = %q, want %q", got, body)
	}
	if envelope.Items[0].Filename != "log.txt" {
		t.Errorf("Filename = %q", envelope.Items[0].Filename)
	}
}

func TestFinalItemNeedsNoTrailingNewline(t *testing.T) {
	// SDKs are not required to emit one, and rejecting an envelope for it
	// would drop real events.
	envelope := parse(t, "{}\n"+`{"type":"event"}`+"\n"+`{"a":1}`)

	if len(envelope.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(envelope.Items))
	}
	if string(envelope.Items[0].Payload) != `{"a":1}` {
		t.Errorf("Payload = %q", envelope.Items[0].Payload)
	}
}

func TestParseMultipleItems(t *testing.T) {
	envelope := parse(t, "{}\n"+
		`{"type":"event","length":7}`+"\n"+`{"a":1}`+"\n"+
		`{"type":"transaction","length":7}`+"\n"+`{"b":2}`+"\n"+
		`{"type":"session"}`+"\n"+`{"c":3}`+"\n")

	if len(envelope.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(envelope.Items))
	}
	for i, want := range []string{"event", "transaction", "session"} {
		if envelope.Items[i].Type != want {
			t.Errorf("item %d type = %q, want %q", i, envelope.Items[i].Type, want)
		}
	}
}

func TestUnknownItemTypesSurviveParsing(t *testing.T) {
	// ADR 002: an SDK that starts sending a new item type must not break an
	// installation that has not been upgraded. The parser hands the type back
	// verbatim and lets the caller decide to skip it.
	envelope := parse(t, "{}\n"+
		`{"type":"event","length":7}`+"\n"+`{"a":1}`+"\n"+
		`{"type":"replay_recording_from_2029","length":7}`+"\n"+`{"b":2}`+"\n")

	if len(envelope.Items) != 2 {
		t.Fatalf("got %d items, want both kept", len(envelope.Items))
	}
	if envelope.Items[1].Type != "replay_recording_from_2029" {
		t.Errorf("the unknown type was rewritten to %q", envelope.Items[1].Type)
	}
}

func TestHeaderKeepsUnknownFields(t *testing.T) {
	// The protocol evolves; a field we do not read today may matter tomorrow.
	envelope := parse(t, `{"event_id":"x","trace":{"trace_id":"abc"}}`+"\n")

	if _, kept := envelope.Header.Extra["trace"]; !kept {
		t.Errorf("unknown header fields were dropped: %v", envelope.Header.Extra)
	}
}

func TestSentAt(t *testing.T) {
	envelope := parse(t, `{"sent_at":"2026-08-24T10:00:00Z"}`+"\n")
	want := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	if !envelope.Header.SentAt.Equal(want) {
		t.Errorf("SentAt = %v, want %v", envelope.Header.SentAt, want)
	}
}

func TestUnparseableSentAtIsIgnored(t *testing.T) {
	// sent_at is advisory. Losing it costs nothing next to losing the error
	// report it came with.
	envelope := parse(t, `{"event_id":"x","sent_at":"hace un rato"}`+"\n"+
		`{"type":"event"}`+"\n"+`{}`+"\n")

	if !envelope.Header.SentAt.IsZero() {
		t.Errorf("SentAt = %v, want the zero time", envelope.Header.SentAt)
	}
	if len(envelope.Items) != 1 {
		t.Error("a bad timestamp cost us the event")
	}
}

func TestEmptyEnvelopeWithNoItems(t *testing.T) {
	envelope := parse(t, "{}\n")
	if len(envelope.Items) != 0 {
		t.Errorf("got %d items, want none", len(envelope.Items))
	}
}

func TestMalformedInput(t *testing.T) {
	cases := map[string]string{
		"empty input":             "",
		"header is not JSON":      "not json\n",
		"header is not an object": "[1,2,3]\n",
		"header is a number":      "42\n",
		"item header not JSON":    "{}\nnot json\n{}\n",
		"item header has no type": "{}\n" + `{"length":2}` + "\n{}\n",
		"item header is an array": "{}\n[1]\n{}\n",
		"length overruns input":   "{}\n" + `{"type":"event","length":9999}` + "\nshort\n",
		"length is wrong":         "{}\n" + `{"type":"event","length":2}` + "\n{\"a\":1}\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(raw), Limits{}); !errors.Is(err, ErrMalformed) {
				t.Errorf("error = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	t.Run("declared length is checked before allocating", func(t *testing.T) {
		// A one-line request must not be able to ask us to reserve gigabytes.
		raw := "{}\n" + `{"type":"event","length":999999999999}` + "\nx\n"
		_, err := Parse(strings.NewReader(raw), Limits{MaxItemBytes: 1024})
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("error = %v, want ErrTooLarge", err)
		}
	})

	t.Run("whole envelope is bounded", func(t *testing.T) {
		var builder strings.Builder
		builder.WriteString("{}\n")
		for range 100 {
			builder.WriteString(`{"type":"event"}` + "\n" + strings.Repeat("x", 500) + "\n")
		}
		_, err := Parse(strings.NewReader(builder.String()), Limits{MaxEnvelopeBytes: 1024})
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("error = %v, want ErrTooLarge", err)
		}
	})

	t.Run("the envelope bound says too large on a length-delimited payload too", func(t *testing.T) {
		// The sibling above uses items with no declared length, so the budget
		// runs out inside readLine — which has always translated the sentinel.
		// Every real SDK, and every decompression bomb, declares a length, so
		// the budget runs out inside io.ReadFull instead; that path used to
		// wrap the unexported sentinel and hand the caller something it could
		// not classify. The ingest handler answered 500 to a bomb, logged it
		// at ERROR, and told the client to retry a request that can never
		// succeed. Same condition, same answer, whichever byte it lands on.
		var builder strings.Builder
		builder.WriteString("{}\n")
		for range 100 {
			builder.WriteString(`{"type":"event","length":500}` + "\n" + strings.Repeat("x", 500) + "\n")
		}
		_, err := Parse(strings.NewReader(builder.String()), Limits{MaxEnvelopeBytes: 1024})
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("error = %v, want ErrTooLarge", err)
		}
	})

	t.Run("item count is bounded", func(t *testing.T) {
		var builder strings.Builder
		builder.WriteString("{}\n")
		for range 20 {
			builder.WriteString(`{"type":"event"}` + "\n{}\n")
		}
		_, err := Parse(strings.NewReader(builder.String()), Limits{MaxItems: 5})
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("error = %v, want ErrTooLarge", err)
		}
	})

	t.Run("a single header line is bounded", func(t *testing.T) {
		raw := "{\"a\":\"" + strings.Repeat("x", 5000) + "\"}\n"
		_, err := Parse(strings.NewReader(raw), Limits{MaxHeaderBytes: 100})
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("error = %v, want ErrTooLarge", err)
		}
	})

	t.Run("zero values fall back to defaults", func(t *testing.T) {
		// A caller must not be able to disable a bound by leaving a field
		// unset, which is the failure mode of "0 means unlimited".
		limits := Limits{}.withDefaults()
		if limits.MaxEnvelopeBytes <= 0 || limits.MaxItems <= 0 ||
			limits.MaxItemBytes <= 0 || limits.MaxHeaderBytes <= 0 {
			t.Errorf("a zero limit survived: %+v", limits)
		}
	})
}
