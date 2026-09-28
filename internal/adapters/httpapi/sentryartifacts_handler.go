package httpapi

import (
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // the field is named `sha1` by somebody else's API and the tool prints it; anything else under that name would be a lie.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// The source-map upload half of the /api/0/ surface (ADR 013, ADR 018).
//
// Everything here is implemented against a recording of sentry-cli 2.57.0
// (compat/sentry-cli/fixtures/), not against documentation. Four
// findings from that recording are load-bearing, and each of them is the kind
// of mistake that leaves every test in this package green while no real upload
// works:
//
//   - The capabilities document decides the protocol. Announce
//     `["artifact_bundles"]` — the obvious answer for a server that only wants
//     the modern path — and sentry-cli stops using debug ids and asks for a
//     release instead. `release_files` has to be in the list for the debug-id
//     path to happen at all (from the recording).
//   - A chunk's checksum is its multipart `filename`, not its field name. The
//     field is `file_gzip` (or `file`) for every part in the request, and the
//     checksum is the sha1 of what the part *decompresses to* — a server that
//     hashed the wire bytes would turn away every gzip-announcing client
//     (from the recording).
//   - The assembly answers a *flat* object with a top-level `state`. Keyed by
//     checksum — which is what the shape of the request suggests — the tool
//     stops with "missing field `state`" (from the recording).
//   - A release file must come back as an object with an `id`. A bare `{}`
//     fails with "missing field `id`" (from the recording).
//
// And one rule that is not about shapes: `{"state":"ok"}` is taken at face
// value. A server that answers it without holding the bundle has told the tool
// the upload succeeded, and nobody finds out until somebody opens a minified
// stack trace weeks later. `ok` is produced in one place in the use case, after
// the write.

// maxChunkPart bounds one decompressed multipart part.
//
// It is the announced chunk size with room to spare rather than the exact
// number: a client that rounds, or a future version that pads, should not have
// its upload refused by a limit that exists to bound memory rather than to
// enforce the protocol.
const maxChunkPart = 4 << 20

// maxChunkParts bounds how many parts one request may carry, so a body that
// stays under maxChunkBody cannot still cost an unbounded number of round
// trips through the store.
const maxChunkParts = 256

// maxChunkBody bounds the whole multipart body, matching what the capabilities
// document announces as maxRequestSize.
const maxChunkBody = 32 << 20

// maxReleaseFile bounds one legacy upload. The old path posts a whole file in
// one request with no chunking at all, so this is the only ceiling there is.
const maxReleaseFile = 100 << 20

// compatRoute is one emulated endpoint and the scope it demands.
type compatRoute struct {
	scope   domain.Scope
	handler http.HandlerFunc
}

// sentryArtifactRoutes are the upload routes of the emulated surface.
//
// Returned as a table rather than registered here so sentryCompatHandler keeps
// one list of everything /api/0/ answers: "which endpoints does this server
// emulate" should be one screen, not two files.
func (s *Server) sentryArtifactRoutes() map[string]compatRoute {
	return map[string]compatRoute{
		// The negotiation. Read-only in effect — it changes nothing — so it
		// asks for the read scope, and a token that can only read can
		// discover what this server speaks without being able to upload.
		"GET " + sentryCompatPrefix + "/organizations/{org}/chunk-upload/{$}": {
			domain.ScopeProjectsRead, s.handleCompatChunkCapabilities},

		// The chunks. Organisation-scoped, and it names no project: that is
		// why the per-project budget is charged at assembly and not here
		// (from the recording).
		"POST " + sentryCompatPrefix + "/organizations/{org}/chunk-upload/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatUploadChunks},

		// The assembly, modern path. The body names the project.
		"POST " + sentryCompatPrefix + "/organizations/{org}/artifactbundle/assemble/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatAssembleBundle},

		// And the assembly a client reaches when the server does not
		// advertise artifact bundles. The archive is byte-for-byte the same
		// one; only the address and the body's fields differ, and the body
		// has no project at all (from the recording).
		"POST " + sentryCompatPrefix + "/organizations/{org}/releases/{version}/assemble/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatAssembleReleaseBundle},

		// The deduplication query, which is emitted on every upload that
		// names a release. It is on the happy path, so answering 404 here
		// would be answering 404 to something normal (from the recording).
		"GET " + sentryCompatPrefix + "/projects/{org}/{project}/releases/{version}/files/{$}": {
			domain.ScopeProjectsRead, s.handleCompatListReleaseFiles},

		// The old way: one file, one multipart POST, no chunks.
		"POST " + sentryCompatPrefix + "/projects/{org}/{project}/releases/{version}/files/{$}": {
			domain.ScopeProjectsWrite, s.handleCompatUploadReleaseFile},
	}
}

// chunkUploadResponse is the capabilities document.
//
// Six fields are mandatory and the client refuses to start without any one of
// them, naming it. `hashAlgorithm` is an enum: "sha256" is not a different
// hash, it is a parse error (from the recording).
type chunkUploadResponse struct {
	URL              string   `json:"url"`
	ChunkSize        int64    `json:"chunkSize"`
	ChunksPerRequest int      `json:"chunksPerRequest"`
	MaxFileSize      int64    `json:"maxFileSize"`
	MaxRequestSize   int64    `json:"maxRequestSize"`
	Concurrency      int      `json:"concurrency"`
	HashAlgorithm    string   `json:"hashAlgorithm"`
	Compression      []string `json:"compression"`
	Accept           []string `json:"accept"`
}

func (s *Server) handleCompatChunkCapabilities(w http.ResponseWriter, r *http.Request) {
	// Absolute, and built from the configured origin rather than from the
	// request. The client posts chunks to whatever this field says and not to
	// the path it asked on, so behind a reverse proxy a URL derived from the
	// request's own Host would send an installation's source maps to an
	// address that is not this server (from the recording). The origin is the one
	// value that is configured precisely because the request cannot be
	// trusted for it (domain.Origin).
	policy := s.artifacts.Policy(s.origin.String() + r.URL.Path)
	writeJSON(w, http.StatusOK, chunkUploadResponse{
		URL:              policy.URL,
		ChunkSize:        policy.ChunkSize,
		ChunksPerRequest: policy.ChunksPerRequest,
		MaxFileSize:      policy.MaxFileSize,
		MaxRequestSize:   policy.MaxRequestSize,
		Concurrency:      policy.Concurrency,
		HashAlgorithm:    policy.HashAlgorithm,
		Compression:      policy.Compression,
		Accept:           policy.Accept,
	})
}

// handleCompatUploadChunks receives the pieces of a bundle.
//
// Streamed part by part rather than through ParseMultipartForm, which would
// spill parts to temporary files on disk: this endpoint exists to receive
// megabytes, and a server that wrote them to /tmp on the way to writing them
// to its database would need a disk budget nobody configured.
func (s *Server) handleCompatUploadChunks(w http.ResponseWriter, r *http.Request) {
	// The ceiling goes on r.Body BEFORE the multipart reader is built, not
	// after. MultipartReader captures whatever r.Body is at the moment it is
	// called, so a MaxBytesReader installed afterwards bounds a stream nobody
	// is reading — the limit compiles, reads correctly, and does nothing.
	r.Body = http.MaxBytesReader(w, r.Body, maxChunkBody)

	parts, err := r.MultipartReader()
	if err != nil {
		writeCompatError(w, wrapBadRequest(err))
		return
	}

	chunks := make([]usecase.Chunk, 0, 8)
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeCompatError(w, wrapBadRequest(err))
			return
		}
		chunk, keep, err := readChunkPart(part)
		_ = part.Close()
		if err != nil {
			writeCompatError(w, err)
			return
		}
		if !keep {
			continue
		}
		if len(chunks) >= maxChunkParts {
			writeCompatError(w, tooManyParts())
			return
		}
		chunks = append(chunks, chunk)
	}

	if err := s.artifacts.UploadChunks(r.Context(), chunks); err != nil {
		writeCompatError(w, err)
		return
	}
	// The client reads nothing out of this response; 200 with an empty object
	// is what the recording shows and what it accepts (from the recording).
	writeJSON(w, http.StatusOK, struct{}{})
}

