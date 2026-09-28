package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestPanelIsServed(t *testing.T) {
	client := newClient(t, newTestServer(t))

	response := client.do(http.MethodGet, "/", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", got)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), `id="root"`) {
		t.Error("the served page is not the panel")
	}
}

func TestPanelServesItsAssets(t *testing.T) {
	client := newClient(t, newTestServer(t))

	for _, asset := range []string{"/assets/panel.js", "/assets/panel.css"} {
		t.Run(asset, func(t *testing.T) {
			response := client.do(http.MethodGet, asset, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			if response.Header.Get("Cache-Control") == "" {
				t.Error("no Cache-Control on an asset")
			}
		})
	}
}

func TestUnknownPathsFallBackToThePanel(t *testing.T) {
	// Without this, a reload on any route but "/" is a 404 — the classic
	// single-page-app deployment bug.
	client := newClient(t, newTestServer(t))

	response := client.do(http.MethodGet, "/projects/42/settings", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), `id="root"`) {
		t.Error("an unknown path did not fall back to the panel")
	}
}

func TestThePanelDoesNotShadowTheAPI(t *testing.T) {
	// The panel is mounted on "/", so this is the mistake that would break
	// every endpoint at once while the page still looked fine.
	client := newClient(t, newTestServer(t))

	response := client.do(http.MethodGet, api("/health"), nil)
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON — the panel is shadowing the API", got)
	}

	// Unknown paths and wrong methods under the API prefix must answer in the
	// API's own error shape, not with the panel and not with net/http's
	// plain-text default. These two responses come from the router rather than
	// from a handler, so they are the easiest place for the contract to leak.
	cases := map[string]struct {
		method, path string
		wantStatus   int
	}{
		"unknown path": {http.MethodGet, api("/does-not-exist"), http.StatusNotFound},
		"wrong method": {http.MethodPut, api("/projects"), http.StatusMethodNotAllowed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			response := client.do(tc.method, tc.path, nil)
			if response.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", response.StatusCode, tc.wantStatus)
			}
			if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", got)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if strings.Contains(string(body), `id="root"`) {
				t.Error("the panel HTML was returned for an API path")
			}
			var payload struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("body is not the API error shape (%v): %q", err, body)
			}
			if payload.Error == "" {
				t.Error("the error shape has no message")
			}
		})
	}
}

func TestTheAPIIsSnakeCaseThroughout(t *testing.T) {
	// Found by the Python compatibility suite: ports.TagCount had no JSON
	// tags, so one corner of the issue-detail response came back PascalCase
	// while everything around it was snake_case. A client should not have to
	// remember which endpoint changes convention.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")

	issues := listIssues(t, client, dsn.ProjectID)
	if len(issues) == 0 {
		t.Fatal("no issue to inspect")
	}

	path := api("/projects/") + strconv.FormatInt(dsn.ProjectID, 10) +
		"/issues/" + strconv.FormatInt(issues[0].ID, 10)
	body, err := io.ReadAll(client.do(http.MethodGet, path, nil).Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	var offenders []string
	walkKeys(decoded, func(key string) {
		if key != strings.ToLower(key) {
			offenders = append(offenders, key)
		}
	})
	if len(offenders) > 0 {
		t.Errorf("non-lowercase JSON keys in the response: %v", offenders)
	}
}

// walkKeys visits every object key this API is responsible for, and stops at
// the stored event payload.
//
// The payload is the user's own data, reproduced as the SDK sent it: an HTTP
// request's "Authorization" and "Accept" headers keep their casing because
// rewriting them would be corrupting the evidence. The convention applies to
// the fields this product invents, not to the ones it is faithfully carrying.
func walkKeys(value any, visit func(string)) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			visit(key)
			if key == "payload" {
				continue
			}
			walkKeys(nested, visit)
		}
	case []any:
		for _, nested := range typed {
			walkKeys(nested, visit)
		}
	}
}
