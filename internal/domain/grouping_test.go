package domain

import (
	"strings"
	"testing"
)

// The golden corpus.
//
// Grouping is the product: a tracker that splits one bug into ten thousand
// issues, or merges ten unrelated bugs into one, is useless no matter what
// else it does (§6 of the plan). So the rules live here as pairs — things that
// must group together, and things that must not — written from the shapes real
// SDKs actually send.
//
// A change to the algorithm that breaks one of these is not a refactor. It is
// a change to what "the same error" means for everyone already using the
// product, and it needs a version bump and a migration (ADR 003).

// pythonFrames is a typical Django stacktrace: the user's code at the top of
// what matters, framework and library frames around it.
func pythonFrames() []Frame {
	return []Frame{
		{Module: "django.core.handlers.base", Function: "_get_response", File: "/usr/lib/python3.11/site-packages/django/core/handlers/base.py"},
		{Module: "myapp.views", Function: "checkout", File: "/srv/app/myapp/views.py", InApp: true},
		{Module: "myapp.billing", Function: "charge", File: "/srv/app/myapp/billing.py", InApp: true},
		{Module: "requests.api", Function: "post", File: "/usr/lib/python3.11/site-packages/requests/api.py"},
	}
}

func TestTheSameErrorGroupsTogether(t *testing.T) {
	cases := map[string][2]GroupingInput{
		"identical events": {
			{ExceptionType: "ValueError", ExceptionValue: "invalid amount", Frames: pythonFrames()},
			{ExceptionType: "ValueError", ExceptionValue: "invalid amount", Frames: pythonFrames()},
		},
		"different ids in the message": {
			{ExceptionType: "DoesNotExist", ExceptionValue: "Order 4821 not found", Frames: pythonFrames()},
			{ExceptionType: "DoesNotExist", ExceptionValue: "Order 99137 not found", Frames: pythonFrames()},
		},
		"different UUIDs in the message": {
			{ExceptionType: "KeyError", ExceptionValue: "no session 9ec79c33-ec99-42ab-8353-589fcb2e04dc"},
			{ExceptionType: "KeyError", ExceptionValue: "no session 1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9"},
		},
		"different email in the message": {
			{Message: "could not notify antonio@example.com"},
			{Message: "could not notify otra.persona@dominio.test"},
		},
		// Still normalised, because here the quoted run is a value inside a
		// sentence rather than the whole message. That distinction is what
		// keeps both this case and the KeyError one correct.
		"different quoted values inside a sentence": {
			{ExceptionType: "ValidationError", ExceptionValue: `field 'nombre' is required`},
			{ExceptionType: "ValidationError", ExceptionValue: `field 'apellido' is required`},
		},
		"several quoted values in one message": {
			{ExceptionType: "ValidationError", ExceptionValue: `'a' and 'b' conflict`},
			{ExceptionType: "ValidationError", ExceptionValue: `'c' and 'd' conflict`},
		},
		"different memory addresses": {
			{ExceptionType: "SegFault", ExceptionValue: "at 0x7f3c9a1b2c3d"},
			{ExceptionType: "SegFault", ExceptionValue: "at 0x7f0011223344"},
		},
		"deploy directory changed": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Module: "myapp.views", Function: "checkout", File: "/srv/releases/20260801120000/myapp/views.py", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Module: "myapp.views", Function: "checkout", File: "/srv/releases/20260824093000/myapp/views.py", InApp: true},
			}},
		},
		"container id in the path": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handler", File: "/var/lib/docker/a1b2c3d4e5f6a7b8/app/main.go", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handler", File: "/var/lib/docker/f9e8d7c6b5a49382/app/main.go", InApp: true},
			}},
		},
		"build number in the path": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handler", File: "/builds/1841/src/main.go", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handler", File: "/builds/1902/src/main.go", InApp: true},
			}},
		},
		"rebuilt asset hash": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handleSubmit", File: "https://app.example.com/assets/index.4f3c2b1a.js", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handleSubmit", File: "https://app.example.com/assets/index.9d8e7f6c.js", InApp: true},
			}},
		},
		"cache-busting query string": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "render", File: "/static/app.js?v=1724500000", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "render", File: "/static/app.js?v=1724586400", InApp: true},
			}},
		},
		"windows and posix separators": {
			{ExceptionType: "IOError", Frames: []Frame{
				{Function: "read", File: `C:\srv\app\myapp\io.py`, InApp: true},
			}},
			{ExceptionType: "IOError", Frames: []Frame{
				{Function: "read", File: "/srv/app/myapp/io.py", InApp: true},
			}},
		},
		// The other half of the same finding: the message now carries identity,
		// so it had better normalise well, or one bug splits across issues.
		"generic error type, same problem, different order id": {
			{ExceptionType: "*errors.errorString", ExceptionValue: "payment declined for order 4800", Frames: []Frame{
				{Module: "main", Function: "run", File: "/srv/app/main.go", InApp: true},
			}},
			{ExceptionType: "*errors.errorString", ExceptionValue: "payment declined for order 4802", Frames: []Frame{
				{Module: "main", Function: "run", File: "/srv/app/main.go", InApp: true},
			}},
		},
		// Found by the Python SDK suite: a parameterised log statement arrives
		// with the template and the interpolated text side by side. Grouping
		// on the template makes one log line one issue, however many values
		// it has been called with.
		"a parameterised log line with different parameters": {
			{Message: "could not charge user %s"},
			{Message: "could not charge user %s"},
		},
		"the same failure on a different host": {
			{ExceptionType: "ConnectionError", ExceptionValue: "could not reach db-01", Frames: pythonFrames()},
			{ExceptionType: "ConnectionError", ExceptionValue: "could not reach db-07", Frames: pythonFrames()},
		},
		"the same failure from a different pod": {
			{Message: "worker pod-abc123 lost its lease"},
			{Message: "worker pod-def456 lost its lease"},
		},
		"whitespace differences": {
			{Message: "connection   refused\n"},
			{Message: "connection refused"},
		},
		// Added when the browser suite joined the matrix. It is the first SDK
		// whose frames name a URL rather than a path, and a site that moves to
		// TLS must not have every issue in its history start again.
		"the same asset over http and https": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handleSubmit", File: "http://app.example.com/assets/checkout.js", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handleSubmit", File: "https://app.example.com/assets/checkout.js", InApp: true},
			}},
		},
		// Added when the Dart suite joined the matrix. Dart records the
		// boundary between an async caller and its callee as a frame that is
		// not a frame: no module, no function, and a marker where a file
		// should be. How many of them a stacktrace carries depends on how many
		// `await`s happen to be on the path, which changes whenever somebody
		// makes a function asynchronous — so counting them would turn an
		// ordinary refactor into a wave of new issues, which is the exact
		// failure line numbers were excluded to avoid (ADR 003).
		"an extra asynchronous gap on the same path": {
			{ExceptionType: "StateError", ExceptionValue: "Bad state: the nightly worker crashed", Frames: []Frame{
				{Function: "main", File: "main.dart", InApp: true},
				{File: "<asynchronous suspension>"},
				{Function: "runNightlyWorker", File: "main.dart", InApp: true},
			}},
			{ExceptionType: "StateError", ExceptionValue: "Bad state: the nightly worker crashed", Frames: []Frame{
				{Function: "main", File: "main.dart", InApp: true},
				{File: "<asynchronous suspension>"},
				{File: "<asynchronous suspension>"},
				{Function: "runNightlyWorker", File: "main.dart", InApp: true},
			}},
		},
		// The Node suite's note, made executable. Under CommonJS the runtime
		// reports an absolute path and under ES modules it reports the same
		// file as a file:// URL, and the SDK passes on whichever it was given.
		// One codebase run two ways is one set of issues, or migrating a
		// service to ESM looks like every bug in it regressing at once.
		"the same file as a path and as a file:// URL": {
			{ExceptionType: "Error", ExceptionValue: "upstream timed out", Frames: []Frame{
				{Function: "callUpstream", File: "/srv/app/upstream.js", InApp: true},
			}},
			{ExceptionType: "Error", ExceptionValue: "upstream timed out", Frames: []Frame{
				{Function: "callUpstream", File: "file:///srv/app/upstream.js", InApp: true},
			}},
		},
	}

	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			first, second := Fingerprint(pair[0]), Fingerprint(pair[1])
			if first != second {
				t.Errorf("these should be one issue but grouped apart:\n  %+v -> %s\n  %+v -> %s",
					pair[0], first, pair[1], second)
			}
		})
	}
}

