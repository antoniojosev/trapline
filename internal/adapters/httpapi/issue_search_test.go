package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// searchIssues runs a listing with a `q`, the way the panel's search box does.
func searchIssues(t *testing.T, c *client, projectID int64, query string) issueListResponse {
	t.Helper()
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + "/issues?q=" + url(query)
	var page issueListResponse
	c.decode(c.do(http.MethodGet, path, nil), &page)
	return page
}

// url escapes a query parameter without pulling in a helper the rest of these
// tests do not have.
func url(raw string) string {
	replacer := strings.NewReplacer(" ", "%20", "\"", "%22", "[", "%5B", "]", "%5D", "/", "%2F")
	return replacer.Replace(raw)
}

func TestSearchFindsTheMiddleOfAnExceptionName(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	// Two compound exception names, which is what real ones look like.
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "ConnectionTimeoutError: upstream gone", "production", "app@1.0.0", 10), "")
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "ECONNREFUSED: nothing listening", "production", "app@1.0.0", 10), "")

	// The search a person actually performs: the part they remember, which is
	// in the middle of the token. Under a word-boundary index every one of
	// these found nothing.
	for query, want := range map[string]int{
		"Timeout":     1,
		"CONNREFUSED": 1,
		"Error":       2,
		"upstream":    1,
		"Kafka":       0,
	} {
		page := searchIssues(t, client, dsn.ProjectID, query)
		if len(page.Issues) != want {
			t.Errorf("search for %q found %d issues, want %d", query, len(page.Issues), want)
		}
		// The counts describe the project, not the query, so they are there
		// whether or not anything matched.
		if page.Counts["unresolved"] != 2 {
			t.Errorf("search for %q reported counts %+v", query, page.Counts)
		}
	}
}

func TestSearchTooShortIsRefusedNotAnsweredEmpty(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "ValueError: nope", "production", "app@1.0.0", 10), "")

	path := api("/projects/") + strconv.FormatInt(dsn.ProjectID, 10) + "/issues?q=ab"
	response := client.do(http.MethodGet, path, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: an empty page would look like 'nothing matched'", response.StatusCode)
	}
	var payload errorBody
	client.decode(response, &payload)
	// The message has to name the minimum, or the only way to learn it is to
	// keep typing until something happens.
	if !strings.Contains(payload.Error, "3 characters") {
		t.Errorf("error = %q, want it to say how long a term has to be", payload.Error)
	}

	// And a short term beside a usable one is dropped, not refused.
	if page := searchIssues(t, client, dsn.ProjectID, "ab ValueError"); len(page.Issues) != 1 {
		t.Errorf("search = %+v, want the one issue", page.Issues)
	}
}

func TestSearchIgnoresAccentsThroughTheAPI(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)
	// The compat suite sends messages with accents, so this is not a
	// hypothetical: the tokenizer changed and the folding had to survive it.
	sendEnvelope(t, server.URL, dsn, statsEnvelope(dsn, "PaymentError: no se pudo procesar la conexión", "production", "app@1.0.0", 10), "")

	for _, query := range []string{"conexión", "conexion", "CONEXION"} {
		if page := searchIssues(t, client, dsn.ProjectID, query); len(page.Issues) != 1 {
			t.Errorf("search for %q found %d issues, want 1", query, len(page.Issues))
		}
	}
}
