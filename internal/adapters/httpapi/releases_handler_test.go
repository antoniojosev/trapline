package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

const releaseProjectID = 1

func releasesPath(suffix string) string {
	return api("/projects/") + strconv.Itoa(releaseProjectID) + "/releases" + suffix
}

// releaseEnvelope is an event that names a release, otherwise identical to
// the one every other ingest test sends.
//
// Every call gets a later timestamp than the last, because an issue's release
// follows its newest occurrence rather than the newest arrival: two events
// stamped the same instant would leave the release pointer where it was, and
// the test would be asserting about a clock rather than about releases.
var releaseEnvelopeClock = 0

func releaseEnvelope(dsn domain.DSN, message, release string) string {
	releaseEnvelopeClock++
	event := fmt.Sprintf(`{
		"event_id":"9ec79c33ec9942ab8353589fcb2e04dc",
		"timestamp":%q,
		"platform":"python",
		"level":"error",
		"release":%q,
		"environment":"production",
		"exception":{"values":[{"type":"ValueError","value":%q,"stacktrace":{"frames":[
			{"filename":"app/views.py","function":"checkout","lineno":42,"in_app":true}
		]}}]}
	}`, time.Date(2026, 8, 24, 10, releaseEnvelopeClock, 0, 0, time.UTC).Format(time.RFC3339),
		release, message)
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(event)); err != nil {
		panic("the test's own event payload is not valid JSON: " + err.Error())
	}
	event = compact.String()
	return fmt.Sprintf("{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\",\"dsn\":%q}\n"+
		"{\"type\":\"event\",\"length\":%d}\n%s\n", dsn.String(), len(event), event)
}

func TestAnEventCreatesTheReleaseNobodyRegistered(t *testing.T) {
	// Most installations never run a deploy tool. A product that only knew
	// about releases somebody remembered to register would know about almost
	// none of them.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.0"), "")

	var page releaseListResponse
	client.decode(client.do(http.MethodGet, releasesPath(""), nil), &page)
	if len(page.Releases) != 1 || page.Releases[0].Version != "app@1.0.0" {
		t.Fatalf("releases = %+v, want one created from the event", page.Releases)
	}
	if page.Releases[0].FirstEventAt == nil {
		t.Error("the release does not know when its first event arrived")
	}
}

func TestCreatingAReleaseTwiceIsNotAConflict(t *testing.T) {
	// Every caller of this is a deploy pipeline, and a rerun of a step is
	// normal operation rather than something anybody can act on.
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	first := client.do(http.MethodPost, releasesPath(""), map[string]string{"version": "app@1.0.0"})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d, want 201", first.StatusCode)
	}
	second := client.do(http.MethodPost, releasesPath(""), map[string]string{"version": "app@1.0.0"})
	if second.StatusCode != http.StatusOK {
		t.Errorf("second create = %d, want 200", second.StatusCode)
	}
}

func TestCreatingAReleaseWithNoVersion(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodPost, releasesPath(""), map[string]string{"version": "  "})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

func TestReleasesOfAProjectThatDoesNotExist(t *testing.T) {
	// An empty page is what a quiet project looks like, and a typo must not
	// be able to impersonate one.
	server := newTestServer(t)
	client := newClient(t, server)
	client.setUpAndLogIn()

	response := client.do(http.MethodGet, api("/projects/999/releases"), nil)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

func TestReadingAReleaseNobodyShipped(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodGet, releasesPath("/app%401.0.0"), nil)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

func TestFinalizingIsIdempotent(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)
	client.do(http.MethodPost, releasesPath(""), map[string]string{"version": "app@1.0.0"})

	var first releaseResponse
	client.decode(client.do(http.MethodPut, releasesPath("/app%401.0.0"), map[string]any{}), &first)
	if first.DateReleased == nil {
		t.Fatal("PUT did not finalise the release")
	}

	var second releaseResponse
	client.decode(client.do(http.MethodPut, releasesPath("/app%401.0.0"),
		map[string]any{"date_released": time.Now().Add(72 * time.Hour)}), &second)
	if !second.DateReleased.Equal(*first.DateReleased) {
		t.Errorf("a second finalize moved the date to %v", second.DateReleased)
	}
}

