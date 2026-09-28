package httpapi

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// The status page is the one surface here that answers an anonymous caller, so
// every test below reads it with a plain client and no credential of any kind.

func statusPageServer(t *testing.T) (baseURL, token string, projectID int64, slug string) {
	t.Helper()
	baseURL, token, projectID = newUptimeServer(t)
	// newUptimeServer names its project "venekambio", which slugifies to
	// itself; asserting it here means a change to Slugify fails in this file
	// rather than somewhere further away.
	return baseURL, token, projectID, "venekambio"
}

func publishStatusPage(t *testing.T, baseURL, token string, projectID int64, enabled bool) {
	t.Helper()
	response := tokenRequest(t, baseURL, token, http.MethodPut,
		api("/projects/"+strconv.FormatInt(projectID, 10)+"/config"),
		map[string]any{"status_page": map[string]any{"enabled": enabled}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("configuring the status page answered %d", response.StatusCode)
	}
	_ = response.Body.Close()
}

func addStatusMonitor(t *testing.T, baseURL, token string, projectID int64, name string, public bool) int64 {
	t.Helper()
	response := tokenRequest(t, baseURL, token, http.MethodPost, projectMonitorsPath(projectID),
		map[string]any{"name": name, "url": publicTarget, "public": public})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("creating %s answered %d: %s", name, response.StatusCode, errorMessage(t, response))
	}
	var monitor struct {
		ID int64 `json:"id"`
	}
	decodeBody(t, response, &monitor)
	return monitor.ID
}

// anonymousGet reads a URL the way a stranger with a link would: no session,
// no bearer token, no CSRF header. That is the whole contract of this page.
func anonymousGet(t *testing.T, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("building a request for %s: %v", url, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return response
}

func getStatus(t *testing.T, baseURL, slug string) (response *http.Response, page string) {
	t.Helper()
	response = anonymousGet(t, baseURL+"/status/"+slug)
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("reading the page: %v", err)
	}
	return response, string(body)
}

func TestStatusPageIsAbsentUntilTheProjectPublishesOne(t *testing.T) {
	baseURL, _, _, slug := statusPageServer(t)

	response, _ := getStatus(t, baseURL, slug)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unpublished project answered %d, want 404", response.StatusCode)
	}
	// And the page a project never had must be indistinguishable from a
	// project that does not exist: the difference is the operator's choice not
	// to publish, which is not a fact an anonymous reader is owed.
	missing, _ := getStatus(t, baseURL, "no-such-project")
	if missing.StatusCode != response.StatusCode {
		t.Errorf("an unknown project answered %d and an unpublished one %d",
			missing.StatusCode, response.StatusCode)
	}
}

func TestStatusPageShowsPublicMonitorsAndNotPrivateOnes(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	addStatusMonitor(t, baseURL, token, projectID, "public checkout", true)
	addStatusMonitor(t, baseURL, token, projectID, "internal billing", false)
	publishStatusPage(t, baseURL, token, projectID, true)

	response, body := getStatus(t, baseURL, slug)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a published page answered %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("content type = %q, want HTML", got)
	}
	if !strings.Contains(body, "public checkout") {
		t.Errorf("the page does not show the public monitor:\n%s", body)
	}
	// The one that matters. A monitor an operator did not mark public is the
	// name of an internal service, and this page is read by anybody with the
	// link.
	if strings.Contains(body, "internal billing") {
		t.Errorf("the page leaks a private monitor:\n%s", body)
	}
	// And no script tag: the page has to work with JavaScript off, which is
	// the whole of ADR 017's "no React".
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Errorf("the status page carries JavaScript:\n%s", body)
	}
}

func TestStatusPageIsCacheable(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	publishStatusPage(t, baseURL, token, projectID, true)

	response, _ := getStatus(t, baseURL, slug)
	cacheControl := response.Header.Get("Cache-Control")
	if !strings.Contains(cacheControl, "public") || !strings.Contains(cacheControl, "max-age=30") {
		t.Errorf("Cache-Control = %q, want a public thirty-second window", cacheControl)
	}
	// A 404 must not be cached: the page somebody just published has to
	// appear, and a proxy holding "not found" for half a minute is how that
	// looks broken.
	missing, _ := getStatus(t, baseURL, "no-such-project")
	if missing.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a 404 is cached as %q", missing.Header.Get("Cache-Control"))
	}
}

// Switching the page off has to take effect now, not in thirty seconds. The
// cache is keyed by slug and dropped on the write that changes it.
func TestUnpublishingTakesEffectImmediately(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	publishStatusPage(t, baseURL, token, projectID, true)
	if response, _ := getStatus(t, baseURL, slug); response.StatusCode != http.StatusOK {
		t.Fatalf("the published page answered %d", response.StatusCode)
	}

	publishStatusPage(t, baseURL, token, projectID, false)
	response, _ := getStatus(t, baseURL, slug)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("an unpublished page still answers %d from the cache", response.StatusCode)
	}
}

