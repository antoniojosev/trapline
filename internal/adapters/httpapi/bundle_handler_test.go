package httpapi

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestIssueBundleIsMarkdownAndAnswersTheWholeQuestion is the endpoint's
// contract in one test: an event goes in through the public ingest path, and
// what comes back is a document rather than a record.
//
// The assertions are about *what the reader gets*, not about the exact
// wording — that is what the golden fixtures in `internal/usecase` are for.
// What this has to prove is the part the fixtures cannot: that the route
// exists, that it is behind the same guard as everything else, that the
// content type says markdown, and that the sections are populated from a real
// database rather than from a struct somebody filled in by hand.
func TestIssueBundleIsMarkdownAndAnswersTheWholeQuestion(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "").StatusCode; got != http.StatusOK {
		t.Fatalf("ingest status = %d, want 200", got)
	}

	issues := listIssues(t, client, dsn.ProjectID)
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	issue := issues[0]

	response := client.do(http.MethodGet, bundlePath(dsn.ProjectID, issue.ID), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != bundleContentType {
		t.Errorf("Content-Type = %q, want %q", got, bundleContentType)
	}

	document := readAll(t, response.Body)
	for _, want := range []string{
		"# ",                            // the title, as a heading
		"- **issue**: ",                 // the identifiers an agent calls back with
		"## Frequency",                  // read from the hourly aggregates
		"## Recent occurrences",         // the events that are still stored
		strconv.FormatInt(issue.ID, 10), // the id it was asked about
		"invalid amount",                // the message that actually arrived
	} {
		if !strings.Contains(document, want) {
			t.Errorf("the bundle does not contain %q:\n%s", want, document)
		}
	}
}

// TestIssueBundleRefusesAnIssueFromAnotherProject is the reason the route is
// nested under a project.
//
// An issue id is only unique within its project everywhere else in this API.
// A top-level /issues/{id}/bundle would have been the one endpoint where
// guessing a number reaches somebody else's data, and this is the test that
// would have caught it.
func TestIssueBundleRefusesAnIssueFromAnotherProject(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "invalid amount"), "")
	issue := listIssues(t, client, dsn.ProjectID)[0]

	var other projectResponse
	client.decode(client.do(http.MethodPost, api("/projects"), createProjectRequest{Name: "otro"}), &other)

	response := client.do(http.MethodGet, bundlePath(other.ID, issue.ID), nil)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — an issue was read through a project that does not own it",
			response.StatusCode)
	}
}

// TestIssueBundleNeedsACredential: the document is every fact about an issue,
// so it must not be the one read that skipped the guard.
func TestIssueBundleNeedsACredential(t *testing.T) {
	server := newTestServer(t)

	response := tokenRequest(t, server.URL, "", http.MethodGet, bundlePath(1, 1), nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.StatusCode)
	}
}

func bundlePath(projectID, issueID int64) string {
	return api("/projects/") + strconv.FormatInt(projectID, 10) +
		"/issues/" + strconv.FormatInt(issueID, 10) + "/bundle"
}

func readAll(t *testing.T, reader io.Reader) string {
	t.Helper()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(body)
}