func TestCommitsAndTheirPaths(t *testing.T) {
	// The paths are the whole input to answering "which commit caused this"
	// later, so they have to survive the round trip.
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	body := map[string]any{"commits": []map[string]any{
		{
			"id":           "a3f9c1e",
			"message":      "fix the checkout total",
			"author_name":  "Ana",
			"author_email": "ana@example.com",
			"patch_set": []map[string]string{
				{"path": "app/views.py", "type": "M"},
				{"path": "app/new.py", "type": "A"},
			},
		},
		{"id": "b7d2f04", "message": "tidy up"},
	}}
	// set-commits before the release exists: it is often the first thing a
	// pipeline runs.
	response := client.do(http.MethodPost, releasesPath("/app%401.0.0/commits"), body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	var detail releaseDetailResponse
	client.decode(client.do(http.MethodGet, releasesPath("/app%401.0.0"), nil), &detail)
	if len(detail.Commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(detail.Commits))
	}
	if detail.Commits[0].ID != "a3f9c1e" {
		t.Errorf("commits came back out of the order they were sent: %+v", detail.Commits)
	}
	if len(detail.Commits[0].PatchSet) != 2 {
		t.Errorf("patch set = %+v, want both paths", detail.Commits[0].PatchSet)
	}
	if detail.CommitCount != 2 {
		t.Errorf("CommitCount = %d, want 2", detail.CommitCount)
	}
}

func TestDeploysAreRecordedAgainstARelease(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodPost, releasesPath("/app%401.0.0/deploys"),
		map[string]string{"environment": "production", "name": "pipeline-42"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", response.StatusCode)
	}

	var detail releaseDetailResponse
	client.decode(client.do(http.MethodGet, releasesPath("/app%401.0.0"), nil), &detail)
	if len(detail.Deploys) != 1 || detail.Deploys[0].Environment != "production" {
		t.Errorf("deploys = %+v", detail.Deploys)
	}
}

func TestDeployWithNoEnvironment(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodPost, releasesPath("/app%401.0.0/deploys"),
		map[string]string{"environment": ""})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

// TestResolvingInTheNextReleaseOverTheAPI is the feature's whole story, driven
// the way the panel and the CLI drive it.
func TestResolvingInTheNextReleaseOverTheAPI(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.0"), "")
	issues := listIssues(t, client, releaseProjectID)
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	issueID := issues[0].ID

	statusPath := api("/projects/") + strconv.Itoa(releaseProjectID) +
		"/issues/" + strconv.FormatInt(issueID, 10) + "/status"
	response := client.do(http.MethodPost, statusPath,
		map[string]any{"status": "resolved", "in_next_release": true})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve = %d, want 200", response.StatusCode)
	}

	// The old build, still emitting.
	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.0"), "")

	detail := readIssueDetail(t, client, issueID)
	if detail.Status != string(domain.StatusResolved) {
		t.Errorf("Status = %q, want it to stay resolved", detail.Status)
	}
	if detail.SeenInResolvedReleaseCount != 1 {
		t.Errorf("seen_in_resolved_release_count = %d, want 1", detail.SeenInResolvedReleaseCount)
	}
	if detail.Regressions != 0 {
		t.Errorf("regressions = %d, want 0", detail.Regressions)
	}

	// The new build, still broken.
	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.1"), "")

	detail = readIssueDetail(t, client, issueID)
	if detail.Status != string(domain.StatusUnresolved) {
		t.Errorf("Status = %q, want unresolved", detail.Status)
	}
	if detail.Regressions != 1 {
		t.Errorf("regressions = %d, want 1", detail.Regressions)
	}
	if detail.FirstRelease != "app@1.0.0" {
		t.Errorf("first_release = %q, want the release it was born in", detail.FirstRelease)
	}

	// And the release page says what each build did. Three events went in:
	// two from 1.0.0 and one from 1.0.1. Each release has to report its own
	// share — the failure that hides here is a page that reports the
	// project's total, which looks right until there is a second release.
	var born releaseDetailResponse
	client.decode(client.do(http.MethodGet, releasesPath("/app%401.0.0"), nil), &born)
	if born.NewIssues != 1 {
		t.Errorf("1.0.0 new_issues = %d, want 1", born.NewIssues)
	}
	if born.Events != 2 {
		t.Errorf("1.0.0 events = %d, want the 2 it produced", born.Events)
	}
	var broke releaseDetailResponse
	client.decode(client.do(http.MethodGet, releasesPath("/app%401.0.1"), nil), &broke)
	if broke.RegressedIssues != 1 {
		t.Errorf("1.0.1 regressed_issues = %d, want 1", broke.RegressedIssues)
	}
	if broke.Events != 1 {
		t.Errorf("1.0.1 events = %d, want the 1 it produced", broke.Events)
	}
}

