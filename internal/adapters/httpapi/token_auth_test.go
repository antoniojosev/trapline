package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// tokenRequest sends a request authenticated with a bearer token and no
// cookies, which is how the CLI, MCP and any integration talk to the API.
func tokenRequest(t *testing.T, baseURL, token, method, path string, body any) *http.Response {
	t.Helper()

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request: %v", err)
		}
		payload = encoded
	}

	request, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(CSRFHeader, "1")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestBearerTokenAuthenticates(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	_, plaintext, err := tokens.Create(context.Background(), "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// No cookie anywhere: the CLI must be able to do everything the panel can.
	response := tokenRequest(t, server.URL, plaintext, http.MethodPost, api("/projects"),
		createProjectRequest{Name: "desde-la-cli"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", response.StatusCode)
	}

	var created projectResponse
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if created.DSN == "" {
		t.Error("a project created through the API returned no DSN")
	}
	if _, err := domain.ParseDSN(created.DSN); err != nil {
		t.Errorf("the DSN is not parseable: %v", err)
	}
}

func TestReadOnlyTokenCannotWrite(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	_, readOnly, err := tokens.Create(context.Background(), "dashboard", []domain.Scope{domain.ScopeProjectsRead}, nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	if got := tokenRequest(t, server.URL, readOnly, http.MethodGet, api("/projects"), nil).StatusCode; got != http.StatusOK {
		t.Errorf("read status = %d, want 200", got)
	}

	// 403, not 401: the credential is valid, the scope is not. Conflating them
	// would send the holder off to re-authenticate a token that is fine.
	writes := []struct{ method, path string }{
		{http.MethodPost, api("/projects")},
		{http.MethodDelete, api("/projects/1")},
		{http.MethodPost, api("/projects/1/keys")},
	}
	for _, tc := range writes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if got := tokenRequest(t, server.URL, readOnly, tc.method, tc.path, nil).StatusCode; got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

func TestInvalidAndExpiredTokensAreRejected(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	past := time.Now().UTC().Add(time.Millisecond)
	_, expiring, err := tokens.Create(context.Background(), "temporal", domain.AllScopes(), &past)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	cases := map[string]string{
		"unknown token": "ek_no-existe-este-token",
		"empty bearer":  "",
		"expired token": expiring,
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			got := tokenRequest(t, server.URL, token, http.MethodGet, api("/projects"), nil).StatusCode
			if got != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", got)
			}
		})
	}
}

func TestTokenLastUsedIsRecorded(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	ctx := context.Background()

	_, plaintext, err := tokens.Create(ctx, "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	before, err := tokens.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if before[0].LastUsed != nil {
		t.Error("a token that has never authenticated has a last-used time")
	}

	tokenRequest(t, server.URL, plaintext, http.MethodGet, api("/projects"), nil)

	after, err := tokens.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	// This is what lets an operator tell a live integration from a forgotten
	// credential worth revoking.
	if after[0].LastUsed == nil {
		t.Error("last-used was not recorded after an authenticated request")
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)
	ctx := context.Background()

	_, plaintext, err := tokens.Create(ctx, "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	if got := tokenRequest(t, server.URL, plaintext, http.MethodGet, api("/projects"), nil).StatusCode; got != http.StatusOK {
		t.Fatalf("status before revoking = %d, want 200", got)
	}

	if err := tokens.Revoke(ctx, plaintext); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if got := tokenRequest(t, server.URL, plaintext, http.MethodGet, api("/projects"), nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status after revoking = %d, want 401", got)
	}
	// Revoking twice must be safe.
	if err := tokens.Revoke(ctx, plaintext); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
}
