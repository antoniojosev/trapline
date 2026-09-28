// Package sentry decodes the event payload the official SDKs send.
//
// Like the envelope parser, this is a pure package that reads attacker-chosen
// bytes, and for the same reason it depends on nothing but the standard
// library.
//
// Most of the work here is absorbing polymorphism the protocol really has.
// Several fields arrive in more than one shape depending on the SDK and its
// version — a timestamp is a float or a string, tags are an object or a list
// of pairs, a message is a string or an object, breadcrumbs are a list or an
// object wrapping a list. A decoder that assumes one shape does not fail
// loudly; it silently drops the field for half the SDKs in the matrix, and
// nobody notices until an issue detail page is mysteriously empty.
package sentry

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidEvent means the payload is not a decodable event.
var ErrInvalidEvent = errors.New("invalid event payload")

// Level is an event's severity.
type Level string

// The levels the protocol defines.
const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
	LevelDebug   Level = "debug"
)

// Valid reports whether l is a known level.
func (l Level) Valid() bool {
	switch l {
	case LevelFatal, LevelError, LevelWarning, LevelInfo, LevelDebug:
		return true
	default:
		return false
	}
}

// Frame is one stacktrace entry as the SDK sent it.
//
// Line and column are kept here even though grouping ignores them: they are
// what a human reads on the issue page, and what source map resolution needs
// later. What must not happen is them leaking into identity (ADR 003).
type Frame struct {
	Filename    string   `json:"filename"`
	AbsPath     string   `json:"abs_path"`
	Function    string   `json:"function"`
	Module      string   `json:"module"`
	Lineno      int      `json:"lineno"`
	Colno       int      `json:"colno"`
	InApp       bool     `json:"in_app"`
	ContextLine string   `json:"context_line"`
	PreContext  []string `json:"pre_context"`
	PostContext []string `json:"post_context"`
	// Raw is the frame as the SDK sent it, kept when symbolication rewrote
	// this one. It is never set by an SDK; this product writes it.
	//
	// Kept because the two answer different questions. The symbolicated frame
	// is where the bug is; the raw frame is what the browser actually ran, and
	// it is the only thing that can be compared against a deployed bundle when
	// somebody suspects the map is wrong or stale. Discarding it would make a
	// bad upload indistinguishable from a bad line number (ADR 018).
	Raw *RawFrame `json:"raw,omitempty"`
}

// RawFrame is the minified location a frame had before symbolication.
//
// A separate, smaller type rather than a nested Frame: it holds only what was
// overwritten, it can never itself be symbolicated, and a self-referential
// Frame would let a malformed payload nest raw frames until the decoder ran
// out of stack.
type RawFrame struct {
	Filename string `json:"filename,omitempty"`
	AbsPath  string `json:"abs_path,omitempty"`
	Function string `json:"function,omitempty"`
	Module   string `json:"module,omitempty"`
	Lineno   int    `json:"lineno,omitempty"`
	Colno    int    `json:"colno,omitempty"`
}

// Path returns the best available file path for a frame.
func (f Frame) Path() string {
	if f.AbsPath != "" {
		return f.AbsPath
	}
	return f.Filename
}

// Stacktrace is a list of frames.
//
// Frames arrive oldest first: the call that actually failed is the LAST one.
// Getting this backwards puts the least interesting frame at the top of the
// issue page and makes the product look like it does not understand its own
// data.
type Stacktrace struct {
	Frames []Frame `json:"frames"`
}

// Exception is one exception in a chain.
type Exception struct {
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	Module     string      `json:"module"`
	Stacktrace *Stacktrace `json:"stacktrace"`
}

// Breadcrumb is one recorded step before the error.
type Breadcrumb struct {
	Timestamp time.Time
	Type      string
	Category  string
	Level     string
	Message   string
	Data      map[string]any
}

