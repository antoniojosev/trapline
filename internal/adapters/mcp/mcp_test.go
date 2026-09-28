package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheTableIsWellFormed is the one test that reads the whole table.
//
// Every property here is something a client breaks on rather than something
// this code breaks on, which is why none of it is enforced by the type system:
// a duplicate name silently replaces a tool (the SDK documents that it does),
// a schema that is not an object is rejected at registration by some clients
// and ignored by others, and an argument with no description is an argument
// the model will guess at.
func TestTheTableIsWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for _, entry := range tools() {
		if entry.Name == "" {
			t.Fatal("a tool has no name")
		}
		if seen[entry.Name] {
			t.Errorf("two tools are called %q; the second silently replaces the first", entry.Name)
		}
		seen[entry.Name] = true

		if entry.Description == "" {
			t.Errorf("%s has no description, which is the only documentation an agent reads", entry.Name)
		}
		var schema struct {
			Type       string                    `json:"type"`
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		}
		if err := json.Unmarshal(entry.Schema, &schema); err != nil {
			t.Fatalf("%s: the schema is not JSON: %v", entry.Name, err)
		}
		if schema.Type != "object" {
			t.Errorf("%s: schema type is %q, and MCP requires an object", entry.Name, schema.Type)
		}
		for name, property := range schema.Properties {
			if description, _ := property["description"].(string); description == "" {
				t.Errorf("%s: argument %q has no description", entry.Name, name)
			}
		}
		for _, required := range schema.Required {
			if _, declared := schema.Properties[required]; !declared {
				t.Errorf("%s: %q is required but is not a declared property", entry.Name, required)
			}
		}
	}
	// A table that silently emptied would pass every assertion above.
	if len(seen) < 10 {
		t.Errorf("the table has %d tools; the surface promises ten", len(seen))
	}
}

// TestEveryToolBuildsTheCallItPromises is the mapping, spelled out.
//
// Written as a table of expected method and path rather than derived from the
// tools themselves, because a test that asked the tool what it built would
// agree with it by construction. This is the second opinion: it is where
// somebody reads "resolve_issue posts to .../status" and can check it.
func TestEveryToolBuildsTheCallItPromises(t *testing.T) {
	for _, testCase := range []struct {
		tool   string
		args   map[string]any
		method string
		want   string
		body   string
	}{
		{"list_projects", nil, "GET", "/projects", ""},
		{"list_issues", map[string]any{"project": 1}, "GET", "/projects/1/issues", ""},
		{
			"list_issues",
			map[string]any{"project": 1, "status": "unresolved", "q": "timeout", "limit": 5},
			"GET", "/projects/1/issues?limit=5&q=timeout&status=unresolved", "",
		},
		{"get_issue", map[string]any{"project": 2, "issue": 7}, "GET", "/projects/2/issues/7", ""},
		{
			"get_issue_bundle", map[string]any{"project": 2, "issue": 7},
			"GET", "/projects/2/issues/7/bundle", "",
		},
		{
			"resolve_issue", map[string]any{"project": 1, "issue": 3},
			"POST", "/projects/1/issues/3/status", `{"status":"resolved"}`,
		},
		{
			"resolve_issue", map[string]any{"project": 1, "issue": 3, "in_next_release": true},
			"POST", "/projects/1/issues/3/status", `{"in_next_release":true,"status":"resolved"}`,
		},
		{
			"ignore_issue", map[string]any{"project": 1, "issue": 3},
			"POST", "/projects/1/issues/3/status", `{"status":"ignored"}`,
		},
		{
			"reopen_issue", map[string]any{"project": 1, "issue": 3},
			"POST", "/projects/1/issues/3/status", `{"status":"unresolved"}`,
		},
		{"get_release_health", map[string]any{"project": 1}, "GET", "/projects/1/health", ""},
		{
			"get_release_health", map[string]any{"project": 1, "version": "shop@1.4.2"},
			"GET", "/projects/1/releases/shop@1.4.2/health", "",
		},
		{
			"query_stats", map[string]any{"project": 1, "from": "2026-09-01", "to": "2026-09-20"},
			"GET", "/projects/1/stats?from=2026-09-01&to=2026-09-20", "",
		},
		{
			"query_stats", map[string]any{"project": 1, "by": "release"},
			"GET", "/projects/1/stats/breakdown?by=release", "",
		},
		{
			"list_transactions", map[string]any{"project": 1, "sort": "fail", "limit": 10},
			"GET", "/projects/1/transactions?limit=10&sort=fail", "",
		},
	} {
		t.Run(testCase.tool+" "+testCase.want, func(t *testing.T) {
			request := build(t, testCase.tool, testCase.args)
			if request.Method != testCase.method {
				t.Errorf("method = %q, want %q", request.Method, testCase.method)
			}
			if got := request.URL(); got != testCase.want {
				t.Errorf("url = %q, want %q", got, testCase.want)
			}
			if testCase.body == "" {
				if request.Body != nil {
					t.Errorf("body = %v, want none", request.Body)
				}
				return
			}
			encoded, err := json.Marshal(request.Body)
			if err != nil {
				t.Fatalf("encoding the body: %v", err)
			}
			if string(encoded) != testCase.body {
				t.Errorf("body = %s, want %s", encoded, testCase.body)
			}
		})
	}
}

