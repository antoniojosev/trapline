package httpapi

import (
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
	"github.com/antoniojosev/trapline/internal/wiring"
)

// The synthetic hour every assertion below is written against, fixed rather
// than relative to now for the reason the tracing tests fix theirs: crash-free
// rate is a statement about particular hours, and a test that drifted with the
// clock would assert a different thing every afternoon.
const (
	healthDay      = "2026-08-24"
	healthHour     = healthDay + "T10"
	healthRangeQry = "from=" + healthHour + "&to=" + healthHour
)

// newHealthServer serves the real application and hands back the use case, so
// a test can drive the flush by hand instead of waiting a minute for the job.
func newHealthServer(t *testing.T) (*client, *usecase.Health, *wiring.Stack) {
	t.Helper()
	stack, _ := newStack(t)
	server := serve(t, stack, false)
	return newClient(t, server), stack.Health, stack
}

// sessionEnvelope is a `session` item as the official SDKs send one — the
// shape sentry_sdk's Session.to_json() produces, verbatim (ADR 002).
func sessionEnvelope(dsn domain.DSN, sid, status string, errorCount int) string {
	payload := fmt.Sprintf(`{"sid":%q,"init":true,"started":"%s:16:00.000000Z",`+
		`"timestamp":"%s:16:04.000000Z","status":%q,"errors":%d,`+
		`"attrs":{"release":"shop@1.4.2","environment":"production"}}`,
		sid, healthHour, healthHour, status, errorCount)
	return fmt.Sprintf("{\"sent_at\":\"2026-08-24T10:16:05Z\",\"dsn\":%q}\n"+
		"{\"type\":\"session\",\"length\":%d}\n%s\n", dsn.String(), len(payload), payload)
}

// aggregateEnvelope is a `sessions` item, which is what an SDK in request mode
// sends instead of individual sessions.
func aggregateEnvelope(dsn domain.DSN, release string, exited, errored, crashed int) string {
	payload := fmt.Sprintf(`{"attrs":{"release":%q,"environment":"production"},`+
		`"aggregates":[{"started":"%s:16:00.000000Z","exited":%d,"errored":%d,"crashed":%d}]}`,
		release, healthHour, exited, errored, crashed)
	return fmt.Sprintf("{\"sent_at\":\"2026-08-24T10:16:05Z\",\"dsn\":%q}\n"+
		"{\"type\":\"sessions\",\"length\":%d}\n%s\n", dsn.String(), len(payload), payload)
}

// enableSessions switches the category on. Without it the limiter refuses the
// item before anything is parsed, which is what makes a subsystem nobody
// switched on cost nothing (ADR 005).
func enableSessions(t *testing.T, c *client, projectID int64) {
	t.Helper()
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + "/config"
	response := c.do(http.MethodPut, path, map[string]any{
		"enabled_categories": []string{"error", "session"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enabling sessions returned %d", response.StatusCode)
	}
}

func healthPath(projectID int64, suffix, query string) string {
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + suffix
	if query != "" {
		path += "?" + query
	}
	return path
}

// TestOneHundredSessionsWithFiveCrashesAreNinetyFivePercentCrashFree is the
// end-to-end form of the one number this whole subsystem exists to produce.
func TestOneHundredSessionsWithFiveCrashesAreNinetyFivePercentCrashFree(t *testing.T) {
	client, health, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)
	enableSessions(t, client, dsn.ProjectID)

	for index := range 100 {
		sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", index)
		status := "exited"
		if index%20 == 0 {
			status = "crashed"
		}
		sendEnvelope(t, client.base, dsn, sessionEnvelope(dsn, sid, "ok", 0), "")
		sendEnvelope(t, client.base, dsn, sessionEnvelope(dsn, sid, status, 0), "")
	}

	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	var body releaseHealthResponse
	client.decode(client.do(http.MethodGet,
		healthPath(dsn.ProjectID, "/releases/"+neturl.PathEscape("shop@1.4.2")+"/health",
			healthRangeQry), nil), &body)

	if body.Started != 100 || body.Crashed != 5 {
		t.Fatalf("counted %+v, want 100 started and 5 crashed", body.SessionCounts)
	}
	if body.Healthy != 95 {
		t.Fatalf("healthy %d, want 95", body.Healthy)
	}
	if body.CrashFreeRate == nil || *body.CrashFreeRate != 0.95 {
		t.Fatalf("crash-free rate %v, want 0.95", body.CrashFreeRate)
	}
	if len(body.Series) != 1 || body.Series[0].Hour != healthHour {
		t.Fatalf("series %+v, want the one hour asked for", body.Series)
	}
	// The caveat rides with the number, because that is the only place it can
	// be read at the moment it matters (ADR 008).
	if body.Window.Note == "" {
		t.Fatal("the response does not say the window is volatile")
	}
}

// TestAggregatesAndIndividualSessionsMeetInOneNumber: an SDK may send either,
// and a reader comparing two services must not have to know which mode each
// one was in.
func TestAggregatesAndIndividualSessionsMeetInOneNumber(t *testing.T) {
	client, health, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)
	enableSessions(t, client, dsn.ProjectID)

	sendEnvelope(t, client.base, dsn, aggregateEnvelope(dsn, "shop@1.4.2", 40, 3, 2), "")
	sendEnvelope(t, client.base, dsn, sessionEnvelope(dsn, "aaaaaaaa-0000-4000-8000-000000000001", "crashed", 1), "")
	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	var body releaseHealthResponse
	client.decode(client.do(http.MethodGet,
		healthPath(dsn.ProjectID, "/releases/"+neturl.PathEscape("shop@1.4.2")+"/health",
			healthRangeQry), nil), &body)

	want := domain.SessionCounts{Started: 46, Errored: 3, Crashed: 3}
	if body.SessionCounts != want {
		t.Fatalf("got %+v, want %+v", body.SessionCounts, want)
	}
}

