package sentry

import (
	"encoding/json"
	"fmt"
	"time"
)

// ErrInvalidSession means the payload is not a decodable session item.
var ErrInvalidSession = fmt.Errorf("%w: not a session", ErrInvalidEvent)

// MaxSessionAggregates bounds how many buckets one `sessions` item may carry.
//
// The payload arrives on the public ingest endpoint, so its length is chosen
// by whoever is talking to us, and each bucket becomes an upsert. A thousand
// is far past what an SDK produces — its flusher batches at most a minute of
// buckets — and small enough that the worst one request can cost is bounded.
// Extra buckets are dropped rather than the item refused: a truncated
// aggregate still counts most of what was sent, and a rejected one counts
// none of it.
const MaxSessionAggregates = 1000

// Session is the `session` envelope item: one update about one run of
// somebody's application.
//
// Deliberately not "a session". An SDK does not send finished sessions, it
// sends updates of one — an `init`, then heartbeats, then an ending — and the
// whole design of this subsystem follows from that (ADR 008).
type Session struct {
	// SID is the session identity. Required: without it there is nothing to
	// tie two updates together, and counting each update as a session would
	// multiply every release's numbers by however often its SDK flushes.
	SID string
	// Init marks the first update of a session. Read but not relied upon: an
	// SDK that never sets it still produces correct counts, because identity
	// comes from SID.
	Init bool
	// Started is when the session began, and it decides which hour the
	// session is counted in — not when the update arrived.
	Started time.Time
	// Timestamp is when this update was produced. Used only as a fallback for
	// Started, for the SDKs that omit it on later updates.
	Timestamp time.Time
	// Status is the protocol's session status, unnormalised.
	Status string
	// Errors is how many errors the session has seen so far. Cumulative, not
	// a delta: an SDK resends the running total on every update.
	Errors int64
	// Release and Environment come from `attrs`, which is where the protocol
	// puts them.
	Release     string
	Environment string
}

// SessionAggregates is the `sessions` envelope item: counts an SDK added up
// itself before sending.
//
// Several SDKs — the Python one in its request mode, most notably — never send
// individual sessions at all: a server handling a thousand requests a second
// would spend more bandwidth reporting its health than doing its work, so the
// SDK aggregates locally and sends buckets. Accepting this item is what makes
// release health work for those, and it needs no window at all: an aggregate
// carries no session ids, so there is nothing to deduplicate and no ending to
// wait for.
type SessionAggregates struct {
	Release     string
	Environment string
	Buckets     []SessionAggregate
	// Truncated is true when the payload carried more than
	// MaxSessionAggregates buckets.
	Truncated bool
}

// SessionAggregate is one bucket of a `sessions` item.
//
// The four counters are disjoint on the wire, and stay disjoint here: a
// session is counted once, under the worst thing that happened to it. The
// total that started is their sum, which is why there is no `started` field to
// read.
type SessionAggregate struct {
	Started  time.Time
	Exited   int64
	Errored  int64
	Crashed  int64
	Abnormal int64
}

// Total is how many sessions this bucket accounts for.
func (a SessionAggregate) Total() int64 {
	return a.Exited + a.Errored + a.Crashed + a.Abnormal
}

// sessionAttrs is the `attrs` object both item types carry.
type sessionAttrs struct {
	Release     string `json:"release"`
	Environment string `json:"environment"`
}

type wireSession struct {
	SID       string          `json:"sid"`
	Init      bool            `json:"init"`
	Started   json.RawMessage `json:"started"`
	Timestamp json.RawMessage `json:"timestamp"`
	Status    string          `json:"status"`
	Errors    float64         `json:"errors"`
	Attrs     sessionAttrs    `json:"attrs"`
	// Some SDKs put these at the top level as well as, or instead of, in
	// attrs. Read both rather than guessing which SDK is talking: a session
	// whose release could not be found is a session this product refuses, so
	// the cost of looking in one place too few is a release with no numbers.
	Release     string `json:"release"`
	Environment string `json:"environment"`
}

