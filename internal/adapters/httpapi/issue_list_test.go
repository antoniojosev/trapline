package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// sendDistinctErrors produces count issues by varying what identifies them.
func sendDistinctErrors(t *testing.T, serverURL string, dsn domain.DSN, count int) {
	t.Helper()
	for i := range count {
		body := pythonEnvelope(dsn, fmt.Sprintf("failure of kind %s", []string{
			"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta",
		}[i%8])+strconv.Itoa(i/8))
		if got := sendEnvelope(t, serverURL, dsn, body, "").StatusCode; got != http.StatusOK {
			t.Fatalf("event %d: status = %d", i, got)
		}
	}
}

func listPage(t *testing.T, c *client, projectID int64, query string) issueListResponse {
	t.Helper()
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + "/issues"
	if query != "" {
		path += "?" + query
	}
	var page issueListResponse
	c.decode(c.do(http.MethodGet, path, nil), &page)
	return page
}

func TestTheListReportsCountsAndPaging(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendDistinctErrors(t, server.URL, dsn, 12)

	page := listPage(t, client, dsn.ProjectID, "limit=5")

	if len(page.Issues) != 5 {
		t.Errorf("got %d issues, want the requested 5", len(page.Issues))
	}
	if page.NextCursor == "" {
		t.Error("no cursor, so a client cannot tell there is more")
	}
	// The counts label the filter buttons, so they cover the project rather
	// than the page.
	if page.Counts["unresolved"] != 12 {
		t.Errorf("unresolved count = %d, want 12 for the whole project", page.Counts["unresolved"])
	}
	for _, status := range []string{"unresolved", "resolved", "ignored"} {
		if _, present := page.Counts[status]; !present {
			t.Errorf("status %q missing from the counts", status)
		}
	}

	second := listPage(t, client, dsn.ProjectID, "limit=5&cursor="+page.NextCursor)
	if len(second.Issues) != 5 {
		t.Errorf("second page has %d issues", len(second.Issues))
	}
	if second.Issues[0].ID == page.Issues[0].ID {
		t.Error("the second page repeated the first")
	}
}

func TestFilteringByEnvironment(t *testing.T) {
	// environment is an ordinary tag, promoted on ingest, but it gets its own
	// parameter because it is the commonest question during an incident.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "in production"), "")

	sendEnvelope(t, server.URL, dsn, pythonEnvelopeIn(dsn, "in staging", "staging"), "")

	production := listPage(t, client, dsn.ProjectID, "environment=production")
	if len(production.Issues) != 1 {
		t.Fatalf("got %d issues in production, want 1:\n%+v", len(production.Issues), production.Issues)
	}
	if production.Issues[0].Title != "ValueError: in production" {
		t.Errorf("wrong issue: %q", production.Issues[0].Title)
	}

	// And the counts still describe the project, not the filter.
	if production.Counts["unresolved"] != 2 {
		t.Errorf("unresolved count = %d, want 2", production.Counts["unresolved"])
	}
}

func TestFilteringByAnArbitraryTag(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "tagged"), "")

	matching := listPage(t, client, dsn.ProjectID, "tag=server:web-01")
	if len(matching.Issues) != 1 {
		t.Errorf("got %d issues for a tag the event carries", len(matching.Issues))
	}

	missing := listPage(t, client, dsn.ProjectID, "tag=server:web-99")
	if len(missing.Issues) != 0 {
		t.Errorf("got %d issues for a tag nothing carries", len(missing.Issues))
	}
}

func TestBadListParameters(t *testing.T) {
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)
	base := api("/projects/") + strconv.FormatInt(dsn.ProjectID, 10) + "/issues?"

	for name, query := range map[string]string{
		"unknown status":    "status=muted",
		"bad limit":         "limit=abc",
		"zero limit":        "limit=0",
		"tag with no colon": "tag=server",
		"bad cursor":        "cursor=!!!",
	} {
		t.Run(name, func(t *testing.T) {
			if got := client.do(http.MethodGet, base+query, nil).StatusCode; got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}
}

func TestTheListingNamesItsProject(t *testing.T) {
	// The screen that reads this list is titled with the project's name, and
	// asking a second endpoint for one string means the heading can arrive
	// late, arrive wrong or not arrive at all while the rows below it are
	// already on screen.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendDistinctErrors(t, server.URL, dsn, 1)

	page := listPage(t, client, dsn.ProjectID, "")

	if page.Project.ID != dsn.ProjectID {
		t.Errorf("Project.ID = %d, want %d", page.Project.ID, dsn.ProjectID)
	}
	if page.Project.Name != "venekambio" {
		t.Errorf("Project.Name = %q, want the project's name", page.Project.Name)
	}
}

func TestAnEmptyListingStillNamesItsProject(t *testing.T) {
	// A quiet project is the common case for this screen, and it is the case
	// where a heading fed by the rows themselves would have nothing to read.
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	page := listPage(t, client, dsn.ProjectID, "")

	if len(page.Issues) != 0 {
		t.Fatalf("got %d issues, want none", len(page.Issues))
	}
	if page.Project.Name != "venekambio" {
		t.Errorf("Project.Name = %q, want the project's name even with no issues", page.Project.Name)
	}
}

func TestListingIssuesForAMissingProject(t *testing.T) {
	// An empty page is what a quiet project looks like. A mistyped id must not
	// be able to impersonate one: it would read as "nothing is broken", which
	// is the most expensive wrong answer this product can give.
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	path := api("/projects/9999/issues")
	if got := client.do(http.MethodGet, path, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}