// TestTheProjectPageRanksReleases answers the question somebody actually opens
// this page with: not "how is 1.4.2 doing" but "which of these is the bad one".
func TestTheProjectPageRanksReleases(t *testing.T) {
	client, health, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)
	enableSessions(t, client, dsn.ProjectID)

	sendEnvelope(t, client.base, dsn, aggregateEnvelope(dsn, "shop@1.4.2", 190, 0, 10), "")
	sendEnvelope(t, client.base, dsn, aggregateEnvelope(dsn, "shop@1.4.1", 50, 0, 0), "")
	if _, err := health.Flush(t.Context()); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	var body projectHealthResponse
	client.decode(client.do(http.MethodGet,
		healthPath(dsn.ProjectID, "/health", healthRangeQry), nil), &body)

	if len(body.Releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(body.Releases))
	}
	if body.Releases[0].Release != "shop@1.4.2" {
		t.Fatalf("the busiest release is not first: %+v", body.Releases)
	}
	if body.Releases[0].CrashFreeRate == nil || *body.Releases[0].CrashFreeRate != 0.95 {
		t.Fatalf("the bad release reports %v, want 0.95", body.Releases[0].CrashFreeRate)
	}
	if body.Releases[1].CrashFreeRate == nil || *body.Releases[1].CrashFreeRate != 1 {
		t.Fatalf("the good release reports %v, want 1", body.Releases[1].CrashFreeRate)
	}
	if body.Started != 250 || body.Crashed != 10 {
		t.Fatalf("project totals %+v", body.SessionCounts)
	}
	if body.Releases[0].LastSeen != healthHour {
		t.Fatalf("last seen %q, want %q", body.Releases[0].LastSeen, healthHour)
	}
}

// TestAReleaseNobodyReportedAnswersWithNothingRatherThanAnError: the panel
// opens this page before any session has arrived, and a 404 there would read
// as a broken product rather than an empty one.
func TestAReleaseNobodyReportedAnswersWithNothingRatherThanAnError(t *testing.T) {
	client, _, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)
	enableSessions(t, client, dsn.ProjectID)

	var body releaseHealthResponse
	response := client.do(http.MethodGet,
		healthPath(dsn.ProjectID, "/releases/"+neturl.PathEscape("never@0.0.1")+"/health",
			healthRangeQry), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", response.StatusCode)
	}
	client.decode(response, &body)
	if body.Started != 0 {
		t.Fatalf("counted %d sessions for a release nobody reported", body.Started)
	}
	if body.CrashFreeRate != nil {
		t.Fatalf("a release with no sessions reported a rate of %v", *body.CrashFreeRate)
	}

	var project projectHealthResponse
	client.decode(client.do(http.MethodGet, healthPath(dsn.ProjectID, "/health", ""), nil), &project)
	if len(project.Releases) != 0 || project.CrashFreeRate != nil {
		t.Fatalf("an empty project reported %+v", project)
	}
}

// TestHealthEndpointsRefuseWhatDoesNotExist checks the two ways a caller gets
// this wrong, and that each is answered with the status that says which.
func TestHealthEndpointsRefuseWhatDoesNotExist(t *testing.T) {
	client, _, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)

	for _, test := range []struct {
		name string
		path string
		want int
	}{
		{"a project that is not there", healthPath(dsn.ProjectID+999, "/health", ""), http.StatusNotFound},
		{"a range that is not one", healthPath(dsn.ProjectID, "/health", "from=yesterday"), http.StatusBadRequest},
		{"a limit that is not one", healthPath(dsn.ProjectID, "/health", "limit=-3"), http.StatusBadRequest},
		{"a range that runs backwards",
			healthPath(dsn.ProjectID, "/releases/"+neturl.PathEscape("shop@1.4.2")+"/health",
				"from=2026-08-24T10&to=2026-08-23T10"), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := client.do(http.MethodGet, test.path, nil).StatusCode; got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}

// TestASessionForAProjectWithSessionsOffIsRefusedOnTheWire is ADR 005: the SDK
// is told to stop sending, which is what makes the toggle actually free.
func TestASessionForAProjectWithSessionsOffIsRefusedOnTheWire(t *testing.T) {
	client, health, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)

	response := sendEnvelope(t, client.base, dsn, sessionEnvelope(dsn, "aaaaaaaa-0000-4000-8000-000000000001", "crashed", 0), "")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", response.StatusCode)
	}
	if inFlight := health.Window().InFlight; inFlight != 0 {
		t.Fatalf("a refused session reached the window: %d in flight", inFlight)
	}
}

// TestASessionWithoutAReleaseIsDroppedNotFatal: it cannot be attributed to
// anything, so it is counted as dropped rather than inventing a bucket for it
// — and the rest of the envelope is unaffected.
func TestASessionWithoutAReleaseIsDroppedNotFatal(t *testing.T) {
	client, health, _ := newHealthServer(t)
	dsn := createProjectForIngest(t, client)
	enableSessions(t, client, dsn.ProjectID)

	payload := fmt.Sprintf(`{"sid":"aaaaaaaa-0000-4000-8000-000000000009","started":"%s:16:00Z","status":"exited"}`, healthHour)
	envelope := fmt.Sprintf("{\"dsn\":%q}\n{\"type\":\"session\",\"length\":%d}\n%s\n",
		dsn.String(), len(payload), payload)

	response := sendEnvelope(t, client.base, dsn, envelope, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want the envelope accepted", response.StatusCode)
	}
	if written, err := health.Flush(t.Context()); err != nil || written != 0 {
		t.Fatalf("a session with no release was written down (%d buckets, err %v)", written, err)
	}
}
