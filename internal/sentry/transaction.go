package sentry

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidTransaction means the payload is not a decodable transaction.
var ErrInvalidTransaction = fmt.Errorf("%w: not a transaction", ErrInvalidEvent)

// MaxSpans bounds how many spans one transaction may carry.
//
// The same ceiling Sentry's own SDKs apply, and it is a bound on this
// process rather than a protocol rule: the payload arrives over the public
// ingest endpoint, so the number of spans is chosen by whoever is talking to
// us. A transaction that exceeds it keeps its first MaxSpans spans and says
// so, because a waterfall missing its tail is still a waterfall while a
// rejected transaction is a hole in the aggregates.
const MaxSpans = 1000

// Transaction is the `transaction` envelope item, as an SDK sends it.
//
// It is a different animal from an Event despite sharing a wire shape. An
// event is a thing that went wrong and is worth keeping whole; a transaction
// is one sample of a distribution, and what matters about it is its duration,
// its name and whether it failed. That difference is why this has its own
// decoder rather than a flag on DecodeEvent: almost every field DecodeEvent
// works hard to normalise — the exception chain, the stacktrace, the
// breadcrumbs — is absent here, and almost everything here is absent there.
type Transaction struct {
	EventID string
	// Name is the `transaction` field: the route, the job, the command. It is
	// the aggregation key, so it is the one field whose cardinality decides
	// whether the whole feature is affordable.
	Name string
	// Start and End are the span's ends. Both are required: a duration
	// derived from one of them and the server's clock would be a measurement
	// of the network.
	Start time.Time
	End   time.Time
	// Trace is the `contexts.trace` object, which carries the identity that
	// ties several transactions into one request.
	Trace TraceContext
	// Spans are the children, in the order the SDK sent them.
	Spans []Span
	// SpansTruncated is true when the payload carried more than MaxSpans.
	SpansTruncated bool

	Platform    string
	Release     string
	Environment string
	ServerName  string
	Tags        map[string]string
	// Contexts is everything else the SDK attached — runtime, os, device.
	// Kept because it is what a waterfall shows beside the timings, and
	// scrubbed by the ingest path like every other user-shaped map.
	Contexts map[string]any
}

// TraceContext is `contexts.trace`.
type TraceContext struct {
	// TraceID is the identity of the whole request. It is what sampling
	// hashes, so that every transaction of one trace shares a fate and a
	// waterfall is never half-stored (ADR 021).
	TraceID string
	// SpanID is this transaction's own span, and ParentSpanID is the span
	// upstream that caused it — the two halves of a distributed waterfall.
	SpanID       string
	ParentSpanID string
	// Op is the kind of work: http.server, queue.task, db.
	Op string
	// Status is the protocol's span status. Empty is normal and is treated as
	// unknown rather than as a failure.
	Status string
}

// Span is one child of a transaction.
type Span struct {
	SpanID       string
	ParentSpanID string
	Op           string
	Description  string
	Status       string
	Start        time.Time
	End          time.Time
	Tags         map[string]string
	// Data is the span's own attributes — the SQL it ran, the URL it called.
	// Arbitrary JSON from an SDK this product does not control, and therefore
	// scrubbed before it is stored.
	Data map[string]any
}

// Duration is how long the transaction took.
func (t *Transaction) Duration() time.Duration { return t.End.Sub(t.Start) }

// wireTransaction is the payload's literal shape.
//
// Tolerant in the same direction the rest of this package is: an unknown field
// is ignored rather than rejected, because an SDK that starts sending
// something new must not break an installation that has not been upgraded
// (ADR 002).
type wireTransaction struct {
	EventID        string          `json:"event_id"`
	Transaction    string          `json:"transaction"`
	StartTimestamp json.RawMessage `json:"start_timestamp"`
	Timestamp      json.RawMessage `json:"timestamp"`
	Platform       string          `json:"platform"`
	Release        string          `json:"release"`
	Environment    string          `json:"environment"`
	ServerName     string          `json:"server_name"`
	Contexts       map[string]any  `json:"contexts"`
	Spans          []wireSpan      `json:"spans"`
	Tags           json.RawMessage `json:"tags"`
}

