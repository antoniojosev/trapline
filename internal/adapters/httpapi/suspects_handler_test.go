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

// suspectEnvelopeClock keeps every event in this file a minute apart, for the
// reason releaseEnvelope does: an issue's release follows its newest
// occurrence, and two events stamped the same instant would leave the pointer
// where it was.
var suspectEnvelopeClock = 0

// suspectEnvelope is a browser error whose frames name original source files,
// which is what an event looks like after symbolication resolved it.
func suspectEnvelope(dsn domain.DSN, release, frames string) string {
	suspectEnvelopeClock++
	event := fmt.Sprintf(`{
		"event_id":"2f3f7a1f16f34bd0a2b90e0f5f5a3c21",
		"timestamp":%q,
		"platform":"javascript",
		"level":"error",
		"release":%q,
		"environment":"production",
		"exception":{"values":[{"type":"TypeError","value":"total is not a function",
			"stacktrace":{"frames":[%s]}}]}
	}`, time.Date(2026, 9, 20, 10, suspectEnvelopeClock, 0, 0, time.UTC).Format(time.RFC3339),
		release, frames)
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(event)); err != nil {
		panic("the test's own event payload is not valid JSON: " + err.Error())
	}
	body := compact.String()
	return fmt.Sprintf("{\"event_id\":\"2f3f7a1f16f34bd0a2b90e0f5f5a3c21\",\"dsn\":%q}\n"+
		"{\"type\":\"event\",\"length\":%d}\n%s\n", dsn.String(), len(body), body)
}

// resolvedFrames is a stacktrace as symbolication leaves one: original paths,
// and the minified frame kept underneath in `raw`.
const resolvedFrames = `
	{"filename":"src/main.ts","function":"boot","lineno":3,"in_app":true,
	 "raw":{"filename":"~/bundle.min.js","lineno":1,"colno":48}},
	{"filename":"src/checkout.ts","function":"total","lineno":42,"in_app":true,
	 "raw":{"filename":"~/bundle.min.js","lineno":1,"colno":3120}}`

// minifiedFrames is the same crash from a build whose maps nobody uploaded.
const minifiedFrames = `{"filename":"~/bundle.min.js","lineno":1,"colno":3120,"in_app":true}`

func suspectsPath(projectID, issueID int64) string {
	return api("/projects/") + strconv.FormatInt(projectID, 10) +
		"/issues/" + strconv.FormatInt(issueID, 10) + "/suspects"
}

// seedSuspects associates a commit set with the release under test and
// returns the issue the ingested event grouped into.
func seedSuspects(t *testing.T, c *client, version string, commits any) int64 {
	t.Helper()

	// The "@" is escaped by hand: `net/url` cannot be imported here because
	// another test in this package already uses `url` as an identifier.
	escaped := strings.ReplaceAll(version, "@", "%40")
	response := c.do(http.MethodPost, releasesPath("/"+escaped+"/commits"), commits)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("associating commits = %d", response.StatusCode)
	}

	issues := listIssues(t, c, releaseProjectID)
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want the one the event created", len(issues))
	}
	return issues[0].ID
}

// commitSet is the patch-set shape `sentry-cli set-commits --local` sends.
func commitSet(paths ...string) map[string]any {
	commits := make([]map[string]any, 0, len(paths))
	for index, path := range paths {
		commits = append(commits, map[string]any{
			"id":          fmt.Sprintf("%040d", index+1),
			"message":     "change " + path,
			"author_name": "Antonio Vila",
			"patch_set":   []map[string]string{{"path": path, "type": "M"}},
		})
	}
	return map[string]any{"commits": commits}
}

