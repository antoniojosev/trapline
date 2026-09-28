package domain

import (
	"regexp"
	"strings"
	"testing"
)

func TestSensitiveKeysAreRedacted(t *testing.T) {
	scrubber := NewScrubber(nil, nil)

	// Matched as substrings because frameworks name the same thing a dozen
	// ways. An exact-match list looks tidier and misses most of them.
	sensitive := []string{
		"password", "Password", "user_password", "passwd", "pwd",
		"secret", "client_secret", "api_key", "apiKey", "APIKEY",
		"authorization", "Authorization", "X-Auth-Token", "auth_header",
		"cookie", "Cookie", "session_id", "csrf_token",
		"card_number", "cvv", "ssn", "private_key",
	}
	for _, key := range sensitive {
		t.Run(key, func(t *testing.T) {
			scrubbed := scrubber.ScrubMap(map[string]any{key: "el valor real"})
			if scrubbed[key] != Redacted {
				t.Errorf("%q survived scrubbing as %v", key, scrubbed[key])
			}
		})
	}
}

func TestOrdinaryKeysSurvive(t *testing.T) {
	// An error report scrubbed down to nothing useful is its own failure.
	scrubber := NewScrubber(nil, nil)
	harmless := map[string]any{
		"user_id": "42", "url": "/checkout", "method": "POST",
		"status_code": 500, "order_total": 19.99, "username": "antonio",
	}

	scrubbed := scrubber.ScrubMap(harmless)
	for key, want := range harmless {
		if scrubbed[key] != want {
			t.Errorf("%q = %v, want %v", key, scrubbed[key], want)
		}
	}
}

func TestRedactionIsAMarkerNotADeletion(t *testing.T) {
	// Someone debugging needs to know the field was there and was removed, or
	// they waste an afternoon looking for why the SDK "isn't sending" it.
	scrubber := NewScrubber(nil, nil)
	scrubbed := scrubber.ScrubMap(map[string]any{"authorization": "Bearer abc"})

	if _, present := scrubbed["authorization"]; !present {
		t.Fatal("the key was deleted rather than redacted")
	}
	if scrubbed["authorization"] != Redacted {
		t.Errorf("value = %v", scrubbed["authorization"])
	}
}

func TestNestedStructuresAreScrubbed(t *testing.T) {
	scrubber := NewScrubber(nil, nil)

	scrubbed := scrubber.ScrubMap(map[string]any{
		"request": map[string]any{
			"url": "/api/checkout",
			"headers": map[string]any{
				"Authorization": "Bearer sk_live_abc123",
				"Content-Type":  "application/json",
			},
			"cookies": []any{
				map[string]any{"name": "session", "value": "abc"},
			},
		},
	})

	request, _ := scrubbed["request"].(map[string]any)
	headers, _ := request["headers"].(map[string]any)

	if headers["Authorization"] != Redacted {
		t.Errorf("a nested header survived: %v", headers["Authorization"])
	}
	if headers["Content-Type"] != "application/json" {
		t.Errorf("a harmless nested header was scrubbed: %v", headers["Content-Type"])
	}
	if request["url"] != "/api/checkout" {
		t.Errorf("the url was scrubbed: %v", request["url"])
	}
}

func TestScrubbingCopiesRatherThanMutates(t *testing.T) {
	// A function that silently rewrites its argument is one somebody
	// eventually calls twice.
	scrubber := NewScrubber(nil, nil)
	original := map[string]any{
		"password": "hunter2",
		"nested":   map[string]any{"token": "abc"},
	}

	scrubber.ScrubMap(original)

	if original["password"] != "hunter2" {
		t.Error("the caller's map was mutated")
	}
	nested, _ := original["nested"].(map[string]any)
	if nested["token"] != "abc" {
		t.Error("a nested map of the caller's was mutated")
	}
}

func TestSecretsInsideValues(t *testing.T) {
	scrubber := NewScrubber(nil, nil)

	cases := map[string]string{
		"bearer token": "failed with Authorization: Bearer sk_live_51H8xKfLkd93ldkfj",
		"basic auth":   "curl -H 'Authorization: Basic YWRtaW46aHVudGVyMg=='",
		"a JWT":        "token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			scrubbed := scrubber.ScrubString(value)
			if !strings.Contains(scrubbed, Redacted) {
				t.Errorf("nothing was redacted in %q", scrubbed)
			}
		})
	}
}

