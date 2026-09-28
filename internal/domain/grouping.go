package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// GroupingVersion is the version of the algorithm below.
//
// It is stored with every fingerprint because changing how events group is a
// destructive act for anyone already using the product: resolved issues
// reappear, counters reset, history breaks (ADR 003). Persisting the version
// is what makes a future change a migration with a plan rather than a morning
// where everyone's dashboard is wrong.
const GroupingVersion = 1

// Frame is one entry in a stacktrace, normalised.
//
// Line and column numbers are deliberately absent. Including them would make
// every refactor that moves code create brand new issues, resetting counters
// and faking a wave of regressions — the signal the product exists to give
// would be the first thing it destroyed.
type Frame struct {
	Module   string
	Function string
	File     string
	// InApp marks a frame as the user's own code rather than a library.
	InApp bool
}

// GroupingInput is everything the algorithm looks at.
//
// It is protocol-agnostic on purpose: the wire format is one adapter's
// problem, and the rule for what counts as "the same error" belongs to the
// domain, where it can be tested without constructing an HTTP request.
type GroupingInput struct {
	// CustomFingerprint, when the SDK sent one, wins outright.
	CustomFingerprint []string
	ExceptionType     string
	ExceptionValue    string
	Frames            []Frame
	// Message is the log message for events with no exception.
	Message string
}

// Fingerprint reduces an event to the identity of its issue.
//
// The order of preference is the whole design:
//
//  1. A fingerprint the SDK sent, used verbatim. Someone who took the trouble
//     to set one knows something about their application that no heuristic
//     here can infer, and overriding it would be arrogant.
//  2. The stacktrace, preferring the frames that belong to the user's code.
//  3. The exception type and a normalised message.
//  4. The normalised message alone.
func Fingerprint(in GroupingInput) string {
	if len(in.CustomFingerprint) > 0 {
		return hashParts(append([]string{"custom"}, in.CustomFingerprint...))
	}

	if parts := stacktraceParts(in.Frames); len(parts) > 0 {
		// The exception type AND the normalised message join the stacktrace.
		//
		// The type alone is not enough, and a real SDK proved it: in Go every
		// error made with errors.New or fmt.Errorf has the same type
		// (*errors.errorString) and, when raised from one function, the same
		// frames. "payment declined" and "upstream timed out" collapsed into a
		// single issue.
		//
		// The usual defence is the line number, and this design deliberately
		// does not have one: including it would make every refactor that moves
		// code invent a wave of new issues (ADR 003). Having given that up, the
		// message has to carry the distinction — normalised first, so that the
		// same error with a different order id stays one issue.
		//
		// The trade is real and worth naming: a message that varies in a way
		// the normaliser does not recognise, such as an embedded hostname, will
		// split one bug across issues. That is the failure a user can fix with
		// a custom fingerprint; the opposite one, unrelated bugs silently
		// merged, is the failure they cannot even see.
		identity := []string{"stack", in.ExceptionType, normalizeMessage(exceptionText(in))}
		return hashParts(append(identity, parts...))
	}

	if in.ExceptionType != "" {
		return hashParts([]string{"exception", in.ExceptionType, normalizeMessage(in.ExceptionValue)})
	}
	return hashParts([]string{"message", normalizeMessage(in.Message)})
}

