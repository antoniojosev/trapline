package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

type issueResponse struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	Fingerprint string    `json:"fingerprint"`
	Title       string    `json:"title"`
	Culprit     string    `json:"culprit"`
	Level       string    `json:"level"`
	Status      string    `json:"status"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Times       int64     `json:"times"`
	LastRelease string    `json:"last_release,omitempty"`
}

func newIssueResponse(issue *domain.Issue) issueResponse {
	return issueResponse{
		ID:          issue.ID,
		ProjectID:   issue.ProjectID,
		Fingerprint: issue.Fingerprint,
		Title:       issue.Title,
		Culprit:     issue.Culprit,
		Level:       string(issue.Level),
		Status:      string(issue.Status),
		FirstSeen:   issue.FirstSeen,
		LastSeen:    issue.LastSeen,
		Times:       issue.Times,
		LastRelease: issue.LastRelease,
	}
}

type eventResponse struct {
	ID          int64     `json:"id"`
	EventID     string    `json:"event_id"`
	OccurredAt  time.Time `json:"occurred_at"`
	ReceivedAt  time.Time `json:"received_at"`
	Level       string    `json:"level"`
	Release     string    `json:"release,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Message     string    `json:"message,omitempty"`
	// Payload is the whole stored event, already scrubbed. It is what a
	// detail view renders and what an agent reads to fix the bug, so it is
	// returned raw rather than summarised into fields somebody has to keep in
	// sync with the protocol.
	Payload jsonRaw `json:"payload"`
}

// jsonRaw embeds already-encoded JSON without re-encoding it.
type jsonRaw []byte

// MarshalJSON writes the stored bytes through untouched.
func (r jsonRaw) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

type issueDetailResponse struct {
	issueResponse
	Events []eventResponse             `json:"events"`
	Tags   map[string][]ports.TagCount `json:"tags"`
	// The release-aware half of the lifecycle. On the detail and not on the
	// listing: it is read one issue at a time, and carrying six more columns
	// through every page of a list would cost every reader for one reader's
	// benefit.
	FirstRelease      string     `json:"first_release,omitempty"`
	ResolvedAt        *time.Time `json:"resolved_at,omitempty"`
	ResolvedInRelease string     `json:"resolved_in_release,omitempty"`
	// ResolveNextRelease is what makes the status believable: with it on,
	// events from the release it was resolved in are expected rather than a
	// regression.
	ResolveNextRelease bool  `json:"resolve_next_release"`
	Regressions        int64 `json:"regressions"`
	// RegressedInRelease is which deploy brought it back, which is the other
	// half of "is this new or is it back?".
	RegressedInRelease string `json:"regressed_in_release,omitempty"`
	// SeenInResolvedReleaseCount is how many events were counted but not
	// treated as a regression. Without it the product would look like it had
	// lost them.
	SeenInResolvedReleaseCount int64 `json:"seen_in_resolved_release_count"`
}

// issueListResponse is a page of issues.
//
// An object rather than a bare array, because a page needs to say whether
// there is another one and what the filter buttons should read. Returning the
// array alone forced a client to guess at both.
type issueListResponse struct {
	Issues []issueResponse `json:"issues"`
	// NextCursor is empty when there is nothing more.
	NextCursor string `json:"next_cursor,omitempty"`
	// Counts covers the whole project rather than the current filter, so the
	// numbers do not change depending on which button is already pressed.
	Counts map[string]int64 `json:"counts"`
	// Project is who these issues belong to. A listing is always read under a
	// heading that names the project, and without this every client had to ask
	// a second endpoint for one string — a request that can fail, arrive late
	// and leave the heading reading "#3" while the rows below it are already
	// there.
	Project projectRef `json:"project"`
}