// readChunkPart turns one multipart part into a chunk.
//
// Both field names are accepted. Which one arrives depends on what the
// capabilities document announced: with `compression: ["gzip"]` the parts are
// called `file_gzip` and carry compressed bytes, without it they are called
// `file` and carry the file. A server that only looked for `file` would refuse
// every client it had itself told to use gzip, silently (from the recording).
func readChunkPart(part *multipart.Part) (usecase.Chunk, bool, error) {
	name := part.FormName()
	if name != "file" && name != "file_gzip" {
		// A field this endpoint does not know. Ignored rather than refused,
		// like every other unknown thing on this surface (ADR 013).
		return usecase.Chunk{}, false, nil
	}

	// The checksum is the *filename* of the part, not its field name. Every
	// part in the request shares one field name; the filename is the only
	// thing that tells them apart (from the recording).
	checksum := strings.TrimSpace(part.FileName())
	if checksum == "" {
		return usecase.Chunk{}, false, missingChunkName()
	}

	reader := io.Reader(part)
	if name == "file_gzip" {
		unzipped, err := gzip.NewReader(part)
		if err != nil {
			return usecase.Chunk{}, false, wrapBadRequest(err)
		}
		defer func() { _ = unzipped.Close() }()
		reader = unzipped
	}

	// One byte past the limit, so a part that is exactly at it is accepted
	// and one over it is refused — rather than trusting a declared length,
	// which for a gzip stream does not exist at all.
	content, err := io.ReadAll(io.LimitReader(reader, maxChunkPart+1))
	if err != nil {
		return usecase.Chunk{}, false, wrapBadRequest(err)
	}
	if len(content) > maxChunkPart {
		return usecase.Chunk{}, false, chunkTooLarge(checksum)
	}
	return usecase.Chunk{Checksum: checksum, Content: content}, true, nil
}