// TestTheBundleToolAsksForMarkdown is small and load-bearing: without the
// Accept header the endpoint would still answer, and a future content
// negotiation would quietly hand the agent JSON.
func TestTheBundleToolAsksForMarkdown(t *testing.T) {
	request := build(t, "get_issue_bundle", map[string]any{"project": 1, "issue": 1})
	if request.Accept != bundleMediaType {
		t.Errorf("Accept = %q, want %q", request.Accept, bundleMediaType)
	}
}

// TestAProjectSlugIsResolvedThroughTheAPI: the lookup is a real call under the
// caller's own credential, so it cannot become a way to learn that a project
// exists without being allowed to read it.
func TestAProjectSlugIsResolvedThroughTheAPI(t *testing.T) {
	caller := &recordingCaller{
		responses: map[string]Response{
			"GET /projects": jsonResponse(200,
				`[{"id":4,"name":"Venekambio","slug":"venekambio"},{"id":9,"name":"Pax","slug":"pax"}]`),
		},
	}
	env := &toolEnv{caller: caller, credential: "ek_test"}

	for _, testCase := range []struct{ given, want string }{
		{"venekambio", "/projects/4/issues"},
		{"VENEKAMBIO", "/projects/4/issues"},
		{"Pax", "/projects/9/issues"},
		{"9", "/projects/9/issues"},
	} {
		request, err := buildWith(env, "list_issues", map[string]any{"project": testCase.given})
		if err != nil {
			t.Fatalf("%q: %v", testCase.given, err)
		}
		if request.Path != testCase.want {
			t.Errorf("%q resolved to %q, want %q", testCase.given, request.Path, testCase.want)
		}
	}
	if caller.credentials["GET /projects"] != "ek_test" {
		t.Errorf("the lookup was made with %q, not the caller's own credential",
			caller.credentials["GET /projects"])
	}
}

// TestAnUnknownProjectNamesTheOnesThatExist. An agent told only "not found"
// will guess again; the names are ones it is already allowed to read, because
// the call that failed just read them.
func TestAnUnknownProjectNamesTheOnesThatExist(t *testing.T) {
	caller := &recordingCaller{
		responses: map[string]Response{
			"GET /projects": jsonResponse(200, `[{"id":4,"name":"Venekambio","slug":"venekambio"}]`),
		},
	}
	env := &toolEnv{caller: caller}

	_, err := buildWith(env, "list_issues", map[string]any{"project": "shop"})
	if err == nil {
		t.Fatal("a project that does not exist resolved anyway")
	}
	if !strings.Contains(err.Error(), "venekambio") {
		t.Errorf("the error does not say what does exist: %v", err)
	}
}

