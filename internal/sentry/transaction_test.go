package sentry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// goSDKTransaction is what the official Go SDK puts on the wire, trimmed to
// the fields this decoder reads.
//
// A literal rather than a builder, because the point of it is to be the shape
// somebody else chose. A payload this package generated for itself would only
// prove that the decoder agrees with its own author (ADR 002).
const goSDKTransaction = `{
  "event_id": "2ee5a2a1e29e4a3ea1ea1de4f7c1b3d5",
  "type": "transaction",
  "transaction": "GET /api/checkout",
  "transaction_info": {"source": "route"},
  "start_timestamp": "2026-08-29T14:32:10.100000Z",
  "timestamp": "2026-08-29T14:32:10.350000Z",
  "platform": "go",
  "release": "billing@4.11.2",
  "environment": "production",
  "server_name": "web-07",
  "tags": {"region": "sa-east-1"},
  "contexts": {
    "trace": {
      "trace_id": "4c79f60c11214eb38604f4ae0781bfb2",
      "span_id": "fa90fdead5f74052",
      "parent_span_id": "aa11bb22cc33dd44",
      "op": "http.server",
      "status": "ok"
    },
    "runtime": {"name": "go", "version": "go1.26.6"}
  },
  "spans": [
    {
      "span_id": "1111111111111111",
      "parent_span_id": "fa90fdead5f74052",
      "trace_id": "4c79f60c11214eb38604f4ae0781bfb2",
      "op": "db.sql",
      "description": "SELECT * FROM orders WHERE id = $1",
      "status": "ok",
      "start_timestamp": 1787063530.12,
      "timestamp": 1787063530.2,
      "data": {"db.system": "postgresql", "password": "hunter2"},
      "tags": {"shard": "b"}
    }
  ]
}`

func TestDecodeTransactionReadsWhatTheGoSDKSends(t *testing.T) {
	decoded, err := DecodeTransaction([]byte(goSDKTransaction))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if decoded.Name != "GET /api/checkout" {
		t.Errorf("name %q", decoded.Name)
	}
	if decoded.EventID != "2ee5a2a1e29e4a3ea1ea1de4f7c1b3d5" {
		t.Errorf("event id %q", decoded.EventID)
	}
	if got := decoded.Duration(); got != 250*time.Millisecond {
		t.Errorf("duration %s, want 250ms", got)
	}
	if decoded.Trace.TraceID != "4c79f60c11214eb38604f4ae0781bfb2" {
		t.Errorf("trace id %q", decoded.Trace.TraceID)
	}
	if decoded.Trace.SpanID != "fa90fdead5f74052" || decoded.Trace.ParentSpanID != "aa11bb22cc33dd44" {
		t.Errorf("span identity %q/%q", decoded.Trace.SpanID, decoded.Trace.ParentSpanID)
	}
	if decoded.Trace.Op != "http.server" || decoded.Trace.Status != "ok" {
		t.Errorf("op %q status %q", decoded.Trace.Op, decoded.Trace.Status)
	}
	if decoded.Release != "billing@4.11.2" || decoded.Environment != "production" {
		t.Errorf("release %q environment %q", decoded.Release, decoded.Environment)
	}
	if decoded.Tags["region"] != "sa-east-1" {
		t.Errorf("tags %v", decoded.Tags)
	}

	if len(decoded.Spans) != 1 {
		t.Fatalf("%d spans, want 1", len(decoded.Spans))
	}
	span := decoded.Spans[0]
	if span.Op != "db.sql" || span.SpanID != "1111111111111111" {
		t.Errorf("span %+v", span)
	}
	// The unix-float form, which the same SDK uses for spans while using a
	// string for the transaction's own ends. Both have to work or half a
	// waterfall comes out with no timings.
	if got := span.End.Sub(span.Start); got < 79*time.Millisecond || got > 81*time.Millisecond {
		t.Errorf("span duration %s, want about 80ms", got)
	}
	if span.Data["db.system"] != "postgresql" {
		t.Errorf("span data %v", span.Data)
	}
}