// assembleRequestBody is what either assembly endpoint receives.
//
// One struct for both because the fields are a superset: the modern endpoint
// sends checksum, chunks and projects, plus version and dist when the upload
// named a release; the fallback one sends checksum and chunks only, and takes
// the version from its own path (from the recording).
type assembleRequestBody struct {
	Checksum string   `json:"checksum"`
	Chunks   []string `json:"chunks"`
	Projects []string `json:"projects"`
	Version  string   `json:"version"`
	Dist     string   `json:"dist"`
}

// assembleResponseBody is the answer, and its shape is the finding.
//
// Flat, with `state` at the top level. A map keyed by checksum — which is what
// a request carrying a checksum invites — stops the tool with "missing field
// `state`". `detail` is null rather than absent, and it is the only way this
// protocol has of telling a user why an upload failed: the tool prints it
// (from the recording).
type assembleResponseBody struct {
	State         string   `json:"state"`
	MissingChunks []string `json:"missingChunks"`
	Detail        *string  `json:"detail"`
}

func (s *Server) handleCompatAssembleBundle(w http.ResponseWriter, r *http.Request) {
	var request assembleRequestBody
	if err := decodeCompatJSON(r, &request); err != nil {
		writeCompatError(w, err)
		return
	}

	project := ""
	if len(request.Projects) > 0 {
		// The first, and the body's is the authority. The manifest inside the
		// archive names a project too, and where they disagree the request
		// wins: the manifest is a file that could claim to belong anywhere
		// (from the recording, ADR 013).
		project = request.Projects[0]
	}
	s.assemble(w, r, usecase.AssembleRequest{
		Checksum:   request.Checksum,
		Chunks:     request.Chunks,
		ProjectRef: project,
		Version:    request.Version,
		Dist:       request.Dist,
	})
}

// handleCompatAssembleReleaseBundle answers the endpoint a client falls back
// to when the server does not advertise artifact bundles.
//
// This server does advertise them, so nothing sentry-cli 2.57.0 does reaches
// here — but an older client, or one configured against a different server
// once, will, and the archive it sends is byte-for-byte the one the modern
// path sends. Refusing it would mean a pipeline that works everywhere else
// fails here for a reason nobody could read off the error (from the recording).
func (s *Server) handleCompatAssembleReleaseBundle(w http.ResponseWriter, r *http.Request) {
	var request assembleRequestBody
	if err := decodeCompatJSON(r, &request); err != nil {
		writeCompatError(w, err)
		return
	}
	s.assemble(w, r, usecase.AssembleRequest{
		Checksum: request.Checksum,
		Chunks:   request.Chunks,
		// No project anywhere in this request: it is organisation-scoped and
		// its body carries only a checksum and a chunk list. The version in
		// the path is what resolves it, the same way a commit set is resolved
		// (ADR 013 §6).
		Version: r.PathValue("version"),
		Dist:    request.Dist,
	})
}

func (s *Server) assemble(w http.ResponseWriter, r *http.Request, request usecase.AssembleRequest) {
	result, err := s.artifacts.AssembleBundle(r.Context(), request)
	if err != nil {
		// Only a genuine failure of this server reaches here. Everything the
		// caller could have done wrong comes back as a state, because a
		// status code is not a channel this protocol reads: a 413 on this
		// endpoint reaches the user as "unknown error" (from the recording).
		writeCompatError(w, err)
		return
	}

	body := assembleResponseBody{
		State:         string(result.State),
		MissingChunks: result.MissingChunks,
	}
	if body.MissingChunks == nil {
		body.MissingChunks = []string{}
	}
	if result.Detail != "" {
		detail := result.Detail
		body.Detail = &detail
	}
	writeJSON(w, http.StatusOK, body)
}

// sentryReleaseFileResponse is one stored artefact in the shape the tool
// parses and prints.
//
// `id` is not optional: the tool fails with "missing field `id`" against a
// bare object, which is how the recording found out (from the recording). It is a
// string because that is what the API being emulated returns for it, and a
// client parsing it as one would fail on a number.
type sentryReleaseFileResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Size        int64             `json:"size"`
	SHA1        string            `json:"sha1"`
	Headers     map[string]string `json:"headers"`
	DateCreated time.Time         `json:"dateCreated"`
}