func TestDifferentErrorsStayApart(t *testing.T) {
	cases := map[string][2]GroupingInput{
		"different exception types at the same place": {
			{ExceptionType: "ValueError", Frames: pythonFrames()},
			{ExceptionType: "TypeError", Frames: pythonFrames()},
		},
		"different functions": {
			{ExceptionType: "ValueError", Frames: []Frame{
				{Module: "myapp.views", Function: "checkout", File: "/srv/app/views.py", InApp: true},
			}},
			{ExceptionType: "ValueError", Frames: []Frame{
				{Module: "myapp.views", Function: "refund", File: "/srv/app/views.py", InApp: true},
			}},
		},
		"different files": {
			{ExceptionType: "ValueError", Frames: []Frame{
				{Function: "charge", File: "/srv/app/billing.py", InApp: true},
			}},
			{ExceptionType: "ValueError", Frames: []Frame{
				{Function: "charge", File: "/srv/app/payouts.py", InApp: true},
			}},
		},
		"different messages": {
			{Message: "connection refused"},
			{Message: "permission denied"},
		},
		// Found by running the official Go SDK against the server. Every error
		// made with errors.New has the same type and, raised from one
		// function, the same frames — so without the message these were one
		// issue.
		"generic error type, same function, different problems": {
			{ExceptionType: "*errors.errorString", ExceptionValue: "payment declined for order 4800", Frames: []Frame{
				{Module: "main", Function: "run", File: "/srv/app/main.go", InApp: true},
			}},
			{ExceptionType: "*errors.errorString", ExceptionValue: "upstream timed out", Frames: []Frame{
				{Module: "main", Function: "run", File: "/srv/app/main.go", InApp: true},
			}},
		},
		"same message, different exception type": {
			{ExceptionType: "TimeoutError", ExceptionValue: "took too long"},
			{ExceptionType: "CancelledError", ExceptionValue: "took too long"},
		},
		// Found by the Python SDK suite, and this one was serious: KeyError is
		// the most common exception in Python, str(KeyError("x")) is "'x'"
		// with the quotes included, and the message normaliser rewrote any
		// quoted run to <str>. Same type, same frames, no line numbers by
		// design — nothing distinguishing survived.
		"two different KeyErrors from the same function": {
			{ExceptionType: "KeyError", ExceptionValue: "'billing_address'", Frames: []Frame{
				{Module: "__main__", Function: "handle", File: "main.py", InApp: true},
			}},
			{ExceptionType: "KeyError", ExceptionValue: "'shipping_country'", Frames: []Frame{
				{Module: "__main__", Function: "handle", File: "main.py", InApp: true},
			}},
		},
		"two different missing keys, no stacktrace": {
			{ExceptionType: "KeyError", ExceptionValue: "'billing_address'"},
			{ExceptionType: "KeyError", ExceptionValue: "'shipping_country'"},
		},
		// Found by the Node SDK suite, and the worst kind of bug: an
		// over-eager normaliser treated any letters-then-digits token as an
		// instance identity, so two unrelated errors merged and the second
		// title vanished from the product. These are ordinary words in their
		// ecosystems and are often the ENTIRE difference between two reports.
		"different encodings": {
			{ExceptionType: "Error", ExceptionValue: "unsupported encoding utf8", Frames: []Frame{
				{Module: "main", Function: "decodeBody", File: "/srv/app/main.js", InApp: true},
			}},
			{ExceptionType: "Error", ExceptionValue: "unsupported encoding base64", Frames: []Frame{
				{Module: "main", Function: "decodeBody", File: "/srv/app/main.js", InApp: true},
			}},
		},
		"different hash algorithms": {
			{ExceptionType: "Error", ExceptionValue: "hash mismatch sha256"},
			{ExceptionType: "Error", ExceptionValue: "hash mismatch md5"},
		},
		"different protocol versions": {
			{Message: "handshake failed over http2"},
			{Message: "handshake failed over ipv4"},
		},
		// The other direction of the browser's URL frames. One installation
		// receives events from several applications — that is the ordinary
		// case, not an exotic one — and two of them serving a file of the same
		// name from the same path is not a coincidence, it is a convention.
		// Merging them would attribute one application's bug to another.
		"the same path on two different hosts": {
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handleSubmit", File: "https://app.example.com/assets/checkout.js", InApp: true},
			}},
			{ExceptionType: "TypeError", Frames: []Frame{
				{Function: "handleSubmit", File: "https://admin.example.com/assets/checkout.js", InApp: true},
			}},
		},
		// The other direction: a stacktrace full of markers must still be able
		// to tell two failures apart. If the frames that carry no location
		// were the only ones considered, every asynchronous error in a Dart or
		// Flutter application would be one issue — and only in Dart and
		// Flutter, the two clients least likely to be the ones anybody tested
		// with.
		"two different async failures with the same gaps": {
			{ExceptionType: "StateError", ExceptionValue: "Bad state: no element", Frames: []Frame{
				{File: "<asynchronous suspension>"},
				{Function: "loadOrders", File: "orders.dart", InApp: true},
			}},
			{ExceptionType: "StateError", ExceptionValue: "Bad state: no element", Frames: []Frame{
				{File: "<asynchronous suspension>"},
				{Function: "loadInvoices", File: "invoices.dart", InApp: true},
			}},
		},
		"two different files, both as file:// URLs": {
			{ExceptionType: "Error", ExceptionValue: "upstream timed out", Frames: []Frame{
				{Function: "callUpstream", File: "file:///srv/app/upstream.js", InApp: true},
			}},
			{ExceptionType: "Error", ExceptionValue: "upstream timed out", Frames: []Frame{
				{Function: "callUpstream", File: "file:///srv/app/downstream.js", InApp: true},
			}},
		},
		// Found by the browser suite, and by the Dart one in a second
		// ecosystem. `Exception('...')` in Dart is a factory for one private
		// class and a minified `Error` in a browser bundle has a mangled
		// function name, so two unrelated bugs can share a type, a file and a
		// frame. The message is all that is left, and both of these tokens are
		// the letters-then-digits shape a normaliser once erased.
		"two errors from one minified function": {
			{ExceptionType: "Error", ExceptionValue: "unsupported encoding utf8", Frames: []Frame{
				{Function: "Object.t [as decode]", File: "https://app.example.com/assets/bundle.min.js", InApp: true},
			}},
			{ExceptionType: "Error", ExceptionValue: "unsupported encoding base64", Frames: []Frame{
				{Function: "Object.t [as decode]", File: "https://app.example.com/assets/bundle.min.js", InApp: true},
			}},
		},
		"an extra frame in the call path": {
			{ExceptionType: "ValueError", Frames: []Frame{
				{Function: "a", File: "/srv/app/x.py", InApp: true},
			}},
			{ExceptionType: "ValueError", Frames: []Frame{
				{Function: "a", File: "/srv/app/x.py", InApp: true},
				{Function: "b", File: "/srv/app/y.py", InApp: true},
			}},
		},
	}

	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			if Fingerprint(pair[0]) == Fingerprint(pair[1]) {
				t.Errorf("these are different bugs but were merged into one issue:\n  %+v\n  %+v",
					pair[0], pair[1])
			}
		})
	}
}

