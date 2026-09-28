package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// listIssues reads a project's issues through the API, the way the panel and
// the CLI do.
func listIssues(t *testing.T, c *client, projectID int64) []issueResponse {
	t.Helper()
	var page issueListResponse
	c.decode(c.do(http.MethodGet, api("/projects/")+strconv.FormatInt(projectID, 10)+"/issues", nil), &page)
	return page.Issues
}

// listEvents reads an issue's stored events, payloads included.
func listEvents(t *testing.T, c *client, projectID, issueID int64) []struct {
	Payload json.RawMessage `json:"payload"`
} {
	t.Helper()
	var detail struct {
		Events []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	path := api("/projects/") + strconv.FormatInt(projectID, 10) + "/issues/" + strconv.FormatInt(issueID, 10)
	c.decode(c.do(http.MethodGet, path, nil), &detail)
	return detail.Events
}
