package httpapi

import (
	"net/http"

	"github.com/antoniojosev/trapline/internal/usecase"
)

// releaseHealthResponse is one release's crash-free story over a range.
//
// The counters are flattened in from usecase.ReleaseHealth, so the body reads
// `{"started":…, "crashed":…, "crash_free_rate":…}` — the shape the API
// promises, and the shape a chart wants: one object per hour with the same field
// names as the total above it, so a client renders both with one function.
type releaseHealthResponse struct {
	Project projectRef    `json:"project"`
	Range   rangeResponse `json:"range"`
	usecase.ReleaseHealth
}

// projectHealthResponse is every release of a project at once, which is the
// question somebody opens the page with: not "how is 1.4.2 doing" but "which
// one of these is the bad one".
type projectHealthResponse struct {
	Project projectRef    `json:"project"`
	Range   rangeResponse `json:"range"`
	usecase.ProjectHealth
}

// handleReleaseHealth answers "how is this version doing".
func (s *Server) handleReleaseHealth(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}
	// net/http already percent-decodes a path wildcard, so `shop%401.4.2`
	// arrives as the version somebody chose — which is why the CLI escapes it
	// on the way out. An empty version is refused by the use case rather than
	// here: "a release version is required" is a rule about releases, and a
	// second copy of it in the transport is a second place for it to drift.
	health, err := s.health.Release(r.Context(), project.ID, r.PathValue("version"), window)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, releaseHealthResponse{
		Project:       projectRef{ID: project.ID, Name: project.Name},
		Range:         newRangeResponse(health.Range),
		ReleaseHealth: health,
	})
}

// handleProjectHealth ranks every release that reported sessions in the range.
//
// It answers over the same range the other dashboard endpoints do rather than
// a fixed day, so a reader who narrowed the window on one chart does not find
// this one silently answering about something else.
func (s *Server) handleProjectHealth(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	health, err := s.health.Project(r.Context(), project.ID, window, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, projectHealthResponse{
		Project:       projectRef{ID: project.ID, Name: project.Name},
		Range:         newRangeResponse(health.Range),
		ProjectHealth: health,
	})
}