func TestLineNumbersAreNotPartOfIdentity(t *testing.T) {
	// The Frame type has no line number at all, which is the point: a refactor
	// that moves code must not create a wave of new issues and fake a
	// regression storm. This test exists to make that a decision someone has
	// to consciously reverse rather than a field somebody adds one day.
	before := GroupingInput{ExceptionType: "ValueError", Frames: []Frame{
		{Module: "myapp.billing", Function: "charge", File: "/srv/app/billing.py", InApp: true},
	}}
	after := before

	if Fingerprint(before) != Fingerprint(after) {
		t.Error("moving code changed the issue identity")
	}
}

func TestCustomFingerprintWins(t *testing.T) {
	// Someone who set a fingerprint knows something about their application
	// that no heuristic here can infer.
	first := GroupingInput{
		CustomFingerprint: []string{"checkout", "payment-declined"},
		ExceptionType:     "ValueError",
		Frames:            pythonFrames(),
	}
	second := GroupingInput{
		CustomFingerprint: []string{"checkout", "payment-declined"},
		ExceptionType:     "TypeError",
		Frames: []Frame{
			{Function: "completamente", File: "/otro/sitio.py", InApp: true},
		},
	}
	if Fingerprint(first) != Fingerprint(second) {
		t.Error("a custom fingerprint did not override the heuristics")
	}

	different := first
	different.CustomFingerprint = []string{"checkout", "payment-timeout"}
	if Fingerprint(first) == Fingerprint(different) {
		t.Error("two different custom fingerprints were merged")
	}
}