// stacktraceParts renders the frames that identify an error.
//
// Only in-app frames are used when there are any. A stacktrace usually ends in
// library code — the JSON decoder, the HTTP client — and grouping on that
// would merge every unrelated failure that happens to fail in the same
// library. When nothing is marked in-app, all frames are used rather than
// giving up: a partial signal beats none.
func stacktraceParts(frames []Frame) []string {
	relevant := make([]Frame, 0, len(frames))
	for _, frame := range frames {
		if frame.InApp {
			relevant = append(relevant, frame)
		}
	}
	if len(relevant) == 0 {
		relevant = frames
	}

	parts := make([]string, 0, len(relevant))
	for _, frame := range relevant {
		part := frame.Module + "|" + frame.Function + "|" + normalizePath(frame.File)
		if strings.Trim(part, "|") == "" {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

// Patterns for values that differ between two occurrences of the same error.
// Leaving them in would give every occurrence its own issue, which is the
// failure mode that makes an error tracker useless: ten thousand issues that
// are one bug.
var (
	uuidPattern    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	hexPattern     = regexp.MustCompile(`(?i)\b(?:0x)?[0-9a-f]{16,}\b`)
	numberPattern  = regexp.MustCompile(`\b\d+\b`)
	quotedPattern  = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	emailPattern   = regexp.MustCompile(`(?i)\b[\w.+-]+@[\w-]+\.[\w.-]+\b`)
	addressPattern = regexp.MustCompile(`(?i)\b(?:0x)?[0-9a-f]{6,}\b`)
)

// There was once a pattern here that treated any token mixing letters and
// digits as an instance identity — host-01, pod-abc123, worker3 — to stop
// grouping splitting one bug across machines.
//
// It was removed because the Node SDK suite showed what it actually did:
// utf8, base64, sha256, md5, http2, ipv4, oauth2, v8, s3 and es2015 are all
// "letters then digits", and all of them were being erased. Two errors whose
// only difference was "unsupported encoding utf8" versus "base64" became one
// issue, and the second title vanished from the product entirely.
//
// That is the failure this design says out loud it will not accept: a split
// bug is visible and a user can fix it with a custom fingerprint, while
// silently merged bugs cannot even be noticed. The structural difference
// between "worker3" and "http2" does not exist, so no regex can tell them
// apart, and the honest move is not to try.
//
// What the pattern was wanted for is already covered: "db-01" normalises
// through the number rule, and "pod-abc123" through the hex rule. What is
// genuinely lost is "worker3" versus "worker7" — a visible split, which is
// the error worth having.

// normalizeMessage strips the parts of a message that vary per occurrence.
//
// The order matters: the most specific patterns run first, so that a UUID is
// replaced as a UUID rather than being shredded into digits by the number
// rule.
func normalizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	message = emailPattern.ReplaceAllString(message, "<email>")
	message = uuidPattern.ReplaceAllString(message, "<uuid>")
	message = hexPattern.ReplaceAllString(message, "<hex>")

	// A quoted run inside a longer message is a value — "field 'nombre' is
	// required" — and normalising it is what keeps those one issue. But when
	// the message is nothing BUT a quoted string, that string is the message,
	// and erasing it erases the only identity there was.
	//
	// The Python SDK proved this the expensive way: str(KeyError("x")) is
	// "'x'", quotes included, so every KeyError raised from one function
	// collapsed into a single issue. KeyError is the most common exception in
	// Python, so this was not a corner case.
	if !isEntirelyQuoted(message) {
		message = quotedPattern.ReplaceAllString(message, "<str>")
	}
	message = addressPattern.ReplaceAllString(message, "<hex>")
	message = numberPattern.ReplaceAllString(message, "<n>")
	return strings.Join(strings.Fields(message), " ")
}

// normalizePath removes what differs between two deployments of the same code.
//
// A path carries a build directory, a deploy timestamp, a container id — all
// of which change on every release while the file does not. Keeping the last
// few segments preserves enough to tell two files apart without letting the
// machine that compiled it decide what an issue is.
func normalizePath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "file://")
	if path == "" {
		return ""
	}

	// A query string on a JS bundle URL is a cache-buster, not identity.
	if index := strings.IndexAny(path, "?#"); index >= 0 {
		path = path[:index]
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")

	// A segment that is nothing but digits or a long hex string is a release
	// timestamp, a build number, a container id or a commit sha — it changes
	// on every deploy while the file does not. Left alone, a deploy directory
	// like /srv/releases/20260824093000/ would give every deploy a brand new
	// set of issues and bury the regression signal the product exists to give.
	for i, segment := range segments {
		switch {
		case isAllDigits(segment) && len(segment) >= 4:
			segments[i] = "<n>"
		case isHex(segment) && len(segment) >= 12:
			segments[i] = "<hash>"
		}
	}

	const keep = 3
	if len(segments) > keep {
		segments = segments[len(segments)-keep:]
	}
	normalized := strings.Join(segments, "/")

	// Hashed asset names (app.4f3c2b1a.js) change on every build.
	return hashedAssetPattern.ReplaceAllString(normalized, ".<hash>$1")
}

var hashedAssetPattern = regexp.MustCompile(`\.[0-9a-f]{8,}(\.\w+)$`)

// isEntirelyQuoted reports whether a message is a single quoted token and
// nothing else.
func isEntirelyQuoted(message string) bool {
	trimmed := strings.TrimSpace(message)
	if len(trimmed) < 2 {
		return false
	}
	first, last := trimmed[0], trimmed[len(trimmed)-1]
	if (first != '\'' && first != '"') || first != last {
		return false
	}
	// The quotes must be the outermost pair, or "'a' and 'b'" would count.
	inner := trimmed[1 : len(trimmed)-1]
	return !strings.ContainsRune(inner, rune(first))
}

func isAllDigits(segment string) bool {
	if segment == "" {
		return false
	}
	return strings.Trim(segment, "0123456789") == ""
}

func isHex(segment string) bool {
	if segment == "" {
		return false
	}
	return strings.Trim(strings.ToLower(segment), "0123456789abcdef") == ""
}

// exceptionText is the message that distinguishes two exceptions, preferring
// the exception's own value over the event message.
func exceptionText(in GroupingInput) string {
	if in.ExceptionValue != "" {
		return in.ExceptionValue
	}
	return in.Message
}

func hashParts(parts []string) string {
	hasher := sha256.New()
	for _, part := range parts {
		// Length-prefixed so that ["ab","c"] and ["a","bc"] cannot collide.
		// Without it, an attacker — or an unlucky stacktrace — could merge two
		// unrelated issues by splitting a value across a boundary.
		_, _ = hasher.Write([]byte(part))
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))[:32]
}
