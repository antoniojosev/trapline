package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/antoniojosev/trapline/internal/domain"
)

// The MCP endpoint, driven by a real MCP client.
//
// By the SDK's own client rather than by hand-rolled JSON-RPC, for the reason
// the SDK suites exist elsewhere in this repository: what is under test is
// whether a client that this project did not write can connect, list the
// tools and call them. A test that posted the frames this server expects
// would only prove the server agrees with itself (ADR 002, applied to a
// different protocol).
//
// The full end-to-end — both transports at once, `tools/list` compared
// between them, and the bundle compared against the REST endpoint — is
// `scripts/mcp.sh`, because stdio needs a subprocess. What is here is
// everything that can be checked without one, so that a break is caught in
// `make check` rather than by the gate.

// mcpSession connects a client to the server's /mcp endpoint with a token.
func mcpSession(t *testing.T, baseURL, token string) *sdk.ClientSession {
	t.Helper()

	client := sdk.NewClient(&sdk.Implementation{Name: "trapline-test", Version: "test"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint: baseURL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{
			token: token,
			next:  http.DefaultTransport,
		}},
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connecting to the MCP endpoint: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// bearer adds the credential to every request the MCP client makes.
//
// A RoundTripper because that is where an MCP client lets a host put one: the
// protocol has no credential of its own, so authentication is whatever the
// transport carries — which for this server is the same `Authorization:
// Bearer` header the REST API takes, and deliberately not a second scheme
// (ADR 022).
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	response, err := b.next.RoundTrip(clone)
	if err != nil {
		return nil, err //nolint:wrapcheck // RoundTripper contract: pass through untouched.
	}
	return response, nil
}

func callTool(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return result
}

func toolText(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	var text strings.Builder
	for _, content := range result.Content {
		if typed, isText := content.(*sdk.TextContent); isText {
			text.WriteString(typed.Text)
		}
	}
	return text.String()
}

// TestMCPOverHTTPServesTheBundleAndTheRESTEndpointAgrees is the claim the
// whole adapter rests on: an MCP tool is a call into this same API, so the
// two answers are the same bytes.
//
// If they ever differ, the tool has stopped being an adapter and has become a
// second implementation — which is the failure ADR 022 exists to prevent, and
// the one that would otherwise be discovered by an agent acting on a stale
// answer.
func TestMCPOverHTTPServesTheBundleAndTheRESTEndpointAgrees(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")
	issue := listIssues(t, client, dsn.ProjectID)[0]

	_, token, err := tokens.Create(context.Background(), "agent", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	session := mcpSession(t, server.URL, token)

	result := callTool(t, session, "get_issue_bundle", map[string]any{
		"project": dsn.ProjectID,
		"issue":   issue.ID,
	})
	if result.IsError {
		t.Fatalf("get_issue_bundle failed: %s", toolText(t, result))
	}

	overREST := readAll(t, client.do(http.MethodGet, bundlePath(dsn.ProjectID, issue.ID), nil).Body)
	if got := toolText(t, result); got != overREST {
		t.Errorf("the MCP tool and the REST endpoint disagree.\n--- MCP ---\n%s\n--- REST ---\n%s",
			got, overREST)
	}
}

// TestMCPAcceptsAProjectSlug is the difference between a tool an agent can
// use and one it has to be taught: a model that read `"slug": "venekambio"`
// out of list_projects should be able to hand it straight back.
func TestMCPAcceptsAProjectSlug(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")

	_, token, err := tokens.Create(context.Background(), "agent", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	session := mcpSession(t, server.URL, token)

	result := callTool(t, session, "list_issues", map[string]any{"project": "venekambio"})
	if result.IsError {
		t.Fatalf("list_issues by slug failed: %s", toolText(t, result))
	}
	if !strings.Contains(toolText(t, result), "invalid amount") {
		t.Errorf("the issue is missing from the answer:\n%s", toolText(t, result))
	}
}

// TestMCPWriteToolIsRefusedWithoutTheScope is the property that makes this an
// adapter rather than a back door.
//
// The tools are API calls made with the caller's own token, so a token
// without `projects:write` is refused by the same line of code that refuses
// `POST .../status` over REST. The refusal has to arrive as a *tool* error —
// `isError` with a sentence — and not as a protocol error, because a protocol
// error aborts the agent's turn where this should cost it one retry with a
// better token.
func TestMCPWriteToolIsRefusedWithoutTheScope(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")
	issue := listIssues(t, client, dsn.ProjectID)[0]

	_, readOnly, err := tokens.Create(context.Background(), "dashboard",
		[]domain.Scope{domain.ScopeProjectsRead}, nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	session := mcpSession(t, server.URL, readOnly)

	// The read still works, which is what makes the refusal meaningful: the
	// connection is fine and the credential is real.
	if result := callTool(t, session, "get_issue", map[string]any{
		"project": dsn.ProjectID, "issue": issue.ID,
	}); result.IsError {
		t.Fatalf("a read-only token could not read: %s", toolText(t, result))
	}

	result := callTool(t, session, "resolve_issue", map[string]any{
		"project": dsn.ProjectID, "issue": issue.ID, "in_next_release": true,
	})
	if !result.IsError {
		t.Fatalf("a read-only token resolved an issue: %s", toolText(t, result))
	}
	message := toolText(t, result)
	if !strings.Contains(message, "403") {
		t.Errorf("the refusal does not name the status, so an agent cannot tell it "+
			"from a wrong id: %q", message)
	}

	// And the issue really is untouched — the refusal is not cosmetic.
	var detail struct {
		Status string `json:"status"`
	}
	path := api("/projects/") + strconv.FormatInt(dsn.ProjectID, 10) +
		"/issues/" + strconv.FormatInt(issue.ID, 10)
	client.decode(client.do(http.MethodGet, path, nil), &detail)
	if detail.Status != string(domain.StatusUnresolved) {
		t.Errorf("status = %q, want unresolved", detail.Status)
	}
}

// TestMCPNeedsACredential: the endpoint is behind the same guard as the API,
// and an MCP client with no token must not get as far as a tool list.
func TestMCPNeedsACredential(t *testing.T) {
	server := newTestServer(t)

	response := tokenRequest(t, server.URL, "", http.MethodPost, "/mcp",
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.StatusCode)
	}
}

// TestMCPListsEveryToolInTheTable ties the surface to the table.
//
// Without it, a tool could be dropped from the registration loop and every
// other test here would still pass: they each call one tool by name.
func TestMCPListsEveryToolInTheTable(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	_, token, err := tokens.Create(context.Background(), "agent", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	session := mcpSession(t, server.URL, token)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	found := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		found[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description, which is the only documentation "+
				"an agent ever reads", tool.Name)
		}
	}
	for _, want := range []string{
		"list_projects", "list_issues", "get_issue", "get_issue_bundle",
		"resolve_issue", "ignore_issue", "reopen_issue",
		"get_release_health", "query_stats", "list_transactions",
	} {
		if !found[want] {
			t.Errorf("the tool %q is not served", want)
		}
	}
}