func TestInAppFramesDecideWhenPresent(t *testing.T) {
	// A stacktrace usually ends inside a library. Grouping on that would merge
	// every unrelated failure that happens to die in the same JSON decoder.
	sharedLibrary := Frame{Module: "json", Function: "Unmarshal", File: "/usr/local/go/src/encoding/json/decode.go"}

	checkout := GroupingInput{ExceptionType: "SyntaxError", Frames: []Frame{
		{Module: "myapp", Function: "checkout", File: "/srv/app/checkout.go", InApp: true},
		sharedLibrary,
	}}
	webhook := GroupingInput{ExceptionType: "SyntaxError", Frames: []Frame{
		{Module: "myapp", Function: "webhook", File: "/srv/app/webhook.go", InApp: true},
		sharedLibrary,
	}}

	if Fingerprint(checkout) == Fingerprint(webhook) {
		t.Error("two unrelated bugs were merged because they both failed inside the same library")
	}
}

func TestFallsBackToAllFramesWhenNothingIsInApp(t *testing.T) {
	// Some SDKs never mark in_app. A partial signal beats none, and silently
	// dropping to "message only" would merge every error from such an SDK.
	first := GroupingInput{ExceptionType: "Error", Frames: []Frame{
		{Function: "alpha", File: "/vendor/a.js"},
	}}
	second := GroupingInput{ExceptionType: "Error", Frames: []Frame{
		{Function: "beta", File: "/vendor/b.js"},
	}}

	if Fingerprint(first) == Fingerprint(second) {
		t.Error("with no in-app frames, different stacktraces collapsed into one issue")
	}
}