func TestDecodeTransactionRefusesWhatWouldBeAPlausibleLie(t *testing.T) {
	cases := map[string]string{
		"not JSON at all": `{`,
		"no start": `{"timestamp": 1787063530.35,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
		"no end": `{"start_timestamp": 1787063530.1,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
		"no trace context": `{"start_timestamp": 1787063530.1, "timestamp": 1787063530.35}`,
		"a trace context with no id": `{"start_timestamp": 1787063530.1, "timestamp": 1787063530.35,
			"contexts":{"trace":{"op":"http.server"}}}`,
		"an end before its start": `{"start_timestamp": 1787063530.35, "timestamp": 1787063530.1,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
		"a start that is not a timestamp": `{"start_timestamp": "yesterday", "timestamp": 1787063530.35,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
		"a start three billion years out": `{"start_timestamp": 1e17, "timestamp": 1e17,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
		"a start that is not a number": `{"start_timestamp": {}, "timestamp": 1787063530.35,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
		"a null start": `{"start_timestamp": null, "timestamp": 1787063530.35,
			"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`,
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeTransaction([]byte(payload)); err == nil {
				t.Fatal("accepted")
			} else if !errors.Is(err, ErrInvalidTransaction) || !errors.Is(err, ErrInvalidEvent) {
				t.Errorf("error is %v, which a caller cannot match", err)
			}
		})
	}
}

// TestDecodeTransactionDoesNotInventTimestamps is the difference between this
// decoder and DecodeEvent, stated as a test.
//
// An event with an unusable timestamp falls back to the server's clock,
// because a wrong clock must not cost somebody their error report. A
// transaction cannot: the duration would then measure the network, the queue
// and the SDK's flush interval, it would land in the latency sketch, and it
// would look exactly like a real measurement.
func TestDecodeTransactionDoesNotInventTimestamps(t *testing.T) {
	payload := `{"transaction":"GET /","timestamp":"not a time","start_timestamp":"not a time",
		"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`
	if _, err := DecodeTransaction([]byte(payload)); err == nil {
		t.Fatal("an unusable pair of timestamps was replaced with the server's clock")
	}

	// While an event with the same field still gets the fallback, which is
	// the behaviour that must not have changed.
	event, err := DecodeEvent([]byte(`{"timestamp":"not a time","message":"hello"}`))
	if err != nil {
		t.Fatalf("decoding an event: %v", err)
	}
	if event.Timestamp.IsZero() {
		t.Error("an event lost its fallback timestamp")
	}
}

func TestDecodeTransactionAcceptsWhatIsMerelyIncomplete(t *testing.T) {
	// A transaction with no name, no spans and no status is what a minimal
	// SDK sends, and it is a perfectly good latency sample. Refusing it would
	// put a hole in the p95 over a naming problem.
	payload := `{"start_timestamp": 1787063530.1, "timestamp": 1787063530.35,
		"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}}}`
	decoded, err := DecodeTransaction([]byte(payload))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded.Name != "" {
		t.Errorf("name %q, want empty — naming it is the domain's job", decoded.Name)
	}
	if len(decoded.Spans) != 0 {
		t.Errorf("%d spans from a payload with none", len(decoded.Spans))
	}
	if decoded.Trace.Status != "" {
		t.Errorf("status %q, want empty", decoded.Trace.Status)
	}
}

func TestDecodeTransactionKeepsSpansWithNoTimings(t *testing.T) {
	payload := `{"start_timestamp": 1787063530.1, "timestamp": 1787063530.35,
		"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}},
		"spans":[{"span_id":"1111111111111111","op":"cache.get"},
		         {"span_id":"2222222222222222","op":"db","start_timestamp":1787063530.3,"timestamp":1787063530.2}]}`
	decoded, err := DecodeTransaction([]byte(payload))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(decoded.Spans) != 2 {
		// Dropping them would leave a parent pointing at a child that is not
		// there, which reads as data loss rather than as an incomplete SDK.
		t.Fatalf("%d spans, want 2", len(decoded.Spans))
	}
	for index, span := range decoded.Spans {
		if span.End.Before(span.Start) {
			t.Errorf("span %d ends before it starts", index)
		}
	}
}

func TestDecodeTransactionBoundsTheSpanCount(t *testing.T) {
	spans := make([]string, 0, MaxSpans+10)
	for index := range MaxSpans + 10 {
		spans = append(spans, `{"span_id":"`+strings.Repeat("a", 15)+string(rune('a'+index%26))+`","op":"x"}`)
	}
	payload := `{"start_timestamp": 1787063530.1, "timestamp": 1787063530.35,
		"contexts":{"trace":{"trace_id":"4c79f60c11214eb38604f4ae0781bfb2"}},
		"spans":[` + strings.Join(spans, ",") + `]}`

	decoded, err := DecodeTransaction([]byte(payload))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(decoded.Spans) != MaxSpans {
		t.Errorf("%d spans kept, want the ceiling of %d", len(decoded.Spans), MaxSpans)
	}
	if !decoded.SpansTruncated {
		t.Error("the truncation was not reported, so nothing downstream could say the waterfall is partial")
	}
}

func TestDecodeTransactionIgnoresATraceContextThatIsNotAnObject(t *testing.T) {
	payload := `{"start_timestamp": 1787063530.1, "timestamp": 1787063530.35,
		"contexts":{"trace":"4c79f60c11214eb38604f4ae0781bfb2"}}`
	if _, err := DecodeTransaction([]byte(payload)); err == nil {
		t.Fatal("a trace context that is a string was accepted")
	}
}

func TestSampleRateFromTrace(t *testing.T) {
	cases := map[string]struct {
		raw   string
		want  float64
		found bool
	}{
		"a number":                 {`{"sample_rate":0.25}`, 0.25, true},
		"a quoted number":          {`{"sample_rate":"0.25"}`, 0.25, true},
		"one":                      {`{"sample_rate":1}`, 1, true},
		"absent":                   {`{"trace_id":"abc"}`, 0, false},
		"zero":                     {`{"sample_rate":0}`, 0, false},
		"above one":                {`{"sample_rate":2}`, 0, false},
		"not a number at all":      {`{"sample_rate":"soon"}`, 0, false},
		"not an object":            {`[]`, 0, false},
		"a rate that is an object": {`{"sample_rate":{}}`, 0, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, found := SampleRateFromTrace(json.RawMessage(tc.raw))
			if found != tc.found || got != tc.want {
				t.Errorf("got (%v, %v), want (%v, %v)", got, found, tc.want, tc.found)
			}
		})
	}

	if _, found := SampleRateFromTrace(nil); found {
		t.Error("an absent dynamic sampling context reported a rate")
	}
}

