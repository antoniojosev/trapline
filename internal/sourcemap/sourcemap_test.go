package sourcemap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The fixture is the recorded one: a bundle built by esbuild, run
// through `sentry-cli sourcemaps inject`, loaded by a real Chromium and made to
// throw. The map beside it is the injected one — the one whose `mappings` were
// shifted by two lines to pay for the debug-id snippet — so a parser that read
// a pre-injection map would resolve every frame two lines off and nothing here
// would say so (from the recording).
const fixtureMap = "testdata/bundle.min.js.map"

func readFixture(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func parseFixture(t *testing.T) *Map {
	t.Helper()
	parsed, err := Parse(readFixture(t, fixtureMap))
	if err != nil {
		t.Fatalf("parsing the recorded map: %v", err)
	}
	return parsed
}

// TestRecordedFrameResolves is the whole point of the package, checked against
// the one position verified by hand when it was recorded: generated (3, 489) is the
// `throw new Error(...)` on line 10 of ../src/checkout.js (from the recording).
func TestRecordedFrameResolves(t *testing.T) {
	parsed := parseFixture(t)

	position, ok := parsed.Lookup(3, 489)
	if !ok {
		t.Fatal("the recorded frame did not resolve")
	}
	if position.Source != "../src/checkout.js" {
		t.Errorf("source = %q, want ../src/checkout.js", position.Source)
	}
	if position.Line != 10 || position.Column != 9 {
		t.Errorf("resolved to %d:%d, want 10:9", position.Line, position.Column)
	}

	_, line, _, ok := parsed.Context(position, 5)
	if !ok {
		t.Fatal("the resolved position has no context line")
	}
	if !strings.Contains(line, "throw new Error") {
		t.Errorf("context line = %q, want the throw", line)
	}
}

// TestRecordedDebugID pins the identifier that ties this map to the frames of
// the bundle beside it. Without it the modern resolution path has no key at all
// (ADR 018).
func TestRecordedDebugID(t *testing.T) {
	if got := parseFixture(t).DebugID; got != "fc31aec3-520c-531b-836e-0d5e872e8a18" {
		t.Errorf("debug id = %q, want the one sourcemaps inject wrote", got)
	}
}

// TestDebugIDCamelCase covers the other spelling, which several bundler
// plugins write. Reading only one of the two means half the toolchains upload
// maps that can never be matched to an event, and the symptom is silence.
func TestDebugIDCamelCase(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[],` +
		`"mappings":"AAAA","debugId":"1e0b4d9d-0000-4000-8000-000000000000"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if parsed.DebugID != "1e0b4d9d-0000-4000-8000-000000000000" {
		t.Errorf("debug id = %q, want the camelCase one to be read", parsed.DebugID)
	}
}

// TestContextAroundAResolvedLine checks the two-sided window, including the
// truncation at the top of a file: line 1 has no five lines before it, and
// asking for them must not produce five empty strings.
func TestContextAroundAResolvedLine(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[],` +
		`"sourcesContent":["one\ntwo\nthree\nfour\nfive"],"mappings":"AAAA;AACA;AACA"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	position, ok := parsed.Lookup(3, 1)
	if !ok {
		t.Fatal("line 3 did not resolve")
	}
	if position.Line != 3 {
		t.Fatalf("resolved to line %d, want 3", position.Line)
	}

	pre, line, post, ok := parsed.Context(position, 5)
	if !ok {
		t.Fatal("no context")
	}
	if line != "three" {
		t.Errorf("context line = %q, want three", line)
	}
	if len(pre) != 2 || pre[0] != "one" || pre[1] != "two" {
		t.Errorf("pre-context = %q, want [one two]", pre)
	}
	if len(post) != 2 || post[0] != "four" || post[1] != "five" {
		t.Errorf("post-context = %q, want [four five]", post)
	}
}

// TestLookupNeverCrossesALine is the rule that keeps a wrong answer from
// looking like a right one. A minified bundle is a handful of enormous lines,
// so the segment before an unmapped position is very often on another line —
// and answering with it would give every unmapped frame a confident location.
func TestLookupNeverCrossesALine(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA;;IACA"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, ok := parsed.Lookup(2, 40); ok {
		t.Error("a position on an unmapped line resolved; it must not")
	}
	if _, ok := parsed.Lookup(3, 1); ok {
		t.Error("a column before the line's first segment resolved; it must not")
	}
	if _, ok := parsed.Lookup(3, 5); !ok {
		t.Error("a column at the line's segment did not resolve")
	}
}

