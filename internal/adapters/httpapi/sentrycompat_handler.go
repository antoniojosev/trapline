package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// The /api/0/ surface: enough of somebody else's API for sentry-cli to run a
// release through a deploy pipeline against this server (ADR 013).
//
// Every route here exists because a recording showed sentry-cli calling it.
// None of it was written from documentation, and the difference is not
// academic — the recording contradicts the documented shape in three places at
// once:
//
//   - Creating a release is addressed to the *project*
//     (POST /projects/{org}/{project}/releases/) while setting its commits is
//     addressed to the *organisation*
//     (PUT /organizations/{org}/releases/{version}/). Same tool, same command
//     sequence, two namespaces, and a server that implemented one of them
//     would fail halfway through a pipeline with everything already created.
//   - `set-commits --local` makes four calls, not one, and two of them must
//     answer with a JSON *array*. Answering `{}` — the obvious stub — makes
//     the tool stop with "invalid type: map, expected a sequence".
//   - `finalize` refuses a release object without a `version` field, so an
//     empty 200 is not a valid answer to any of these.
//
// That is the same lesson as ADR 002: @sentry/node sends no authentication
// header at all, and a server built from the documented one would have turned
// away every Node installation while its own tests stayed green.
//
// The fixtures in compat/sentry-cli/fixtures/ are the recording, they are
// committed, and sentrycompat_handler_test.go replays them through this file.

// maxCompatBody bounds a body on this surface. A commit set is the large one —
// a first `set-commits` on a real repository carries every path of every
// commit — so it shares the release budget rather than the panel's.
const maxCompatBody = maxCommitSetBody

// sentryCompatPrefix is the namespace this surface answers on. It is fixed by
// the client, not chosen here: sentry-cli builds every path from SENTRY_URL
// plus /api/0/.
const sentryCompatPrefix = "/api/0"

// sentryCompatHandler builds the emulated router.
//
// Returns nil when releases are not mounted: this surface is a second face on
// the release use cases and has nothing to say without them.
func (s *Server) sentryCompatHandler() http.Handler {
	if s.releases == nil {
		return nil
	}

	mux := http.NewServeMux()

	// Every pattern ends in {$}. Without it a pattern ending in "/" is a
	// subtree match in net/http, and `/releases/` would then swallow
	// `/releases/app@1.0.0/deploys/` — the routes below differ only by how
	// many segments they have, so anchoring them is what keeps them apart.
	//
	// The trailing slash itself is the client's: every path sentry-cli builds
	// has one, and the recording is what says so.
	routes := map[string]struct {
		scope   domain.Scope
		handler http.HandlerFunc
	}{
		// `releases new`, and the first thing `set-commits` does after
		// listing repositories. Both send the same body, so both are the same
		// idempotent create.
		"POST " + sentryCompatPrefix + "/projects/{org}/{project}/releases/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatCreateRelease},

		// `releases finalize`.
		"PUT " + sentryCompatPrefix + "/projects/{org}/{project}/releases/{version}/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatFinalizeRelease},

		// `releases set-commits`. Addressed to the organisation, which names
		// no project — see usecase.Releases.ProjectsWithVersion for how the
		// call is resolved without one.
		"PUT " + sentryCompatPrefix + "/organizations/{org}/releases/{version}/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatSetCommits},

		// `releases deploys new`. Also organisation-scoped.
		"POST " + sentryCompatPrefix + "/organizations/{org}/releases/{version}/deploys/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatCreateDeploy},

		// Read by `set-commits` before it decides what to send.
		"GET " + sentryCompatPrefix + "/organizations/{org}/repos/{$}": {
			domain.ScopeProjectsRead, s.handleCompatListRepos},
		"GET " + sentryCompatPrefix + "/organizations/{org}/releases/{version}/previous-with-commits/{$}": {
			domain.ScopeProjectsRead, s.handleCompatPreviousRelease},
	}
	for pattern, route := range routes {
		mux.Handle(pattern, s.requireToken(route.scope, route.handler))
	}

	// The source-map upload half of the same surface (ADR 018,
	// sentryartifacts_handler.go), mounted only when it was assembled. It is
	// registered on this mux rather than on a router of its own so that
	// "which endpoints does this server emulate" stays one question with one
	// answer, and so both halves share requireToken and the error shape.
	if s.artifacts != nil {
		for pattern, route := range s.sentryArtifactRoutes() {
			mux.Handle(pattern, s.requireToken(route.scope, route.handler))
		}
	}

	return compatRouterErrors(mux)
}