type wireSpan struct {
	SpanID         string          `json:"span_id"`
	ParentSpanID   string          `json:"parent_span_id"`
	Op             string          `json:"op"`
	Description    string          `json:"description"`
	Status         string          `json:"status"`
	StartTimestamp json.RawMessage `json:"start_timestamp"`
	Timestamp      json.RawMessage `json:"timestamp"`
	Tags           json.RawMessage `json:"tags"`
	Data           map[string]any  `json:"data"`
}

// DecodeTransaction reads a transaction payload.
//
// Three things are required rather than defaulted, and each of them for the
// same reason: without it the record would be a plausible-looking row that
// answers a question wrongly.
//
//   - Both timestamps. A duration computed from one end and the server's
//     clock measures the network and the queue, not the request.
//   - A trace id. Sampling hashes it, so a transaction without one could not
//     share the fate of its siblings and a waterfall would come out with
//     holes — which is the exact failure ADR 021 exists to avoid.
//   - An end at or after the start. A negative duration cannot go into a
//     latency sketch, and a clamped zero would silently pull a p50 down. A
//     clock that ran backwards is worth one refused transaction and a line in
//     the drop log.
func DecodeTransaction(payload []byte) (Transaction, error) {
	var wire wireTransaction
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Transaction{}, fmt.Errorf("%w: %w", ErrInvalidTransaction, err)
	}

	start, hasStart := decodeInstant(wire.StartTimestamp)
	if !hasStart {
		return Transaction{}, fmt.Errorf("%w: start_timestamp is required", ErrInvalidTransaction)
	}
	end, hasEnd := decodeInstant(wire.Timestamp)
	if !hasEnd {
		return Transaction{}, fmt.Errorf("%w: timestamp is required", ErrInvalidTransaction)
	}
	if end.Before(start) {
		return Transaction{}, fmt.Errorf("%w: timestamp %s is before start_timestamp %s",
			ErrInvalidTransaction, end.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
	}

	trace, found := decodeTraceContext(wire.Contexts)
	if !found || trace.TraceID == "" {
		return Transaction{}, fmt.Errorf("%w: contexts.trace.trace_id is required", ErrInvalidTransaction)
	}

	transaction := Transaction{
		EventID:     strings.TrimSpace(wire.EventID),
		Name:        strings.TrimSpace(wire.Transaction),
		Start:       start,
		End:         end,
		Trace:       trace,
		Platform:    strings.TrimSpace(wire.Platform),
		Release:     strings.TrimSpace(wire.Release),
		Environment: strings.TrimSpace(wire.Environment),
		ServerName:  strings.TrimSpace(wire.ServerName),
		Tags:        decodeTags(wire.Tags),
		Contexts:    wire.Contexts,
	}
	transaction.Spans, transaction.SpansTruncated = decodeSpans(wire.Spans)
	return transaction, nil
}

// decodeSpans reads the children, bounded.
func decodeSpans(wire []wireSpan) ([]Span, bool) {
	truncated := false
	if len(wire) > MaxSpans {
		wire, truncated = wire[:MaxSpans], true
	}
	if len(wire) == 0 {
		return nil, truncated
	}

	spans := make([]Span, 0, len(wire))
	for index := range wire {
		raw := &wire[index]
		span := Span{
			SpanID:       strings.TrimSpace(raw.SpanID),
			ParentSpanID: strings.TrimSpace(raw.ParentSpanID),
			Op:           strings.TrimSpace(raw.Op),
			Description:  raw.Description,
			Status:       strings.ToLower(strings.TrimSpace(raw.Status)),
			Tags:         decodeTags(raw.Tags),
			Data:         raw.Data,
		}
		// A span with no usable timings still belongs in the waterfall, drawn
		// as a zero-width mark: dropping it would leave a parent pointing at a
		// child that is not there, which reads as data loss rather than as an
		// SDK that sent an incomplete span.
		span.Start, _ = decodeInstant(raw.StartTimestamp)
		span.End, _ = decodeInstant(raw.Timestamp)
		if span.End.Before(span.Start) {
			span.End = span.Start
		}
		spans = append(spans, span)
	}
	return spans, truncated
}

