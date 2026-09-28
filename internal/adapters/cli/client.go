package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrNoToken means no API token was supplied for a command that talks to a
// server.
var ErrNoToken = errors.New("no API token: set TRAPLINE_TOKEN or pass -token (create one with `trapline token create`)")

// apiClient talks to a running server.
//
// The CLI is a client of the same REST API as the panel, not a second
// implementation reaching into the database. That is what makes it able to
// administer a server it is not running on, and what guarantees it cannot
// drift from what the panel can do (ADR 006).
type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newAPIClient(baseURL, token string) (*apiClient, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	return &apiClient{
		baseURL: baseURL,
		token:   token,
		// A timeout, not the zero value: a CLI that hangs forever against an
		// unreachable server is worse than one that fails.
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// apiError is a failure reported by the server.
type apiError struct {
	StatusCode int
	Message    string
}

func (e apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server returned %d", e.StatusCode)
	}
	return e.Message
}

// do sends a request and decodes the response into target, which may be nil.
func (c *apiClient) do(ctx context.Context, method, path string, body, target any) error {
	var payload io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+apiPath(path), payload)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	// The API requires this header on state-changing requests as its CSRF
	// defence. A CLI is not a browser and was never at risk, but sending it
	// keeps one rule on the server instead of one rule per kind of client.
	request.Header.Set(csrfHeader, "1")

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("calling %s: %w", c.baseURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusBadRequest {
		return decodeAPIError(response)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// upload sends bytes that are not JSON.
//
// One endpoint needs it: an artefact bundle is an archive, and wrapping a
// binary in a JSON string would base64 it — a third more bytes on the wire and
// a second encoding of something the server already knows how to read.
//
// It shares everything else with do, including the CSRF header, so there is
// one rule on the server rather than one per kind of client.
func (c *apiClient) upload(
	ctx context.Context, path string, contentType string, body []byte, target any,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+apiPath(path), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set(csrfHeader, "1")

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("calling %s: %w", c.baseURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusBadRequest {
		return decodeAPIError(response)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// fetch reads a body that is not JSON.
//
// One endpoint needs it: the issue bundle is a markdown document, and
// decoding it as JSON would fail on the first character. It shares
// everything else with do — the bearer token, the CSRF header, the error
// shape — so there is one client here and not two.
func (c *apiClient) fetch(ctx context.Context, path, accept string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+apiPath(path), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", accept)
	request.Header.Set(csrfHeader, "1")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", c.baseURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusBadRequest {
		return nil, decodeAPIError(response)
	}
	// Bounded like every other read from a server this process does not own.
	// A bundle is tens of kilobytes; a megabyte is far more than the endpoint
	// can produce and is here so a misconfigured URL cannot fill memory.
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return body, nil
}

func decodeAPIError(response *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	// A body that is not the documented error shape is not worth reporting
	// over the status code, which is always meaningful.
	_ = json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&body)
	return apiError{StatusCode: response.StatusCode, Message: body.Error}
}

func apiPath(path string) string { return "/api/" + apiVersion + path }
