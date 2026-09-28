package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// rangeResponse is how every stats endpoint reports the window it actually
// used.
//
// Echoed rather than assumed, because the caller may have named neither end,
// or named ends that were rounded to the hour the buckets are kept in. A chart
// drawn against the range it asked for instead of the range it got is a chart
// whose axis is off by up to an hour and nobody knows why.
type rangeResponse struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Hours int       `json:"hours"`
}

func newRangeResponse(window domain.Range) rangeResponse {
	return rangeResponse{From: window.From, To: window.To, Hours: window.Hours()}
}

type seriesResponse struct {
	Project projectRef            `json:"project"`
	Range   rangeResponse         `json:"range"`
	Total   int64                 `json:"total"`
	Series  []usecase.HourlyPoint `json:"series"`
	// Levels is the range's total per level: the chart's legend, which every
	// client would otherwise compute by walking the series and would each get
	// subtly differently once one of them started paginating.
	Levels map[string]int64 `json:"by_level"`
}

type topResponse struct {
	Project projectRef      `json:"project"`
	Range   rangeResponse   `json:"range"`
	Issues  []topIssueEntry `json:"issues"`
}

// topIssueEntry is an issue plus how loud it was in the range.
//
// Count is deliberately not called "times": the issue already carries a
// lifetime total under that name, and a response with two fields meaning
// different totals under interchangeable names is a bug waiting for whoever
// reads it fastest.
type topIssueEntry struct {
	issueResponse
	Count int64 `json:"count"`
}

type breakdownResponse struct {
	Project projectRef             `json:"project"`
	Range   rangeResponse          `json:"range"`
	By      string                 `json:"by"`
	Values  []ports.DimensionCount `json:"values"`
}

type issueStatsResponse struct {
	IssueID int64             `json:"issue_id"`
	Window  string            `json:"range"`
	Range   rangeResponse     `json:"range_bounds"`
	Total   int64             `json:"total"`
	Series  []ports.HourCount `json:"series"`
}

