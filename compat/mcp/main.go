// Command compat-mcp drives this product's MCP server the way an agent does.
//
// It is the gate's client, and it is a *real* MCP client — the official Go
// SDK, unmodified — for the same reason the SDK suites in the sibling
// directories are the official SDKs: what is under test is whether software
// this project did not write can connect, list the tools and call them. A
// client that posted the frames this server expects would only prove the
// server agrees with itself (ADR 002, applied to a different protocol).
//
// It speaks **both** transports in one run, which is the point:
//
//  1. stdio, by launching `trapline mcp` as a subprocess, which is how an
//     agent on the operator's own machine reaches the product;
//  2. streamable HTTP, against `POST /mcp` on the running server, which is how
//     one somewhere else does.
//
// And then it compares them. `tools/list` has to be identical, because there
// is one table (ADR 022) and two lists would mean an agent that can resolve an
// issue beside the server and cannot from CI. The bundle has to be identical
// to what `GET .../bundle` returns over plain HTTP, because an MCP tool is an
// API call and not a second implementation of one.
//
// The last check is the one with teeth: a token without `projects:write` must
// be refused when it calls `resolve_issue`, the refusal must arrive as a tool
// error rather than a protocol error, and it must name the status — because a
// protocol error would abort an agent's turn where this should cost it one
// retry.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	binary := flag.String("binary", "./trapline", "path to the trapline binary")
	baseURL := flag.String("url", "", "base URL of the running server")
	token := flag.String("token", "", "an API token with every scope")
	readOnly := flag.String("read-only-token", "", "an API token with projects:read only")
	project := flag.String("project", "", "project slug to pass to the tools, exercising slug resolution")
	projectID := flag.Int64("project-id", 0, "the same project's numeric id, for the REST reference call")
	issue := flag.Int64("issue", 0, "issue id to fetch a bundle for")
	flag.Parse()

	if *baseURL == "" || *token == "" || *readOnly == "" || *project == "" || *projectID == 0 || *issue == 0 {
		fmt.Fprintln(os.Stderr,
			"compat-mcp: -url, -token, -read-only-token, -project, -project-id and -issue are required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := run(ctx, *binary, *baseURL, *token, *readOnly, *project, *projectID, *issue); err != nil {
		fmt.Fprintf(os.Stderr, "\n   FAIL %v\n", err)
		os.Exit(1)
	}
}

// run drives both transports.
//
// `project` is the slug and `projectID` the number, and they are separate
// arguments on purpose. The tools take the slug, because resolving one is
// what saves an agent a call and is therefore worth exercising; the REST
// reference call takes the id, because the API's paths are built from ids and
// nothing resolves a slug for them. Passing the slug to both would test the
// resolution twice and the comparison not at all.
func run(
	ctx context.Context, binary, baseURL, token, readOnly, project string, projectID, issue int64,
) error {
	// stdio first, because it is the transport with a process behind it: if
	// the binary cannot serve at all, everything after this would fail with a
	// message about HTTP.
	stdio, err := connectStdio(ctx, binary, baseURL, token)
	if err != nil {
		return fmt.Errorf("connecting over stdio: %w", err)
	}
	defer func() { _ = stdio.Close() }()
	ok("connected over stdio (trapline mcp)")

	streamable, err := connectHTTP(ctx, baseURL, token)
	if err != nil {
		return fmt.Errorf("connecting over HTTP: %w", err)
	}
	defer func() { _ = streamable.Close() }()
	ok("connected over HTTP (POST /mcp)")

	for name, session := range map[string]*mcp.ClientSession{"stdio": stdio, "http": streamable} {
		info := session.InitializeResult().ServerInfo
		if info.Name == "" {
			return fmt.Errorf("%s: the server did not name itself", name)
		}
		ok(fmt.Sprintf("%s: initialize → %s %s", name, info.Name, info.Version))
	}

	// One table, two transports (ADR 022). Compared as JSON rather than field
	// by field so that a schema, a description or an annotation that differs
	// is caught too: a tool whose description is richer on one transport is a
	// tool that behaves differently for the agent, whatever the code says.
	overStdio, err := listTools(ctx, stdio)
	if err != nil {
		return err
	}
	overHTTP, err := listTools(ctx, streamable)
	if err != nil {
		return err
	}
	if overStdio != overHTTP {
		return fmt.Errorf("the two transports list different tools.\n--- stdio ---\n%s\n--- http ---\n%s",
			overStdio, overHTTP)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(overStdio), &listed); err != nil {
		return fmt.Errorf("reading the tool list: %w", err)
	}
	if len(listed.Tools) == 0 {
		return fmt.Errorf("the server lists no tools at all")
	}
	ok(fmt.Sprintf("tools/list is identical on both transports (%d tools)", len(listed.Tools)))

	// The bundle, three ways: over each transport and over plain HTTP. All
	// three have to be the same bytes, because all three are the one endpoint.
	viaREST, err := fetchBundle(ctx, baseURL, token, projectID, issue)
	if err != nil {
		return err
	}
	if len(viaREST) == 0 {
		return fmt.Errorf("GET .../bundle returned nothing")
	}
	for name, session := range map[string]*mcp.ClientSession{"stdio": stdio, "http": streamable} {
		result, err := call(ctx, session, "get_issue_bundle", map[string]any{
			"project": project,
			"issue":   issue,
		})
		if err != nil {
			return fmt.Errorf("%s: get_issue_bundle: %w", name, err)
		}
		if result.IsError {
			return fmt.Errorf("%s: get_issue_bundle failed: %s", name, textOf(result))
		}
		if got := textOf(result); got != viaREST {
			return fmt.Errorf("%s: the tool and GET .../bundle disagree.\n--- tool ---\n%s\n--- REST ---\n%s",
				name, got, viaREST)
		}
	}
	ok("get_issue_bundle matches GET .../bundle byte for byte, on both transports")

	// A read that goes through the same table with a different id shape: the
	// slug an agent just read out of list_projects.
	result, err := call(ctx, streamable, "list_projects", map[string]any{})
	if err != nil {
		return fmt.Errorf("list_projects: %w", err)
	}
	if result.IsError {
		return fmt.Errorf("list_projects failed: %s", textOf(result))
	}
	ok("list_projects answers")

	// And the refusal. A second HTTP session, with a token that can read and
	// not write: the tools are API calls made with the caller's own
	// credential, so this must be refused by the same line of code that
	// refuses POST .../status.
	limited, err := connectHTTP(ctx, baseURL, readOnly)
	if err != nil {
		return fmt.Errorf("connecting with a read-only token: %w", err)
	}
	defer func() { _ = limited.Close() }()

	if reading, err := call(ctx, limited, "get_issue", map[string]any{
		"project": project, "issue": issue,
	}); err != nil || reading.IsError {
		return fmt.Errorf("a read-only token could not even read, so the refusal below "+
			"would prove nothing: %v %v", err, reading)
	}

	refused, err := call(ctx, limited, "resolve_issue", map[string]any{
		"project": project, "issue": issue, "in_next_release": true,
	})
	if err != nil {
		return fmt.Errorf("a refused tool came back as a protocol error, which aborts an "+
			"agent's turn instead of costing it a retry: %w", err)
	}
	if !refused.IsError {
		return fmt.Errorf("a token without projects:write resolved an issue")
	}
	message := textOf(refused)
	if !strings.Contains(message, "403") {
		return fmt.Errorf("the refusal does not name the status, so an agent cannot tell "+
			"it from a wrong id: %q", message)
	}
	ok("resolve_issue without projects:write → a well-formed MCP error naming 403")

	return nil
}