// TestSegmentWithNoSourceDoesNotResolve covers the one-field segment, which is
// a bundler saying "the code from here came from nowhere". Treating it as a
// match would resolve the frame to whatever the previous real segment pointed
// at, which is a different file.
func TestSegmentWithNoSourceDoesNotResolve(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA,I"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, ok := parsed.Lookup(1, 10); ok {
		t.Error("a source-less segment resolved; it must not")
	}
}

// TestNamesAreRead checks the fifth field, which is the only thing that can
// give a symbolicated frame a readable function name.
func TestNamesAreRead(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":["decode"],"mappings":"AAAAA"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	position, ok := parsed.Lookup(1, 1)
	if !ok {
		t.Fatal("did not resolve")
	}
	if position.Name != "decode" {
		t.Errorf("name = %q, want decode", position.Name)
	}
}

// TestSourceRootIsAPrefixNotAPath is the difference between what the
// specification says and what a path join would do. `webpack:///` joined with
// `./src/a.js` has to come out as `webpack:///./src/a.js`; a cleaner would
// produce `webpack:/src/a.js`, which matches nothing in a repository.
func TestSourceRootIsAPrefixNotAPath(t *testing.T) {
	for _, tc := range []struct{ root, source, want string }{
		{"webpack:///", "./src/a.js", "webpack:///./src/a.js"},
		{"/base", "src/a.js", "/base/src/a.js"},
		{"/base", "/already/absolute.js", "/already/absolute.js"},
		{"/base", "https://cdn.example/a.js", "https://cdn.example/a.js"},
		{"", "src/a.js", "src/a.js"},
	} {
		document := fmt.Sprintf(`{"version":3,"sourceRoot":%q,"sources":[%q],"names":[],"mappings":"AAAA"}`,
			tc.root, tc.source)
		parsed, err := Parse([]byte(document))
		if err != nil {
			t.Fatalf("parsing %s: %v", document, err)
		}
		if got := parsed.Sources()[0]; got != tc.want {
			t.Errorf("sourceRoot %q + %q = %q, want %q", tc.root, tc.source, got, tc.want)
		}
	}
}

// TestSectionsAreFlattened covers the index-map form, and specifically the
// half of it that is easy to get wrong: a section's column offset applies to
// its first generated line and to no other. Applying it to every line still
// finds a segment for most frames — just not the right one.
func TestSectionsAreFlattened(t *testing.T) {
	first := `{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA;AACA"}`
	second := `{"version":3,"sources":["b.js"],"names":[],"mappings":"AAAA;AACA"}`
	document := fmt.Sprintf(
		`{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":%s},`+
			`{"offset":{"line":5,"column":100},"map":%s}]}`, first, second)

	parsed, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parsing an index map: %v", err)
	}
	if got := parsed.Sources(); len(got) != 2 || got[0] != "a.js" || got[1] != "b.js" {
		t.Fatalf("sources = %q, want both sections' sources in order", got)
	}

	// The first section is where it always was.
	position, ok := parsed.Lookup(1, 1)
	if !ok || position.Source != "a.js" {
		t.Errorf("generated 1:1 = %+v, ok=%v; want a.js", position, ok)
	}
	// The second section's first line starts at column 101, not column 1.
	if _, ok := parsed.Lookup(6, 1); ok {
		t.Error("generated 6:1 resolved, but the section starts at column 101")
	}
	position, ok = parsed.Lookup(6, 101)
	if !ok || position.Source != "b.js" || position.Line != 1 {
		t.Errorf("generated 6:101 = %+v, ok=%v; want b.js line 1", position, ok)
	}
	// And its second line starts at column 1 again: the offset was for the
	// first line only.
	position, ok = parsed.Lookup(7, 1)
	if !ok || position.Source != "b.js" || position.Line != 2 {
		t.Errorf("generated 7:1 = %+v, ok=%v; want b.js line 2", position, ok)
	}
}

// TestSectionWithoutAMapIsSkipped covers the deprecated `url` form. This
// product does not fetch a URL a build artefact names, and half a map is worth
// more than none when the half covers the frame somebody is looking at.
func TestSectionWithoutAMapIsSkipped(t *testing.T) {
	inner := `{"version":3,"sources":["b.js"],"names":[],"mappings":"AAAA"}`
	document := fmt.Sprintf(
		`{"version":3,"sections":[{"offset":{"line":0,"column":0},"url":"http://elsewhere/a.map"},`+
			`{"offset":{"line":1,"column":0},"map":%s}]}`, inner)

	parsed, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	position, ok := parsed.Lookup(2, 1)
	if !ok || position.Source != "b.js" {
		t.Errorf("the section that did have a map did not resolve: %+v ok=%v", position, ok)
	}
}

