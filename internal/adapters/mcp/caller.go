// Package mcp is the fourth client of this product's API (ADR 006, ADR 022).
//
// It exposes the same operations the REST API, the CLI and the panel expose,
// as MCP tools, over two transports: `trapline mcp` speaks stdio to an agent
// running on the same machine, and `POST /mcp` speaks streamable HTTP to one
// that is not. Both serve the same table, in `tools.go`.
//
// The thing to understand about this package is that it makes **API calls**.
// It does not reach for a use case, and the stdio transport does not open the
// database. A tool is a `Request` — a method, a path under `/api/v1/` and some
// parameters — and the only thing that differs between the transports is who
// carries it:
//
//   - over stdio, an `HTTPCaller` sends it to `TRAPLINE_URL` with the token
//     from `TRAPLINE_TOKEN`, exactly as the CLI does;
//   - over HTTP, a `LoopbackCaller` hands it to this process's own router,
//     with the credential the MCP request arrived with.
//
// That is the decision ADR 022 records, and it is what makes the two surfaces
// the same surface. An MCP server that called use cases directly would be a
// second implementation of every endpoint — with its own idea of what a scope
// permits, which is the half nobody notices has drifted until somebody reads
// an issue they should not have been able to see.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request is one call into this product's REST API.
type Request struct {
	// Method is the HTTP method.
	Method string
	// Path is relative to the version prefix — `/projects/1/issues`, never
	// `/api/v1/projects/1/issues`. Relative for the reason the route table is
	// (httpapi/routes.go): the prefix belongs to the transport, and a table
	// that spelled it out would have to be edited when the prefix moves.
	Path string
	// Query is appended when non-empty.
	Query url.Values
	// Body is marshalled as JSON when non-nil.
	Body any
	// Accept is the media type the caller wants back. Empty means JSON. The
	// one tool that sets it is the bundle, which asks for markdown.
	Accept string
}

// URL renders the path and query a Request addresses.
func (r Request) URL() string {
	if len(r.Query) == 0 {
		return r.Path
	}
	return r.Path + "?" + r.Query.Encode()
}

// Response is what came back.
//
// The status code is carried rather than folded into an error, because the
// difference between 403 and 404 is the whole of what a tool has to tell an
// agent: one means "ask for a token that can do this" and the other means
// "you have the wrong id", and an agent given "it failed" will retry the
// wrong one.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

// OK reports whether the call succeeded.
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Message is the error the API reported, or a description of the status.
func (r Response) Message() string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &body); err == nil && body.Error != "" {
		return body.Error
	}
	if text := strings.TrimSpace(string(r.Body)); text != "" {
		return text
	}
	return http.StatusText(r.Status)
}

// Caller performs one API call on behalf of whoever asked.
//
// The credential is a parameter rather than state on the caller, because the
// two transports disagree about where it comes from and only one of them can
// know it at construction: over stdio there is one operator and one token for
// the life of the process, and over HTTP there is a different bearer on every
// request. A caller that captured the credential would have to be rebuilt per
// request in the one case where that is a shared server.
type Caller interface {
	Call(ctx context.Context, credential string, request Request) (Response, error)
}

// maxResponseBody bounds what a tool will read back.
//
// A bundle with a deep stacktrace is tens of kilobytes and a list of issues is
// smaller; a megabyte is far more than any of these endpoints returns, and it
// is here so that a misconfigured URL pointing at something that streams
// cannot fill the agent's memory before anyone notices.
const maxResponseBody = 1 << 20

// HTTPCaller reaches a server over the network, as the CLI does.
type HTTPCaller struct {
	// BaseURL is the server root, without the API prefix and without a
	// trailing slash.
	BaseURL string
	// Token is the credential used when the caller supplies none, which over
	// stdio is always: one process, one operator, one token.
	Token string
	// Prefix is the versioned API prefix, e.g. "/api/v1".
	Prefix string
	// Client is the transport. A timeout is not optional: an MCP tool that
	// hangs forever takes the agent's turn with it.
	Client *http.Client
}