// connectStdio launches the binary and speaks the protocol down its pipes.
func connectStdio(ctx context.Context, binary, baseURL, token string) (*mcp.ClientSession, error) {
	command := exec.CommandContext(ctx, binary, "mcp") //nolint:gosec // the gate passes its own path.
	command.Env = append(os.Environ(),
		"TRAPLINE_URL="+baseURL,
		"TRAPLINE_TOKEN="+token,
	)
	// The subprocess's stderr goes to ours, so a server that fails to start
	// says why here rather than presenting as a hung pipe.
	command.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "compat-mcp", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		return nil, fmt.Errorf("launching %s mcp: %w", binary, err)
	}
	return session, nil
}

// connectHTTP opens a session against POST /mcp with a bearer token.
func connectHTTP(ctx context.Context, baseURL, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "compat-mcp", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   strings.TrimRight(baseURL, "/") + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token}, Timeout: 30 * time.Second},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s/mcp: %w", baseURL, err)
	}
	return session, nil
}

// bearer is how an MCP client carries a credential: the protocol has none of
// its own, so it is whatever the transport puts on the request.
type bearer struct{ token string }

func (b bearer) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	response, err := http.DefaultTransport.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}
	return response, nil
}

// listTools returns the tool list as canonical JSON, for comparison.
func listTools(ctx context.Context, session *mcp.ClientSession) (string, error) {
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("listing tools: %w", err)
	}
	encoded, err := json.MarshalIndent(listed, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding the tool list: %w", err)
	}
	return string(encoded), nil
}

func call(
	ctx context.Context, session *mcp.ClientSession, name string, args map[string]any,
) (*mcp.CallToolResult, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", name, err)
	}
	return result, nil
}

func textOf(result *mcp.CallToolResult) string {
	var out strings.Builder
	for _, content := range result.Content {
		if typed, isText := content.(*mcp.TextContent); isText {
			out.WriteString(typed.Text)
		}
	}
	return out.String()
}

// fetchBundle reads the document over plain HTTP, which is the reference the
// tool is compared against.
func fetchBundle(ctx context.Context, baseURL, token string, projectID, issue int64) (string, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%d/issues/%d/bundle",
		strings.TrimRight(baseURL, "/"), projectID, issue)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("building the request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/markdown")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("calling %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading the bundle: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s returned %d: %s", url, response.StatusCode, body)
	}
	if kind := response.Header.Get("Content-Type"); !strings.HasPrefix(kind, "text/markdown") {
		return "", fmt.Errorf("the bundle came back as %q, and the whole point is that it "+
			"is a document", kind)
	}
	return string(body), nil
}

func ok(what string) { fmt.Printf("   ok   %s\n", what) }
