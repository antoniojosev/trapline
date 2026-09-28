package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/clientip"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

func cronPath(projectID int64, suffix string) string {
	return api("/projects/" + strconv.FormatInt(projectID, 10) + "/monitors/cron" + suffix)
}

// newProjectAndClient sets the installation up and returns a project to hang
// monitors off.
func newProjectAndClient(t *testing.T) (panel *client, projectID int64, baseURL string) {
	t.Helper()
	server := newTestServer(t)
	panel = newClient(t, server)
	panel.setUpAndLogIn()

	var project projectResponse
	panel.decode(panel.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "venekambio"}), &project)
	return panel, project.ID, server.URL
}

func createMonitor(t *testing.T, client *client, projectID int64, request *monitorRequest) monitorResponse {
	t.Helper()
	response := client.do(http.MethodPost, cronPath(projectID, ""), request)
	if response.StatusCode != http.StatusCreated {
		var body errorBody
		client.decode(response, &body)
		t.Fatalf("creating a monitor answered %d: %s", response.StatusCode, body.Error)
	}
	var monitor monitorResponse
	client.decode(response, &monitor)
	return monitor
}

func TestAMonitorIsCreatedWithAPingURL(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)

	monitor := createMonitor(t, client, projectID, &monitorRequest{
		Slug: "nightly-backup", Schedule: "0 3 * * *", Timezone: "America/Caracas",
	})
	switch {
	case monitor.ID == 0:
		t.Fatal("the monitor came back without an id")
	case len(monitor.PingKey) != 32:
		t.Fatalf("ping key %q is not 32 hex characters", monitor.PingKey)
	case !strings.HasSuffix(monitor.PingURL, "/ping/"+monitor.PingKey):
		t.Fatalf("ping url %q does not end in the key", monitor.PingURL)
	case !strings.HasPrefix(monitor.PingURL, "https://errors.example.com/"):
		// Built from the configured origin, never from the request: a URL
		// pasted into a crontab has to be the public one.
		t.Fatalf("ping url %q was not built from the configured origin", monitor.PingURL)
	case monitor.Status != "unknown":
		t.Fatalf("a brand-new monitor is %q and must be unknown", monitor.Status)
	case monitor.NextExpectedAt == nil:
		t.Fatal("a new monitor has no deadline")
	case monitor.Timezone != "America/Caracas":
		t.Fatalf("timezone = %q", monitor.Timezone)
	}
}

func TestAnInvalidScheduleIsRejectedWithAReason(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)

	response := client.do(http.MethodPost, cronPath(projectID, ""), &monitorRequest{
		Slug: "backup", Schedule: "every night please",
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a nonsense schedule answered %d, want 400", response.StatusCode)
	}
	var body errorBody
	client.decode(response, &body)
	if !strings.Contains(body.Error, "five fields") {
		t.Fatalf("the error is %q, which does not say what to fix", body.Error)
	}
}

func TestTwoMonitorsCannotShareASlug(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})

	response := client.do(http.MethodPost, cronPath(projectID, ""),
		&monitorRequest{Slug: "backup", Schedule: "@hourly"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a duplicate slug answered %d, want 400", response.StatusCode)
	}
}

// A monitor id from another project must not be reachable through this
// project's URL, or the project is a label rather than a boundary.
func TestAMonitorIsNotReadableThroughAnotherProject(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})

	var other projectResponse
	client.decode(client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "otra"}), &other)

	response := client.do(http.MethodGet,
		cronPath(other.ID, "/"+strconv.FormatInt(monitor.ID, 10)), nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("reading it through another project answered %d, want 404", response.StatusCode)
	}
}

func TestAMonitorIsUpdatedInPlace(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})
	path := cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10))

	var updated monitorResponse
	client.decode(client.do(http.MethodPut, path, map[string]any{
		"schedule": "@hourly", "enabled": false,
	}), &updated)

	switch {
	case updated.Schedule != "@hourly":
		t.Fatalf("schedule = %q", updated.Schedule)
	case updated.Enabled:
		t.Fatal("the monitor is still enabled")
	case updated.PingKey != monitor.PingKey:
		// Editing a schedule must not invalidate the key at the end of
		// somebody's crontab line.
		t.Fatal("the ping key changed when the schedule did")
	case updated.Timezone != monitor.Timezone:
		t.Fatalf("an untouched field changed: timezone = %q", updated.Timezone)
	}
}

