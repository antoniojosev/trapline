package httpapi

import (
	"io"
	"net/http"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// This product's own artefact endpoints, as opposed to the emulated ones.
//
// They exist because of ADR 006: every operation is reachable from the REST
// API, the CLI and the panel, and "upload source maps" must not be an
// operation you can only perform by installing somebody else's tool. `trapline
// artifacts upload` builds an artifact bundle locally — the same archive
// sentry-cli builds — and posts it here, so both paths converge on one reader
// and one set of rules (usecase.Artifacts.StoreBundle).
//
// The upload takes the whole archive as the request body rather than a
// multipart form. There is exactly one thing in the request, its size is
// already bounded, and a form would only be a second encoding of the same
// bytes for a client this project writes.

// maxUploadedBundle bounds a bundle posted to the native endpoint. It is the
// same ceiling the emulated surface reports as maxFileSize, so the two paths
// refuse the same uploads.
const maxUploadedBundle = 200 << 20

// artifactPayload is one stored artefact, without its bytes.
type artifactPayload struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	ReleaseID *int64 `json:"release_id"`
	Dist      string `json:"dist"`
	// DebugID is empty for an artefact uploaded the old way, which is not a
	// gap: those are found by release and url instead (ADR 018).
	DebugID string `json:"debug_id"`
	// BundleDebugID says which upload this file arrived in. Nothing looks an
	// artefact up by it; it is here so somebody staring at two versions of
	// one file can tell them apart (from the recording).
	BundleDebugID string `json:"bundle_debug_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	// SourceMapRef is the file name of a script's map. It is what joins a
	// script to its map when there is no debug id.
	SourceMapRef string    `json:"sourcemap_ref"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
}

func newArtifactPayload(artifact domain.Artifact) artifactPayload {
	return artifactPayload{
		ID:            artifact.ID,
		ProjectID:     artifact.ProjectID,
		ReleaseID:     artifact.ReleaseID,
		Dist:          artifact.Dist,
		DebugID:       artifact.DebugID,
		BundleDebugID: artifact.BundleDebugID,
		Name:          artifact.Name,
		Kind:          string(artifact.Kind),
		SourceMapRef:  artifact.SourceMapRef,
		SHA256:        artifact.SHA256,
		Size:          artifact.Size,
		CreatedAt:     artifact.CreatedAt,
	}
}

type artifactListResponse struct {
	Artifacts []artifactPayload `json:"artifacts"`
	// Used and Budget are here rather than behind an endpoint of their own
	// because they are what the reader of this list actually wants to know:
	// an upload that starts failing does so for one reason, and the answer
	// should be on the same screen as the files.
	Used   int64 `json:"used_bytes"`
	Budget int64 `json:"budget_bytes"`
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	artifacts, err := s.artifacts.List(r.Context(), projectID, ports.ArtifactFilter{
		Release: query.Get("release"),
		Dist:    query.Get("dist"),
		DebugID: query.Get("debug_id"),
		Name:    query.Get("name"),
		Limit:   limit,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	used, budget, err := s.artifacts.Budget(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}

	response := artifactListResponse{
		Artifacts: make([]artifactPayload, 0, len(artifacts)),
		Used:      used,
		Budget:    budget,
	}
	for index := range artifacts {
		response.Artifacts = append(response.Artifacts, newArtifactPayload(artifacts[index]))
	}
	writeJSON(w, http.StatusOK, response)
}

// handleUploadArtifacts stores a bundle this product's own CLI built.
//
// The release and the dist come from the query string rather than the body,
// because the body is the archive. They are optional: a bundle whose files
// carry debug ids needs neither, which is the whole point of debug ids
// (ADR 018).
func (s *Server) handleUploadArtifacts(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxUploadedBundle)
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, wrapBadRequest(err))
		return
	}

	query := r.URL.Query()
	stored, err := s.artifacts.StoreBundle(r.Context(), projectID,
		query.Get("release"), query.Get("dist"), data)
	if err != nil {
		writeError(w, err)
		return
	}

	response := artifactListResponse{Artifacts: make([]artifactPayload, 0, len(stored))}
	for index := range stored {
		response.Artifacts = append(response.Artifacts, newArtifactPayload(stored[index]))
	}
	if used, budget, err := s.artifacts.Budget(r.Context(), projectID); err == nil {
		response.Used, response.Budget = used, budget
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	artifactID, err := pathID(r, "artifactID")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.artifacts.Delete(r.Context(), projectID, artifactID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
