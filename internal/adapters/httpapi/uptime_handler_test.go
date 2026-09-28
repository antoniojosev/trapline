package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// Every URL here is an IP literal rather than a host name, deliberately: the
// guard resolves what it is given, and a test that named example.com would be
// a test of whatever DNS the machine running it happens to have.
const (
	publicTarget    = "http://93.184.216.34/health"
	loopbackTarget  = "http://127.0.0.1:1/"
	privateTarget   = "http://10.1.2.3/health"
	metadataTarget  = "http://169.254.169.254/latest/meta-data/"
	monitorsAPIPath = "/monitors/uptime/"
)

func newUptimeServer(t *testing.T) (baseURL, token string, projectID int64) {
	t.Helper()

	server, tokens := newTestServerWithTokens(t)
	_, plaintext, err := tokens.Create(context.Background(), "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	response := tokenRequest(t, server.URL, plaintext, http.MethodPost, api("/projects"),
		createProjectRequest{Name: "venekambio"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("creating a project answered %d", response.StatusCode)
	}
	var project struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatalf("decoding the project: %v", err)
	}
	return server.URL, plaintext, project.ID
}

func projectMonitorsPath(projectID int64) string {
	return api("/projects/" + strconv.FormatInt(projectID, 10) + "/monitors/uptime")
}

func decodeBody(t *testing.T, response *http.Response, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
}

func errorMessage(t *testing.T, response *http.Response) string {
	t.Helper()
	var body errorBody
	decodeBody(t, response, &body)
	return body.Error
}

func TestCreateAndReadAnUptimeMonitor(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "api", URL: publicTarget})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create answered %d: %s", response.StatusCode, errorMessage(t, response))
	}
	var created uptimeMonitorResponse
	decodeBody(t, response, &created)

	// The defaults are the domain's, and they arrive filled in rather than as
	// zeros somebody has to interpret.
	if created.Method != "GET" || created.IntervalSeconds != 60 || created.TimeoutSeconds != 10 {
		t.Errorf("defaults did not arrive: %+v", created)
	}
	if created.ExpectedStatusMin != 200 || created.ExpectedStatusMax != 299 {
		t.Errorf("expected status = %d–%d", created.ExpectedStatusMin, created.ExpectedStatusMax)
	}
	if created.Status != domain.UptimeUnknown {
		t.Errorf("a monitor was born %q rather than unknown", created.Status)
	}
	if !created.Enabled || !created.FollowRedirects {
		t.Error("a monitor nobody switched off arrived switched off")
	}

	response = tokenRequest(t, baseURL, token, http.MethodGet,
		api(monitorsAPIPath+strconv.FormatInt(created.ID, 10)), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read answered %d", response.StatusCode)
	}

	response = tokenRequest(t, baseURL, token, http.MethodGet, projectMonitorsPath(projectID), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list answered %d", response.StatusCode)
	}
	var page struct {
		Monitors []uptimeMonitorResponse `json:"monitors"`
	}
	decodeBody(t, response, &page)
	if len(page.Monitors) != 1 {
		t.Fatalf("listed %d monitors", len(page.Monitors))
	}

	response = tokenRequest(t, baseURL, token, http.MethodDelete,
		api(monitorsAPIPath+strconv.FormatInt(created.ID, 10)), nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete answered %d", response.StatusCode)
	}
	response = tokenRequest(t, baseURL, token, http.MethodGet,
		api(monitorsAPIPath+strconv.FormatInt(created.ID, 10)), nil)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted monitor answered %d", response.StatusCode)
	}
}

// TestARefusedTargetIs422 is the gate's assertion, at this layer. 422 rather
// than 400: the request was understood, and what is being refused is the
// target.
func TestARefusedTargetIs422(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	cases := []struct {
		name     string
		url      string
		contains string
	}{
		{"loopback", loopbackTarget, "loopback"},
		{"a private address", privateTarget, "private"},
		{"the cloud metadata endpoint", metadataTarget, "link-local"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
				uptimeMonitorRequest{Name: tc.name, URL: tc.url})
			if response.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%s answered %d, want 422", tc.url, response.StatusCode)
			}
			message := errorMessage(t, response)
			if !strings.Contains(message, tc.contains) {
				t.Errorf("the message does not name the category: %q", message)
			}
			// And it says what would have to change, which is the difference
			// between a refusal and a wall.
			if !strings.Contains(message, "-uptime-allow-private") {
				t.Errorf("the message does not say how to allow it: %q", message)
			}
		})
	}
}

// TestTheMonitorOptInAloneIsNotEnough: this server was not started with
// -uptime-allow-private, so a monitor asking for a private target is still
// refused — and the message says which half is missing.
func TestTheMonitorOptInAloneIsNotEnough(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "internal", URL: privateTarget, AllowPrivate: true})
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("answered %d, want 422", response.StatusCode)
	}
	message := errorMessage(t, response)
	if !strings.Contains(message, "the server was not started with -uptime-allow-private") {
		t.Errorf("the message does not name the missing half: %q", message)
	}
}