func TestStatusPageHeadingComesFromSettings(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	publishStatusPage(t, baseURL, token, projectID, true)

	// The default is built from the project's name.
	if _, body := getStatus(t, baseURL, slug); !strings.Contains(body, "venekambio status") {
		t.Errorf("the default heading is not the project's name:\n%s", body)
	}

	response := tokenRequest(t, baseURL, token, http.MethodPut, api("/system/settings/status-page"),
		map[string]any{"title": "Acme systems", "description": "live service status"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("writing the settings answered %d: %s", response.StatusCode, errorMessage(t, response))
	}
	_ = response.Body.Close()

	_, body := getStatus(t, baseURL, slug)
	if !strings.Contains(body, "Acme systems") || !strings.Contains(body, "live service status") {
		t.Errorf("the configured heading is not on the page:\n%s", body)
	}

	read := tokenRequest(t, baseURL, token, http.MethodGet, api("/system/settings/status-page"), nil)
	var settings struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	decodeBody(t, read, &settings)
	if settings.Title != "Acme systems" {
		t.Errorf("reading the settings back gave %+v", settings)
	}
}

func TestStatusPageSettingsAreBounded(t *testing.T) {
	baseURL, token, _, _ := statusPageServer(t)
	response := tokenRequest(t, baseURL, token, http.MethodPut, api("/system/settings/status-page"),
		map[string]any{"title": strings.Repeat("a", domain.MaxStatusPageTitle+1)})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("an over-long title answered %d, want 400", response.StatusCode)
	}
}

// A path under /status/ must never fall through to the panel's single-page
// app: a project slug with a typo would then answer 200 with the panel's HTML,
// and the reader would conclude the status page is an error tracker.
func TestStatusPathNeverFallsThroughToThePanel(t *testing.T) {
	server := newTestServer(t)

	for _, path := range []string{"/status/", "/status/nope", "/status/a/b"} {
		response := anonymousGet(t, server.URL+path)
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, response.StatusCode)
		}
		if strings.Contains(string(body), "<div id=\"root\"") {
			t.Errorf("%s served the panel", path)
		}
	}
}

// The per-address ceiling has to be high enough for a real page. Two hundred
// consecutive reads is a browser reloading during an incident plus everybody
// else on the same office NAT, and the gate makes the same claim end to end.
func TestStatusPageSurvivesTwoHundredRequests(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	addStatusMonitor(t, baseURL, token, projectID, "checkout", true)
	publishStatusPage(t, baseURL, token, projectID, true)

	for i := range 200 {
		response := anonymousGet(t, baseURL+"/status/"+slug)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d answered %d", i, response.StatusCode)
		}
	}
}

// The panel and the CLI read the same project config, so the toggle has to
// come back out of it — otherwise the settings screen cannot show its own
// state (ADR 006).
func TestProjectConfigCarriesTheStatusPageToggle(t *testing.T) {
	baseURL, token, projectID, _ := statusPageServer(t)
	publishStatusPage(t, baseURL, token, projectID, true)

	response := tokenRequest(t, baseURL, token, http.MethodGet,
		api("/projects/"+strconv.FormatInt(projectID, 10)+"/config"), nil)
	var config struct {
		StatusPage struct {
			Enabled bool `json:"enabled"`
		} `json:"status_page"`
	}
	decodeBody(t, response, &config)
	if !config.StatusPage.Enabled {
		t.Error("the project config does not report the status page as enabled")
	}
}

// A monitor that has never been checked must not be reported as fine. The page
// says "no data yet" and counts it as not operational.
func TestAMonitorWithNoChecksIsNotOperational(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	addStatusMonitor(t, baseURL, token, projectID, "checkout", true)
	publishStatusPage(t, baseURL, token, projectID, true)

	_, body := getStatus(t, baseURL, slug)
	if strings.Contains(body, "All systems operational") {
		t.Errorf("a monitor nobody has checked is reported as operational:\n%s", body)
	}
	if !strings.Contains(body, "No data yet") {
		t.Errorf("the page does not say the monitor has no data:\n%s", body)
	}
}

func TestStatusPageEscapesWhatAnOperatorTyped(t *testing.T) {
	baseURL, token, projectID, slug := statusPageServer(t)
	addStatusMonitor(t, baseURL, token, projectID, `<img src=x onerror=alert(1)>`, true)
	publishStatusPage(t, baseURL, token, projectID, true)

	_, body := getStatus(t, baseURL, slug)
	if strings.Contains(body, "<img src=x") {
		t.Errorf("a monitor name reached the page unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("the name is not on the page at all:\n%s", body)
	}
}

// The public page is anonymous; configuring it is not.
func TestStatusPageSettingsNeedTheWriteScope(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	anonymous := anonymousGet(t, server.URL+api("/system/settings/status-page"))
	_ = anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated read answered %d, want 401", anonymous.StatusCode)
	}

	_, readOnly, err := tokens.Create(context.Background(), "dashboard",
		[]domain.Scope{domain.ScopeProjectsRead}, nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	response := tokenRequest(t, server.URL, readOnly, http.MethodGet,
		api("/system/settings/status-page"), nil)
	if response.StatusCode != http.StatusOK {
		t.Errorf("projects:read could not read the heading: %d", response.StatusCode)
	}
	_ = response.Body.Close()

	response = tokenRequest(t, server.URL, readOnly, http.MethodPut,
		api("/system/settings/status-page"), map[string]any{"title": "nope"})
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("projects:read wrote the heading: %d", response.StatusCode)
	}
	_ = response.Body.Close()
}