// TestLoopbackCallerCarriesTheCredentialIntoTheRouter is the property the
// whole HTTP transport rests on.
//
// The tools' calls are re-authorised by the route table, and they can only be
// re-authorised if the token actually arrives. The CSRF header is checked in
// the same test because a write tool would otherwise be refused by a defence
// aimed at browsers.
func TestLoopbackCallerCarriesTheCredentialIntoTheRouter(t *testing.T) {
	var seen *http.Request
	var body []byte
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		body, _ = readBody(r)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"resolved"}`))
	})

	caller := NewLoopbackCaller(router, "/api/v1")
	response, err := caller.Call(context.Background(), "ek_secret", Request{
		Method: "POST",
		Path:   "/projects/1/issues/3/status",
		Body:   map[string]any{"status": "resolved"},
	})
	if err != nil {
		t.Fatalf("calling: %v", err)
	}

	if seen.URL.Path != "/api/v1/projects/1/issues/3/status" {
		t.Errorf("path = %q, want the prefixed one", seen.URL.Path)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer ek_secret" {
		t.Errorf("Authorization = %q; the route table cannot check a scope without it", got)
	}
	if seen.Header.Get(csrfHeader) == "" {
		t.Errorf("no %s header: every write would be refused by the CSRF guard", csrfHeader)
	}
	if string(body) != `{"status":"resolved"}` {
		t.Errorf("body = %q", body)
	}
	if response.Status != http.StatusCreated {
		t.Errorf("status = %d, want 201", response.Status)
	}
	if !response.OK() {
		t.Error("a 201 was not reported as success")
	}
}

// TestLoopbackCallerReportsAHandlerThatWroteNothing: a handler that returns
// without writing produces a 200 in net/http, and the caller must report the
// same thing the network would, not a zero.
func TestLoopbackCallerReportsAHandlerThatWroteNothing(t *testing.T) {
	caller := NewLoopbackCaller(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "/api/v1")

	response, err := caller.Call(context.Background(), "", Request{Method: "GET", Path: "/projects"})
	if err != nil {
		t.Fatalf("calling: %v", err)
	}
	if response.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Status)
	}
}

// TestHTTPCallerTalksToAServer covers the stdio half: the same request, over
// a socket, with the token the process was started with.
func TestHTTPCallerTalksToAServer(t *testing.T) {
	var path, authorization, accept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization, accept = r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("# an issue"))
	}))
	t.Cleanup(server.Close)

	caller := NewHTTPCaller(server.URL+"/", "ek_env", "/api/v1")
	response, err := caller.Call(context.Background(), "", Request{
		Method: "GET", Path: "/projects/1/issues/2/bundle", Accept: bundleMediaType,
	})
	if err != nil {
		t.Fatalf("calling: %v", err)
	}

	if path != "/api/v1/projects/1/issues/2/bundle" {
		t.Errorf("path = %q", path)
	}
	if authorization != "Bearer ek_env" {
		t.Errorf("Authorization = %q; an empty credential must fall back to the process token", authorization)
	}
	if accept != bundleMediaType {
		t.Errorf("Accept = %q, want %q", accept, bundleMediaType)
	}
	if string(response.Body) != "# an issue" {
		t.Errorf("body = %q", response.Body)
	}
}

// TestAFailureReportsWhatTheAPISaid. The difference between 403 and 404 is the
// whole of what a tool has to tell an agent, and the message is how it does.
func TestAFailureReportsWhatTheAPISaid(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want string
	}{
		{"the documented error shape", `{"error":"token lacks scope projects:write"}`, "token lacks scope projects:write"},
		{"something else entirely", `<html>nope</html>`, "<html>nope</html>"},
		{"nothing at all", ``, "Forbidden"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := Response{Status: http.StatusForbidden, Body: []byte(testCase.body)}
			if response.OK() {
				t.Error("a 403 was reported as success")
			}
			if got := response.Message(); got != testCase.want {
				t.Errorf("message = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestArgumentsAreReadLeniently, but only where leniency has a cause.
//
// A model that has just read `"id": 3` out of list_projects sends `3` about
// half the time, so a scalar where a string was asked for is rendered rather
// than refused. Everything that cannot be rendered is still an error with a
// sentence the model can act on.
func TestArgumentsAreReadLeniently(t *testing.T) {
	args, err := decodeArguments([]byte(`{
		"project": 3, "issue": "42", "in_next_release": "true",
		"limit": 2.0, "q": "  timeout  "
	}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got := args.String("project"); got != "3" {
		t.Errorf("project = %q, want \"3\"", got)
	}
	if got := args.String("q"); got != "timeout" {
		t.Errorf("q = %q; surrounding space should not reach the server", got)
	}
	issue, found, err := args.Int("issue")
	if err != nil || !found || issue != 42 {
		t.Errorf("issue = (%d, %v, %v), want (42, true, nil)", issue, found, err)
	}
	if !args.Bool("in_next_release") {
		t.Error("a string \"true\" was not read as a flag")
	}
	if limit, found, err := args.Int("limit"); err != nil || !found || limit != 2 {
		t.Errorf("limit = (%d, %v, %v), want (2, true, nil)", limit, found, err)
	}
}

