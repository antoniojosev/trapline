package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// The synthetic day every assertion below is written against. Fixed rather
// than relative to now, because a chart is a statement about particular hours
// and a test that drifted with the clock would be asserting a different thing
// every afternoon.
const (
	statsDay      = "2026-08-24"
	statsFrom     = statsDay + "T08"
	statsTo       = statsDay + "T13"
	statsRangeQry = "from=" + statsFrom + "&to=" + statsTo
)

// statsEnvelope is a python-SDK envelope whose hour, release and environment
// are all chosen by the caller: those three are exactly the dimensions the
// aggregates split on.
func statsEnvelope(dsn domain.DSN, message, environment, release string, hour int) string {
	event := fmt.Sprintf(`{
		"event_id":"9ec79c33ec9942ab8353589fcb2e04dc",
		"timestamp":"%sT%02d:30:00Z",
		"platform":"python",
		"level":"error",
		"release":%q,
		"environment":%q,
		"exception":{"values":[{"type":"ValueError","value":%q,"stacktrace":{"frames":[
			{"filename":"app/views.py","abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}
		]}}]},
		"tags":{"server":"web-01"}
	}`, statsDay, hour, release, environment, message)

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(event)); err != nil {
		panic("the test's own event payload is not valid JSON: " + err.Error())
	}
	body := compact.String()
	return fmt.Sprintf("{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\",\"dsn\":%q}\n"+
		"{\"type\":\"event\",\"length\":%d}\n%s\n", dsn.String(), len(body), body)
}

func statsPath(projectID int64, suffix, query string) string {
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + suffix
	if query != "" {
		path += "?" + query
	}
	return path
}

func TestProjectStatsSeriesCoversTheWholeRange(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	for range 3 {
		sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "boom", "production", "app@1.0.0", 10), "")
	}
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "other", "staging", "app@1.1.0", 12), "")

	var payload seriesResponse
	client.decode(client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats", statsRangeQry), nil), &payload)

	// Six hours asked for, six points returned: the quiet ones are the point,
	// or a chart draws a straight line across a quiet night.
	if len(payload.Series) != 6 {
		t.Fatalf("series has %d points, want 6 (08:00 through 13:00 inclusive)", len(payload.Series))
	}
	if payload.Total != 4 {
		t.Errorf("total = %d, want 4", payload.Total)
	}
	if payload.Range.Hours != 6 {
		t.Errorf("range hours = %d, want 6", payload.Range.Hours)
	}
	if payload.Project.Name != "venekambio" {
		t.Errorf("project = %+v, want it named so a heading can be drawn", payload.Project)
	}

	byHour := map[string]int64{}
	for _, point := range payload.Series {
		byHour[point.Hour] = point.Count
	}
	if byHour[statsDay+"T10"] != 3 || byHour[statsDay+"T12"] != 1 {
		t.Errorf("series = %+v, want 3 at 10:00 and 1 at 12:00", payload.Series)
	}
	if byHour[statsDay+"T09"] != 0 {
		t.Errorf("a quiet hour reported %d", byHour[statsDay+"T09"])
	}
	if payload.Levels["error"] != 4 {
		t.Errorf("legend = %+v, want 4 errors", payload.Levels)
	}
}

func TestProjectStatsDefaultsToTheLastDay(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	var payload seriesResponse
	client.decode(client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats", ""), nil), &payload)

	if payload.Range.Hours != domain.DefaultRangeHours {
		t.Errorf("hours = %d, want the default %d", payload.Range.Hours, domain.DefaultRangeHours)
	}
	// The events above are from a fixed day in the past, so the default window
	// legitimately finds nothing. What matters is that it answers with a whole
	// axis rather than an empty array a client has to interpret.
	if len(payload.Series) != domain.DefaultRangeHours {
		t.Errorf("series has %d points, want a full axis", len(payload.Series))
	}
}

func TestTopIssuesIsRankedByTheRange(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// "quiet" is louder overall, but almost all of it is outside the range.
	for range 4 {
		sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "quiet", "production", "app@1.0.0", 3), "")
	}
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "quiet", "production", "app@1.0.0", 10), "")
	for range 2 {
		sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "loud", "production", "app@1.0.0", 10), "")
	}

	var payload topResponse
	client.decode(client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats/top", statsRangeQry), nil), &payload)

	if len(payload.Issues) != 2 {
		t.Fatalf("issues = %+v, want two", payload.Issues)
	}
	if payload.Issues[0].Count != 2 {
		t.Errorf("loudest count = %d, want 2 (only what is inside the range)", payload.Issues[0].Count)
	}
	// The lifetime total is still there, under its own name, and is a
	// different number. Conflating the two is how a dashboard starts lying.
	if payload.Issues[1].Times != 5 || payload.Issues[1].Count != 1 {
		t.Errorf("second issue: times = %d, count = %d; want 5 and 1",
			payload.Issues[1].Times, payload.Issues[1].Count)
	}
}