func TestSuspectsNameTheCommitThatTouchedTheFailingFrame(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, suspectEnvelope(dsn, "app@1.4.0", resolvedFrames), "")

	// Three commits, one of which touched the file the failing frame names —
	// the gate's scenario, asserted here where it is cheap.
	issueID := seedSuspects(t, client, "app@1.4.0",
		commitSet("README.md", "src/checkout.ts", "package-lock.json"))

	var response suspectsResponse
	client.decode(client.do(http.MethodGet, suspectsPath(releaseProjectID, issueID), nil), &response)

	if response.Release != "app@1.4.0" {
		t.Fatalf("blamed %q, want the release the issue was first seen in", response.Release)
	}
	if len(response.Suspects) != 1 {
		t.Fatalf("got %d suspects, want exactly the one that touched the frame: %+v",
			len(response.Suspects), response.Suspects)
	}
	suspect := response.Suspects[0]
	if suspect.ID != fmt.Sprintf("%040d", 2) {
		t.Fatalf("suspect is %q, want the second commit", suspect.ID)
	}
	if len(suspect.Reasons) != 1 {
		t.Fatalf("got %d reasons, want one", len(suspect.Reasons))
	}
	reason := suspect.Reasons[0]
	if reason.Path != "src/checkout.ts" || reason.FrameDepth != 0 || reason.Type != "M" {
		t.Fatalf("reason is %+v, want src/checkout.ts modified at the failing frame", reason)
	}
	if !response.Symbolicated {
		t.Error("the answer says nothing was symbolicated, but every frame carries its raw form")
	}
	if response.Warning != "" {
		t.Errorf("an answer arrived with a warning: %q", response.Warning)
	}
	if response.CommitCount != 3 {
		t.Errorf("counted %d commits, want 3", response.CommitCount)
	}
	// The patch set travels with the candidates, so a client can show what
	// was compared without a second request.
	if len(response.Commits) != 3 || len(response.Commits[0].PatchSet) != 1 {
		t.Errorf("candidates = %+v, want three commits with their paths", response.Commits)
	}
}

func TestSuspectsListTheCommitsWhenNoPathsArrived(t *testing.T) {
	// ADR 019's explicit fallback: `sentry-cli set-commits` without --local
	// sends no patch set, and the honest answer is the commit list plus a
	// warning that names what to change.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, suspectEnvelope(dsn, "app@1.4.0", resolvedFrames), "")

	issueID := seedSuspects(t, client, "app@1.4.0", map[string]any{
		"commits": []map[string]any{{"id": fmt.Sprintf("%040d", 1), "message": "no patch set"}},
	})

	var response suspectsResponse
	client.decode(client.do(http.MethodGet, suspectsPath(releaseProjectID, issueID), nil), &response)

	if len(response.Suspects) != 0 {
		t.Fatalf("got %+v, want nothing marked", response.Suspects)
	}
	if len(response.Commits) != 1 {
		t.Fatalf("got %d candidates, want the release's one commit", len(response.Commits))
	}
	if response.Warning == "" {
		t.Fatal("an empty answer arrived with no explanation")
	}
}

func TestSuspectsSayWhenTheStacktraceIsStillMinified(t *testing.T) {
	// The answer a front end gets before anybody uploads source maps. Saying
	// only "no suspect" would send the reader looking for a bug in the
	// scoring instead of at their build pipeline (ADR 018, ADR 019).
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, suspectEnvelope(dsn, "app@1.4.0", minifiedFrames), "")

	issueID := seedSuspects(t, client, "app@1.4.0", commitSet("src/checkout.ts"))

	var response suspectsResponse
	client.decode(client.do(http.MethodGet, suspectsPath(releaseProjectID, issueID), nil), &response)

	if len(response.Suspects) != 0 {
		t.Fatalf("got %+v from a minified stacktrace", response.Suspects)
	}
	if response.Symbolicated {
		t.Error("the answer claims the frames were symbolicated")
	}
	if response.Warning == "" {
		t.Fatal("no explanation for an empty answer")
	}
}

func TestSuspectsRefuseAnIssueThatIsNotThere(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	createProjectForIngest(t, client)

	response := client.do(http.MethodGet, suspectsPath(releaseProjectID, 987), nil)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d for an issue nobody has, want 404", response.StatusCode)
	}
}