// decodeTraceContext reads `contexts.trace`.
//
// The contexts map is `map[string]any` because that is how the rest of this
// package already carries it, so this walks it by hand rather than decoding
// the payload twice. Every field is read defensively: a context is arbitrary
// JSON from an SDK, and a type assertion that assumed a string would panic on
// a number somebody sent.
func decodeTraceContext(contexts map[string]any) (TraceContext, bool) {
	raw, found := contexts["trace"].(map[string]any)
	if !found {
		return TraceContext{}, false
	}
	trace := TraceContext{
		TraceID:      contextString(raw, "trace_id"),
		SpanID:       contextString(raw, "span_id"),
		ParentSpanID: contextString(raw, "parent_span_id"),
		Op:           contextString(raw, "op"),
		Status:       strings.ToLower(contextString(raw, "status")),
	}
	return trace, true
}

func contextString(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return strings.TrimSpace(value)
}

// decodeInstant reads a timestamp and says whether there really was one.
//
// It deliberately does not reuse decodeTimestamp. That function falls back to
// the server's clock, which is exactly right for an event — a wrong clock must
// not cost somebody their error report — and exactly wrong here. A start or an
// end invented by the server produces a duration that measures the network,
// the queue and the SDK's flush interval, and it looks identical to a real
// one: it would land in the sketch, move the p95, and nothing downstream could
// ever tell it apart. Saying "there was no timestamp" is the only honest
// answer, and the caller refuses the transaction on it.
func decodeInstant(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}

	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return unixInstant(asNumber)
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, asString); err == nil {
			return parsed.UTC(), true
		}
	}
	// Some SDKs send a unix timestamp as a quoted string.
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(asString), 64); err == nil {
		return unixInstant(seconds)
	}
	return time.Time{}, false
}

// unixInstant turns unix seconds into an instant, refusing the ones this
// product cannot store. The range check is not pedantry: the field is a
// number chosen by whoever is talking to the ingest endpoint, and 1e17 lands
// three billion years out (ADR 033).
func unixInstant(seconds float64) (time.Time, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return time.Time{}, false
	}
	whole, fraction := math.Modf(seconds)
	instant := time.Unix(int64(whole), int64(fraction*float64(time.Second))).UTC()
	if instant.Before(earliestStorable) || instant.After(latestStorable) {
		return time.Time{}, false
	}
	return instant, true
}

// SampleRateFromTrace reads the rate an SDK already applied, from the dynamic
// sampling context an envelope header carries.
//
// This is the `trace` object of the envelope header — the same information the
// `baggage` HTTP header carries, in the place the official SDKs actually put
// it. It matters because sampling composes: an SDK sending one in four and a
// server keeping one in ten together keep one in forty, and a panel that
// showed the server's knob alone would be telling an operator something that
// is off by the SDK's factor (ADR 021).
func SampleRateFromTrace(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var context struct {
		SampleRate json.RawMessage `json:"sample_rate"`
	}
	if err := json.Unmarshal(raw, &context); err != nil {
		return 0, false
	}
	return decodeRate(context.SampleRate)
}

// SampleRateFromBaggage reads `sentry-sample_rate` out of a W3C baggage
// header.
//
// The second place the same fact appears. Some SDKs — and every proxy that
// forwards a trace — put the dynamic sampling context on the header rather
// than in the envelope, so a server that read only one of the two would see
// the SDK's rate for some clients and not for others, which is worse than
// seeing it for none.
func SampleRateFromBaggage(header string) (float64, bool) {
	for _, member := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(member), "=")
		if !found || strings.TrimSpace(key) != "sentry-sample_rate" {
			continue
		}
		// Properties after a semicolon are part of the baggage grammar and
		// are not part of the value.
		value, _, _ = strings.Cut(value, ";")
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || rate <= 0 || rate > 1 {
			return 0, false
		}
		return rate, true
	}
	return 0, false
}

// decodeRate reads a rate sent as a number or as a quoted number, which is
// what the baggage-shaped form of the same context does.
func decodeRate(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return validRate(asNumber)
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(asString), 64)
		if err != nil {
			return 0, false
		}
		return validRate(parsed)
	}
	return 0, false
}

// validRate refuses anything outside (0, 1]. A rate of zero would mean the SDK
// sent a transaction it had decided not to send, and a rate above one is not a
// rate; in both cases the honest answer is "the SDK did not say".
func validRate(rate float64) (float64, bool) {
	if rate <= 0 || rate > 1 {
		return 0, false
	}
	return rate, true
}