// projectRef is the smallest useful identification of a project: enough to
// title a screen, deliberately not a second copy of the project resource that
// would have to be kept in sync with it.
type projectRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	// The project is read before the filter is parsed, so a listing for an id
	// nobody owns answers 404 instead of an empty page. An empty page is what
	// a quiet project looks like, and a typo should not be able to impersonate
	// one — the same reason the config endpoint already refuses to hand back
	// defaults for an id somebody mistyped.
	project, err := s.projects.Find(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}

	query := r.URL.Query()
	filter := ports.IssueFilter{
		ProjectID: projectID,
		Status:    domain.IssueStatus(query.Get("status")),
		Query:     query.Get("q"),
		Cursor:    query.Get("cursor"),
		Tags:      map[string]string{},
	}
	if filter.Status != "" && !filter.Status.ValidStatus() {
		writeError(w, fmt.Errorf("%w: unknown status %q", errBadRequest, filter.Status))
		return
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			writeError(w, fmt.Errorf("%w: limit must be a positive integer", errBadRequest))
			return
		}
		filter.Limit = limit
	}

	// environment and release are ordinary tags, promoted on ingest. They get
	// their own parameters anyway because they are what people filter by, and
	// making someone spell tag=environment:production to ask the commonest
	// question is a needless riddle.
	for _, name := range []string{"environment", "release"} {
		if value := query.Get(name); value != "" {
			filter.Tags[name] = value
		}
	}
	for _, pair := range query["tag"] {
		key, value, found := strings.Cut(pair, ":")
		if !found || key == "" {
			writeError(w, fmt.Errorf("%w: tag must be key:value, got %q", errBadRequest, pair))
			return
		}
		filter.Tags[key] = value
	}

	page, err := s.issues.List(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}

	response := issueListResponse{
		Issues:     make([]issueResponse, 0, len(page.Issues)),
		NextCursor: page.NextCursor,
		Counts:     make(map[string]int64, len(page.Counts)),
		Project:    projectRef{ID: project.ID, Name: project.Name},
	}
	for index := range page.Issues {
		response.Issues = append(response.Issues, newIssueResponse(&page.Issues[index]))
	}
	for status, count := range page.Counts {
		response.Counts[string(status)] = count
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
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

	detail, err := s.issues.Get(r.Context(), projectID, issueID, 0)
	if err != nil {
		writeError(w, err)
		return
	}

	response := issueDetailResponse{
		issueResponse: newIssueResponse(&detail.Issue),
		Events:        make([]eventResponse, 0, len(detail.Events)),
		Tags:          detail.Tags,
	}
	// Read from the repository that owns those columns rather than from the
	// issue row this handler already has, because they are written by a
	// different transaction from the one that records an event (ADR 012).
	if s.releases != nil {
		resolution, err := s.releases.Resolution(r.Context(), projectID, issueID)
		if err != nil {
			writeError(w, err)
			return
		}
		response.FirstRelease = resolution.FirstRelease
		response.ResolvedAt = resolution.ResolvedAt
		response.ResolvedInRelease = resolution.ResolvedInRelease
		response.ResolveNextRelease = resolution.ResolveNextRelease
		response.Regressions = resolution.Regressions
		response.RegressedInRelease = resolution.RegressedInRelease
		response.SeenInResolvedReleaseCount = resolution.SeenInResolvedReleaseCount
	}
	for index := range detail.Events {
		event := &detail.Events[index]
		response.Events = append(response.Events, eventResponse{
			ID:          event.ID,
			EventID:     event.EventID,
			OccurredAt:  event.OccurredAt,
			ReceivedAt:  event.ReceivedAt,
			Level:       string(event.Level),
			Release:     event.Release,
			Environment: event.Environment,
			Message:     event.Message,
			Payload:     event.Payload,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// handleSetIssueStatus resolves, ignores or reopens an issue.
//
// One endpoint taking a status rather than three verbs, because the three are
// the same operation with different arguments and an agent should not have to
// learn three routes to change one field.
func (s *Server) handleSetIssueStatus(w http.ResponseWriter, r *http.Request) {
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

	var request struct {
		Status string `json:"status"`
		// InNextRelease qualifies "resolved": fixed, and the fix ships next,
		// so events from the release it is currently being seen in are
		// expected rather than proof the fix failed.
		InNextRelease bool `json:"in_next_release"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	status := domain.IssueStatus(request.Status)
	if !status.ValidStatus() {
		writeError(w, fmt.Errorf("%w: status must be one of unresolved, resolved, ignored", errBadRequest))
		return
	}
	if request.InNextRelease && status != domain.StatusResolved {
		// Silently ignoring it would leave a caller believing they had asked
		// for something the server never did.
		writeError(w, fmt.Errorf("%w: in_next_release only applies to status \"resolved\"", errBadRequest))
		return
	}

	var action func(context.Context, int64, int64) error
	switch status {
	case domain.StatusResolved:
		action = s.issues.Resolve
		if request.InNextRelease {
			action = s.issues.ResolveInNextRelease
		}
	case domain.StatusIgnored:
		action = s.issues.Ignore
	case domain.StatusUnresolved:
		action = s.issues.Reopen
	}

	if err := action(r.Context(), projectID, issueID); err != nil {
		writeError(w, err)
		return
	}
	response := map[string]any{"status": string(status)}
	if request.InNextRelease {
		response["in_next_release"] = true
	}
	writeJSON(w, http.StatusOK, response)
}
