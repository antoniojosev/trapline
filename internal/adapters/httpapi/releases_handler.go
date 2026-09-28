package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// releaseResponse is one release as the API returns it.
type releaseResponse struct {
	Version   string    `json:"version"`
	ProjectID int64     `json:"project_id"`
	CreatedAt time.Time `json:"created_at"`
	// DateReleased is null while the release has not been finalised, which is
	// how "we are still deploying this" is told apart from "this is live".
	DateReleased *time.Time `json:"date_released"`
	FirstEventAt *time.Time `json:"first_event_at"`
	LastEventAt  *time.Time `json:"last_event_at"`
	CommitCount  int64      `json:"commit_count"`
}

func newReleaseResponse(release *domain.Release) releaseResponse {
	return releaseResponse{
		Version:      release.Version,
		ProjectID:    release.ProjectID,
		CreatedAt:    release.CreatedAt,
		DateReleased: release.DateReleased,
		FirstEventAt: release.FirstEventAt,
		LastEventAt:  release.LastEventAt,
		CommitCount:  release.CommitCount,
	}
}

type releaseListResponse struct {
	Releases []releaseResponse `json:"releases"`
}

type commitFileResponse struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

type commitResponse struct {
	ID          string     `json:"id"`
	Message     string     `json:"message,omitempty"`
	AuthorName  string     `json:"author_name,omitempty"`
	AuthorEmail string     `json:"author_email,omitempty"`
	Timestamp   *time.Time `json:"timestamp,omitempty"`
	Repository  string     `json:"repository,omitempty"`
	// PatchSet is the paths this commit touched, named as the protocol names
	// them so a tool that already speaks that shape needs no translation.
	PatchSet []commitFileResponse `json:"patch_set,omitempty"`
}

// newCommitResponse encodes one commit.
//
// Shared with the suspect list rather than written twice: a commit that
// rendered one way on the release page and another beside a stacktrace would
// be two vocabularies for one record, and the patch set is exactly the field
// somebody would forget in the second copy.
func newCommitResponse(commit *domain.Commit) commitResponse {
	encoded := commitResponse{
		ID:          commit.SHA,
		Message:     commit.Message,
		AuthorName:  commit.AuthorName,
		AuthorEmail: commit.AuthorEmail,
		Timestamp:   commit.Timestamp,
		Repository:  commit.Repository,
	}
	for _, file := range commit.Files {
		encoded.PatchSet = append(encoded.PatchSet,
			commitFileResponse{Path: file.Path, Type: string(file.ChangeType)})
	}
	return encoded
}