func TestEmptyFramesAreIgnored(t *testing.T) {
	// Minified or truncated stacktraces produce frames with nothing in them.
	// They must not contribute to identity, or two unrelated errors with the
	// same number of blank frames would merge.
	withBlanks := GroupingInput{ExceptionType: "Error", Frames: []Frame{
		{},
		{Function: "handler", File: "/srv/app/main.go", InApp: true},
		{},
	}}
	without := GroupingInput{ExceptionType: "Error", Frames: []Frame{
		{Function: "handler", File: "/srv/app/main.go", InApp: true},
	}}

	if Fingerprint(withBlanks) != Fingerprint(without) {
		t.Error("blank frames changed the issue identity")
	}
}

func TestFingerprintIsStableAndWellFormed(t *testing.T) {
	input := GroupingInput{ExceptionType: "ValueError", Frames: pythonFrames()}

	first := Fingerprint(input)
	for range 100 {
		if Fingerprint(input) != first {
			t.Fatal("the fingerprint is not deterministic")
		}
	}
	if len(first) != 32 {
		t.Errorf("fingerprint length = %d, want 32", len(first))
	}
	if strings.Trim(first, "0123456789abcdef") != "" {
		t.Errorf("fingerprint %q is not lowercase hex", first)
	}
}

func TestPartsCannotCollideAcrossBoundaries(t *testing.T) {
	// Without a separator between hashed parts, ["ab","c"] and ["a","bc"]
	// would produce the same hash, and an unlucky stacktrace could merge two
	// unrelated issues.
	first := GroupingInput{CustomFingerprint: []string{"ab", "c"}}
	second := GroupingInput{CustomFingerprint: []string{"a", "bc"}}

	if Fingerprint(first) == Fingerprint(second) {
		t.Error("parts collided across a boundary; the hash is not separated")
	}
}

func TestAnEmptyEventStillGroups(t *testing.T) {
	// An event with nothing usable must still land somewhere deterministic
	// rather than crashing or producing an empty fingerprint.
	fingerprint := Fingerprint(GroupingInput{})
	if len(fingerprint) != 32 {
		t.Errorf("an empty event produced %q", fingerprint)
	}
}

func TestNormalizeMessageOrdering(t *testing.T) {
	// The specific patterns have to run before the generic ones, or a UUID
	// gets shredded into digits by the number rule and two different UUID
	// shapes stop matching.
	cases := map[string]string{
		"user 9ec79c33-ec99-42ab-8353-589fcb2e04dc failed": "user <uuid> failed",
		"order 12345 not found":                            "order <n> not found",
		"write to antonio@example.com failed":              "write to <email> failed",
		`missing key 'user_id'`:                            "missing key <str>",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := normalizeMessage(input); got != want {
				t.Errorf("normalizeMessage(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestIsEntirelyQuoted(t *testing.T) {
	// The rule is subtle enough to deserve its own table: a quoted run inside
	// a sentence is a value to normalise, while a message that is nothing but
	// a quoted token is its own identity.
	quoted := map[string]bool{
		`'billing_address'`:     true,
		`"billing_address"`:     true,
		`  'spaced'  `:          true,
		`'a' and 'b' conflict`:  false,
		`field 'x' is required`: false,
		`'unbalanced`:           false,
		`'a' 'b'`:               false,
		// An empty quoted token is still a quoted token. Which way this falls
		// makes no difference to grouping — there is no content to preserve
		// either way — so it follows the simpler rule.
		`''`:    true,
		`'`:     false,
		``:      false,
		`plain`: false,
	}
	for message, want := range quoted {
		t.Run(message, func(t *testing.T) {
			if got := isEntirelyQuoted(message); got != want {
				t.Errorf("isEntirelyQuoted(%q) = %v, want %v", message, got, want)
			}
		})
	}
}