func TestAMonitorIsRemoved(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})
	path := cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10))

	if got := client.do(http.MethodDelete, path, nil).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete answered %d", got)
	}
	if got := client.do(http.MethodGet, path, nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("reading a deleted monitor answered %d", got)
	}
}

// The whole feature, through the surface it exists for: a URL, no
// authentication, one line of text back.
func TestAPingIsOneLineOfTextAndNeedsNoAuthentication(t *testing.T) {
	client, projectID, base := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "*/1 * * * *"})

	// A bare HTTP client: no cookie jar, no CSRF header, no token. This is
	// what a crontab line has.
	body, response := getPlain(t, base+"/ping/"+monitor.PingKey)
	switch {
	case response.StatusCode != http.StatusOK:
		t.Fatalf("ping answered %d", response.StatusCode)
	case !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain"):
		t.Fatalf("content type is %q", response.Header.Get("Content-Type"))
	case body != "ok backup\n":
		t.Fatalf("body = %q", body)
	case response.Header.Get("Cache-Control") != "no-store":
		t.Fatal("a ping is cacheable, so a proxy could report a job as having run")
	}

	// And the monitor moved.
	var read monitorResponse
	client.decode(client.do(http.MethodGet,
		cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)), nil), &read)
	if read.Status != "ok" {
		t.Fatalf("status after a ping = %q", read.Status)
	}
	if read.LastCheckinAt == nil {
		t.Fatal("the monitor was never marked as heard from")
	}

	// A start opens a run, which the history shows as still running.
	if _, response := getPlain(t, base+"/ping/"+monitor.PingKey+"/start"); response.StatusCode != http.StatusOK {
		t.Fatalf("start answered %d", response.StatusCode)
	}
	var page struct {
		CheckIns []checkInResponse `json:"checkins"`
	}
	client.decode(client.do(http.MethodGet,
		cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)+"/checkins"), nil), &page)
	if len(page.CheckIns) != 2 {
		t.Fatalf("the history has %d rows, want 2", len(page.CheckIns))
	}
	if page.CheckIns[0].Status != "in_progress" || page.CheckIns[0].FinishedAt != nil {
		t.Fatalf("the newest check-in is %+v, want an open run", page.CheckIns[0])
	}

	// A failure closes it, and the duration is derived from the two.
	if _, response := getPlain(t, base+"/ping/"+monitor.PingKey+"/fail"); response.StatusCode != http.StatusOK {
		t.Fatalf("fail answered %d", response.StatusCode)
	}
	client.decode(client.do(http.MethodGet,
		cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)+"/checkins"), nil), &page)
	if len(page.CheckIns) != 2 {
		t.Fatalf("a finish opened a third row instead of closing the open one: %d rows", len(page.CheckIns))
	}
	if page.CheckIns[0].Status != "error" || page.CheckIns[0].FinishedAt == nil {
		t.Fatalf("the open run was not closed: %+v", page.CheckIns[0])
	}
	client.decode(client.do(http.MethodGet,
		cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)), nil), &read)
	if read.Status != "error" {
		t.Fatalf("status after a failure = %q", read.Status)
	}
}

func TestPingAcceptsPost(t *testing.T) {
	client, projectID, base := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@hourly"})

	request, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, base+"/ping/"+monitor.PingKey, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST to a ping answered %d", response.StatusCode)
	}
}

func TestAnUnknownPingKeyIs404AndADisabledOneIs410(t *testing.T) {
	client, projectID, base := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@hourly"})

	if _, response := getPlain(t, base+"/ping/"+strings.Repeat("f", 32)); response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown key answered %d, want 404", response.StatusCode)
	}
	if _, response := getPlain(t, base+"/ping/"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("an empty key answered %d, want 404", response.StatusCode)
	}

	client.do(http.MethodPut, cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)),
		map[string]any{"enabled": false})

	body, response := getPlain(t, base+"/ping/"+monitor.PingKey)
	if response.StatusCode != http.StatusGone {
		// 410 rather than 404: a script that has pinged the same key for a
		// year has no other way to learn that somebody switched it off.
		t.Fatalf("a disabled monitor answered %d, want 410", response.StatusCode)
	}
	if !strings.Contains(body, "disabled") {
		t.Fatalf("body = %q", body)
	}
}

// The panel's catch-all must not swallow the ping surface: a path under /ping/
// that does not resolve has to answer text, not HTML.
func TestThePingSurfaceNeverFallsThroughToThePanel(t *testing.T) {
	_, _, base := newProjectAndClient(t)

	body, response := getPlain(t, base+"/ping/whatever/nonsense")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("answered %d", response.StatusCode)
	}
	if strings.Contains(body, "<") {
		t.Fatalf("the panel answered a ping path: %q", body)
	}
}