func TestBreakdownByReleaseAndEnvironment(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "a", "production", "app@1.0.0", 10), "")
	for range 2 {
		sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "b", "staging", "app@1.1.0", 11), "")
	}

	for dimension, expected := range map[string]string{"release": "app@1.1.0", "environment": "staging"} {
		var payload breakdownResponse
		query := statsRangeQry + "&by=" + dimension
		client.decode(client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats/breakdown", query), nil), &payload)

		if payload.By != dimension {
			t.Errorf("by = %q, want %q", payload.By, dimension)
		}
		if len(payload.Values) != 2 {
			t.Fatalf("%s = %+v, want two values", dimension, payload.Values)
		}
		if payload.Values[0].Value != expected || payload.Values[0].Count != 2 {
			t.Errorf("%s = %+v, want %q first with 2", dimension, payload.Values, expected)
		}
	}
}

func TestBreakdownRefusesADimensionThatDoesNotExist(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	for _, query := range []string{"", "by=", "by=version"} {
		response := client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats/breakdown", query), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("breakdown %q: status = %d, want 400", query, response.StatusCode)
		}
	}
}

func TestIssueStatsUsesNamedWindows(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "boom", "production", "app@1.0.0", 10), "")

	issues := listIssues(t, client, dsn.ProjectID)
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want one", issues)
	}
	path := statsPath(dsn.ProjectID, "/issues/"+strconv.FormatInt(issues[0].ID, 10)+"/stats", "range=14d")

	var payload issueStatsResponse
	client.decode(client.do(http.MethodGet, path, nil), &payload)

	if payload.Window != "14d" || len(payload.Series) != 14*24 {
		t.Errorf("window = %q with %d points, want 14d with %d", payload.Window, len(payload.Series), 14*24)
	}
	if payload.IssueID != issues[0].ID {
		t.Errorf("issue id = %d, want %d", payload.IssueID, issues[0].ID)
	}
}

func TestIssueStatsRefusesAnUnknownWindowAndAMissingIssue(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	bad := statsPath(dsn.ProjectID, "/issues/1/stats", "range=7d")
	if got := client.do(http.MethodGet, bad, nil).StatusCode; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a window nobody defined", got)
	}
	missing := statsPath(dsn.ProjectID, "/issues/9999/stats", "")
	if got := client.do(http.MethodGet, missing, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("status = %d, want 404: a chart of zeroes would let a typo pass for a quiet issue", got)
	}
}

func TestStatsRefuseARangeNothingCanAnswer(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	cases := []string{
		"from=" + statsTo + "&to=" + statsFrom, // backwards
		"from=1999-01-01",                      // wider than the aggregates live
		"from=whenever",                        // not a time at all
		"limit=0",                              // a page nobody can read
		"limit=-3",
	}
	for _, query := range cases {
		response := client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats/top", query), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", query, response.StatusCode)
		}
	}
}

func TestStatsForAProjectNobodyOwnsAre404(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	// An empty chart is what a quiet project looks like, and a typo must not
	// be able to impersonate one.
	for _, suffix := range []string{"/stats", "/stats/top", "/stats/breakdown"} {
		query := ""
		if suffix == "/stats/breakdown" {
			query = "by=release"
		}
		response := client.do(http.MethodGet, statsPath(9999, suffix, query), nil)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", suffix, response.StatusCode)
		}
	}
}

func TestStatsNeedACredential(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	anonymous := newClient(t, server)
	response := anonymous.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats", ""), nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: the dashboard is not public", response.StatusCode)
	}
}