func TestInNextReleaseOnlyAppliesToResolved(t *testing.T) {
	// Silently ignoring it would leave a caller believing the server did
	// something it never did.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, releaseEnvelope(dsn, "boom", "app@1.0.0"), "")
	issueID := listIssues(t, client, releaseProjectID)[0].ID

	statusPath := api("/projects/") + strconv.Itoa(releaseProjectID) +
		"/issues/" + strconv.FormatInt(issueID, 10) + "/status"
	response := client.do(http.MethodPost, statusPath,
		map[string]any{"status": "ignored", "in_next_release": true})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

// issueResolutionView is the issue detail as a client reads it, minus the
// stored payloads: jsonRaw is write-only by design, so decoding the whole
// response here would need a reverse it does not have and does not need.
type issueResolutionView struct {
	Status                     string `json:"status"`
	FirstRelease               string `json:"first_release"`
	ResolvedInRelease          string `json:"resolved_in_release"`
	ResolveNextRelease         bool   `json:"resolve_next_release"`
	Regressions                int64  `json:"regressions"`
	SeenInResolvedReleaseCount int64  `json:"seen_in_resolved_release_count"`
}

// readIssueDetail reads one issue with its resolution.
func readIssueDetail(t *testing.T, c *client, issueID int64) issueResolutionView {
	t.Helper()
	var detail issueResolutionView
	path := api("/projects/") + strconv.Itoa(releaseProjectID) + "/issues/" + strconv.FormatInt(issueID, 10)
	c.decode(c.do(http.MethodGet, path, nil), &detail)
	return detail
}

func TestReleaseListRejectsANonsenseLimit(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	for _, limit := range []string{"0", "-3", "many"} {
		response := client.do(http.MethodGet, releasesPath("?limit="+limit), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("limit=%s gave %d, want 400", limit, response.StatusCode)
		}
	}
	if got := client.do(http.MethodGet, releasesPath("?limit=5"), nil).StatusCode; got != http.StatusOK {
		t.Errorf("a usable limit gave %d", got)
	}
}

func TestReleaseEndpointsRejectAMisspelledField(t *testing.T) {
	// A client that misspells a field should be told, not silently given a
	// release called "".
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodPost, releasesPath(""), map[string]string{"realese": "app@1.0.0"})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

func TestReleaseWritesNeedTheWriteScope(t *testing.T) {
	// Registering a release is a change to shared state another operator will
	// see, so it belongs on the write side of the read/write split.
	server, tokens := newTestServerWithTokens(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	_, readOnly, err := tokens.Create(context.Background(), "reader",
		[]domain.Scope{domain.ScopeProjectsRead}, nil)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, releasesPath(""), http.StatusOK},
		{http.MethodPost, releasesPath(""), http.StatusForbidden},
		{http.MethodPut, releasesPath("/app%401.0.0"), http.StatusForbidden},
		{http.MethodPost, releasesPath("/app%401.0.0/commits"), http.StatusForbidden},
		{http.MethodPost, releasesPath("/app%401.0.0/deploys"), http.StatusForbidden},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.path, func(t *testing.T) {
			request, err := http.NewRequestWithContext(context.Background(), testCase.method,
				server.URL+testCase.path, bytes.NewReader([]byte(`{}`)))
			if err != nil {
				t.Fatalf("building request: %v", err)
			}
			request.Header.Set("Authorization", "Bearer "+readOnly)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(CSRFHeader, "1")

			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("sending: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != testCase.want {
				t.Errorf("status = %d, want %d", response.StatusCode, testCase.want)
			}
		})
	}
}

func TestFinalizingWithAMalformedBody(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)
	client.do(http.MethodPost, releasesPath(""), map[string]string{"version": "app@1.0.0"})

	response := client.do(http.MethodPut, releasesPath("/app%401.0.0"),
		map[string]string{"date_released": "yesterday"})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

func TestCommitsWithAMalformedBody(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodPost, releasesPath("/app%401.0.0/commits"),
		map[string]string{"commits": "not a list"})
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}