func TestSampleRateFromBaggage(t *testing.T) {
	cases := map[string]struct {
		header string
		want   float64
		found  bool
	}{
		"the shape the SDKs send": {
			"sentry-trace_id=abc,sentry-public_key=xyz,sentry-sample_rate=0.25", 0.25, true},
		"with spaces around the members": {
			"sentry-trace_id=abc, sentry-sample_rate=0.5 , other=1", 0.5, true},
		"with baggage properties": {
			"sentry-sample_rate=0.5;metadata=x", 0.5, true},
		"no sentry members": {"other=1,another=2", 0, false},
		"an empty header":   {"", 0, false},
		"a rate that is not one": {
			"sentry-sample_rate=nope", 0, false},
		"a rate above one": {"sentry-sample_rate=4", 0, false},
		"a member with no value": {
			"sentry-sample_rate", 0, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, found := SampleRateFromBaggage(tc.header)
			if found != tc.found || got != tc.want {
				t.Errorf("got (%v, %v), want (%v, %v)", got, found, tc.want, tc.found)
			}
		})
	}
}

// FuzzDecodeTransaction drives the decoder the way the ingest endpoint does:
// with bytes somebody else chose.
//
// The transaction payload arrives on the same public endpoint the event
// payload does, authenticated by a key that ships inside browser bundles, so
// every byte here is attacker-chosen (ADR 002). The property is the modest
// one that matters: it never panics, and anything it accepts has the two
// timestamps that make a duration meaningful.
func FuzzDecodeTransaction(f *testing.F) {
	f.Add([]byte(goSDKTransaction))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"contexts":{"trace":{"trace_id":"a"}},"start_timestamp":1,"timestamp":2}`))
	f.Add([]byte(`{"spans":[{}]}`))

	f.Fuzz(func(t *testing.T, payload []byte) {
		decoded, err := DecodeTransaction(payload)
		if err != nil {
			return
		}
		if decoded.Start.IsZero() || decoded.End.IsZero() {
			t.Fatalf("accepted a transaction with no ends: %+v", decoded.Trace)
		}
		if decoded.Duration() < 0 {
			t.Fatalf("accepted a negative duration: %s", decoded.Duration())
		}
		if decoded.Trace.TraceID == "" {
			t.Fatal("accepted a transaction with no trace id")
		}
		if len(decoded.Spans) > MaxSpans {
			t.Fatalf("%d spans, above the ceiling of %d", len(decoded.Spans), MaxSpans)
		}
	})
}