func getPlain(t *testing.T, url string) (string, *http.Response) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), response
}

// A token minted for a dashboard must not be able to read a ping key. The key
// is a credential: anything holding it can report a backup as successful from
// anywhere on the internet, which is exactly the thing a monitor exists to
// notice the absence of.
func TestMonitorRoutesDemandTheirOwnScopes(t *testing.T) {
	server, tokens := newTestServerWithTokens(t)

	_, projectsOnly, err := tokens.Create(context.Background(), "dashboard",
		[]domain.Scope{domain.ScopeProjectsRead, domain.ScopeProjectsWrite}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, readOnly, err := tokens.Create(context.Background(), "monitors-read",
		[]domain.Scope{domain.ScopeProjectsWrite, domain.ScopeMonitorsRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, full, err := tokens.Create(context.Background(), "monitors",
		[]domain.Scope{domain.ScopeProjectsWrite, domain.ScopeMonitorsRead, domain.ScopeMonitorsWrite}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var project projectResponse
	response := tokenRequest(t, server.URL, full, http.MethodPost, api("/projects"),
		createProjectRequest{Name: "venekambio"})
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	path := cronPath(project.ID, "")

	if got := tokenRequest(t, server.URL, projectsOnly, http.MethodGet, path, nil).StatusCode; got != http.StatusForbidden {
		t.Fatalf("a projects:* token read the monitors and got %d, want 403", got)
	}
	if got := tokenRequest(t, server.URL, readOnly, http.MethodPost, path,
		&monitorRequest{Slug: "backup", Schedule: "@daily"}).StatusCode; got != http.StatusForbidden {
		t.Fatalf("a monitors:read token created a monitor and got %d, want 403", got)
	}
	if got := tokenRequest(t, server.URL, full, http.MethodPost, path,
		&monitorRequest{Slug: "backup", Schedule: "@daily"}).StatusCode; got != http.StatusCreated {
		t.Fatalf("a monitors:write token got %d, want 201", got)
	}
	if got := tokenRequest(t, server.URL, readOnly, http.MethodGet, path, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("a monitors:read token got %d listing, want 200", got)
	}
}

// checkInEnvelope is a check-in in the shape an SDK's cron decorator sends.
func checkInEnvelope(dsn domain.DSN, payload string) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(payload)); err != nil {
		panic("the test's own check-in payload is not valid JSON: " + err.Error())
	}
	item := compact.String()
	return fmt.Sprintf("{\"dsn\":%q}\n{\"type\":\"check_in\",\"length\":%d}\n%s\n",
		dsn.String(), len(item), item)
}

// The second entrance, and the one that makes instrumenting a cron job a
// decorator: a check-in carrying monitor_config creates the monitor (ADR 016).
func TestACheckInWithAConfigCreatesTheMonitorThroughTheEnvelope(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// Check-ins are opt-in like every other category (ADR 005): an
	// installation that has not enabled them pays nothing for the feature.
	response := client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": []string{"error", "check_in"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enabling check-ins answered %d", response.StatusCode)
	}

	body := checkInEnvelope(dsn, `{
		"check_in_id":"aaaabbbbccccdddd",
		"monitor_slug":"nightly-backup",
		"status":"ok",
		"duration":2.5,
		"environment":"production",
		"monitor_config":{
			"schedule":{"type":"crontab","value":"0 3 * * *"},
			"checkin_margin":5,
			"max_runtime":10,
			"timezone":"America/Caracas"
		}
	}`)
	if got := sendEnvelope(t, server.URL, dsn, body, "").StatusCode; got != http.StatusOK {
		t.Fatalf("the check-in envelope answered %d", got)
	}

	var page struct {
		Monitors []monitorResponse `json:"monitors"`
	}
	client.decode(client.do(http.MethodGet, cronPath(dsn.ProjectID, ""), nil), &page)
	if len(page.Monitors) != 1 {
		t.Fatalf("%d monitors exist, want the SDK's declaration to have created one", len(page.Monitors))
	}
	monitor := page.Monitors[0]
	switch {
	case monitor.Slug != "nightly-backup":
		t.Fatalf("slug = %q", monitor.Slug)
	case monitor.Schedule != "0 3 * * *":
		t.Fatalf("schedule = %q", monitor.Schedule)
	case monitor.Timezone != "America/Caracas":
		t.Fatalf("timezone = %q", monitor.Timezone)
	case monitor.CheckinMarginSeconds != 300:
		// The protocol sends minutes; the API speaks seconds. A conversion
		// that went the wrong way would report a monitor as missed sixty
		// times too early.
		t.Fatalf("margin = %ds, want 300", monitor.CheckinMarginSeconds)
	case monitor.MaxRuntimeSeconds != 600:
		t.Fatalf("max runtime = %ds, want 600", monitor.MaxRuntimeSeconds)
	case monitor.Status != "ok":
		t.Fatalf("status = %q", monitor.Status)
	}

	var checkIns struct {
		CheckIns []checkInResponse `json:"checkins"`
	}
	client.decode(client.do(http.MethodGet,
		cronPath(dsn.ProjectID, "/"+strconv.FormatInt(monitor.ID, 10)+"/checkins"), nil), &checkIns)
	if len(checkIns.CheckIns) != 1 || checkIns.CheckIns[0].DurationMS != 2500 {
		t.Fatalf("history = %+v, want one run of 2500ms", checkIns.CheckIns)
	}
	if checkIns.CheckIns[0].CheckInID != "aaaabbbbccccdddd" {
		t.Fatalf("the correlation id was lost: %q", checkIns.CheckIns[0].CheckInID)
	}
}