// Event is a decoded error event.
type Event struct {
	EventID   string
	Timestamp time.Time
	Platform  string
	Level     Level
	Logger    string
	Release   string
	// Dist separates two builds of one release — a debug build and a release
	// build, an iOS bundle and an Android one. It is half the key of the legacy
	// source map lookup, which is why a decoder that never needed it before
	// needs it now (ADR 018).
	Dist        string
	Environment string
	ServerName  string
	Transaction string
	// Message is what a human reads: the interpolated text.
	Message string
	// MessageTemplate is the uninterpolated form, when the SDK sent one.
	//
	// The two are kept apart because they answer different questions. A log
	// line reported as "could not charge user %s" with params ["alice"]
	// arrives as both; the interpolated text is what belongs on an issue
	// page, and the template is what makes every occurrence of that log line
	// ONE issue instead of one per user. Preferring the interpolated form for
	// both — which is what this did until the Python SDK proved otherwise —
	// splits a single log statement into an issue per parameter value.
	MessageTemplate string
	Fingerprint     []string
	Exceptions      []Exception
	Stacktrace      *Stacktrace
	Tags            map[string]string
	User            map[string]any
	Request         map[string]any
	Contexts        map[string]any
	Extra           map[string]any
	Breadcrumbs     []Breadcrumb
	// DebugMeta is where a JavaScript SDK says which bundle a frame came from,
	// by debug id. It is the modern half of source map resolution and the one
	// that works without a release (ADR 018, from the recording).
	DebugMeta *DebugMeta
}

// DebugMeta carries the debug images an event refers to.
type DebugMeta struct {
	Images []DebugImage `json:"images,omitempty"`
}

// DebugImage ties a generated file to the artifact that explains it.
//
// The images are at event level, not per frame: the join is `frame.abs_path`
// (or `filename`) against `code_file`, and from there to `debug_id`. An event
// routinely carries images for some of its scripts and not others — the page's
// own inline script was never injected — and that is the normal case, not a
// malformed payload.
type DebugImage struct {
	// Type is `sourcemap` for the images this product reads. Others exist for
	// native platforms and are ignored.
	Type     string `json:"type,omitempty"`
	CodeFile string `json:"code_file,omitempty"`
	DebugID  string `json:"debug_id,omitempty"`
}

// PrimaryException is the exception that identifies the event: the last in the
// chain, which is the one actually raised. Earlier entries are the causes it
// wrapped.
//
// Pointer receiver: an Event is a large struct and these run once per ingested
// event, which is the product's hot loop. Copying 288 bytes per call there is
// measurable waste, unlike in the domain where values are passed by value on
// purpose.
func (e *Event) PrimaryException() (Exception, bool) {
	if len(e.Exceptions) == 0 {
		return Exception{}, false
	}
	return e.Exceptions[len(e.Exceptions)-1], true
}

// BestStacktrace returns the stacktrace to group and display on, preferring
// the primary exception's over a top-level one.
func (e *Event) BestStacktrace() *Stacktrace {
	if exception, found := e.PrimaryException(); found && exception.Stacktrace != nil {
		return exception.Stacktrace
	}
	return e.Stacktrace
}

// Title is the one line shown in an issue list.
func (e *Event) Title() string {
	if exception, found := e.PrimaryException(); found && exception.Type != "" {
		if exception.Value == "" {
			return exception.Type
		}
		return exception.Type + ": " + truncate(exception.Value, 200)
	}
	if e.Message != "" {
		return truncate(e.Message, 200)
	}
	return "<unlabelled event>"
}

// rawEvent mirrors the wire shape, with the polymorphic fields left as raw
// JSON so each can be decoded by its own tolerant reader.
type rawEvent struct {
	EventID     string          `json:"event_id"`
	Timestamp   json.RawMessage `json:"timestamp"`
	Platform    string          `json:"platform"`
	Level       string          `json:"level"`
	Logger      string          `json:"logger"`
	Release     string          `json:"release"`
	Dist        string          `json:"dist"`
	Environment string          `json:"environment"`
	ServerName  string          `json:"server_name"`
	Transaction string          `json:"transaction"`
	Message     json.RawMessage `json:"message"`
	LogEntry    json.RawMessage `json:"logentry"`
	Fingerprint []string        `json:"fingerprint"`
	Exception   json.RawMessage `json:"exception"`
	Stacktrace  *Stacktrace     `json:"stacktrace"`
	Tags        json.RawMessage `json:"tags"`
	User        map[string]any  `json:"user"`
	Request     map[string]any  `json:"request"`
	Contexts    map[string]any  `json:"contexts"`
	Extra       map[string]any  `json:"extra"`
	Breadcrumbs json.RawMessage `json:"breadcrumbs"`
	DebugMeta   json.RawMessage `json:"debug_meta"`
}

