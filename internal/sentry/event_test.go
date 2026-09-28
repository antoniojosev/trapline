package sentry

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func decode(t *testing.T, payload string) Event {
	t.Helper()
	event, err := DecodeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return event
}

// The shapes below are taken from what the official SDKs actually put on the
// wire. Every polymorphic field here is one a naive decoder drops silently for
// half the compatibility matrix.

func TestDecodePythonEvent(t *testing.T) {
	event := decode(t, `{
		"event_id": "9ec79c33ec9942ab8353589fcb2e04dc",
		"timestamp": 1787654321.5,
		"platform": "python",
		"level": "error",
		"release": "myapp@2.3.1",
		"environment": "production",
		"exception": {"values": [
			{"type": "ConnectionError", "value": "upstream is down"},
			{"type": "ValueError", "value": "invalid amount", "stacktrace": {"frames": [
				{"filename": "app/views.py", "function": "checkout", "lineno": 42, "in_app": true}
			]}}
		]},
		"tags": {"server": "web-01", "retries": 3, "cached": false}
	}`)

	if event.Release != "myapp@2.3.1" || event.Environment != "production" {
		t.Errorf("release/environment = %q/%q", event.Release, event.Environment)
	}
	if event.Level != LevelError {
		t.Errorf("Level = %q", event.Level)
	}

	// The last exception in the chain is the one raised; the earlier ones are
	// the causes it wrapped.
	primary, found := event.PrimaryException()
	if !found || primary.Type != "ValueError" {
		t.Errorf("PrimaryException = %+v, want the last in the chain", primary)
	}
	if event.Title() != "ValueError: invalid amount" {
		t.Errorf("Title = %q", event.Title())
	}

	// Tags arrive with non-string values from real SDKs.
	for key, want := range map[string]string{"server": "web-01", "retries": "3", "cached": "false"} {
		if got := event.Tags[key]; got != want {
			t.Errorf("tag %q = %q, want %q", key, got, want)
		}
	}

	if !event.Timestamp.Equal(time.Unix(1787654321, 500000000).UTC()) {
		t.Errorf("Timestamp = %v", event.Timestamp)
	}
}

func TestTimestampShapes(t *testing.T) {
	// Four shapes, all real. A decoder that handles one silently stamps the
	// rest with the server's clock.
	want := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"unix float":     `{"timestamp": 1787565600.0}`,
		"unix integer":   `{"timestamp": 1787565600}`,
		"rfc3339":        `{"timestamp": "2026-08-24T10:00:00Z"}`,
		"quoted unix":    `{"timestamp": "1787565600"}`,
		"naive datetime": `{"timestamp": "2026-08-24T10:00:00"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decode(t, payload).Timestamp; !got.Equal(want) {
				t.Errorf("Timestamp = %v, want %v", got, want)
			}
		})
	}
}

func TestBadTimestampsFallBackToNow(t *testing.T) {
	// A wrong clock or an unusual serialiser must not cost someone their error
	// report. The server's time is a worse timestamp than the client's and an
	// infinitely better one than none.
	before := time.Now().UTC().Add(-time.Second)
	for _, payload := range []string{
		`{}`,
		`{"timestamp": "hace un rato"}`,
		`{"timestamp": null}`,
		`{"timestamp": -1}`,
		`{"timestamp": 0}`,
		`{"timestamp": {"weird": true}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			got := decode(t, payload).Timestamp
			if got.Before(before) {
				t.Errorf("Timestamp = %v, want roughly now", got)
			}
		})
	}
}

