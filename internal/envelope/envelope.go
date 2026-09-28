// Package envelope parses the wire format the official SDKs send.
//
// This is the single most exposed piece of code in the product: the ingest
// endpoint is public by design (SECURITY.md), so everything here reads bytes
// chosen by whoever is talking to us. It is therefore deliberately a pure
// package — standard library only, no storage, no HTTP, no logging — so it can
// be fuzzed in isolation and reasoned about without a server around it.
//
// The format, from the protocol:
//
//	Envelope = Headers { "\n" Item } [ "\n" ]
//	Item     = Headers "\n" Payload
//
// Headers are one JSON object on one line. An item header may carry a
// "length"; when it does, exactly that many bytes are the payload, which is
// what allows a payload to contain newlines. When it does not, the payload
// runs to the next newline or to the end of the input.
package envelope

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Errors this package returns. A caller distinguishes "this is not an
// envelope" from "this envelope is too big" because the two deserve different
// responses on the wire.
var (
	// ErrMalformed means the bytes are not a well-formed envelope.
	ErrMalformed = errors.New("malformed envelope")
	// ErrTooLarge means the input exceeded a configured limit.
	ErrTooLarge = errors.New("envelope too large")
)

// Limits bound what a single request may cost us.
//
// They are arguments rather than constants because the ingest endpoint has to
// be able to say no before reading, and because a fuzz test needs to drive
// small ones. There is no "unlimited": a zero value falls back to the
// defaults, so a caller cannot accidentally disable a bound by leaving a
// field unset.
type Limits struct {
	MaxEnvelopeBytes int64
	MaxItems         int
	MaxItemBytes     int
	MaxHeaderBytes   int
}

// DefaultLimits are the shipped bounds.
//
// The envelope ceiling is generous enough for a large event with a full
// stacktrace and breadcrumbs, and far below anything that would let one
// request matter to a server this size.
func DefaultLimits() Limits {
	return Limits{
		MaxEnvelopeBytes: 20 << 20, // 20 MiB
		MaxItems:         100,
		MaxItemBytes:     4 << 20, // 4 MiB
		MaxHeaderBytes:   64 << 10,
	}
}

func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxEnvelopeBytes <= 0 {
		l.MaxEnvelopeBytes = defaults.MaxEnvelopeBytes
	}
	if l.MaxItems <= 0 {
		l.MaxItems = defaults.MaxItems
	}
	if l.MaxItemBytes <= 0 {
		l.MaxItemBytes = defaults.MaxItemBytes
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = defaults.MaxHeaderBytes
	}
	return l
}

// Header is the envelope's own header line.
//
// Only the fields this product uses are named. Everything else an SDK sends is
// kept in Extra rather than dropped, because the protocol evolves and a field
// we do not know today may be one we want to read tomorrow without a format
// change.
type Header struct {
	EventID string
	DSN     string
	SentAt  time.Time
	Extra   map[string]json.RawMessage
}

// Item is one entry in an envelope.
type Item struct {
	// Type is the item type verbatim, including types this build does not
	// know. Deciding what to do with an unknown type is the caller's job, not
	// the parser's (ADR 002).
	Type        string
	ContentType string
	// Filename is set on attachment items.
	Filename string
	Payload  []byte
	Extra    map[string]json.RawMessage
}

// Envelope is a parsed envelope.
type Envelope struct {
	Header Header
	Items  []Item
}

// Parse reads one envelope.
//
// It is tolerant in exactly one direction: an item whose type this build does
// not recognise is returned as-is for the caller to skip, never a reason to
// reject the whole envelope. An SDK that starts sending a new item type must
// not break an installation that has not been upgraded (ADR 002). It is strict
// about everything structural, because a parser that guesses at malformed
// input is a parser with undefined behaviour on hostile input.
func Parse(r io.Reader, limits Limits) (*Envelope, error) {
	limits = limits.withDefaults()

	// The hard ceiling is enforced by the reader itself, so no code path
	// downstream can allocate past it however it is reached.
	counted := &limitedReader{r: r, remaining: limits.MaxEnvelopeBytes}
	buffered := bufio.NewReaderSize(counted, 64<<10)

	headerLine, err := readLine(buffered, limits.MaxHeaderBytes)
	if errors.Is(err, io.EOF) {
		// An empty body is not "an envelope that ended early", it is not an
		// envelope: every envelope begins with a header line.
		return nil, fmt.Errorf("%w: empty body", ErrMalformed)
	}
	if err != nil {
		return nil, err
	}
	header, err := parseHeader(headerLine)
	if err != nil {
		return nil, err
	}

	envelope := &Envelope{Header: header}

	for {
		itemHeaderLine, err := readLine(buffered, limits.MaxHeaderBytes)
		if errors.Is(err, io.EOF) {
			// A trailing newline after the last item is allowed, so running
			// out of input between items is the normal end.
			return envelope, nil
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(itemHeaderLine)) == "" {
			// Blank line before EOF: also a normal end.
			continue
		}

		if len(envelope.Items) >= limits.MaxItems {
			return nil, fmt.Errorf("%w: more than %d items", ErrTooLarge, limits.MaxItems)
		}

		item, err := readItem(buffered, itemHeaderLine, limits)
		if err != nil {
			return nil, err
		}
		envelope.Items = append(envelope.Items, item)
	}
}

// itemHeader is the subset of an item header the parser needs to find the
// payload. Everything else is kept as raw JSON.
type itemHeader struct {
	Type        string `json:"type"`
	Length      *int64 `json:"length"`
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
}

