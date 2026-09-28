package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testUser     = "antonio"
	testPassword = "una contraseña larga y buena"
)

// client is a test client that keeps cookies and sends the CSRF header, i.e.
// behaves like the panel.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, server *httptest.Server) *client {
	t.Helper()
	jar := newCookieJar(t)
	return &client{t: t, base: server.URL, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) *http.Response {
	c.t.Helper()

	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encoding request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	request, err := http.NewRequestWithContext(context.Background(), method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(CSRFHeader, "1")

	response, err := c.http.Do(request)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	c.t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (c *client) decode(response *http.Response, target any) {
	c.t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		c.t.Fatalf("decoding response: %v", err)
	}
}

func (c *client) setUpAndLogIn() {
	c.t.Helper()
	if got := c.do(http.MethodPost, api("/setup"), setupRequest{Username: testUser, Password: testPassword}).StatusCode; got != http.StatusCreated {
		c.t.Fatalf("setup returned %d", got)
	}
}

func api(path string) string { return "/api/" + APIVersion + path }

func TestHealthNeedsNoAuth(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)

	response := client.do(http.MethodGet, api("/health"), nil)
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", response.StatusCode)
	}

	// Health is reachable by anyone, so it must not disclose anything.
	var payload map[string]any
	client.decode(response, &payload)
	if len(payload) != 1 || payload["status"] != "ok" {
		t.Errorf("health returned %v, want only a status", payload)
	}
}

func TestSetupThenLoginFlow(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)

	var status struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	client.decode(client.do(http.MethodGet, api("/setup"), nil), &status)
	if !status.NeedsSetup {
		t.Fatal("a fresh installation should report needing setup")
	}

	// Setup logs the new admin straight in: making someone retype the
	// password they just chose is friction with no security value.
	response := client.do(http.MethodPost, api("/setup"), setupRequest{Username: testUser, Password: testPassword})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("setup status = %d, want 201", response.StatusCode)
	}
	if len(response.Cookies()) == 0 {
		t.Error("setup did not set a session cookie")
	}

	var me adminResponse
	client.decode(client.do(http.MethodGet, api("/me"), nil), &me)
	if me.Username != testUser {
		t.Errorf("me.Username = %q, want %q", me.Username, testUser)
	}

	// Setup must close behind itself, or it is open registration.
	if got := client.do(http.MethodPost, api("/setup"), setupRequest{Username: "intruso", Password: testPassword}).StatusCode; got != http.StatusConflict {
		t.Errorf("second setup returned %d, want 409", got)
	}
}

func TestSetupRejectsAWeakPassword(t *testing.T) {
	client := newClient(t, newTestServer(t))

	response := client.do(http.MethodPost, api("/setup"), setupRequest{Username: testUser, Password: "corta"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}

	var body errorBody
	client.decode(response, &body)
	if !strings.Contains(body.Error, "at least") {
		t.Errorf("error = %q, want it to say what the rule is", body.Error)
	}
}

func TestLoginFailureDoesNotDistinguishCause(t *testing.T) {
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	wrongPassword := client.do(http.MethodPost, api("/login"), setupRequest{Username: testUser, Password: "otra cosa"})
	unknownUser := client.do(http.MethodPost, api("/login"), setupRequest{Username: "nadie", Password: testPassword})

	var first, second errorBody
	client.decode(wrongPassword, &first)
	client.decode(unknownUser, &second)

	if wrongPassword.StatusCode != http.StatusUnauthorized || unknownUser.StatusCode != http.StatusUnauthorized {
		t.Errorf("statuses = %d and %d, want both 401", wrongPassword.StatusCode, unknownUser.StatusCode)
	}
	// Identical responses, or the API is an oracle for valid usernames.
	if first.Error != second.Error {
		t.Errorf("errors differ: %q vs %q", first.Error, second.Error)
	}
}

func TestProtectedEndpointsRefuseAnonymousCallers(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	anonymous := newClient(t, server)
	cases := []struct{ method, path string }{
		{http.MethodGet, api("/me")},
		{http.MethodGet, api("/projects")},
		{http.MethodPost, api("/projects")},
		{http.MethodGet, api("/projects/1")},
		{http.MethodDelete, api("/projects/1")},
		{http.MethodPost, api("/projects/1/keys")},
		{http.MethodDelete, api("/keys/abababababababababababababababab")},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if got := anonymous.do(tc.method, tc.path, nil).StatusCode; got != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", got)
			}
		})
	}
}