// A unix timestamp is attacker-chosen arithmetic, and a large enough one lands
// outside the four-digit years the store can write down. Storage clamps such a
// value so it cannot break the ordering of every other row (ADR 033), but a
// clamped year 9999 would sit at the top of the issue list forever, so it is
// treated here like any other unusable timestamp.
func TestTimestampsOutsideStorableYearsFallBackToNow(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	for _, payload := range []string{
		`{"timestamp": 1e17}`,
		`{"timestamp": 999999999999999}`,
		`{"timestamp": "1e17"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			got := decode(t, payload).Timestamp
			if got.Before(before) {
				t.Errorf("Timestamp = %v, want roughly now", got)
			}
			if got.Year() > 9999 || got.Year() < 1 {
				t.Errorf("Timestamp = %v, which the store cannot write down", got)
			}
		})
	}
}

func TestTagShapes(t *testing.T) {
	object := decode(t, `{"tags": {"a": "1", "b": "2"}}`)
	pairs := decode(t, `{"tags": [["a", "1"], ["b", "2"]]}`)

	// Both shapes are in the wild; they must produce the same tags.
	for key, want := range map[string]string{"a": "1", "b": "2"} {
		if object.Tags[key] != want || pairs.Tags[key] != want {
			t.Errorf("tag %q: object=%q pairs=%q, want %q", key, object.Tags[key], pairs.Tags[key], want)
		}
	}
}

func TestNonScalarTagValuesAreDropped(t *testing.T) {
	// An object as a tag value would produce a tag nobody can filter on and a
	// blob in the index.
	event := decode(t, `{"tags": {"good": "yes", "bad": {"nested": true}, "worse": [1,2]}}`)

	if event.Tags["good"] != "yes" {
		t.Errorf("the usable tag was lost: %v", event.Tags)
	}
	for _, key := range []string{"bad", "worse"} {
		if _, kept := event.Tags[key]; kept {
			t.Errorf("a non-scalar tag %q survived: %v", key, event.Tags)
		}
	}
}

func TestMessageShapes(t *testing.T) {
	cases := map[string]string{
		`{"message": "algo se rompió"}`:                                             "algo se rompió",
		`{"message": {"formatted": "user 42 failed", "message": "user %s failed"}}`: "user 42 failed",
		`{"message": {"message": "sin formatear"}}`:                                 "sin formatear",
		`{"logentry": {"formatted": "desde logentry"}}`:                             "desde logentry",
		`{"message": "", "logentry": {"formatted": "el respaldo"}}`:                 "el respaldo",
	}
	for payload, want := range cases {
		t.Run(payload, func(t *testing.T) {
			if got := decode(t, payload).Message; got != want {
				t.Errorf("Message = %q, want %q", got, want)
			}
		})
	}
}

func TestExceptionShapes(t *testing.T) {
	wrapped := decode(t, `{"exception": {"values": [{"type": "ValueError"}]}}`)
	bare := decode(t, `{"exception": [{"type": "ValueError"}]}`)

	for name, event := range map[string]Event{"wrapped": wrapped, "bare": bare} {
		t.Run(name, func(t *testing.T) {
			primary, found := event.PrimaryException()
			if !found || primary.Type != "ValueError" {
				t.Errorf("PrimaryException = %+v", primary)
			}
		})
	}
}

func TestBreadcrumbShapes(t *testing.T) {
	wrapped := decode(t, `{"breadcrumbs": {"values": [{"message": "a"}, {"message": "b"}]}}`)
	bare := decode(t, `{"breadcrumbs": [{"message": "a"}, {"message": "b"}]}`)

	for name, event := range map[string]Event{"wrapped": wrapped, "bare": bare} {
		t.Run(name, func(t *testing.T) {
			if len(event.Breadcrumbs) != 2 {
				t.Fatalf("got %d breadcrumbs, want 2", len(event.Breadcrumbs))
			}
			if event.Breadcrumbs[0].Message != "a" {
				t.Errorf("first breadcrumb = %q", event.Breadcrumbs[0].Message)
			}
		})
	}
}

func TestBestStacktracePrefersThePrimaryException(t *testing.T) {
	event := decode(t, `{
		"stacktrace": {"frames": [{"function": "top_level"}]},
		"exception": {"values": [{"type": "E", "stacktrace": {"frames": [{"function": "from_exception"}]}}]}
	}`)

	stacktrace := event.BestStacktrace()
	if stacktrace == nil || len(stacktrace.Frames) != 1 {
		t.Fatalf("BestStacktrace = %+v", stacktrace)
	}
	if stacktrace.Frames[0].Function != "from_exception" {
		t.Errorf("used the top-level stacktrace instead of the exception's")
	}
}

func TestBestStacktraceFallsBackToTopLevel(t *testing.T) {
	// Some SDKs send a message event with a stacktrace and no exception.
	event := decode(t, `{"stacktrace": {"frames": [{"function": "top_level"}]}}`)

	stacktrace := event.BestStacktrace()
	if stacktrace == nil || stacktrace.Frames[0].Function != "top_level" {
		t.Errorf("BestStacktrace = %+v", stacktrace)
	}
}

func TestFramePathPrefersAbsPath(t *testing.T) {
	frame := Frame{Filename: "views.py", AbsPath: "/srv/app/views.py"}
	if frame.Path() != "/srv/app/views.py" {
		t.Errorf("Path() = %q", frame.Path())
	}
	if (Frame{Filename: "views.py"}).Path() != "views.py" {
		t.Error("Path() did not fall back to filename")
	}
}

func TestUnknownLevelDefaultsToError(t *testing.T) {
	// This is an error tracker: an event with no usable level is far more
	// likely to be an error than an unclassifiable one.
	for _, payload := range []string{`{}`, `{"level": "critical"}`, `{"level": ""}`} {
		if got := decode(t, payload).Level; got != LevelError {
			t.Errorf("%s -> Level = %q, want error", payload, got)
		}
	}
	if got := decode(t, `{"level": "WARNING"}`).Level; got != LevelWarning {
		t.Errorf("Level = %q, want warning — the level should be case-insensitive", got)
	}
}

func TestTitleFallbacks(t *testing.T) {
	cases := map[string]string{
		`{"exception": {"values": [{"type": "ValueError", "value": "bad"}]}}`: "ValueError: bad",
		`{"exception": {"values": [{"type": "ValueError"}]}}`:                 "ValueError",
		`{"message": "just a message"}`:                                       "just a message",
		`{}`:                                                                  "<unlabelled event>",
	}
	for payload, want := range cases {
		t.Run(want, func(t *testing.T) {
			event := decode(t, payload)
			if got := event.Title(); got != want {
				t.Errorf("Title = %q, want %q", got, want)
			}
		})
	}
}

func TestLongTitlesAreTruncated(t *testing.T) {
	event := decode(t, `{"message": "`+strings.Repeat("x", 500)+`"}`)
	if len([]rune(event.Title())) > 210 {
		t.Errorf("title is %d runes; an issue list would be unreadable", len([]rune(event.Title())))
	}
}

func TestMalformedPayload(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":         "",
		"not json":      "no soy json",
		"an array":      "[1,2,3]",
		"a bare string": `"hola"`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEvent([]byte(payload)); !errors.Is(err, ErrInvalidEvent) {
				t.Errorf("error = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

func TestUnknownFieldsDoNotFailTheEvent(t *testing.T) {
	// ADR 002: losing a field we do not understand is cosmetic. Losing the
	// error report is the product failing at its one job.
	event := decode(t, `{
		"message": "sigue funcionando",
		"un_campo_del_futuro": {"lo que sea": [1, 2, 3]},
		"level": "error"
	}`)
	if event.Message != "sigue funcionando" {
		t.Errorf("Message = %q", event.Message)
	}
}