// DecodeEvent reads an event payload.
//
// Tolerant by design: a field in a shape this build does not understand is
// dropped, never a reason to reject the event. Losing a tag is a cosmetic
// problem; losing the error report is the product failing at its one job
// (ADR 002).
func DecodeEvent(payload []byte) (Event, error) {
	var raw rawEvent
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}

	event := Event{
		EventID:     raw.EventID,
		Platform:    raw.Platform,
		Logger:      raw.Logger,
		Release:     strings.TrimSpace(raw.Release),
		Dist:        strings.TrimSpace(raw.Dist),
		Environment: strings.TrimSpace(raw.Environment),
		ServerName:  raw.ServerName,
		Transaction: raw.Transaction,
		Fingerprint: raw.Fingerprint,
		Stacktrace:  raw.Stacktrace,
		User:        raw.User,
		Request:     raw.Request,
		Contexts:    raw.Contexts,
		Extra:       raw.Extra,
		Timestamp:   decodeTimestamp(raw.Timestamp),
		Tags:        decodeTags(raw.Tags),
		Exceptions:  decodeExceptions(raw.Exception),
		Breadcrumbs: decodeBreadcrumbs(raw.Breadcrumbs),
		DebugMeta:   decodeDebugMeta(raw.DebugMeta),
	}

	event.Level = Level(strings.ToLower(raw.Level))
	if !event.Level.Valid() {
		// An unknown or missing level defaults to error rather than being
		// dropped: this is an error tracker, and an event with no level is far
		// more likely to be an error than an unclassifiable one.
		event.Level = LevelError
	}

	event.Message, event.MessageTemplate = decodeMessage(raw.Message)
	if event.Message == "" {
		event.Message, event.MessageTemplate = decodeMessage(raw.LogEntry)
	}

	return event, nil
}

// decodeMessage reads the several shapes a message arrives in: a bare string,
// or an object with "formatted" and/or "message".
//
// It returns both forms. The interpolated one is for display; the template, if
// there is one, is the stable identity of the log statement that produced it.
func decodeMessage(raw json.RawMessage) (formatted, template string) {
	if len(raw) == 0 {
		return "", ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString), ""
	}
	var asObject struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		formatted = strings.TrimSpace(asObject.Formatted)
		template = strings.TrimSpace(asObject.Message)
		if formatted == "" {
			// Only a template was sent, so it is also the best thing to show.
			return template, template
		}
		if template == formatted {
			// Nothing was interpolated; there is no separate identity.
			return formatted, ""
		}
		return formatted, template
	}
	return "", ""
}

// decodeTimestamp reads a timestamp as either a unix number or an RFC 3339
// string, and falls back to now.
//
// Falling back rather than rejecting is deliberate: a clock that is wrong or a
// serialiser that is unusual should not cost someone their error report. The
// server's own time is a worse timestamp than the client's, and an infinitely
// better one than none.
//
// The number form needs a range check the string form does not: RFC 3339
// parsing already refuses anything but a four-digit year, while a unix
// timestamp is attacker-chosen arithmetic and `1e17` lands three billion years
// out. Storage clamps such a value rather than let it break the ordering of
// every other row (ADR 033), but a clamped year 9999 pinned to the top of the
// issue list forever is not a good outcome either — so a timestamp outside the
// range this product can store is treated like any other unusable one, and the
// server's own clock answers instead.
func decodeTimestamp(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Now().UTC()
	}

	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		if math.IsNaN(asNumber) || math.IsInf(asNumber, 0) || asNumber <= 0 {
			return time.Now().UTC()
		}
		seconds, fraction := math.Modf(asNumber)
		return storable(time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC())
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, asString); err == nil {
				return parsed.UTC()
			}
		}
		// Some SDKs send a unix timestamp as a quoted string.
		if seconds, err := strconv.ParseFloat(asString, 64); err == nil && seconds > 0 {
			return storable(time.Unix(int64(seconds), 0).UTC())
		}
	}
	return time.Now().UTC()
}