// TestAnIntervalBelowTheFloorIs400: a bad number is a bad request, and it is
// a different answer from a refused target on purpose.
func TestAnIntervalBelowTheFloorIsRejected(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "too eager", URL: publicTarget, IntervalSeconds: 29})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400", response.StatusCode)
	}
	if message := errorMessage(t, response); !strings.Contains(message, "interval_s must be between") {
		t.Errorf("the message does not say what the floor is: %q", message)
	}
}

func TestAnUnsafeMethodIsRejected(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "poster", URL: publicTarget, Method: "POST"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400", response.StatusCode)
	}
}

func TestASchemeThatIsNotHTTPIsRejected(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "passwd", URL: "file:///etc/passwd"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400", response.StatusCode)
	}
	if message := errorMessage(t, response); !strings.Contains(message, "http or https") {
		t.Errorf("the message is %q", message)
	}
}

func TestSwitchingAMonitorOffKeepsIt(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "api", URL: publicTarget})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create answered %d", response.StatusCode)
	}
	var created uptimeMonitorResponse
	decodeBody(t, response, &created)

	path := api(monitorsAPIPath + strconv.FormatInt(created.ID, 10) + "/enabled")
	response = tokenRequest(t, baseURL, token, http.MethodPost, path, map[string]any{"enabled": false})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("switching off answered %d", response.StatusCode)
	}
	var switched uptimeMonitorResponse
	decodeBody(t, response, &switched)
	if switched.Enabled {
		t.Error("the monitor came back enabled")
	}
	if switched.ID != created.ID {
		t.Error("switching a monitor off replaced it")
	}
}

func TestResultsAndDailyOfAFreshMonitor(t *testing.T) {
	baseURL, token, projectID := newUptimeServer(t)

	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		uptimeMonitorRequest{Name: "api", URL: publicTarget})
	var created uptimeMonitorResponse
	decodeBody(t, response, &created)

	base := api(monitorsAPIPath + strconv.FormatInt(created.ID, 10))

	response = tokenRequest(t, baseURL, token, http.MethodGet, base+"/results", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("results answered %d", response.StatusCode)
	}
	var results struct {
		Results []checkResponse `json:"results"`
	}
	decodeBody(t, response, &results)
	// An empty list, not null: a client that has to special-case null for
	// "nothing yet" is a client this API made harder to write.
	if results.Results == nil {
		t.Error("results came back as null rather than an empty list")
	}

	response = tokenRequest(t, baseURL, token, http.MethodGet, base+"/daily", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("daily answered %d", response.StatusCode)
	}

	response = tokenRequest(t, baseURL, token, http.MethodGet, base+"/results?limit=nonsense", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("a nonsense limit answered %d", response.StatusCode)
	}
	response = tokenRequest(t, baseURL, token, http.MethodGet, base+"/daily?days=0", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("days=0 answered %d", response.StatusCode)
	}
}

// TestMonitorRoutesDemandTheirOwnScope: `monitors:write` is the permission to
// make this installation issue outbound requests of the caller's choosing, and
// a token minted for a dashboard must not carry it.
func TestMonitorRoutesDemandTheirOwnScope(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	_, wide, err := tokens.Create(context.Background(), "admin", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	response := tokenRequest(t, server.URL, wide, http.MethodPost, api("/projects"),
		createProjectRequest{Name: "venekambio"})
	var project struct {
		ID int64 `json:"id"`
	}
	decodeBody(t, response, &project)

	_, narrow, err := tokens.Create(context.Background(), "dashboard",
		[]domain.Scope{domain.ScopeProjectsRead, domain.ScopeProjectsWrite}, nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	response = tokenRequest(t, server.URL, narrow, http.MethodPost, projectMonitorsPath(project.ID),
		uptimeMonitorRequest{Name: "api", URL: publicTarget})
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("a projects:* token created a monitor: %d", response.StatusCode)
	}

	response = tokenRequest(t, server.URL, narrow, http.MethodGet, projectMonitorsPath(project.ID), nil)
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("a projects:* token read the monitors: %d", response.StatusCode)
	}

	// And a read-only monitors token can look but not touch.
	_, readOnly, err := tokens.Create(context.Background(), "status",
		[]domain.Scope{domain.ScopeMonitorsRead}, nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	response = tokenRequest(t, server.URL, readOnly, http.MethodGet, projectMonitorsPath(project.ID), nil)
	if response.StatusCode != http.StatusOK {
		t.Errorf("monitors:read could not list: %d", response.StatusCode)
	}
	response = tokenRequest(t, server.URL, readOnly, http.MethodPost, projectMonitorsPath(project.ID),
		uptimeMonitorRequest{Name: "api", URL: publicTarget})
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("monitors:read created a monitor: %d", response.StatusCode)
	}
}