// A check-in for a monitor nobody declared is dropped and counted, never a
// reason to reject the envelope: an SDK batches items, and one bad check-in
// must not lose the errors alongside it (ADR 002).
func TestAnUndeclarableCheckInDoesNotRejectTheEnvelope(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": []string{"error", "check_in"},
	})

	body := checkInEnvelope(dsn, `{"monitor_slug":"nobody-declared-me","status":"ok"}`)
	if got := sendEnvelope(t, server.URL, dsn, body, "").StatusCode; got != http.StatusOK {
		t.Fatalf("the envelope answered %d, want it accepted", got)
	}

	var page struct {
		Monitors []monitorResponse `json:"monitors"`
	}
	client.decode(client.do(http.MethodGet, cronPath(dsn.ProjectID, ""), nil), &page)
	if len(page.Monitors) != 0 {
		t.Fatalf("a check-in with no schedule invented %d monitors", len(page.Monitors))
	}
}

// And with check-ins switched off, the item costs nothing: the limiter refuses
// it before anything is parsed (ADR 005).
func TestCheckInsAreOptIn(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	body := checkInEnvelope(dsn, `{"monitor_slug":"backup","status":"ok",
		"monitor_config":{"schedule":"@daily"}}`)
	response := sendEnvelope(t, server.URL, dsn, body, "")
	// 429 with the protocol's rate-limit header, which is what makes a
	// switched-off category cost nothing on the wire either: the official
	// SDKs stop sending it for the stated window (ADR 005).
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the envelope answered %d, want 429", response.StatusCode)
	}
	if !strings.Contains(response.Header.Get("X-Sentry-Rate-Limits"), "check_in") {
		t.Fatalf("the refusal does not name the category: %q", response.Header.Get("X-Sentry-Rate-Limits"))
	}

	var page struct {
		Monitors []monitorResponse `json:"monitors"`
	}
	client.decode(client.do(http.MethodGet, cronPath(dsn.ProjectID, ""), nil), &page)
	if len(page.Monitors) != 0 {
		t.Fatalf("a project that has not enabled check-ins grew %d monitors", len(page.Monitors))
	}
}