// TestAnAbsentNumberIsNotZero is why Int has three results.
//
// Without the distinction every omitted `limit` would be sent as `limit=0`,
// and the API rejects that — correctly, since zero results is not a page
// anybody asked for.
func TestAnAbsentNumberIsNotZero(t *testing.T) {
	args, err := decodeArguments([]byte(`{"project": 1}`))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, found, err := args.Int("limit"); found || err != nil {
		t.Errorf("an absent limit reported (found=%v, err=%v)", found, err)
	}

	request := buildFrom(t, "list_issues", args)
	if strings.Contains(request.URL(), "limit") {
		t.Errorf("an absent limit reached the server: %q", request.URL())
	}
}

// TestBadArgumentsAreRefusedWithASentence checks the messages, not the fact of
// the refusal.
//
// The reader of these is a model that will otherwise send the same call again
// with the same value, so "invalid argument" alone is a retry loop: each case
// asserts on the words that tell it what to change.
func TestBadArgumentsAreRefusedWithASentence(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, tool, want string
	}{
		{"not an object", `["project", 1]`, "list_issues", "not a JSON object"},
		{"a fractional id", `{"project":1,"issue":4.5}`, "get_issue", "not a whole number"},
		{"an unparseable id", `{"project":1,"issue":"soon"}`, "get_issue", "not a number"},
		{"no project at all", `{}`, "list_issues", "project is required"},
		{"no issue", `{"project":1}`, "get_issue", "issue is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			args, err := decodeArguments([]byte(testCase.raw))
			if err == nil {
				_, err = buildWith(&toolEnv{caller: &recordingCaller{}}, testCase.tool, args.values)
			}
			if err == nil {
				t.Fatal("it was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error = %q, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestNoArgumentsIsNotAnError: clients spell "nothing" three ways.
func TestNoArgumentsIsNotAnError(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`} {
		args, err := decodeArguments([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if args.String("project") != "" {
			t.Errorf("%q produced an argument out of nothing", raw)
		}
	}
}

// The helpers.

func build(t *testing.T, name string, args map[string]any) Request {
	t.Helper()
	request, err := buildWith(&toolEnv{caller: &recordingCaller{}}, name, args)
	if err != nil {
		t.Fatalf("building %s: %v", name, err)
	}
	return request
}

func buildFrom(t *testing.T, name string, args arguments) Request {
	t.Helper()
	request, err := buildWith(&toolEnv{caller: &recordingCaller{}}, name, args.values)
	if err != nil {
		t.Fatalf("building %s: %v", name, err)
	}
	return request
}

// buildWith runs one tool's Build over arguments that have been through JSON.
//
// Through JSON deliberately: a Go literal would hand the tool an `int` where
// a client hands it a `float64`, and a test that skipped the round trip would
// be exercising a type that never arrives — which is exactly how a decoder
// ends up with a case nothing reaches and missing the case everything does.
func buildWith(env *toolEnv, name string, values map[string]any) (Request, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return Request{}, err
	}
	args, err := decodeArguments(encoded)
	if err != nil {
		return Request{}, err
	}
	for _, entry := range tools() {
		if entry.Name == name {
			return entry.Build(context.Background(), env, args)
		}
	}
	return Request{}, errBadArgument
}

// recordingCaller answers from a table and remembers the credential it was
// handed, which is the only thing the tools' own tests need from a caller.
type recordingCaller struct {
	responses   map[string]Response
	credentials map[string]string
}

func (c *recordingCaller) Call(_ context.Context, credential string, request Request) (Response, error) {
	if c.credentials == nil {
		c.credentials = map[string]string{}
	}
	key := request.Method + " " + request.URL()
	c.credentials[key] = credential
	if response, found := c.responses[key]; found {
		return response, nil
	}
	return Response{Status: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)}, nil
}

func jsonResponse(status int, body string) Response {
	return Response{Status: status, ContentType: "application/json", Body: []byte(body)}
}

func readBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buffer := make([]byte, 4096)
	n, err := r.Body.Read(buffer)
	if n > 0 {
		return buffer[:n], nil
	}
	return nil, err
}