// The window a stored timestamp can occupy: four digits of year, which is what
// the storage layout is built on and what RFC 3339 allows.
var (
	earliestStorable = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latestStorable   = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
)

// storable returns t, or now if t is a year this product cannot store.
func storable(t time.Time) time.Time {
	if t.Before(earliestStorable) || t.After(latestStorable) {
		return time.Now().UTC()
	}
	return t
}

// decodeDebugMeta reads the debug images, tolerating the shapes that are not
// the documented one.
//
// Tolerant like every other reader here: a `debug_meta` this decoder cannot
// read costs symbolication, not the event. An SDK that starts sending a new
// image type must not turn a report into a dropped item (ADR 002).
func decodeDebugMeta(raw json.RawMessage) *DebugMeta {
	if len(raw) == 0 {
		return nil
	}
	var decoded DebugMeta
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	if len(decoded.Images) == 0 {
		return nil
	}
	return &decoded
}

// decodeTags reads tags as an object or as a list of [key, value] pairs.
func decodeTags(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}

	// Values are decoded loosely because SDKs send numbers and booleans as tag
	// values even though the protocol calls for strings.
	var asObject map[string]any
	if err := json.Unmarshal(raw, &asObject); err == nil {
		tags := make(map[string]string, len(asObject))
		for key, value := range asObject {
			if rendered := renderScalar(value); rendered != "" {
				tags[key] = rendered
			}
		}
		return tags
	}

	var asPairs [][]any
	if err := json.Unmarshal(raw, &asPairs); err == nil {
		tags := make(map[string]string, len(asPairs))
		for _, pair := range asPairs {
			if len(pair) != 2 {
				continue
			}
			key := renderScalar(pair[0])
			if key == "" {
				continue
			}
			tags[key] = renderScalar(pair[1])
		}
		return tags
	}
	return nil
}