type deployResponse struct {
	ID          int64      `json:"id"`
	Environment string     `json:"environment"`
	Name        string     `json:"name,omitempty"`
	URL         string     `json:"url,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// releaseDetailResponse is a release with the numbers it is read for.
type releaseDetailResponse struct {
	releaseResponse
	// NewIssues are issues whose first event carried this release, and
	// RegressedIssues are issues that came back and were last seen in it.
	// Between them they are the whole point of a release page: did I break
	// something new, and did I bring something back.
	NewIssues       int64 `json:"new_issues"`
	RegressedIssues int64 `json:"regressed_issues"`
	// Events is how many events this release produced, from the hourly
	// aggregates (ADR 010). Always present: a release with no events has
	// zero, and a caller should not have to tell "no events" apart from
	// "this build cannot say".
	Events  int64            `json:"events"`
	Commits []commitResponse `json:"commits"`
	Deploys []deployResponse `json:"deploys"`
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			writeError(w, fmt.Errorf("%w: limit must be a positive integer", errBadRequest))
			return
		}
	}

	releases, err := s.releases.List(r.Context(), projectID, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	response := releaseListResponse{Releases: make([]releaseResponse, 0, len(releases))}
	for index := range releases {
		response.Releases = append(response.Releases, newReleaseResponse(&releases[index]))
	}
	writeJSON(w, http.StatusOK, response)
}

// handleCreateRelease registers a release before its first error arrives.
//
// Answering 200 rather than 409 when it already exists, because every caller
// of this is a deploy pipeline: a rerun of a step, or a deploy annotation
// that arrives after an event already created the release implicitly, is
// normal operation and not a conflict anybody can act on.
func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	var request struct {
		Version      string     `json:"version"`
		DateReleased *time.Time `json:"date_released"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	release, created, err := s.releases.Create(r.Context(), projectID, request.Version, request.DateReleased)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, newReleaseResponse(&release))
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	detail, err := s.releases.Get(r.Context(), projectID, r.PathValue("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newReleaseDetailResponse(&detail))
}

func newReleaseDetailResponse(detail *ports.ReleaseDetail) releaseDetailResponse {
	response := releaseDetailResponse{
		releaseResponse: newReleaseResponse(&detail.Release),
		NewIssues:       detail.Stats.NewIssues,
		RegressedIssues: detail.Stats.RegressedIssues,
		Events:          detail.Stats.Events,
		Commits:         make([]commitResponse, 0, len(detail.Commits)),
		Deploys:         make([]deployResponse, 0, len(detail.Deploys)),
	}
	for index := range detail.Commits {
		response.Commits = append(response.Commits, newCommitResponse(&detail.Commits[index]))
	}
	for index := range detail.Deploys {
		deploy := &detail.Deploys[index]
		response.Deploys = append(response.Deploys, deployResponse{
			ID:          deploy.ID,
			Environment: deploy.Environment,
			Name:        deploy.Name,
			URL:         deploy.URL,
			StartedAt:   deploy.StartedAt,
			FinishedAt:  deploy.FinishedAt,
		})
	}
	return response
}

// handleFinalizeRelease records when a release was declared shipped.
//
// PUT rather than a POST to a verb: finalising twice is the same as
// finalising once — the domain keeps the first date — so the operation is
// idempotent, and idempotent is what PUT means.
func (s *Server) handleFinalizeRelease(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	var request struct {
		DateReleased *time.Time `json:"date_released"`
	}
	// An empty body means "now", which is what a deploy script sends.
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, err)
			return
		}
	}

	release, err := s.releases.Finalize(r.Context(), projectID, r.PathValue("version"), request.DateReleased)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newReleaseResponse(&release))
}

// handleSetReleaseCommits associates a commit set with a release.
//
// The whole set replaces whatever was there, because that is what every
// sender sends and appending would double it on a pipeline rerun.
func (s *Server) handleSetReleaseCommits(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	var request struct {
		Commits []struct {
			ID          string     `json:"id"`
			Message     string     `json:"message"`
			AuthorName  string     `json:"author_name"`
			AuthorEmail string     `json:"author_email"`
			Timestamp   *time.Time `json:"timestamp"`
			Repository  string     `json:"repository"`
			PatchSet    []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"patch_set"`
		} `json:"commits"`
	}
	if err := decodeJSONLarge(r, &request); err != nil {
		writeError(w, err)
		return
	}

	commits := make([]domain.Commit, 0, len(request.Commits))
	for _, sent := range request.Commits {
		commit := domain.Commit{
			SHA:         sent.ID,
			Message:     sent.Message,
			AuthorName:  sent.AuthorName,
			AuthorEmail: sent.AuthorEmail,
			Timestamp:   sent.Timestamp,
			Repository:  sent.Repository,
		}
		for _, file := range sent.PatchSet {
			commit.Files = append(commit.Files,
				domain.CommitFile{Path: file.Path, ChangeType: domain.ChangeType(file.Type)})
		}
		commits = append(commits, commit)
	}

	count, err := s.releases.SetCommits(r.Context(), projectID, r.PathValue("version"), commits)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"commits": count})
}

func (s *Server) handleCreateDeploy(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	var request struct {
		Environment string     `json:"environment"`
		Name        string     `json:"name"`
		URL         string     `json:"url"`
		StartedAt   *time.Time `json:"started_at"`
		FinishedAt  *time.Time `json:"finished_at"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	deploy, err := s.releases.AddDeploy(r.Context(), projectID, r.PathValue("version"), domain.Deploy{
		Environment: request.Environment,
		Name:        request.Name,
		URL:         request.URL,
		StartedAt:   request.StartedAt,
		FinishedAt:  request.FinishedAt,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, deployResponse{
		ID:          deploy.ID,
		Environment: deploy.Environment,
		Name:        deploy.Name,
		URL:         deploy.URL,
		StartedAt:   deploy.StartedAt,
		FinishedAt:  deploy.FinishedAt,
	})
}