// handleProjectStats is the dashboard's main chart: events per hour, split by
// level.
func (s *Server) handleProjectStats(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}

	series, err := s.stats.ProjectSeries(r.Context(), project.ID, window)
	if err != nil {
		writeError(w, err)
		return
	}

	response := seriesResponse{
		Project: projectRef{ID: project.ID, Name: project.Name},
		Range:   newRangeResponse(series.Range),
		Total:   series.Total,
		Series:  series.Points,
		Levels:  map[string]int64{},
	}
	for index := range series.Points {
		for level, count := range series.Points[index].ByLevel {
			response.Levels[level] += count
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// handleTopIssues is "what is loudest right now", which is a different
// question from the issue list's "what happened most recently".
func (s *Server) handleTopIssues(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	top, err := s.stats.TopIssues(r.Context(), project.ID, window, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	response := topResponse{
		Project: projectRef{ID: project.ID, Name: project.Name},
		Range:   newRangeResponse(window),
		Issues:  make([]topIssueEntry, 0, len(top)),
	}
	for index := range top {
		response.Issues = append(response.Issues, topIssueEntry{
			issueResponse: newIssueResponse(&top[index].Issue),
			Count:         top[index].Count,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// handleBreakdown answers "which release brought this" and "is it only
// staging".
func (s *Server) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	dimension, err := domain.ParseDimension(r.URL.Query().Get("by"))
	if err != nil {
		writeError(w, err)
		return
	}

	values, err := s.stats.Breakdown(r.Context(), project.ID, dimension, window, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if values == nil {
		values = []ports.DimensionCount{}
	}

	writeJSON(w, http.StatusOK, breakdownResponse{
		Project: projectRef{ID: project.ID, Name: project.Name},
		Range:   newRangeResponse(window),
		By:      string(dimension),
		Values:  values,
	})
}

// handleIssueStats is one issue's own chart, over a named window rather than
// arbitrary ends: it is read beside the issue, where the question is "is this
// still happening" and not "between which two instants".
func (s *Server) handleIssueStats(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	issueID, err := pathID(r, "issueID")
	if err != nil {
		writeError(w, err)
		return
	}
	window, err := domain.ParseWindow(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, err)
		return
	}

	series, err := s.stats.IssueSeries(r.Context(), projectID, issueID, window)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, issueStatsResponse{
		IssueID: series.Issue.ID,
		Window:  string(series.Window),
		Range:   newRangeResponse(series.Range),
		Total:   series.Total,
		Series:  series.Points,
	})
}

type sparklinesResponse struct {
	Project projectRef               `json:"project"`
	Window  string                   `json:"range"`
	Range   rangeResponse            `json:"range_bounds"`
	Hours   []string                 `json:"hours"`
	Issues  []usecase.IssueSparkline `json:"issues"`
}

// handleIssueSparklines draws the little chart beside every row of a listing.
//
// One endpoint for a page of issues rather than one call per row. Twenty-five
// rows is twenty-five requests, twenty-five authentications and twenty-five
// transactions to answer a question one index scan already covers — and the
// list would visibly fill in from the top as they landed.
func (s *Server) handleIssueSparklines(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	project, err := s.projects.Find(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}
	window, err := domain.ParseWindow(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, err)
		return
	}
	issueIDs, err := parseIssueIDs(r.URL.Query().Get("issues"))
	if err != nil {
		writeError(w, err)
		return
	}

	span, sparklines, err := s.stats.IssuesSeries(r.Context(), projectID, issueIDs, window)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, sparklinesResponse{
		Project: projectRef{ID: project.ID, Name: project.Name},
		Window:  string(window),
		Range:   newRangeResponse(span),
		// The hours the counts line up with, sent once rather than repeated
		// on every point of every series. A chart that has to label an axis
		// needs them; one that only draws a shape can ignore them.
		Hours:  span.Buckets(),
		Issues: sparklines,
	})
}

// parseIssueIDs reads the comma-separated list of issues a sparkline request
// names.
//
// Bounded, because this is the one endpoint whose cost a caller sets: an
// unbounded list is an unbounded IN clause. The limit is the longest page the
// listing serves, so a client that asks for what it is showing never hits it.
func parseIssueIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: issues must name at least one issue id", errBadRequest)
	}
	fields := strings.Split(raw, ",")
	if len(fields) > domain.MaxSparklineIssues {
		return nil, fmt.Errorf("%w: %d issues requested, at most %d at a time",
			errBadRequest, len(fields), domain.MaxSparklineIssues)
	}

	issueIDs := make([]int64, 0, len(fields))
	seen := make(map[int64]bool, len(fields))
	for _, field := range fields {
		issueID, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || issueID <= 0 {
			return nil, fmt.Errorf("%w: %q is not an issue id", errBadRequest, field)
		}
		// Deduplicated so a client cannot ask for the same issue a hundred
		// times and get a hundred copies back.
		if seen[issueID] {
			continue
		}
		seen[issueID] = true
		issueIDs = append(issueIDs, issueID)
	}
	return issueIDs, nil
}

// statsRequest resolves the two things every project-level stats endpoint
// needs: that the project exists, and what range was asked for.
//
// The project is read first for the same reason the issue listing reads it: a
// project id nobody owns answers 404 rather than a chart of zeroes, so a typo
// cannot impersonate a quiet project.
func (s *Server) statsRequest(
	w http.ResponseWriter, r *http.Request,
) (domain.Project, domain.Range, bool) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return domain.Project{}, domain.Range{}, false
	}
	project, err := s.projects.Find(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return domain.Project{}, domain.Range{}, false
	}

	query := r.URL.Query()
	window, err := s.stats.Range(query.Get("from"), query.Get("to"))
	if err != nil {
		writeError(w, err)
		return domain.Project{}, domain.Range{}, false
	}
	return project, window, true
}

// errPositiveLimit is the one message every endpoint that takes a limit
// returns, so a client does not get a different sentence per route.
var errPositiveLimit = fmt.Errorf("%w: limit must be a positive integer", errBadRequest)

// parseLimit reads an optional positive limit. Zero means "the endpoint's
// default", which each repository bounds for itself.
func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		writeError(w, errPositiveLimit)
		return 0, false
	}
	return limit, true
}