// decodeExceptions reads the exception field as {"values":[...]} or as a bare
// list.
func decodeExceptions(raw json.RawMessage) []Exception {
	if len(raw) == 0 {
		return nil
	}
	var wrapped struct {
		Values []Exception `json:"values"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Values != nil {
		return wrapped.Values
	}
	var bare []Exception
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare
	}
	return nil
}

// decodeBreadcrumbs reads breadcrumbs as {"values":[...]} or as a bare list.
func decodeBreadcrumbs(raw json.RawMessage) []Breadcrumb {
	if len(raw) == 0 {
		return nil
	}

	type rawCrumb struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Type      string          `json:"type"`
		Category  string          `json:"category"`
		Level     string          `json:"level"`
		Message   string          `json:"message"`
		Data      map[string]any  `json:"data"`
	}

	var crumbs []rawCrumb
	var wrapped struct {
		Values []rawCrumb `json:"values"`
	}
	switch {
	case json.Unmarshal(raw, &wrapped) == nil && wrapped.Values != nil:
		crumbs = wrapped.Values
	case json.Unmarshal(raw, &crumbs) == nil:
	default:
		return nil
	}

	decoded := make([]Breadcrumb, 0, len(crumbs))
	for _, crumb := range crumbs {
		decoded = append(decoded, Breadcrumb{
			Timestamp: decodeTimestamp(crumb.Timestamp),
			Type:      crumb.Type,
			Category:  crumb.Category,
			Level:     crumb.Level,
			Message:   crumb.Message,
			Data:      crumb.Data,
		})
	}
	return decoded
}

// renderScalar turns a loosely typed JSON value into a string, refusing
// anything that is not a scalar.
func renderScalar(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e15 {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		// Objects and arrays are not tag values. Rendering them as JSON would
		// produce tags nobody can filter on and blobs in an index.
		return ""
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// EncodeEvent re-serialises an event after scrubbing.
//
// The output is not byte-identical to what arrived, and does not need to be:
// what is stored is the event as the product understands it, after secrets
// have been removed. Keeping the original bytes would defeat scrubbing
// entirely, which is the one thing that must not happen here.
func EncodeEvent(event *Event) ([]byte, error) {
	encoded, err := json.Marshal(storedEvent{
		EventID:     event.EventID,
		Timestamp:   event.Timestamp,
		Platform:    event.Platform,
		Level:       string(event.Level),
		Logger:      event.Logger,
		Release:     event.Release,
		Dist:        event.Dist,
		Environment: event.Environment,
		ServerName:  event.ServerName,
		Transaction: event.Transaction,
		Message:     event.Message,
		Template:    event.MessageTemplate,
		Fingerprint: event.Fingerprint,
		Exception:   exceptionEnvelope{Values: event.Exceptions},
		Stacktrace:  event.Stacktrace,
		Tags:        event.Tags,
		User:        event.User,
		Request:     event.Request,
		Contexts:    event.Contexts,
		Extra:       event.Extra,
		Breadcrumbs: encodeBreadcrumbs(event.Breadcrumbs),
		DebugMeta:   event.DebugMeta,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding event: %w", err)
	}
	return encoded, nil
}

// storedEvent is the canonical shape written to storage.
//
// One shape rather than whatever the SDK happened to send: the reader of a
// stored payload is this product's own detail view and issue bundle, and they
// should not have to re-absorb the polymorphism the decoder already resolved.
type storedEvent struct {
	EventID     string             `json:"event_id,omitempty"`
	Timestamp   time.Time          `json:"timestamp"`
	Platform    string             `json:"platform,omitempty"`
	Level       string             `json:"level,omitempty"`
	Logger      string             `json:"logger,omitempty"`
	Release     string             `json:"release,omitempty"`
	Dist        string             `json:"dist,omitempty"`
	Environment string             `json:"environment,omitempty"`
	ServerName  string             `json:"server_name,omitempty"`
	Transaction string             `json:"transaction,omitempty"`
	Message     string             `json:"message,omitempty"`
	Template    string             `json:"message_template,omitempty"`
	Fingerprint []string           `json:"fingerprint,omitempty"`
	Exception   exceptionEnvelope  `json:"exception,omitempty"`
	Stacktrace  *Stacktrace        `json:"stacktrace,omitempty"`
	Tags        map[string]string  `json:"tags,omitempty"`
	User        map[string]any     `json:"user,omitempty"`
	Request     map[string]any     `json:"request,omitempty"`
	Contexts    map[string]any     `json:"contexts,omitempty"`
	Extra       map[string]any     `json:"extra,omitempty"`
	Breadcrumbs []storedBreadcrumb `json:"breadcrumbs,omitempty"`
	// Kept in the stored shape so an issue detail can say which bundle a frame
	// came from even after symbolication rewrote the frame — and so that a map
	// uploaded tomorrow can be checked against the ids of an event stored today.
	DebugMeta *DebugMeta `json:"debug_meta,omitempty"`
}

type exceptionEnvelope struct {
	Values []Exception `json:"values,omitempty"`
}

type storedBreadcrumb struct {
	Timestamp time.Time      `json:"timestamp"`
	Type      string         `json:"type,omitempty"`
	Category  string         `json:"category,omitempty"`
	Level     string         `json:"level,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

func encodeBreadcrumbs(crumbs []Breadcrumb) []storedBreadcrumb {
	if len(crumbs) == 0 {
		return nil
	}
	encoded := make([]storedBreadcrumb, 0, len(crumbs))
	for index := range crumbs {
		encoded = append(encoded, storedBreadcrumb(crumbs[index]))
	}
	return encoded
}