// DecodeSession reads one `session` item.
func DecodeSession(payload []byte) (Session, error) {
	var wire wireSession
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Session{}, fmt.Errorf("%w: %w", ErrInvalidSession, err)
	}
	if wire.SID == "" {
		return Session{}, fmt.Errorf("%w: sid is required", ErrInvalidSession)
	}

	session := Session{
		SID:         wire.SID,
		Init:        wire.Init,
		Status:      wire.Status,
		Release:     firstNonBlank(wire.Attrs.Release, wire.Release),
		Environment: firstNonBlank(wire.Attrs.Environment, wire.Environment),
	}
	session.Started, _ = decodeInstant(wire.Started)
	session.Timestamp, _ = decodeInstant(wire.Timestamp)
	if session.Started.IsZero() {
		// A later update may carry only the update's own time. Falling back to
		// it puts the session in the hour it was last heard from rather than
		// the hour it began, which is wrong by at most the session's length
		// and is far better than dropping it.
		session.Started = session.Timestamp
	}
	session.Errors = countFrom(wire.Errors)
	return session, nil
}

type wireSessionAggregates struct {
	Attrs      sessionAttrs `json:"attrs"`
	Aggregates []struct {
		Started  json.RawMessage `json:"started"`
		Exited   float64         `json:"exited"`
		Errored  float64         `json:"errored"`
		Crashed  float64         `json:"crashed"`
		Abnormal float64         `json:"abnormal"`
	} `json:"aggregates"`
	Release     string `json:"release"`
	Environment string `json:"environment"`
}

// DecodeSessionAggregates reads one `sessions` item.
//
// Buckets with no start time are skipped rather than counted against the hour
// the request happened to arrive in. An aggregate exists to say *when* those
// sessions ran, and attributing them to now would put yesterday's crashes on
// today's release — the one mistake that would make this number actively
// misleading rather than merely absent.
func DecodeSessionAggregates(payload []byte) (SessionAggregates, error) {
	var wire wireSessionAggregates
	if err := json.Unmarshal(payload, &wire); err != nil {
		return SessionAggregates{}, fmt.Errorf("%w: %w", ErrInvalidSession, err)
	}

	aggregates := SessionAggregates{
		Release:     firstNonBlank(wire.Attrs.Release, wire.Release),
		Environment: firstNonBlank(wire.Attrs.Environment, wire.Environment),
	}
	if len(wire.Aggregates) > MaxSessionAggregates {
		wire.Aggregates = wire.Aggregates[:MaxSessionAggregates]
		aggregates.Truncated = true
	}

	aggregates.Buckets = make([]SessionAggregate, 0, len(wire.Aggregates))
	for _, raw := range wire.Aggregates {
		started, found := decodeInstant(raw.Started)
		if !found {
			continue
		}
		bucket := SessionAggregate{
			Started:  started,
			Exited:   countFrom(raw.Exited),
			Errored:  countFrom(raw.Errored),
			Crashed:  countFrom(raw.Crashed),
			Abnormal: countFrom(raw.Abnormal),
		}
		if bucket.Total() == 0 {
			continue
		}
		aggregates.Buckets = append(aggregates.Buckets, bucket)
	}
	return aggregates, nil
}

// countFrom turns a JSON number into a counter.
//
// Read as a float and floored, because JSON has one number type and an SDK is
// free to send `1.0`. Negative counts are zero: they are not a thing that can
// happen, and letting one through would let a payload subtract sessions that
// really were reported.
func countFrom(value float64) int64 {
	if !(value > 0) {
		// Written as a negated comparison so NaN, which is neither greater nor
		// less, lands here rather than being converted to an int64 the
		// specification calls undefined.
		return 0
	}
	return int64(value)
}

// firstNonBlank returns the first value that is not empty.
func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