// TestVendorExtensionsAreIgnored is the `x_facebook_*` rule stated as a test.
// Ignoring them is a decision: `x_facebook_offsets` changes what a line number
// means, and a parser that read the key without implementing the semantics
// would resolve every frame of a React Native bundle to a confidently wrong
// line. Ignoring produces the plain-v3 answer, which is at least the answer
// every other tool gives.
func TestVendorExtensionsAreIgnored(t *testing.T) {
	document := `{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA",` +
		`"x_facebook_offsets":[1,2,3],"x_facebook_sources":[[{"names":["<global>"]}]],` +
		`"x_google_ignoreList":[0]}`
	parsed, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if position, ok := parsed.Lookup(1, 1); !ok || position.Source != "a.js" {
		t.Errorf("a map with vendor extensions did not resolve plainly: %+v ok=%v", position, ok)
	}
}

// TestOnlyVersionThreeIsAccepted. A version this parser does not implement is
// a document whose mappings mean something else, and guessing is how a
// symbolicator returns wrong lines rather than no lines.
func TestOnlyVersionThreeIsAccepted(t *testing.T) {
	for _, document := range []string{
		`{"sources":[],"names":[],"mappings":""}`,
		`{"version":2,"sources":[],"names":[],"mappings":""}`,
		`{"version":4,"sources":[],"names":[],"mappings":""}`,
		`{"version":"three","sources":[],"names":[],"mappings":""}`,
		`{"version":{},"sources":[],"names":[],"mappings":""}`,
	} {
		if _, err := Parse([]byte(document)); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("Parse(%s) error = %v, want ErrUnsupportedVersion", document, err)
		}
	}
	// The string form of 3 is accepted: enough tools emit it.
	if _, err := Parse([]byte(`{"version":"3","sources":[],"names":[],"mappings":""}`)); err != nil {
		t.Errorf(`Parse with version "3" = %v, want it accepted`, err)
	}
}

// TestMalformedMappingsAreRefused. Each of these is a way a hand-made or
// truncated document can be wrong, and every one of them has a wrong answer
// available that looks like a right one.
func TestMalformedMappingsAreRefused(t *testing.T) {
	for name, mappings := range map[string]string{
		"not base64":        "AA*A",
		"truncated vlq":     "AAAAg",
		"two fields":        "AAAA,AA",
		"three fields":      "AAA",
		"six fields":        "AAAAAA",
		"overflowing value": "ggggggggggggA",
		"negative column":   "AAAA,DAAA",
		"negative source":   "ADAA",
	} {
		document := fmt.Sprintf(`{"version":3,"sources":["a.js"],"names":[],"mappings":%q}`, mappings)
		if _, err := Parse([]byte(document)); err == nil {
			t.Errorf("%s (%q) parsed without error", name, mappings)
		}
	}
}

// TestEmptySegmentsAreTolerated. A stray separator carries no information and
// cannot produce a wrong position, so skipping one costs nothing while
// refusing costs every frame in the file. The strictness above is spent on the
// shapes that *can* resolve to somewhere real and wrong.
func TestEmptySegmentsAreTolerated(t *testing.T) {
	for _, mappings := range []string{"AAAA,,IAAA", "AAAA,", ";;AAAA;;"} {
		document := fmt.Sprintf(`{"version":3,"sources":["a.js"],"names":[],"mappings":%q}`, mappings)
		if _, err := Parse([]byte(document)); err != nil {
			t.Errorf("Parse(%q) = %v, want it tolerated", mappings, err)
		}
	}
}

// TestAMapWithNoMappingsIsUsable. It resolves nothing, which is the correct
// answer, and it is not an error: refusing would turn a useless upload into a
// failed one and lose the artefact that would have said so.
func TestAMapWithNoMappingsIsUsable(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[]}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, ok := parsed.Lookup(1, 1); ok {
		t.Error("a map with no mappings resolved something")
	}
}

// TestOversizedDocumentIsRefusedBeforeParsing. The ceiling is checked on the
// bytes, before any allocation happens: a parser that decoded first and
// measured afterwards has already spent the memory it was defending.
func TestOversizedDocumentIsRefused(t *testing.T) {
	oversized := make([]byte, MaxDocumentBytes+1)
	if _, err := Parse(oversized); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Parse of an oversized document = %v, want ErrTooLarge", err)
	}
}

// TestRunOfLineBreaksIsBounded is the cheapest amplification a source map
// offers: a few kilobytes of `;` is millions of generated lines, and an int32
// line counter that wrapped would sort before everything and make the binary
// search answer from the wrong end of the file.
func TestRunOfLineBreaksIsBounded(t *testing.T) {
	document := `{"version":3,"sources":["a.js"],"names":[],"mappings":"` +
		strings.Repeat(";", maxGeneratedLine+1) + `"}`
	if _, err := Parse([]byte(document)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Parse of a document with too many generated lines = %v, want ErrTooLarge", err)
	}
}