func TestCardNumbersUseLuhn(t *testing.T) {
	// Without the check the pattern eats order numbers, timestamps and phone
	// numbers, and a report full of [redacted] where the context was is its
	// own kind of failure.
	scrubber := NewScrubber(nil, nil)

	t.Run("a real card shape is redacted", func(t *testing.T) {
		for _, card := range []string{
			"4111111111111111",    // Visa test number
			"4111 1111 1111 1111", // spaced
			"4111-1111-1111-1111", // hyphenated
			"5500005555555559",    // Mastercard test number
		} {
			if got := scrubber.ScrubString("payment failed for " + card); !strings.Contains(got, Redacted) {
				t.Errorf("card %q survived: %q", card, got)
			}
		}
	})

	t.Run("ordinary long numbers survive", func(t *testing.T) {
		for _, number := range []string{
			"1234567890123",             // fails Luhn
			"order 9876543210987654321", // too long for a card
			"timestamp 1787565600",      // too short
		} {
			if got := scrubber.ScrubString(number); strings.Contains(got, Redacted) {
				t.Errorf("%q was redacted as a card: %q", number, got)
			}
		}
	})
}

func TestConfiguredExtras(t *testing.T) {
	// An installation knows its own secrets: an internal header name, an
	// application-specific id format. The shipped list cannot know those.
	scrubber := NewScrubber(
		[]string{"x_internal_ref"},
		[]*regexp.Regexp{regexp.MustCompile(`CUST-\d{6}`)},
	)

	scrubbed := scrubber.ScrubMap(map[string]any{
		"x_internal_ref": "abc",
		"note":           "cliente CUST-123456 afectado",
		"safe":           "nada sensible",
	})

	if scrubbed["x_internal_ref"] != Redacted {
		t.Errorf("a configured key survived: %v", scrubbed["x_internal_ref"])
	}
	if note, _ := scrubbed["note"].(string); !strings.Contains(note, Redacted) {
		t.Errorf("a configured pattern did not match: %q", note)
	}
	if scrubbed["safe"] != "nada sensible" {
		t.Errorf("an unrelated value was scrubbed: %v", scrubbed["safe"])
	}
}

func TestDepthIsBoundedAndFailsClosed(t *testing.T) {
	// Payloads are attacker-controlled. Past the limit the structure is
	// replaced rather than passed through, or the depth guard would itself be
	// a way to smuggle secrets past the scrubber.
	scrubber := NewScrubber(nil, nil)

	deepest := map[string]any{"password": "hunter2"}
	current := deepest
	for range MaxScrubDepth + 5 {
		current = map[string]any{"nested": current}
	}

	scrubbed := scrubber.ScrubMap(current)

	// Walk down and assert nothing readable made it through.
	rendered := renderDeep(scrubbed, 0)
	if strings.Contains(rendered, "hunter2") {
		t.Error("a secret past the depth limit was returned unscrubbed")
	}
}

func renderDeep(value any, depth int) string {
	if depth > 40 {
		return ""
	}
	switch typed := value.(type) {
	case map[string]any:
		var builder strings.Builder
		for key, nested := range typed {
			builder.WriteString(key)
			builder.WriteString(renderDeep(nested, depth+1))
		}
		return builder.String()
	case []any:
		var builder strings.Builder
		for _, nested := range typed {
			builder.WriteString(renderDeep(nested, depth+1))
		}
		return builder.String()
	case string:
		return typed
	default:
		return ""
	}
}

func TestScrubMapHandlesNil(t *testing.T) {
	if got := NewScrubber(nil, nil).ScrubMap(nil); got != nil {
		t.Errorf("ScrubMap(nil) = %v, want nil", got)
	}
}

func TestArraysAreScrubbed(t *testing.T) {
	scrubber := NewScrubber(nil, nil)
	scrubbed := scrubber.ScrubMap(map[string]any{
		"headers": []any{
			map[string]any{"name": "authorization", "value": "Bearer abc123def456"},
		},
	})

	headers, _ := scrubbed["headers"].([]any)
	if len(headers) != 1 {
		t.Fatalf("headers = %v", headers)
	}
	first, _ := headers[0].(map[string]any)
	// The key here is "value", not "authorization", so this only gets caught
	// by the in-value pattern — which is exactly why both layers exist.
	if value, _ := first["value"].(string); !strings.Contains(value, Redacted) {
		t.Errorf("a bearer token inside an array survived: %q", value)
	}
}
