package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is how this server introduces itself to a client.
const ServerName = "trapline"

// Server is the MCP adapter: one table of tools, served over two transports.
//
// It holds one `sdk.Server` rather than building one per request. The tools it
// registers take their credential from the request they are answering, so
// nothing about a session belongs to the server object — which is what lets
// the HTTP transport be stateless and lets `tools/list` be provably identical
// on both, since there is only ever one list.
type Server struct {
	server *sdk.Server
	caller Caller
	// credential is used when a request carries none, which over stdio is
	// every request: there is one operator and one token for the life of the
	// process, and it came from the environment.
	credential string

	// The HTTP handler, built on first use. See handler().
	once        sync.Once
	httpHandler http.Handler
}

// New builds the MCP server over a caller.
//
// The caller decides where the API calls go — a socket for stdio, this
// process's own router for HTTP — and is the only thing that differs between
// the two transports (ADR 022).
func New(caller Caller, credential, version string) *Server {
	server := &Server{
		server: sdk.NewServer(&sdk.Implementation{
			Name:    ServerName,
			Version: version,
			Title:   "trapline — errors, releases and performance for one installation",
		}, &sdk.ServerOptions{
			// What the client shows a user before it calls anything. It says
			// what this server is for rather than what it contains, because
			// the tools already say what they do and an instruction block
			// that repeats them is context spent twice.
			Instructions: "Read an error before changing code: get_issue_bundle returns the " +
				"symbolicated stacktrace, the breadcrumbs, how often it is happening and " +
				"the commits that probably caused it, in one call. When you have shipped " +
				"a fix, resolve_issue with in_next_release so the machines still running " +
				"the broken build do not reopen it.",
		}),
		caller:     caller,
		credential: credential,
	}

	for _, entry := range tools() {
		server.register(entry)
	}
	return server
}

// register adds one table entry to the SDK server.
//
// Through the low-level `Server.AddTool` rather than the generic top-level
// `AddTool`, because the generic one infers a schema from a Go type and this
// table's schemas are written by hand: they are the only documentation the
// agent reads, and a schema generated from a struct tag would describe
// `in_next_release` as "bool" where the whole point of it needs a sentence.
func (s *Server) register(entry tool) {
	definition := &sdk.Tool{
		Name:        entry.Name,
		Description: entry.Description,
		InputSchema: entry.Schema,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: entry.ReadOnly},
	}
	s.server.AddTool(definition, func(ctx context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return s.call(ctx, entry, request), nil
	})
}

// call runs one tool and turns whatever happened into a tool result.
//
// It never returns a protocol error. A protocol error aborts the agent's turn,
// and none of the things that can go wrong here deserve that: a bad argument,
// a missing scope and an id that does not exist are all conditions the agent
// can act on, and `IsError` plus a sentence is how MCP says so. The one thing
// this does have to get right is that the sentence names the status, because
// "forbidden" and "not found" lead to opposite next moves.
func (s *Server) call(ctx context.Context, entry tool, request *sdk.CallToolRequest) *sdk.CallToolResult {
	args, err := decodeArguments(request.Params.Arguments)
	if err != nil {
		return toolError(err.Error())
	}

	env := &toolEnv{caller: s.caller, credential: s.credentialFor(request)}
	apiRequest, err := entry.Build(ctx, env, args)
	if err != nil {
		return toolError(err.Error())
	}

	response, err := s.caller.Call(ctx, env.credential, apiRequest)
	if err != nil {
		if errors.Is(err, errBadArgument) {
			return toolError(err.Error())
		}
		return toolError(fmt.Sprintf("%s could not reach the trapline API: %v", entry.Name, err))
	}
	if !response.OK() {
		return toolError(fmt.Sprintf("%s %s returned %d: %s",
			apiRequest.Method, apiRequest.URL(), response.Status, response.Message()))
	}
	return toolText(string(response.Body))
}

// credentialFor is which token this request's API calls are made with.
//
// Over HTTP it is the bearer the MCP request itself arrived with, forwarded
// unchanged. That is what makes the scope check real: the route table refuses
// `POST /issues/{id}/status` to a token without `projects:write`, and
// forwarding means the MCP surface is refused by exactly the same line rather
// than by a second copy of the rule (ADR 022).
//
// Over stdio there is no header at all, and the token from the environment is
// used — the one the operator configured for this process, exactly as the CLI
// does.
func (s *Server) credentialFor(request *sdk.CallToolRequest) string {
	if extra := request.GetExtra(); extra != nil && extra.Header != nil {
		const prefix = "Bearer "
		header := extra.Header.Get("Authorization")
		if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
			return strings.TrimSpace(header[len(prefix):])
		}
	}
	return s.credential
}

func toolText(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

func toolError(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: text}},
	}
}

// RunStdio serves the protocol over standard input and output until the
// client goes away.
//
// This is the transport an agent on the same machine uses: it launches the
// binary as a subprocess and talks to it down a pipe, so there is no port to
// choose, no certificate and no second credential. Nothing may be written to
// stdout except protocol messages — a stray log line is a parse error at the
// other end — which is why the CLI command that calls this sends its logging
// to stderr.
func (s *Server) RunStdio(ctx context.Context) error {
	if err := s.server.Run(ctx, &sdk.StdioTransport{}); err != nil {
		return fmt.Errorf("serving MCP over stdio: %w", err)
	}
	return nil
}

// Handler serves the protocol over HTTP.
//
// Stateless, and answering `application/json` rather than an event stream.
// Both follow from what this endpoint is: one route in a table that the
// OpenAPI gate checks, answering one request at a time. A stateful transport
// would add `Mcp-Session-Id`, a session store and two more methods on the same
// path — state this server would then have to expire, for a protocol feature
// none of these tools use, since not one of them streams or calls back to the
// client.
func (s *Server) Handler() http.Handler {
	return sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
}

// ServeMCP answers one MCP request over HTTP.
//
// A method rather than an exported handler field so the HTTP adapter can
// depend on an interface of one method and not on this package: the dependency
// runs from here to the API, because this is a client of it, and an import
// back would be a cycle (httpapi/server.go).
func (s *Server) ServeMCP(w http.ResponseWriter, r *http.Request) {
	s.handler().ServeHTTP(w, r)
}

// handler builds the HTTP handler once, lazily.
//
// Lazily because the API router this server's loopback caller talks to is
// built after the server object exists, and eagerly building the handler here
// would be harmless but would put a second thing in the constructor that has
// to happen in the right order. Once because the SDK's handler is safe for
// concurrent use and rebuilding it per request would allocate a router per
// call.
func (s *Server) handler() http.Handler {
	s.once.Do(func() { s.httpHandler = s.Handler() })
	return s.httpHandler
}