// TestSizeIsChargedForWhatIsHeld. The cache's budget is only a budget if the
// number it enforces moves with what was actually allocated.
func TestSizeIsChargedForWhatIsHeld(t *testing.T) {
	small, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	withContent, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[],` +
		`"sourcesContent":["` + strings.Repeat("x", 10_000) + `"],"mappings":"AAAA"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if small.Size() <= 0 {
		t.Errorf("a parsed map is priced at %d bytes", small.Size())
	}
	if withContent.Size() < small.Size()+10_000 {
		t.Errorf("embedded source is not charged: %d vs %d", withContent.Size(), small.Size())
	}
}

// TestOutOfOrderSectionsStillResolve. The format guarantees order and a real
// tool provides it, so the sort is the tolerance path — but a lookup is a
// binary search, and a binary search over an unsorted table does not fail, it
// answers wrongly.
func TestOutOfOrderSectionsStillResolve(t *testing.T) {
	first := `{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA"}`
	second := `{"version":3,"sources":["b.js"],"names":[],"mappings":"AAAA"}`
	document := fmt.Sprintf(
		`{"version":3,"sections":[{"offset":{"line":9,"column":0},"map":%s},`+
			`{"offset":{"line":0,"column":0},"map":%s}]}`, first, second)

	parsed, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	position, ok := parsed.Lookup(1, 1)
	if !ok || position.Source != "b.js" {
		t.Errorf("generated 1:1 = %+v ok=%v, want b.js", position, ok)
	}
	position, ok = parsed.Lookup(10, 1)
	if !ok || position.Source != "a.js" {
		t.Errorf("generated 10:1 = %+v ok=%v, want a.js", position, ok)
	}
}

// TestNonStringNamesKeepTheirIndex. A bundler has shipped numeric entries in
// `names`. Dropping one would shift every later index by one and silently
// rename every symbol in the file, which is worse than an ugly name.
func TestNonStringNamesKeepTheirIndex(t *testing.T) {
	parsed, err := Parse([]byte(`{"version":3,"sources":["a.js"],"names":[7,"decode"],"mappings":"AAAAC"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	position, ok := parsed.Lookup(1, 1)
	if !ok {
		t.Fatal("did not resolve")
	}
	if position.Name != "decode" {
		t.Errorf("name = %q, want decode — the numeric entry must still hold index 0", position.Name)
	}
}

// FuzzSourceMap is the gate on the surface that reads a file somebody
// uploaded. Parse must never panic and never hang, and whatever it returns
// must survive being asked questions.
func FuzzSourceMap(f *testing.F) {
	f.Add(readFixture(f, fixtureMap))
	f.Add([]byte(`{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA"}`))
	f.Add([]byte(`{"version":3,"sources":["a.js"],"names":["x"],` +
		`"sourcesContent":["one\ntwo\n"],"mappings":"AAAA;AACAA;;IACA,IA"}`))
	f.Add([]byte(`{"version":3,"sections":[{"offset":{"line":0,"column":0},` +
		`"map":{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAA"}}]}`))
	f.Add([]byte(`{"version":3,"sourceRoot":"webpack:///","sources":[null],` +
		`"sourcesContent":[null],"names":[1],"mappings":";;;;"}`))
	f.Add([]byte(`{"version":"3","mappings":"gggggggggggggggg"}`))
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := Parse(data)
		if err != nil {
			if parsed != nil {
				t.Fatalf("Parse returned both a map and %v", err)
			}
			return
		}
		if parsed == nil {
			t.Fatal("Parse returned neither a map nor an error")
		}

		// Every lookup must answer without panicking, including the ones
		// outside anything the document describes.
		for _, probe := range []struct{ line, column int }{
			{0, 0}, {1, 1}, {1, 1 << 20}, {1 << 20, 1}, {-1, -1}, {3, 489},
		} {
			position, ok := parsed.Lookup(probe.line, probe.column)
			if !ok {
				continue
			}
			if position.Line < 1 || position.Column < 1 {
				t.Fatalf("Lookup(%d,%d) returned a position at %d:%d, which is not one-based",
					probe.line, probe.column, position.Line, position.Column)
			}
			// Context must not read outside the source it belongs to.
			parsed.Context(position, 5)
			parsed.SourceLine(position, position.Line+1<<20)
			parsed.SourceLine(position, -1)
		}

		if parsed.Size() < 0 {
			t.Fatalf("a parsed map is priced at %d bytes", parsed.Size())
		}
		// Whatever came out has to be storable: the cache holds these, and a
		// map that cannot be described is one nothing can budget for.
		if _, err := json.Marshal(parsed.Sources()); err != nil {
			t.Fatalf("sources are not representable: %v", err)
		}
	})
}
