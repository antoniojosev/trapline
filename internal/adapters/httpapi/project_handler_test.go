package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

func TestProjectLifecycleOverHTTP(t *testing.T) {
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	// An empty store must return [], not null: a client should not have to
	// handle two shapes for "nothing here".
	var empty []projectResponse
	client.decode(client.do(http.MethodGet, api("/projects"), nil), &empty)
	if empty == nil {
		t.Error("an empty list decoded as null")
	}
	if len(empty) != 0 {
		t.Errorf("got %d projects from a fresh installation", len(empty))
	}

	response := client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "venekambio"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", response.StatusCode)
	}

	var created projectResponse
	client.decode(response, &created)
	if created.ID <= 0 {
		t.Errorf("ID = %d, want a positive id", created.ID)
	}
	if created.Name != "venekambio" {
		t.Errorf("Name = %q", created.Name)
	}
	if len(created.Keys) != 1 {
		t.Fatalf("got %d keys, want 1 on creation", len(created.Keys))
	}

	// The DSN is the deliverable of this endpoint: it must be complete and
	// parseable, because the next thing that happens to it is a paste into
	// an SDK config.
	if created.DSN == "" {
		t.Fatal("the created project has no DSN")
	}
	dsn, err := domain.ParseDSN(created.DSN)
	if err != nil {
		t.Fatalf("the DSN the API returned does not parse: %v", err)
	}
	if dsn.ProjectID != created.ID {
		t.Errorf("the DSN points at project %d, want %d", dsn.ProjectID, created.ID)
	}
	if dsn.Host != "errors.example.com" {
		t.Errorf("DSN host = %q, want the configured origin", dsn.Host)
	}

	var fetched projectResponse
	client.decode(client.do(http.MethodGet, api("/projects/")+strconv.FormatInt(created.ID, 10), nil), &fetched)
	if fetched.DSN != created.DSN {
		t.Errorf("fetched DSN = %q, want %q", fetched.DSN, created.DSN)
	}

	if got := client.do(http.MethodDelete, api("/projects/")+strconv.FormatInt(created.ID, 10), nil).StatusCode; got != http.StatusOK {
		t.Errorf("delete status = %d, want 200", got)
	}
	if got := client.do(http.MethodGet, api("/projects/")+strconv.FormatInt(created.ID, 10), nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("status after delete = %d, want 404", got)
	}
}

func TestRotationOverHTTP(t *testing.T) {
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	var project projectResponse
	client.decode(client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "rotating"}), &project)
	original := project.Keys[0].PublicKey

	var rotated keyResponse
	response := client.do(http.MethodPost, api("/projects/")+strconv.FormatInt(project.ID, 10)+"/keys", nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("rotate status = %d, want 201", response.StatusCode)
	}
	client.decode(response, &rotated)
	if rotated.PublicKey == original {
		t.Error("rotation returned the same key")
	}

	var afterRotation projectResponse
	client.decode(client.do(http.MethodGet, api("/projects/")+strconv.FormatInt(project.ID, 10), nil), &afterRotation)
	if len(afterRotation.Keys) != 2 {
		t.Fatalf("got %d active keys, want 2 during rotation", len(afterRotation.Keys))
	}
	// The advertised DSN must not jump to the new key while deployments still
	// carry the old one.
	if afterRotation.DSN != project.DSN {
		t.Errorf("the primary DSN changed to %q before the old key was retired", afterRotation.DSN)
	}

	// A third key is refused: two active keys is rotation, more is a pile of
	// live credentials.
	if got := client.do(http.MethodPost, api("/projects/")+strconv.FormatInt(project.ID, 10)+"/keys", nil).StatusCode; got != http.StatusConflict {
		t.Errorf("third key status = %d, want 409", got)
	}

	if got := client.do(http.MethodDelete, api("/keys/")+original, nil).StatusCode; got != http.StatusOK {
		t.Errorf("revoke status = %d, want 200", got)
	}
	// Revoking is idempotent, so a retried command is safe.
	if got := client.do(http.MethodDelete, api("/keys/")+original, nil).StatusCode; got != http.StatusOK {
		t.Errorf("second revoke status = %d, want 200", got)
	}

	var afterRevoke projectResponse
	client.decode(client.do(http.MethodGet, api("/projects/")+strconv.FormatInt(project.ID, 10), nil), &afterRevoke)
	if len(afterRevoke.Keys) != 1 {
		t.Errorf("got %d active keys after completing rotation, want 1", len(afterRevoke.Keys))
	}
}

func TestBadRequests(t *testing.T) {
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"empty project name", http.MethodPost, api("/projects"), createProjectRequest{Name: "  "}, http.StatusBadRequest},
		{"non numeric id", http.MethodGet, api("/projects/abc"), nil, http.StatusBadRequest},
		{"zero id", http.MethodGet, api("/projects/0"), nil, http.StatusBadRequest},
		{"missing project", http.MethodGet, api("/projects/9999"), nil, http.StatusNotFound},
		{"wrong method", http.MethodPut, api("/projects"), nil, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := client.do(tc.method, tc.path, tc.body).StatusCode; got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	// A wrong field name must be an error, not a project called "".
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	if got := client.do(http.MethodPost, api("/projects"), map[string]string{"nombre": "venekambio"}).StatusCode; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

func TestStateChangingRequestsNeedTheCSRFHeader(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	// Without the custom header a browser cannot forge this cross-origin,
	// because there is no permissive CORS policy for the preflight to pass.
	request, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, server.URL+api("/projects"), http.NoBody)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	response, err := client.http.Do(request)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without the %s header", response.StatusCode, CSRFHeader)
	}

	// Safe methods change nothing, so they need no header.
	safe, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, server.URL+api("/projects"), http.NoBody)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	safeResponse, err := client.http.Do(safe)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	defer func() { _ = safeResponse.Body.Close() }()
	if safeResponse.StatusCode != http.StatusOK {
		t.Errorf("GET status = %d, want 200 without the header", safeResponse.StatusCode)
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	client := newClient(t, newTestServer(t))
	response := client.do(http.MethodGet, api("/health"), nil)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, expected := range want {
		if got := response.Header.Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy header")
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	if got := client.do(http.MethodPost, api("/logout"), nil).StatusCode; got != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", got)
	}
	// Server-side sessions exist so this is actually true, rather than the
	// client merely forgetting a token that still works.
	if got := client.do(http.MethodGet, api("/me"), nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want 401", got)
	}
}