// nowEnvelope is an event that happened in the hour this test is running in.
//
// The rest of this file pins a synthetic day so a chart's shape can be
// asserted exactly. The sparkline endpoint takes a named window relative to
// now instead of two instants, so its events have to be inside that window or
// the assertion is about an empty chart.
func nowEnvelope(dsn domain.DSN, exceptionType string) string {
	event := fmt.Sprintf(`{
		"event_id":"9ec79c33ec9942ab8353589fcb2e04dc",
		"timestamp":%q,
		"platform":"python",
		"level":"error",
		"exception":{"values":[{"type":%q,"value":"boom","stacktrace":{"frames":[
			{"abs_path":"/srv/app/views.py","function":"checkout","lineno":42,"in_app":true}
		]}}]}
	}`, time.Now().UTC().Format(time.RFC3339), exceptionType)

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(event)); err != nil {
		panic("the test's own event payload is not valid JSON: " + err.Error())
	}
	body := compact.String()
	return fmt.Sprintf("{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\",\"dsn\":%q}\n"+
		"{\"type\":\"event\",\"length\":%d}\n%s\n", dsn.String(), len(body), body)
}

func TestSparklinesCoverEveryIssueAskedFor(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, nowEnvelope(dsn, "ValueError"), "")
	sendEnvelope(t, server.URL, dsn, nowEnvelope(dsn, "ValueError"), "")
	sendEnvelope(t, server.URL, dsn, nowEnvelope(dsn, "KeyError"), "")

	issues := listIssues(t, client, dsn.ProjectID)
	if len(issues) != 2 {
		t.Fatalf("issues = %d, want two", len(issues))
	}
	ids := make([]string, 0, len(issues))
	totals := map[int64]int64{}
	for _, issue := range issues {
		ids = append(ids, strconv.FormatInt(issue.ID, 10))
		totals[issue.ID] = issue.Times
	}
	// An id that does not exist rides along: the response must have a row for
	// it, because a client draws one sparkline per row it is showing and a
	// missing entry would silently shift every chart onto the wrong row.
	query := "issues=" + strings.Join(ids, ",") + ",9999&range=24h"

	var payload sparklinesResponse
	client.decode(client.do(http.MethodGet, statsPath(dsn.ProjectID, "/stats/series", query), nil), &payload)

	if len(payload.Issues) != 3 {
		t.Fatalf("returned %d sparklines for 3 ids", len(payload.Issues))
	}
	if len(payload.Hours) != 24 {
		t.Errorf("hours = %d, want 24", len(payload.Hours))
	}
	for _, sparkline := range payload.Issues {
		if len(sparkline.Counts) != len(payload.Hours) {
			t.Fatalf("issue %d has %d counts for %d hours",
				sparkline.IssueID, len(sparkline.Counts), len(payload.Hours))
		}
		want, known := totals[sparkline.IssueID]
		if !known {
			want = 0 // the id nobody owns
		}
		if sparkline.Total != want {
			t.Errorf("issue %d totalled %d, want %d", sparkline.IssueID, sparkline.Total, want)
		}
	}
}

func TestSparklinesRefuseWhatTheyCannotDraw(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	tooMany := make([]string, domain.MaxSparklineIssues+1)
	for index := range tooMany {
		tooMany[index] = strconv.Itoa(index + 1)
	}

	for name, query := range map[string]string{
		"no issues at all":     "",
		"an empty list":        "issues=",
		"something not an id":  "issues=1,two",
		"a negative id":        "issues=-1",
		"more than the bound":  "issues=" + strings.Join(tooMany, ","),
		"a window nobody made": "issues=1&range=7d",
	} {
		t.Run(name, func(t *testing.T) {
			path := statsPath(dsn.ProjectID, "/stats/series", query)
			if got := client.do(http.MethodGet, path, nil).StatusCode; got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}

	t.Run("a project nobody owns", func(t *testing.T) {
		path := statsPath(9999, "/stats/series", "issues=1")
		if got := client.do(http.MethodGet, path, nil).StatusCode; got != http.StatusNotFound {
			t.Errorf("status = %d, want 404", got)
		}
	})
}

// Asking for the same issue a hundred times must not return a hundred rows,
// and must not turn into a hundred-term IN clause.
func TestSparklinesDeduplicateWhatTheyAreAskedFor(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, nowEnvelope(dsn, "ValueError"), "")

	issues := listIssues(t, client, dsn.ProjectID)
	id := strconv.FormatInt(issues[0].ID, 10)

	var payload sparklinesResponse
	path := statsPath(dsn.ProjectID, "/stats/series", "issues="+id+","+id+","+id)
	client.decode(client.do(http.MethodGet, path, nil), &payload)

	if len(payload.Issues) != 1 {
		t.Fatalf("returned %d sparklines for one issue named three times", len(payload.Issues))
	}
	if payload.Issues[0].Total != 1 {
		t.Errorf("total = %d, want the one event that happened", payload.Issues[0].Total)
	}
}