// NewHTTPCaller builds a caller for a server reached over the network.
func NewHTTPCaller(baseURL, token, prefix string) *HTTPCaller {
	return &HTTPCaller{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Prefix:  prefix,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Call sends the request and reads the whole response.
func (c *HTTPCaller) Call(ctx context.Context, credential string, request Request) (Response, error) {
	body, err := encodeBody(request.Body)
	if err != nil {
		return Response{}, err
	}

	httpRequest, err := http.NewRequestWithContext(ctx, request.Method,
		c.BaseURL+c.Prefix+request.URL(), bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("building the request: %w", err)
	}
	token := credential
	if token == "" {
		token = c.Token
	}
	applyHeaders(httpRequest.Header, token, request)

	response, err := c.Client.Do(httpRequest)
	if err != nil {
		return Response{}, fmt.Errorf("calling %s: %w", c.BaseURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return Response{}, fmt.Errorf("reading the response: %w", err)
	}
	return Response{
		Status:      response.StatusCode,
		ContentType: response.Header.Get("Content-Type"),
		Body:        payload,
	}, nil
}

// LoopbackCaller hands the request to this process's own router.
//
// In memory and not over a socket: the server is already here, and a call that
// went out through the loopback interface to come back in would need a port,
// a URL and a credential the process would have to mint for itself — three
// things that can be misconfigured to answer nothing at all. What it gains by
// going through the router rather than straight to a use case is the whole
// reason it exists: the route table's scope check is a real one, so a token
// without `projects:write` is refused by the same line of code that refuses it
// over REST (httpapi/routes.go, ADR 022).
type LoopbackCaller struct {
	// Handler is the API router, which expects absolute paths under Prefix.
	Handler http.Handler
	// Prefix is the versioned API prefix the router is mounted on.
	Prefix string
}

// NewLoopbackCaller builds a caller over an in-process router.
func NewLoopbackCaller(handler http.Handler, prefix string) *LoopbackCaller {
	return &LoopbackCaller{Handler: handler, Prefix: prefix}
}

// Call serves the request against the router and collects what it wrote.
func (c *LoopbackCaller) Call(ctx context.Context, credential string, request Request) (Response, error) {
	body, err := encodeBody(request.Body)
	if err != nil {
		return Response{}, err
	}

	httpRequest, err := http.NewRequestWithContext(ctx, request.Method,
		c.Prefix+request.URL(), bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("building the request: %w", err)
	}
	// RequestURI has to be empty for a server-side request; NewRequest leaves
	// it so, and this comment is here because the opposite mistake — passing
	// a client request to a handler — fails with a message that names neither.
	applyHeaders(httpRequest.Header, credential, request)

	recorder := &recordingWriter{header: http.Header{}, status: http.StatusOK}
	c.Handler.ServeHTTP(recorder, httpRequest)
	return Response{
		Status:      recorder.status,
		ContentType: recorder.header.Get("Content-Type"),
		Body:        recorder.body.Bytes(),
	}, nil
}

// recordingWriter is an http.ResponseWriter that keeps what was written.
//
// Hand-rolled rather than httptest.NewRecorder, because httptest is part of
// the testing tree: importing it here would pull the flag registrations and
// the test-only helpers of `net/http/httptest` into the shipped binary, for a
// struct that is fifteen lines.
type recordingWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	wrote  bool
}

func (w *recordingWriter) Header() http.Header { return w.header }

func (w *recordingWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status, w.wrote = status, true
	}
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.body.Write(b) //nolint:wrapcheck // bytes.Buffer.Write never fails.
}

// applyHeaders sets what every call into this API carries.
func applyHeaders(header http.Header, credential string, request Request) {
	if credential != "" {
		header.Set("Authorization", "Bearer "+credential)
	}
	header.Set("Content-Type", "application/json")
	accept := request.Accept
	if accept == "" {
		accept = "application/json"
	}
	header.Set("Accept", accept)
	// The API's CSRF defence is a custom header on state-changing requests.
	// Nothing here is a browser and nothing here carries an ambient
	// credential, so there was never anything to forge — but sending it keeps
	// one rule on the server instead of one rule per kind of client, which is
	// the same reason the CLI sends it.
	header.Set(csrfHeader, "1")
}

// csrfHeader mirrors httpapi.CSRFHeader.
//
// Spelled out rather than imported, for the reason the CLI spells it out: this
// package is a client of a versioned contract, and sharing constants with the
// server would let the contract move without anybody noticing it had.
const csrfHeader = "X-Trapline-Request"

func encodeBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding the request body: %w", err)
	}
	return encoded, nil
}
