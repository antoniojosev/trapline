package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The transport, over a stub API.
//
// A stub rather than the real server, because what is under test here is the
// half this package owns: that a tool call becomes an API call carrying the
// right credential, and that every way it can fail comes back as a tool error
// with a readable sentence rather than as a protocol error that would abort
// the agent's turn. The other half — that the API then answers the same bytes
// the REST endpoint does — is tested against the real thing in
// `internal/adapters/httpapi/mcp_handler_test.go`, and end to end over both
// transports at once in `scripts/mcp.sh`.

// stubAPI answers the handful of paths the tools call.
func stubAPI(t *testing.T, seen *http.Header) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.URL.Path == "/api/v1/projects":
			_, _ = w.Write([]byte(`[{"id":4,"name":"Venekambio","slug":"venekambio"}]`))
		case strings.HasSuffix(r.URL.Path, "/bundle"):
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte("# TypeError\n\n- **issue**: 7\n"))
		case strings.HasSuffix(r.URL.Path, "/status"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"token lacks scope projects:write"}`))
		default:
			_, _ = w.Write([]byte(`{"issues":[]}`))
		}
	})
}

// mcpOver serves this package's HTTP transport over a stub API and returns a
// connected client session.
func mcpOver(t *testing.T, api http.Handler, token string) *sdk.ClientSession {
	t.Helper()

	server := New(NewLoopbackCaller(api, "/api/v1"), "", "test")
	front := httptest.NewServer(http.HandlerFunc(server.ServeMCP))
	t.Cleanup(front.Close)

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:   front.URL,
		HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	response, err := http.DefaultTransport.RoundTrip(clone)
	if err != nil {
		return nil, err //nolint:wrapcheck // RoundTripper contract: pass through untouched.
	}
	return response, nil
}

func text(result *sdk.CallToolResult) string {
	var out strings.Builder
	for _, content := range result.Content {
		if typed, isText := content.(*sdk.TextContent); isText {
			out.WriteString(typed.Text)
		}
	}
	return out.String()
}

// TestTheCallerCredentialComesFromTheRequest is the property that makes the
// HTTP transport safe to share: one server object, many callers, and the token
// that authorises each call is the one that call arrived with.
//
// A server that captured a credential at construction would authorise every
// agent's calls with whichever token connected first — which is the shape of
// bug that does not fail a test until two people use it at once.
func TestTheCallerCredentialComesFromTheRequest(t *testing.T) {
	var seen http.Header
	session := mcpOver(t, stubAPI(t, &seen), "ek_from_the_client")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "list_projects",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("calling: %v", err)
	}
	if result.IsError {
		t.Fatalf("list_projects failed: %s", text(result))
	}
	if got := seen.Get("Authorization"); got != "Bearer ek_from_the_client" {
		t.Errorf("the API was called with %q, not the credential the MCP request carried", got)
	}
}

// TestTheBundleArrivesAsMarkdownNotJSON: the tool passes the body through, so
// what the agent reads is the document rather than a JSON string of it.
func TestTheBundleArrivesAsMarkdownNotJSON(t *testing.T) {
	session := mcpOver(t, stubAPI(t, nil), "ek_test")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_issue_bundle",
		Arguments: map[string]any{"project": "venekambio", "issue": 7},
	})
	if err != nil {
		t.Fatalf("calling: %v", err)
	}
	if got := text(result); !strings.HasPrefix(got, "# TypeError") {
		t.Errorf("the document was transformed on the way out: %q", got)
	}
}

// TestARefusalIsAToolErrorAndNotAProtocolError is the shape of a refusal.
//
// The distinction is the whole of how an agent recovers: a protocol error ends
// the turn, and `isError` with a sentence costs one retry. The sentence has to
// name the status, because 403 and 404 lead to opposite next moves.
func TestARefusalIsAToolErrorAndNotAProtocolError(t *testing.T) {
	session := mcpOver(t, stubAPI(t, nil), "ek_readonly")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "resolve_issue",
		Arguments: map[string]any{"project": 4, "issue": 7},
	})
	if err != nil {
		t.Fatalf("a refused tool became a protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatal("a 403 was reported as success")
	}
	message := text(result)
	for _, want := range []string{"403", "projects:write"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q: %q", want, message)
		}
	}
}

// TestABadArgumentIsAlsoAToolError, for the same reason and with the same
// shape: the agent can fix it by calling again.
func TestABadArgumentIsAlsoAToolError(t *testing.T) {
	session := mcpOver(t, stubAPI(t, nil), "ek_test")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_issue",
		Arguments: map[string]any{"project": 4},
	})
	if err != nil {
		t.Fatalf("a bad argument became a protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatal("a call with no issue id succeeded")
	}
	if !strings.Contains(text(result), "issue is required") {
		t.Errorf("the message does not say what is missing: %q", text(result))
	}
}

// TestTheReadOnlyHintIsAdvertised. It is not a permission — the permission is
// the scope the underlying route demands — but it is what lets a host ask for
// confirmation on the three tools that change something and not on the seven
// that do not.
func TestTheReadOnlyHintIsAdvertised(t *testing.T) {
	session := mcpOver(t, stubAPI(t, nil), "ek_test")

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	writes := map[string]bool{"resolve_issue": true, "ignore_issue": true, "reopen_issue": true}
	for _, tool := range listed.Tools {
		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		if writes[tool.Name] && readOnly {
			t.Errorf("%s changes state but is advertised read-only", tool.Name)
		}
		if !writes[tool.Name] && !readOnly {
			t.Errorf("%s changes nothing but is not advertised read-only", tool.Name)
		}
	}
}

// TestTheServerNamesItself: an MCP host lists servers by name, and one that
// introduced itself as the empty string would be indistinguishable from a
// broken one.
func TestTheServerNamesItself(t *testing.T) {
	session := mcpOver(t, stubAPI(t, nil), "ek_test")

	if got := session.InitializeResult().ServerInfo.Name; got != ServerName {
		t.Errorf("server name = %q, want %q", got, ServerName)
	}
	if session.InitializeResult().Instructions == "" {
		t.Error("the server offers no instructions, which is the one place it can tell " +
			"an agent to read an error before changing code")
	}
}