// requireToken is requireAuth without the session cookie.
//
// The distinction is the whole reason this surface can sit outside the CSRF
// check. That check defends an *ambient* credential: a browser can be tricked
// into replaying a cookie it holds, so a state-changing request authenticated
// by one must prove it was made deliberately. Nothing here is authenticated by
// a cookie. A bearer token is not ambient — a page cannot make the browser
// attach one — so there is nothing to forge, and requiring a header that
// sentry-cli has never heard of would reject the only client this surface
// exists for. That failure would have been total and silent: every test in
// this package would pass and no real pipeline could deploy.
func (s *Server) requireToken(required domain.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, found := bearerToken(r)
		if !found {
			writeCompatError(w, domain.ErrTokenNotFound)
			return
		}
		if _, err := s.tokens.Authenticate(r.Context(), token, required); err != nil {
			writeCompatError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sentryReleaseResponse is a release in the shape the client parses.
//
// The field names are the other API's, camel-cased, because they are read by a
// binary this project does not control. `version` is not optional: finalize
// fails with "missing field `version`" without it, which is how the recording
// found out.
type sentryReleaseResponse struct {
	Version      string                `json:"version"`
	DateCreated  time.Time             `json:"dateCreated"`
	DateReleased *time.Time            `json:"dateReleased"`
	FirstEvent   *time.Time            `json:"firstEvent"`
	LastEvent    *time.Time            `json:"lastEvent"`
	CommitCount  int64                 `json:"commitCount"`
	Projects     []sentryProjectStanza `json:"projects"`
}

// sentryProjectStanza is how the release names the project it belongs to.
type sentryProjectStanza struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func newSentryReleaseResponse(release *domain.Release, project *domain.Project) sentryReleaseResponse {
	return sentryReleaseResponse{
		Version:      release.Version,
		DateCreated:  release.CreatedAt,
		DateReleased: release.DateReleased,
		FirstEvent:   release.FirstEventAt,
		LastEvent:    release.LastEventAt,
		CommitCount:  release.CommitCount,
		Projects:     []sentryProjectStanza{{Slug: project.Slug, Name: project.Name}},
	}
}

// sentryDeployResponse is a deploy in the shape the client prints.
//
// The id is a string because that is what the other API returns for it, and a
// client that parses it as one would fail on a number.
type sentryDeployResponse struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Environment  string     `json:"environment"`
	URL          string     `json:"url,omitempty"`
	DateStarted  *time.Time `json:"dateStarted"`
	DateFinished *time.Time `json:"dateFinished"`
}

// compatReleaseRequest is the body of both the create and the finalise call.
//
// One struct for both because the client sends the same shape to both, with
// different fields filled in: `releases new` sends version and dateStarted,
// `finalize` sends dateReleased, and both send the project list.
type compatReleaseRequest struct {
	Version      string         `json:"version"`
	DateStarted  *time.Time     `json:"dateStarted"`
	DateReleased *time.Time     `json:"dateReleased"`
	Commits      []compatCommit `json:"commits"`
}

type compatCommit struct {
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
}

type compatDeployRequest struct {
	Environment  string     `json:"environment"`
	Name         string     `json:"name"`
	URL          string     `json:"url"`
	DateStarted  *time.Time `json:"dateStarted"`
	DateFinished *time.Time `json:"dateFinished"`
}

// handleCompatCreateRelease answers `releases new`, and the create that
// `set-commits` performs before sending anything.
func (s *Server) handleCompatCreateRelease(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.ResolveProject(r.Context(), r.PathValue("project"))
	if err != nil {
		writeCompatError(w, err)
		return
	}

	var request compatReleaseRequest
	if err := decodeCompatJSON(r, &request); err != nil {
		writeCompatError(w, err)
		return
	}

	release, created, err := s.releases.Create(r.Context(), project.ID, request.Version, request.DateReleased)
	if err != nil {
		writeCompatError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, newSentryReleaseResponse(&release, &project))
}

// handleCompatFinalizeRelease answers `releases finalize`.
//
// The client stamps the body with its own clock on every run, so a rerun of a
// deploy step arrives asking to move the release date. It does not move it:
// the use case keeps the first date, which is what makes finalising twice the
// same as finalising once.
func (s *Server) handleCompatFinalizeRelease(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.ResolveProject(r.Context(), r.PathValue("project"))
	if err != nil {
		writeCompatError(w, err)
		return
	}

	var request compatReleaseRequest
	if err := decodeCompatJSON(r, &request); err != nil {
		writeCompatError(w, err)
		return
	}

	version := r.PathValue("version")
	release, err := s.releases.Finalize(r.Context(), project.ID, version, request.DateReleased)
	if err != nil {
		writeCompatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSentryReleaseResponse(&release, &project))
}

// handleCompatSetCommits answers `releases set-commits`.
//
// Organisation-scoped: the body names no project, so the release version is
// the only thing left to address it with. The set is applied to every project
// holding that version, because in the API being emulated a release spans the
// organisation and dropping the others would lose a write silently.
func (s *Server) handleCompatSetCommits(w http.ResponseWriter, r *http.Request) {
	var request compatReleaseRequest
	if err := decodeCompatJSON(r, &request); err != nil {
		writeCompatError(w, err)
		return
	}

	version := r.PathValue("version")
	projects, err := s.releases.ProjectsWithVersion(r.Context(), version)
	if err != nil {
		writeCompatError(w, err)
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

	var last domain.Release
	for index := range projects {
		if _, err := s.releases.SetCommits(r.Context(), projects[index].ID, version, commits); err != nil {
			writeCompatError(w, err)
			return
		}
		detail, err := s.releases.Get(r.Context(), projects[index].ID, version)
		if err != nil {
			writeCompatError(w, err)
			return
		}
		last = detail.Release
	}
	writeJSON(w, http.StatusOK, newSentryReleaseResponse(&last, &projects[len(projects)-1]))
}

// handleCompatCreateDeploy answers `releases deploys new`, also
// organisation-scoped.
func (s *Server) handleCompatCreateDeploy(w http.ResponseWriter, r *http.Request) {
	var request compatDeployRequest
	if err := decodeCompatJSON(r, &request); err != nil {
		writeCompatError(w, err)
		return
	}

	version := r.PathValue("version")
	projects, err := s.releases.ProjectsWithVersion(r.Context(), version)
	if err != nil {
		writeCompatError(w, err)
		return
	}

	var last domain.Deploy
	for index := range projects {
		last, err = s.releases.AddDeploy(r.Context(), projects[index].ID, version, domain.Deploy{
			Environment: request.Environment,
			Name:        request.Name,
			URL:         request.URL,
			StartedAt:   request.DateStarted,
			FinishedAt:  request.DateFinished,
		})
		if err != nil {
			writeCompatError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, sentryDeployResponse{
		ID:           strconv.FormatInt(last.ID, 10),
		Name:         last.Name,
		Environment:  last.Environment,
		URL:          last.URL,
		DateStarted:  last.StartedAt,
		DateFinished: last.FinishedAt,
	})
}

// handleCompatListRepos answers with an empty list, which is the truth.
//
// This product integrates with no source host: it stores the commits a deploy
// tool hands it and never fetches any. The answer matters anyway, twice over.
// It has to be an array — the client stops on an object with "invalid type:
// map, expected a sequence" — and an empty one is what puts `set-commits
// --local` on the branch where it derives the commit range from the checkout
// in front of it instead of asking this server to resolve a repository it has
// never heard of.
func (s *Server) handleCompatListRepos(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []struct{}{})
}

// handleCompatPreviousRelease answers "nothing precedes it".
//
// The client asks this to find where the last release stopped, so it can send
// only what happened since. 404 is how it is told there is no answer, and it
// then sends the range it can see from the checkout — the same thing a first
// deploy sends. Answering it properly needs an ordering over releases *that
// carry commits*, which this server can compute (ADR 012) but has no query
// for; until it does, the honest answer is the one that makes the client send
// more rather than the one that makes it send the wrong window.
func (s *Server) handleCompatPreviousRelease(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, compatErrorBody{Detail: "no previous release with commits"})
}

// compatErrorBody is the error shape of the API being emulated. The client
// reads `detail` and prints it, so an error in this project's own shape would
// reach a user as a blank line.
type compatErrorBody struct {
	Detail string `json:"detail"`
}

func writeCompatError(w http.ResponseWriter, err error) {
	status, message := statusFor(err)
	if errors.Is(err, domain.ErrTokenNotFound) {
		message = "authentication credentials were not provided"
	}
	writeJSON(w, status, compatErrorBody{Detail: message})
}

// decodeCompatJSON reads a body from a client this project does not control.
//
// Unknown fields are *ignored*, which is the opposite of what the product's
// own API does. That is deliberate and it is the same rule as the envelope
// parser's tolerance (ADR 002): the fields on this wire are chosen by somebody
// else and gain members between releases of their tool — `dateStarted` on a
// release create, `refs` on a commit set, whatever comes next — and a server
// that rejected the first one it did not recognise would break on a client
// upgrade nobody here participated in.
func decodeCompatJSON(r *http.Request, target any) error {
	if r.ContentLength == 0 {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxCompatBody))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", errBadRequest, err)
	}
	return nil
}

// compatRouterErrors turns the router's own 404 and 405 into this surface's
// error shape, for the same reason jsonRouterErrors exists: a client should
// never have to parse "404 page not found" as a special case, and this client
// parses `detail`.
func compatRouterErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&compatErrorWriter{ResponseWriter: w}, r)
	})
}

type compatErrorWriter struct {
	http.ResponseWriter
	replace bool
	done    bool
}

func (w *compatErrorWriter) WriteHeader(status int) {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		w.replace = true
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compatErrorWriter) Write(b []byte) (int, error) {
	if !w.replace {
		n, err := w.ResponseWriter.Write(b)
		if err != nil {
			return n, err //nolint:wrapcheck // io.Writer contract: pass through untouched.
		}
		return n, nil
	}
	if !w.done {
		w.done = true
		encoded, err := json.Marshal(compatErrorBody{
			Detail: "this server emulates only the endpoints sentry-cli uses for releases",
		})
		if err != nil {
			return 0, err //nolint:wrapcheck // encoding a constant struct cannot realistically fail.
		}
		if _, err := w.ResponseWriter.Write(append(encoded, '\n')); err != nil {
			return 0, err //nolint:wrapcheck // io.Writer contract.
		}
	}
	return len(b), nil
}