// The ping surface is public and unauthenticated, so the per-address ceiling
// is what stops one address from spending the database's write budget on it.
// It shares the ingest limiter rather than minting a third number (ADR 023).
func TestThePingSurfaceIsRateLimitedByAddress(t *testing.T) {
	stack, _ := newStack(t)
	resolver, err := clientip.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	origin := domain.Origin{Scheme: "https", Host: "errors.example.com"}
	api := NewServer(stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues,
		stack.Stats, origin, "test").WithCrons(stack.Crons).WithIPLimits(resolver, 1, 1)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	monitor, err := stack.Crons.Add(context.Background(), 1, usecase.MonitorSpec{
		Slug: "backup", Schedule: "@hourly", Enabled: true,
	})
	if err != nil {
		// No project exists, so this is expected to fail on the foreign key;
		// the ceiling is still what is under test, and an unknown key is
		// refused by the limiter before it is looked up.
		t.Logf("no monitor was created (%v); the ceiling is tested on an unknown key", err)
	}
	key := monitor.PingKey
	if key == "" {
		key = strings.Repeat("a", 32)
	}

	if _, response := getPlain(t, server.URL+"/ping/"+key); response.StatusCode == http.StatusTooManyRequests {
		t.Fatal("the first request was already refused")
	}
	body, response := getPlain(t, server.URL+"/ping/"+key)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the second request answered %d, want 429", response.StatusCode)
	}
	switch {
	case response.Header.Get("Retry-After") == "":
		t.Error("a refusal with no Retry-After invites the caller straight back")
	case !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain"):
		// A JSON body here would be the one response from this surface that
		// is not a line of text, and the caller parsing it is a shell.
		t.Errorf("content type = %q", response.Header.Get("Content-Type"))
	case !strings.Contains(body, "too many requests"):
		t.Errorf("body = %q", body)
	case response.Header.Get("X-Sentry-Rate-Limits") != "":
		// The protocol's backpressure header belongs to ingest. Sending it
		// here would tell an SDK to stop reporting errors because a crontab
		// line was noisy (ADR 023).
		t.Error("a ping refusal carried the protocol's rate-limit header")
	}
}

func TestMonitorPathsRefuseNonsenseIDs(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})

	for name, path := range map[string]string{
		"a monitor id that is not a number": cronPath(projectID, "/not-a-number"),
		"a monitor id of zero":              cronPath(projectID, "/0"),
		"a project id that is not a number": api("/projects/nope/monitors/cron"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := client.do(http.MethodGet, path, nil).StatusCode; got != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400", got)
			}
		})
	}

	// And a limit that is not a limit.
	if got := client.do(http.MethodGet, cronPath(projectID, "/1/checkins?limit=-3"), nil).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("a negative limit answered %d, want 400", got)
	}
	if got := client.do(http.MethodGet, cronPath(projectID, "/1/checkins?limit=lots"), nil).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("a non-numeric limit answered %d, want 400", got)
	}
	// An absent limit is the default, not an error.
	if got := client.do(http.MethodGet, cronPath(projectID, "/1/checkins"), nil).StatusCode; got != http.StatusOK {
		t.Fatalf("no limit answered %d", got)
	}
}

// A misspelled field is told about rather than silently ignored: a client that
// sends "timezon" should not get a monitor in UTC and no explanation.
func TestAMisspelledMonitorFieldIsRejected(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})

	if got := client.do(http.MethodPost, cronPath(projectID, ""),
		map[string]any{"slug": "other", "schedule": "@daily", "timezon": "UTC"}).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("creating with a misspelled field answered %d, want 400", got)
	}
	if got := client.do(http.MethodPut, cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)),
		map[string]any{"schedul": "@hourly"}).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("updating with a misspelled field answered %d, want 400", got)
	}
	// And an update that names nothing is a no-op, not a reset.
	var unchanged monitorResponse
	client.decode(client.do(http.MethodPut,
		cronPath(projectID, "/"+strconv.FormatInt(monitor.ID, 10)), map[string]any{}), &unchanged)
	if unchanged.Schedule != monitor.Schedule || unchanged.Enabled != monitor.Enabled {
		t.Fatalf("an empty update changed the monitor: %+v", unchanged)
	}
}

func TestDeletingAMonitorThroughAnotherProjectIsRefused(t *testing.T) {
	client, projectID, _ := newProjectAndClient(t)
	monitor := createMonitor(t, client, projectID, &monitorRequest{Slug: "backup", Schedule: "@daily"})

	var other projectResponse
	client.decode(client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "otra"}), &other)

	path := cronPath(other.ID, "/"+strconv.FormatInt(monitor.ID, 10))
	if got := client.do(http.MethodDelete, path, nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("delete answered %d, want 404", got)
	}
	if got := client.do(http.MethodPut, path, map[string]any{"enabled": false}).StatusCode; got != http.StatusNotFound {
		t.Fatalf("update answered %d, want 404", got)
	}
	if got := client.do(http.MethodGet, path+"/checkins", nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("checkins answered %d, want 404", got)
	}
}

func TestCreatingAMonitorForAProjectThatDoesNotExistFails(t *testing.T) {
	client, _, _ := newProjectAndClient(t)
	response := client.do(http.MethodPost, cronPath(9999, ""),
		&monitorRequest{Slug: "backup", Schedule: "@daily"})
	if response.StatusCode < 400 {
		t.Fatalf("answered %d, want a failure", response.StatusCode)
	}
}