func readItem(r *bufio.Reader, headerLine []byte, limits Limits) (Item, error) {
	var parsed itemHeader
	if err := json.Unmarshal(headerLine, &parsed); err != nil {
		return Item{}, fmt.Errorf("%w: item header is not JSON: %w", ErrMalformed, err)
	}
	if parsed.Type == "" {
		return Item{}, fmt.Errorf("%w: item header has no type", ErrMalformed)
	}

	extra := map[string]json.RawMessage{}
	if err := json.Unmarshal(headerLine, &extra); err != nil {
		return Item{}, fmt.Errorf("%w: item header is not an object: %w", ErrMalformed, err)
	}

	payload, err := readPayload(r, parsed.Length, limits.MaxItemBytes)
	if err != nil {
		return Item{}, err
	}

	return Item{
		Type:        parsed.Type,
		ContentType: parsed.ContentType,
		Filename:    parsed.Filename,
		Payload:     payload,
		Extra:       extra,
	}, nil
}

// readPayload reads an item body, either by declared length or up to the next
// newline.
//
// The declared length is treated as a claim, not a fact: it is checked against
// the item limit before a single byte is allocated. Trusting it would let a
// one-line request ask us to reserve gigabytes, which is the cheapest possible
// denial of service against an endpoint that cannot require authentication
// beyond a public key.
func readPayload(r *bufio.Reader, declared *int64, maxItemBytes int) ([]byte, error) {
	if declared == nil {
		line, err := readLine(r, maxItemBytes)
		if errors.Is(err, io.EOF) {
			// The final item may end without a trailing newline.
			return line, nil
		}
		if err != nil {
			return nil, err
		}
		return line, nil
	}

	// Decoded as a signed integer so a negative length is rejected with a
	// message about the length, rather than surfacing as an opaque JSON
	// decoding failure that says nothing about what was wrong.
	if *declared < 0 {
		return nil, fmt.Errorf("%w: item declares a negative length (%d)", ErrMalformed, *declared)
	}
	if *declared > int64(maxItemBytes) {
		return nil, fmt.Errorf("%w: item declares %d bytes, limit is %d",
			ErrTooLarge, *declared, maxItemBytes)
	}

	payload := make([]byte, *declared)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: item declares %d bytes but the input ended early",
				ErrMalformed, *declared)
		}
		// Running out of envelope budget inside a payload is the same event as
		// running out of it inside a header line, and readLine has always
		// translated it. Here it used to fall through to the generic wrap, so
		// the sentinel reached the caller unexported and unclassifiable: the
		// ingest handler answered 500 to a decompression bomb instead of 413,
		// logged it at ERROR, and told every SDK the failure was the server's
		// and worth retrying forever. Same condition, same answer, whichever
		// byte of the stream it happens on.
		if errors.Is(err, errEnvelopeLimit) {
			return nil, fmt.Errorf("%w: envelope exceeds its size limit", ErrTooLarge)
		}
		return nil, fmt.Errorf("reading item payload: %w", err)
	}

	// A single newline may follow a length-delimited payload. Anything else
	// means the declared length was wrong and the stream is now out of step,
	// so continuing would parse garbage as the next item header.
	next, err := r.ReadByte()
	switch {
	case errors.Is(err, io.EOF):
	case errors.Is(err, errEnvelopeLimit):
		return nil, fmt.Errorf("%w: envelope exceeds its size limit", ErrTooLarge)
	case err != nil:
		return nil, fmt.Errorf("reading item separator: %w", err)
	case next != '\n':
		return nil, fmt.Errorf("%w: expected a newline after a length-delimited payload", ErrMalformed)
	}
	return payload, nil
}

func parseHeader(line []byte) (Header, error) {
	var raw struct {
		EventID string `json:"event_id"`
		DSN     string `json:"dsn"`
		SentAt  string `json:"sent_at"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return Header{}, fmt.Errorf("%w: envelope header is not JSON: %w", ErrMalformed, err)
	}

	extra := map[string]json.RawMessage{}
	if err := json.Unmarshal(line, &extra); err != nil {
		return Header{}, fmt.Errorf("%w: envelope header is not an object: %w", ErrMalformed, err)
	}

	header := Header{EventID: raw.EventID, DSN: raw.DSN, Extra: extra}
	if raw.SentAt != "" {
		// An unparseable timestamp is not worth rejecting an envelope over:
		// sent_at is advisory, used for clock-skew correction, and losing it
		// costs nothing next to losing the error report it accompanies.
		if sentAt, err := time.Parse(time.RFC3339, raw.SentAt); err == nil {
			header.SentAt = sentAt.UTC()
		}
	}
	return header, nil
}

// readLine reads up to and including a newline, returning the line without it.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			if errors.Is(err, errEnvelopeLimit) {
				return nil, fmt.Errorf("%w: envelope exceeds its size limit", ErrTooLarge)
			}
			return nil, fmt.Errorf("reading line: %w", err)
		}
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, fmt.Errorf("%w: line exceeds %d bytes", ErrTooLarge, limit)
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// errEnvelopeLimit is returned by limitedReader when the whole-envelope budget
// runs out. It is distinct from io.EOF so the caller can tell "the client
// stopped sending" from "the client sent too much" — the first is a malformed
// envelope, the second is an oversized one, and they get different answers.
var errEnvelopeLimit = errors.New("envelope limit reached")

type limitedReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errEnvelopeLimit
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if err != nil {
		return n, err //nolint:wrapcheck // io.Reader contract: pass through untouched.
	}
	return n, nil
}