// handleCompatListReleaseFiles answers the deduplication query.
//
// It is emitted on every upload that names a release —
// `?checksum=…&checksum=…&cursor=` — and measured: the tool uploads the bundle
// whatever this answers, with `[]`, with a full list, or even with `{}` (from
// the recording). So the answer is the empty list, and what matters about this route is
// that it *exists*: it is on the happy path, and a 404 here would be a 404 to
// something entirely normal (from the recording).
//
// The honest alternative — answering which of those sha1 sums this server
// already holds — would mean storing a sha1 of every artefact beside the
// sha256 it is identified by, to serve a question whose answer the client
// discards. That is a column and an index bought for nothing.
func (s *Server) handleCompatListReleaseFiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []sentryReleaseFileResponse{})
}

// handleCompatUploadReleaseFile stores one artefact the old way.
//
// Two parts: `file`, carrying the bytes under its own filename, and `name`, a
// text field holding the url to serve it under — `~/bundle.min.js`. Neither
// says what kind of file it is, which is why the kind is inferred in the use
// case (from the recording).
func (s *Server) handleCompatUploadReleaseFile(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.ResolveProject(r.Context(), r.PathValue("project"))
	if err != nil {
		writeCompatError(w, err)
		return
	}

	// Before the reader, for the reason above.
	r.Body = http.MaxBytesReader(w, r.Body, maxReleaseFile+(1<<20))

	parts, err := r.MultipartReader()
	if err != nil {
		writeCompatError(w, wrapBadRequest(err))
		return
	}

	var (
		content  []byte
		fileName string
		url      string
		seen     bool
	)
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeCompatError(w, wrapBadRequest(err))
			return
		}
		switch part.FormName() {
		case "file":
			content, err = io.ReadAll(io.LimitReader(part, maxReleaseFile+1))
			fileName = part.FileName()
			seen = true
		case "name":
			var raw []byte
			raw, err = io.ReadAll(io.LimitReader(part, domain.MaxArtifactNameLength+1))
			url = strings.TrimSpace(string(raw))
		}
		_ = part.Close()
		if err != nil {
			writeCompatError(w, wrapBadRequest(err))
			return
		}
	}

	if !seen {
		writeCompatError(w, missingFilePart())
		return
	}
	if int64(len(content)) > maxReleaseFile {
		writeCompatError(w, releaseFileTooLarge())
		return
	}
	if url == "" {
		// The tool always sends it, but a hand-rolled caller may not, and the
		// file's own name is the only other thing that could serve. Better
		// than refusing: the url is what the artefact is looked up by, and
		// the file name is what it will be served under anyway.
		url = fileName
	}

	artifact, err := s.artifacts.StoreReleaseFile(r.Context(), project.ID,
		r.PathValue("version"), url, content)
	if err != nil {
		writeCompatError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sentryReleaseFileResponse{
		ID:   strconv.FormatInt(artifact.ID, 10),
		Name: artifact.Name,
		Size: artifact.Size,
		// The sha1 of the bytes, computed here rather than reused from the
		// artefact's sha256. The field's name belongs to somebody else's API
		// and the tool prints its value; putting a different digest under it
		// would be a lie somebody eventually tries to verify.
		SHA1:        sha1Hex(content),
		Headers:     map[string]string{},
		DateCreated: artifact.CreatedAt,
	})
}

// sha1Hex is the digest the emulated surface reports for a stored file.
func sha1Hex(content []byte) string {
	sum := sha1.Sum(content) //nolint:gosec // see the import comment.
	return hex.EncodeToString(sum[:])
}

// wrapBadRequest marks a malformed upload so statusFor answers 400.
func wrapBadRequest(err error) error {
	return fmt.Errorf("%w: %w", errBadRequest, err)
}

func tooManyParts() error {
	return fmt.Errorf("%w: more than %d chunks in one request; the capabilities document "+
		"announces chunksPerRequest", errBadRequest, maxChunkParts)
}

// missingChunkName reports the one mistake a hand-rolled client makes here.
//
// The message names the finding rather than the field, because "filename is
// required" would send somebody looking at the wrong half of the part: the
// checksum is the filename, and the field name is the same for every chunk in
// the request (from the recording).
func missingChunkName() error {
	return fmt.Errorf("%w: a chunk part carries no filename, and the filename is the chunk's "+
		"sha1 — of its decompressed bytes", errBadRequest)
}

func chunkTooLarge(checksum string) error {
	return fmt.Errorf("%w: chunk %s decompresses past the %d byte limit",
		errBadRequest, checksum, maxChunkPart)
}

func missingFilePart() error {
	return fmt.Errorf("%w: no `file` part in the upload", errBadRequest)
}

func releaseFileTooLarge() error {
	return fmt.Errorf("%w: the file is larger than the %d byte limit for a single upload",
		domain.ErrArtifactBudgetExceeded, maxReleaseFile)
}
