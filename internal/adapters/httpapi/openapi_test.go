package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The gate that makes the freeze real.
//
// `docs/api/openapi.yaml` is hand-written (ADR 021), and a hand-written
// document has exactly one failure mode: it stops being true. The two ways it
// stops being true are a route added to the code and not to the document, and
// a route removed from the code while the document still promises it. The
// first ships an undocumented surface; the second is worse, because it is a
// published contract that lies.
//
// So the test walks the same table `Handler` registers from and compares it,
// both ways, against the document. It deliberately does not check field
// names: a test that verified every property would be a second copy of the
// handlers, would agree with them by construction, and would therefore catch
// nothing that mattered. What it checks is the thing a client's code breaks
// on — whether the route is there.

// openAPIPath is the document, relative to this package.
const openAPIPath = "../../../docs/api/openapi.yaml"

// httpMethods are the operation keys a path item can carry. Named rather than
// inferred, so that a mapping key which merely looks like a method — a schema
// property called `options`, say — is never mistaken for one.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func TestRoutesMatchOpenAPI(t *testing.T) {
	documented := documentedOperations(t)
	served := servedOperations()

	var undocumented, phantom []string
	for op := range served {
		if !documented[op] {
			undocumented = append(undocumented, op)
		}
	}
	for op := range documented {
		if !served[op] {
			phantom = append(phantom, op)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(phantom)

	for _, op := range undocumented {
		t.Errorf("served but not in %s: %s\n"+
			"    The API is frozen (ADR 006). A new route is fine — write it down.",
			filepath.Base(openAPIPath), op)
	}
	for _, op := range phantom {
		t.Errorf("documented but not served: %s\n"+
			"    Either the route was removed — which the freeze does not allow "+
			"without a new version prefix — or it was renamed and the document "+
			"was not.", op)
	}
}

// TestOpenAPINamesBothPrefixes ties the document's servers to the constants.
//
// Without it the two can drift in the one way nobody notices: the code moves
// to v2, every route still matches by path, and the document quietly keeps
// telling readers to call v1.
func TestOpenAPINamesBothPrefixes(t *testing.T) {
	document := readOpenAPI(t)
	for _, want := range []string{
		"- url: /api/" + APIVersion,
		"- url: /api/" + LegacyAPIVersion,
	} {
		if !strings.Contains(document, want) {
			t.Errorf("%s does not name the prefix %q under `servers`", openAPIPath, want)
		}
	}
}

// TestBetaPrefixIsAnAnnouncedAlias walks the whole table over HTTP.
//
// Two properties at once, and they need each other. That every route answers
// under the beta prefix is what makes the alias an alias rather than a
// half-migration; that none of them answers 404 is what proves the table was
// actually registered, which the document comparison above cannot see — it
// reads the table, not the router.
//
// Unauthenticated on purpose. A guarded route answers 401 or 403, a public one
// answers, and a route that does not exist answers 404 — so 404 is the single
// signal this test needs, and no credential is required to read it.
func TestBetaPrefixIsAnAnnouncedAlias(t *testing.T) {
	server := newTestServer(t)
	client := server.Client()
	// A redirect would hide the header this test is about.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	for _, rt := range apiRoutes() {
		t.Run(rt.Method+" "+rt.Path, func(t *testing.T) {
			path := concretePath(rt.Path)

			frozen := do(t, client, rt.Method, server.URL+apiPrefix+path)
			defer frozen.Body.Close()
			if frozen.StatusCode == http.StatusNotFound {
				t.Fatalf("%s %s%s is in the route table but the router answers 404",
					rt.Method, apiPrefix, path)
			}
			if got := frozen.Header.Get(deprecationHeader); got != "" {
				t.Errorf("the frozen prefix announces itself deprecated: %s: %q",
					deprecationHeader, got)
			}

			beta := do(t, client, rt.Method, server.URL+legacyAPIPrefix+path)
			defer beta.Body.Close()
			if beta.StatusCode != frozen.StatusCode {
				t.Errorf("the alias answers differently: %s gave %d, %s gave %d",
					apiPrefix+path, frozen.StatusCode, legacyAPIPrefix+path, beta.StatusCode)
			}
			if got := beta.Header.Get(deprecationHeader); got != "true" {
				t.Errorf("no %s header on the beta prefix: got %q, want %q",
					deprecationHeader, got, "true")
			}
			if got := beta.Header.Get("Link"); got != successorLink {
				t.Errorf("Link header = %q, want %q", got, successorLink)
			}
		})
	}
}

func do(t *testing.T, client *http.Client, method, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, http.NoBody) //nolint:noctx // A test's own server.
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return response
}

// pathWildcard matches a `{name}` segment of a net/http pattern.
var pathWildcard = regexp.MustCompile(`\{[^}]*\}`)

