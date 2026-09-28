package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The browser suite in the compatibility matrix found what these tests now
// hold in place: a browser sends a cross-origin POST whether or not the server
// allows the origin, and only blocks the page from reading the answer. So the
// symptom of having no policy is not a failed request. It is a server that
// looks healthy while every SDK in every browser is deaf to it.

func TestIngestAnswersAnyOrigin(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	response := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ingest status = %d, want 200", response.StatusCode)
	}
	if origin := response.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q: without it a browser will not let "+
			"the SDK read this response at all", origin, "*")
	}
}

// TestIngestExposesTheRateLimitHeaders is the one that matters most.
//
// A browser hides every response header a server does not explicitly expose.
// X-Sentry-Rate-Limits is how this server tells an SDK to stop sending a
// category it has switched off, and that instruction is the entire mechanism
// behind "a subsystem that is off costs nothing" (ADR 005). Unexposed, the
// header is sent and never read, and the SDK keeps paying full price on the
// wire for a category nothing will store.
func TestIngestExposesTheRateLimitHeaders(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// Sessions are off on a new project, so this envelope is refused by
	// design and the answer carries the backpressure headers.
	body := fmt.Sprintf("{\"dsn\":%q}\n{\"type\":\"session\"}\n{\"sid\":\"abc\",\"status\":\"ok\"}\n", dsn.String())
	response := sendEnvelope(t, server.URL, dsn, body, "")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a session envelope on an errors-only project answered %d, want 429",
			response.StatusCode)
	}
	if response.Header.Get("X-Sentry-Rate-Limits") == "" {
		t.Fatal("the 429 carried no X-Sentry-Rate-Limits header")
	}

	exposed := response.Header.Get("Access-Control-Expose-Headers")
	for _, header := range []string{"X-Sentry-Rate-Limits", "Retry-After"} {
		if !strings.Contains(exposed, header) {
			t.Errorf("Access-Control-Expose-Headers = %q, which does not include %q: a browser "+
				"would hide it and the SDK could not honour the limit", exposed, header)
		}
	}
}

// TestIngestAnswersThePreflight covers the SDK configurations that do provoke
// one — a custom header, an explicit content type, a tunnel. A 405 here stops
// every event from that page before any of them reach this server to be
// logged, which is a failure with no trace on either side.
func TestIngestAnswersThePreflight(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	url := fmt.Sprintf("%s/api/%d/envelope/", server.URL, dsn.ProjectID)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodOptions, url, http.NoBody)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Origin", "https://app.example.com")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "x-sentry-auth")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("sending preflight: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200", response.StatusCode)
	}
	if origin := response.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("preflight Access-Control-Allow-Origin = %q, want %q", origin, "*")
	}
	if !strings.Contains(response.Header.Get("Access-Control-Allow-Methods"), http.MethodPost) {
		t.Errorf("preflight does not allow POST: %q", response.Header.Get("Access-Control-Allow-Methods"))
	}
	allowed := strings.ToLower(response.Header.Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allowed, "x-sentry-auth") {
		t.Errorf("preflight does not allow the protocol's auth header: %q", allowed)
	}
}

// TestTheIngestPolicyStopsAtIngest is the other half of the decision.
//
// Allowing any origin is safe on ingest because there is no ambient
// credential: the caller must present a key, and that key already ships inside
// public browser bundles. The panel's API is the opposite — it is
// authenticated by a session cookie a browser attaches on its own — so the
// same policy there would hand any page on the internet a logged-in client's
// projects and issues.
func TestTheIngestPolicyStopsAtIngest(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	response := client.do(http.MethodGet, api("/projects"), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("listing projects: status %d", response.StatusCode)
	}
	if origin := response.Header.Get("Access-Control-Allow-Origin"); origin != "" {
		t.Errorf("the panel's API allows origin %q; a cookie-authenticated API must allow none",
			origin)
	}
	if exposed := response.Header.Get("Access-Control-Expose-Headers"); exposed != "" {
		t.Errorf("the panel's API exposes headers cross-origin: %q", exposed)
	}
}
