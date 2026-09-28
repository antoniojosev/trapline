package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/usecase"
)

// projectResponse is a project as the API returns it.
//
// The DSN is included in full rather than left for the client to assemble
// from parts. Assembling it is where a client gets the protocol's one
// hard-coded shape wrong, and it is the single string every user actually
// wants from this endpoint.
type projectResponse struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Slug is the name a deploy tool puts in a URL (ADR 013). It is here
	// rather than only in the compatibility surface because somebody
	// configuring a pipeline has to be able to find out what it is, and the
	// place they look is the project they just created.
	Slug      string        `json:"slug"`
	CreatedAt time.Time     `json:"created_at"`
	DSN       string        `json:"dsn"`
	Keys      []keyResponse `json:"keys"`
}

type keyResponse struct {
	PublicKey string    `json:"public_key"`
	DSN       string    `json:"dsn"`
	CreatedAt time.Time `json:"created_at"`
}

func newProjectResponse(view usecase.ProjectView) projectResponse {
	response := projectResponse{
		ID:        view.Project.ID,
		Name:      view.Project.Name,
		Slug:      view.Project.Slug,
		CreatedAt: view.Project.CreatedAt,
		Keys:      make([]keyResponse, 0, len(view.Keys)),
	}
	if dsn, ok := view.PrimaryDSN(); ok {
		response.DSN = dsn.String()
	}
	for _, key := range view.Keys {
		response.Keys = append(response.Keys, keyResponse{
			PublicKey: key.Key.PublicKey,
			DSN:       key.DSN.String(),
			CreatedAt: key.Key.CreatedAt,
		})
	}
	return response
}

type createProjectRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	views, err := s.projects.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	// An empty list is [], never null: a client should not have to handle two
	// shapes for "nothing here".
	responses := make([]projectResponse, 0, len(views))
	for _, view := range views {
		responses = append(responses, newProjectResponse(view))
	}
	writeJSON(w, http.StatusOK, responses)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var request createProjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	view, err := s.projects.Create(r.Context(), request.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newProjectResponse(view))
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := s.projects.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newProjectResponse(view))
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.projects.Delete(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRotateKey issues an additional key, leaving the current one active.
// Rotation is deliberately two steps — issue, then revoke — so a running
// deployment never loses its key mid-flight.
func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	key, err := s.projects.RotateKey(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, keyResponse{
		PublicKey: key.Key.PublicKey,
		DSN:       key.DSN.String(),
		CreatedAt: key.Key.CreatedAt,
	})
}

// handleRevokeKey retires a key. Idempotent: revoking an already revoked or
// unknown key returns success, so a retried command is safe.
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	publicKey := r.PathValue("publicKey")
	if publicKey == "" {
		writeError(w, fmt.Errorf("%w: missing public key", errBadRequest))
		return
	}
	if err := s.projects.RevokeKey(r.Context(), publicKey); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// pathID reads a positive integer path parameter.
func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive integer, got %q", errBadRequest, name, raw)
	}
	return id, nil
}