// concretePath fills every wildcard with `1`.
//
// One value for all of them because none of these requests gets far enough to
// use it: they carry no credential, so they are refused before any handler
// reads a path value. What is being tested is whether the pattern is
// registered, and for that any segment that is not empty will do.
func concretePath(pattern string) string {
	return pathWildcard.ReplaceAllString(pattern, "1")
}

// servedOperations is the route table, as `METHOD /path` strings.
func servedOperations() map[string]bool {
	served := make(map[string]bool)
	for _, rt := range apiRoutes() {
		served[rt.Method+" "+rt.Path] = true
	}
	// The three surfaces outside the versioned API are part of the same
	// promise even though they are outside the prefix — an SDK, a crontab
	// line and a browser depend on them, and none of the three could follow a
	// version bump. They are spelled with their full path, which is how the
	// document spells them.
	for _, rt := range unversionedRoutes() {
		served[rt.Method+" "+rt.Path] = true
	}
	return served
}

func readOpenAPI(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("reading the API document: %v", err)
	}
	return string(raw)
}

// documentedOperations reads `METHOD /path` out of the document's `paths:`.
//
// A hand-rolled scan rather than a YAML library, and the reason is not
// squeamishness about dependencies in a test: it is that this gate exists to
// keep a *hand-written* document honest, and the smallest thing that can read
// the two levels of keys it needs — a path, and the methods under it — is
// forty lines with no behaviour of its own. It is strict about the shape it
// expects and fails loudly when it does not find it, which is the property
// that matters: a parser that silently read nothing would turn this whole
// test green and mean nothing at all.
func documentedOperations(t *testing.T) map[string]bool {
	t.Helper()

	documented := make(map[string]bool)
	var path string
	inPaths := false
	// blockIndent is the indentation of the key that opened a `|` or `>`
	// block scalar; everything more indented than it is prose and is skipped.
	// Without this, a description that happens to contain a line like
	// `  /foo:` would be read as a path.
	blockIndent := -1

	for number, line := range strings.Split(readOpenAPI(t), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if blockIndent >= 0 {
			if indent > blockIndent {
				continue
			}
			blockIndent = -1
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if opensBlockScalar(trimmed) {
			blockIndent = indent
			continue
		}

		switch {
		case indent == 0:
			inPaths = trimmed == "paths:"
			path = ""
		case !inPaths:
			continue
		case indent == 2:
			if !strings.HasSuffix(trimmed, ":") {
				t.Fatalf("%s:%d: expected a path key, got %q", openAPIPath, number+1, trimmed)
			}
			path = strings.TrimSuffix(trimmed, ":")
			if !strings.HasPrefix(path, "/") {
				t.Fatalf("%s:%d: %q is not a path", openAPIPath, number+1, path)
			}
		case indent == 4 && strings.HasSuffix(trimmed, ":"):
			method := strings.TrimSuffix(trimmed, ":")
			if !httpMethods[method] {
				continue
			}
			if path == "" {
				t.Fatalf("%s:%d: %q sits under no path", openAPIPath, number+1, method)
			}
			documented[fmt.Sprintf("%s %s", strings.ToUpper(method), path)] = true
		}
	}

	if len(documented) == 0 {
		t.Fatalf("%s: no operations found — the document, or this scan, is broken", openAPIPath)
	}
	return documented
}

// opensBlockScalar reports whether a line ends in a `|` or `>` indicator,
// with or without the chomping and indentation modifiers YAML allows.
func opensBlockScalar(trimmed string) bool {
	colon := strings.Index(trimmed, ": ")
	value := ""
	switch {
	case colon >= 0:
		value = strings.TrimSpace(trimmed[colon+2:])
	case strings.HasSuffix(trimmed, ": |"), strings.HasSuffix(trimmed, ": >"):
		value = trimmed[len(trimmed)-1:]
	default:
		return false
	}
	if value == "" {
		return false
	}
	if value[0] != '|' && value[0] != '>' {
		return false
	}
	return strings.Trim(value[1:], "+-0123456789") == ""
}

// TestBetaPrefixAnnouncesItselfOnPathsThatAreGone is the other half.
//
// A client migrating off the alias asks about paths that are not there any
// more as often as about paths that are, and the answer it needs is
// "deprecated, and no" rather than a bare 404 it could get from any typo.
// This is only true because the announcement is decided from the path rather
// than wrapped around a route that, by definition, does not exist.
func TestBetaPrefixAnnouncesItselfOnPathsThatAreGone(t *testing.T) {
	server := newTestServer(t)

	response := do(t, server.Client(), http.MethodGet, server.URL+legacyAPIPrefix+"/no-such-route")
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	if got := response.Header.Get(deprecationHeader); got != "true" {
		t.Errorf("%s = %q, want %q", deprecationHeader, got, "true")
	}
}
