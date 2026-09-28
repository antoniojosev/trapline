package domain

import (
	"regexp"
	"strings"
)

// Redacted is what replaces a scrubbed value.
//
// A marker rather than deletion: someone debugging needs to know the field was
// there and was removed, or they waste an afternoon looking for why the SDK
// "isn't sending" an authorization header.
const Redacted = "[redacted]"

// MaxScrubDepth bounds recursion into nested structures.
//
// Event payloads are attacker-controlled, and a deeply nested object would
// otherwise be a stack overflow waiting to happen on a public endpoint.
const MaxScrubDepth = 12

// sensitiveKeys are field names whose values never belong in an error report.
//
// Matched as substrings, case-insensitively, because SDKs and frameworks name
// the same thing a dozen ways: "authorization", "auth_token", "X-Auth-Token",
// "userPassword". An exact-match list looks tidier and misses most of them.
var sensitiveKeys = []string{
	"password", "passwd", "pwd",
	"secret", "token", "apikey", "api_key", "access_key", "private_key",
	"authorization", "auth", "credential", "session", "cookie",
	"csrf", "xsrf",
	"card_number", "cardnumber", "cvv", "cvc", "ccv",
	"ssn", "social_security",
	"signature", "encryption_key",
}

// Patterns for secrets that appear inside values rather than under an obvious
// key — a bearer token pasted into a log line, a card number in a message.
var (
	bearerPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	jwtPattern    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+`)
	cardPattern   = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)
)

// Scrubber removes sensitive values before anything is written down.
//
// The timing is the whole point: scrubbing at display time would mean the
// secret is already in the database, in the backup, and in whatever an
// operator greps. Once it is persisted it has leaked, and a redaction on the
// way out is theatre (SECURITY.md).
type Scrubber struct {
	extraKeys     []string
	extraPatterns []*regexp.Regexp
}

// NewScrubber builds a scrubber with the shipped rules plus any configured
// extras.
//
// An installation knows its own secrets: an internal header name, an
// application-specific id format. The shipped list cannot know those, so it
// has to be extensible or it will be wrong for everyone in a different way.
func NewScrubber(extraKeys []string, extraPatterns []*regexp.Regexp) *Scrubber {
	normalized := make([]string, 0, len(extraKeys))
	for _, key := range extraKeys {
		if trimmed := strings.ToLower(strings.TrimSpace(key)); trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return &Scrubber{extraKeys: normalized, extraPatterns: extraPatterns}
}

// ScrubMap returns a copy of the map with sensitive values redacted.
//
// A copy, not a mutation: the caller may still hold the original, and a
// function that silently rewrites its argument is one somebody eventually
// calls twice.
func (s *Scrubber) ScrubMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	scrubbed, _ := s.scrubValue(value, 0).(map[string]any)
	return scrubbed
}

// ScrubString redacts secrets found inside a free-form string.
func (s *Scrubber) ScrubString(value string) string {
	if value == "" {
		return value
	}
	value = jwtPattern.ReplaceAllString(value, Redacted)
	value = bearerPattern.ReplaceAllString(value, Redacted)
	value = s.scrubCardNumbers(value)
	for _, pattern := range s.extraPatterns {
		value = pattern.ReplaceAllString(value, Redacted)
	}
	return value
}

// IsSensitiveKey reports whether a field name should have its value removed.
func (s *Scrubber) IsSensitiveKey(key string) bool {
	lowered := strings.ToLower(key)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(lowered, sensitive) {
			return true
		}
	}
	for _, sensitive := range s.extraKeys {
		if strings.Contains(lowered, sensitive) {
			return true
		}
	}
	return false
}

func (s *Scrubber) scrubValue(value any, depth int) any {
	if depth > MaxScrubDepth {
		// Past the limit the structure is replaced rather than kept
		// unscrubbed: silently returning un-inspected data would turn a depth
		// guard into a way to smuggle secrets past the scrubber.
		return Redacted
	}

	switch typed := value.(type) {
	case map[string]any:
		scrubbed := make(map[string]any, len(typed))
		for key, nested := range typed {
			if s.IsSensitiveKey(key) {
				scrubbed[key] = Redacted
				continue
			}
			scrubbed[key] = s.scrubValue(nested, depth+1)
		}
		return scrubbed

	case []any:
		scrubbed := make([]any, len(typed))
		for i, nested := range typed {
			scrubbed[i] = s.scrubValue(nested, depth+1)
		}
		return scrubbed

	case string:
		return s.ScrubString(typed)

	default:
		return value
	}
}

// scrubCardNumbers redacts digit runs that pass the Luhn check.
//
// The check matters: without it the pattern eats order numbers, timestamps and
// phone numbers, and an error report full of [redacted] where the useful
// context was is its own kind of failure. Luhn is what separates "looks like a
// card" from "is shaped like a number".
func (s *Scrubber) scrubCardNumbers(value string) string {
	return cardPattern.ReplaceAllStringFunc(value, func(candidate string) string {
		digits := make([]byte, 0, len(candidate))
		for i := range len(candidate) {
			if candidate[i] >= '0' && candidate[i] <= '9' {
				digits = append(digits, candidate[i])
			}
		}
		if len(digits) < 13 || len(digits) > 19 || !passesLuhn(digits) {
			return candidate
		}
		return Redacted
	})
}

func passesLuhn(digits []byte) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		digit := int(digits[i] - '0')
		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
		double = !double
	}
	return sum%10 == 0
}
