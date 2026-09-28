package usecase

import (
	"context"
	"crypto/sha1" //nolint:gosec // sha1 is the protocol's hash, not a security claim: the chunks are named by it and choosing another would name different chunks (from the recording).
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Artifacts is the upload side of source maps: the chunked protocol
// sentry-cli speaks, the legacy single-file path, and this product's own
// bundle upload.
//
// Everything here answers to a recording of sentry-cli (ADR 013,
// compat/sentry-cli/fixtures/). The protocol has two traps that a
// server written from documentation walks straight into, and both are the same
// shape — the server looks correct, its tests pass, and the client silently
// does the wrong thing:
//
//   - What the server *advertises* decides which protocol the client speaks.
//     Announcing only `artifact_bundles` — the answer anyone writing "we only
//     support the modern path" would give — makes sentry-cli stop using debug
//     ids altogether and demand a release (from the recording). AcceptedUploadKinds
//     is where that is decided, and it is decided against evidence.
//   - Answering `{"state":"ok"}` without holding the bundle makes the tool
//     report success (from the recording). So `ok` is returned from exactly one place
//     in this file: after the chunks were found, concatenated, checksummed,
//     read as an archive and written to storage. Everything before that is
//     `created`, which asks the client to come back.
type Artifacts struct {
	repo     ports.ArtifactRepository
	chunks   ports.ChunkStore
	releases *Releases
	projects *Projects
	config   ports.ProjectConfigStore
	clock    ports.Clock
}

// NewArtifacts wires the use case.
func NewArtifacts(
	repo ports.ArtifactRepository,
	chunks ports.ChunkStore,
	releases *Releases,
	projects *Projects,
	config ports.ProjectConfigStore,
	clock ports.Clock,
) *Artifacts {
	return &Artifacts{repo: repo, chunks: chunks, releases: releases,
		projects: projects, config: config, clock: clock}
}

// UploadPolicy is the capabilities document, which is a negotiation and not a
// description: the client reads it and picks a protocol.
//
// Every field is mandatory. sentry-cli refuses to start with "could not parse
// JSON response / missing field `X`" if one is absent, and `hashAlgorithm` is
// an enum rather than free text — "sha256" is a parse error, not a different
// hash (from the recording).
type UploadPolicy struct {
	// URL is where chunks are posted, absolute. The client uses this field
	// and not the path it asked on, so a server behind a proxy that returned
	// the wrong host here would send an installation's source maps somewhere
	// else entirely.
	URL string
	// ChunkSize is the exact size of every chunk but the last, respected to
	// the byte.
	ChunkSize int64
	// ChunksPerRequest is how many parts fit in one POST.
	ChunksPerRequest int
	// MaxRequestSize bounds the multipart body.
	MaxRequestSize int64
	// MaxFileSize is informative only: the client does not respect it
	// (measured — a 1797-byte bundle uploads fine against a stated 100), so
	// it is set to the ceiling this server actually enforces rather than to
	// a number that would be a lie in both directions.
	MaxFileSize int64
	// Concurrency is how many requests the client may have in flight.
	Concurrency int
	// HashAlgorithm is the enum. There is one value.
	HashAlgorithm string
	// Compression is what the client may send. Announcing gzip renames the
	// multipart field from `file` to `file_gzip`, so a server that announces
	// it must accept both names — the recording shows both.
	Compression []string
	// Accept is what steers the client. See AcceptedUploadKinds.
	Accept []string
}

// Chunk protocol constants.
//
// ChunkSize is one mebibyte rather than the 8 MiB of the API being emulated.
// It is a deliberate trade: a smaller chunk means more requests and a bundle
// that is split more often, which is exactly the case a server implements
// badly and never notices, and this product's own tests and gates therefore
// exercise it on every run. It is still large enough that a real front-end
// bundle is a handful of chunks rather than hundreds.
const (
	uploadChunkSize        = 1 << 20
	uploadChunksPerRequest = 16
	// The multipart body of a full request, with room for the part headers.
	uploadMaxRequestSize = 32 << 20
	// The largest single upload this server will hold. It is the number
	// maxFileSize reports, so the field says something true even though the
	// client ignores it.
	uploadMaxFileSize = 200 << 20
	// StagingCeiling bounds the chunk staging area for the whole
	// installation.
	//
	// Installation-wide and not per project, because the endpoint that
	// receives chunks is told no project: it is organisation-scoped and
	// carries neither a project in its path nor one in its body (from the recording).
	// The per-project budget is charged where the project is finally known,
	// which is the assembly — see AssembleBundle.
	stagingCeiling = 1 << 30
)

// AcceptedUploadKinds is the field that decides the protocol.
//
// `release_files` has to be in the list even though this product would rather
// everyone used debug ids. It is not a compatibility gesture: with
// `["artifact_bundles"]` alone, sentry-cli falls back and reports "a release
// is required for this upload", which is to say announcing only the modern
// path is what turns the modern path off (from the recording, measured against six
// variants).
//
// `artifact_bundles_v2` is deliberately absent, and that is the decision this
// function exists to record. With it advertised, the client calls assemble
// *first*, before uploading anything, so the server can say which chunks it is
// missing. Against a server that answers `ok` optimistically the measured
// result was three requests, zero chunks uploaded, and sentry-cli reporting
// success — a whole build's source maps lost with no error anywhere (from
// the recording). The v1 flow does the same work with one fewer way to lose
// files, and this server has nothing to gain from the round trip: it holds
// chunks by content hash, so the deduplication v2 buys is already free.
func AcceptedUploadKinds() []string { return []string{"release_files", "artifact_bundles"} }

// Policy returns the capabilities document for a server reachable at origin.
func (a *Artifacts) Policy(chunkUploadURL string) UploadPolicy {
	return UploadPolicy{
		URL:              chunkUploadURL,
		ChunkSize:        uploadChunkSize,
		ChunksPerRequest: uploadChunksPerRequest,
		MaxRequestSize:   uploadMaxRequestSize,
		MaxFileSize:      uploadMaxFileSize,
		// One. SQLite has a single writer (ADR 001), so parallel chunk posts
		// would queue on the same lock and the number would be a promise this
		// storage cannot keep.
		Concurrency:   1,
		HashAlgorithm: "sha1",
		Compression:   []string{"gzip"},
		Accept:        AcceptedUploadKinds(),
	}
}

// Chunk is one uploaded piece, already decompressed.
//
// Checksum is the sha1 of Content — of the *inflated* bytes, which is the trap
// the recording caught: the multipart field is named `file_gzip` and its
// `filename` carries the checksum, and a server that verified the sha1 of what
// arrived on the wire would reject every gzip-announcing client while its own
// tests stayed green (from the recording). Decompression happens in the transport;
// what reaches here is always the inflated form.
type Chunk struct {
	Checksum string
	Content  []byte
}

// UploadChunks stores the pieces of a bundle.
//
// The checksum is verified rather than trusted. It costs a hash of bytes that
// are already in memory, and without it a chunk stored under a name that is
// not its content produces an assembly whose checksum does not match and no
// way at all to tell whose fault that was.
func (a *Artifacts) UploadChunks(ctx context.Context, chunks []Chunk) error {
	staged, err := a.chunks.StagedBytes(ctx)
	if err != nil {
		return err
	}

	now := a.clock.Now()
	for _, chunk := range chunks {
		if got := sha1Hex(chunk.Content); got != chunk.Checksum {
			return fmt.Errorf("%w: chunk %q contains bytes whose sha1 is %s",
				domain.ErrInvalidArtifact, chunk.Checksum, got)
		}
		staged += int64(len(chunk.Content))
		if staged > stagingCeiling {
			return fmt.Errorf("%w: the upload staging area is full (%d bytes)",
				domain.ErrArtifactBudgetExceeded, stagingCeiling)
		}
		if err := a.chunks.PutChunk(ctx, chunk.Checksum, chunk.Content, now); err != nil {
			return err
		}
	}
	return nil
}

// AssembleState is the machine sentry-cli polls.
//
// The names are the client's, and so is the meaning of each: `created` and
// `assembling` both make it ask again, `error` makes it fail and print
// `Detail` to the user, and `ok` ends the upload successfully — whether or not
// anything was stored (from the recording). That last clause is why this type exists
// as a type: the only honest way to use it is to make `ok` unreachable except
// from the one place that has finished writing.
type AssembleState string

const (
	// AssembleOK means the bundle is stored. It is returned from one place.
	AssembleOK AssembleState = "ok"
	// AssembleCreated means "come back": the server does not have everything
	// it needs yet, and MissingChunks says what is short.
	AssembleCreated AssembleState = "created"
	// AssembleError means the upload failed, and Detail is shown to the user.
	AssembleError AssembleState = "error"
)

// AssembleResult is the answer to an assembly request.
type AssembleResult struct {
	State AssembleState
	// MissingChunks is what the client should upload before asking again.
	MissingChunks []string
	// Detail is shown to the user verbatim when the state is error. It is the
	// only channel this protocol has for saying why, and a status code is not
	// one: a 413 here reaches the user as "unknown error" (from the recording).
	Detail string
	// Stored is what was written. Empty unless the state is ok.
	Stored []domain.Artifact
}

// AssembleRequest is the body of either assembly endpoint.
type AssembleRequest struct {
	// Checksum is the sha1 of the whole bundle.
	Checksum string
	// Chunks are its pieces, in the order that reproduces it.
	Chunks []string
	// ProjectRef is the slug or numeric id from the `projects` array. Empty
	// on the fallback endpoint, which sends no such field (from the recording).
	ProjectRef string
	// Version and Dist are present when the upload named a release.
	Version string
	Dist    string
}

// AssembleBundle puts the chunks back together and stores what they make.
//
// The order of the checks is the contract. Nothing here reports success on the
// strength of a plan: the chunks are looked for, concatenated, checksummed,
// parsed and written, and only a request that survives all five ends in `ok`.
func (a *Artifacts) AssembleBundle(ctx context.Context, request AssembleRequest) (AssembleResult, error) {
	if len(request.Chunks) == 0 {
		return failed("an assembly needs at least one chunk"), nil
	}

	missing, err := a.chunks.MissingChunks(ctx, request.Chunks)
	if err != nil {
		return AssembleResult{}, err
	}
	if len(missing) > 0 {
		// `created`, never `ok`. The client uploads what is missing and asks
		// again, which is the whole point of the field.
		return AssembleResult{State: AssembleCreated, MissingChunks: missing}, nil
	}

	whole, err := a.chunks.Assemble(ctx, request.Chunks)
	if err != nil {
		if errors.Is(err, domain.ErrArtifactNotFound) {
			// A chunk expired between the check above and this read. That is
			// "come back with it", not a failure of the upload.
			return AssembleResult{State: AssembleCreated, MissingChunks: request.Chunks}, nil
		}
		return AssembleResult{}, err
	}
	if request.Checksum != "" && sha1Hex(whole) != request.Checksum {
		return failed("the assembled bundle's checksum does not match the one declared; " +
			"a chunk arrived out of order or damaged"), nil
	}

	project, err := a.resolveTarget(ctx, request)
	if err != nil {
		return failed(err.Error()), nil
	}

	stored, err := a.StoreBundle(ctx, project.ID, request.Version, request.Dist, whole)
	switch {
	case errors.Is(err, artifactbundle.ErrNotABundle):
		// The wording is the tool's own, so a user searching for it finds the
		// same answers as for the server this emulates.
		return failed("Failed to process uploaded files: bad zip: " + err.Error()), nil
	case errors.Is(err, domain.ErrArtifactBudgetExceeded),
		errors.Is(err, domain.ErrInvalidArtifact):
		return failed(err.Error()), nil
	case err != nil:
		return AssembleResult{}, err
	}

	return AssembleResult{State: AssembleOK, MissingChunks: []string{}, Stored: stored}, nil
}

// resolveTarget finds the project an assembly is for.
//
// Two ways, because there are two endpoints. The modern one carries a
// `projects` array; the fallback one, which a client takes when the server
// does not advertise artifact bundles, carries no project at all and is
// addressed to the organisation (from the recording) — so the release version is the
// only thing left to resolve it with, exactly as it is for `set-commits`
// (ADR 013 §6).
func (a *Artifacts) resolveTarget(ctx context.Context, request AssembleRequest) (domain.Project, error) {
	if request.ProjectRef != "" {
		project, err := a.projects.ResolveProject(ctx, request.ProjectRef)
		if err != nil {
			return domain.Project{}, fmt.Errorf("no project %q on this server", request.ProjectRef)
		}
		return project, nil
	}
	if request.Version == "" {
		return domain.Project{}, errors.New(
			"this upload names neither a project nor a release, so there is nothing to attach it to")
	}
	projects, err := a.releases.ProjectsWithVersion(ctx, request.Version)
	if err != nil || len(projects) == 0 {
		return domain.Project{}, fmt.Errorf(
			"no project on this server has a release %q, so there is nothing to attach the upload to",
			request.Version)
	}
	return projects[0], nil
}

func failed(detail string) AssembleResult {
	return AssembleResult{State: AssembleError, MissingChunks: []string{}, Detail: detail}
}

// StoreBundle reads an artifact bundle and stores what it describes.
//
// It is the one path every upload converges on: the chunked protocol, the
// fallback endpoint and this product's own CLI all end here, so the rules
// about what an artefact is and what a project may hold are written once.
func (a *Artifacts) StoreBundle(
	ctx context.Context, projectID int64, version, dist string, data []byte,
) ([]domain.Artifact, error) {
	bundle, err := artifactbundle.Read(data, artifactbundle.DefaultLimits)
	if err != nil {
		return nil, err
	}

	// The manifest carries a release and a dist of its own, and the assemble
	// body carries them too. The body wins where both are present — it is
	// what the user typed on the command line — and the manifest fills in
	// where the body is silent, which is what the fallback endpoint needs
	// since its body has no such fields at all.
	if version == "" {
		version = bundle.Release
	}
	if dist == "" {
		dist = bundle.Dist
	}
	// bundle.Org is deliberately ignored (ADR 013), and bundle.Project too:
	// the caller resolved the project from the request, and a manifest that
	// disagreed would be a file claiming to belong somewhere else.

	var releaseID *int64
	if version != "" {
		release, err := a.releases.EnsureVersion(ctx, projectID, version)
		if err != nil {
			return nil, err
		}
		id := release.ID
		releaseID = &id
	}

	now := a.clock.Now()
	pending := make([]domain.Artifact, 0, len(bundle.Files))
	contents := make([][]byte, 0, len(bundle.Files))
	for index := range bundle.Files {
		file := &bundle.Files[index]
		if !file.Kind.Known() {
			// A type this product does not store — an indexed RAM bundle, a
			// format that comes later. Skipped rather than refused, for the
			// reason every other field from this tool is tolerated (ADR 013).
			continue
		}
		pending = append(pending, domain.Artifact{
			ProjectID:     projectID,
			ReleaseID:     releaseID,
			Dist:          dist,
			DebugID:       file.DebugID,
			BundleDebugID: bundle.DebugID,
			Name:          domain.ArtifactURL(nameOf(file)),
			Kind:          domain.ArtifactKind(file.Kind),
			SourceMapRef:  file.SourceMap,
			SHA256:        sha256Hex(file.Content),
			Size:          int64(len(file.Content)),
			CreatedAt:     now,
		})
		contents = append(contents, file.Content)
	}
	if len(pending) == 0 {
		return nil, fmt.Errorf("%w: the bundle carries no script and no source map",
			domain.ErrInvalidArtifact)
	}

	if err := a.checkBudget(ctx, projectID, pending); err != nil {
		return nil, err
	}

	stored := make([]domain.Artifact, 0, len(pending))
	for index := range pending {
		saved, err := a.repo.Store(ctx, pending[index], contents[index])
		if err != nil {
			return nil, err
		}
		stored = append(stored, saved)
	}
	return stored, nil
}

// nameOf is the url an artefact is addressed by.
//
// The manifest's `url` when it has one, and the archive path's base name when
// it does not — which happens with bundles produced by tooling other than
// sentry-cli. Without the fallback such a file would be stored under an empty
// name and never match anything.
func nameOf(file *artifactbundle.File) string {
	if file.URL != "" {
		return file.URL
	}
	return path.Base(file.Path)
}

// StoreReleaseFile stores one artefact uploaded the old way.
//
// `releases files <version> upload <file> <url>` is the pre-debug-id path and
// the only one that never touches a chunk: one multipart POST carrying the
// bytes and the url to serve them under (from the recording). It is deprecated in the
// tool and present in a great many pipelines, which is the whole reason it is
// here.
func (a *Artifacts) StoreReleaseFile(
	ctx context.Context, projectID int64, version, name string, content []byte,
) (domain.Artifact, error) {
	release, err := a.releases.EnsureVersion(ctx, projectID, version)
	if err != nil {
		return domain.Artifact{}, err
	}

	artifact := domain.Artifact{
		ProjectID: projectID,
		ReleaseID: &release.ID,
		Name:      domain.ArtifactURL(name),
		Kind:      kindOfName(name, content),
		SHA256:    sha256Hex(content),
		Size:      int64(len(content)),
		CreatedAt: a.clock.Now(),
	}
	if err := a.checkBudget(ctx, projectID, []domain.Artifact{artifact}); err != nil {
		return domain.Artifact{}, err
	}
	return a.repo.Store(ctx, artifact, content)
}

// kindOfName decides whether an uploaded file is a script or a map.
//
// The legacy upload says nothing about what it is sending — the multipart body
// is the bytes and a url, and that is all (from the recording) — so the kind has to be
// inferred. The extension first, because it is what a build produces and what
// every pipeline names these files by; the content as a fallback, because a
// map served under a name that does not end in .map is a real thing people do
// and storing it as a script would make it unfindable as a map.
func kindOfName(name string, content []byte) domain.ArtifactKind {
	if strings.HasSuffix(strings.ToLower(path.Base(name)), ".map") {
		return domain.ArtifactSourceMap
	}
	head := content
	if len(head) > 256 {
		head = head[:256]
	}
	trimmed := strings.TrimLeft(string(head), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"version"`) &&
		(strings.Contains(trimmed, `"mappings"`) || strings.Contains(trimmed, `"sources"`)) {
		return domain.ArtifactSourceMap
	}
	return domain.ArtifactMinifiedSource
}

// checkBudget refuses an upload that would put a project past its ceiling.
//
// The bytes a re-upload replaces are discounted first. Without that, a
// pipeline deploying the same build twice would count it twice and a project
// would drift into its own budget without ever growing — the kind of limit
// that appears to work for a month and then blocks a deploy for no reason
// anybody can see.
func (a *Artifacts) checkBudget(ctx context.Context, projectID int64, incoming []domain.Artifact) error {
	config, err := a.config.ProjectConfig(ctx, projectID)
	if err != nil {
		return err
	}
	budget := config.ArtifactsBudgetBytes()

	used, err := a.repo.UsedBytes(ctx, projectID)
	if err != nil {
		return err
	}

	var adding int64
	for index := range incoming {
		adding += incoming[index].Size
		replaced, err := a.replacedBytes(ctx, projectID, incoming[index])
		if err != nil {
			return err
		}
		adding -= replaced
	}

	if used+adding > budget {
		return fmt.Errorf(
			"%w: this upload would put project %d at %d bytes of artifacts and its budget is %d "+
				"(%d MB); raise artifacts_max_mb or delete artifacts it no longer needs",
			domain.ErrArtifactBudgetExceeded, projectID, used+adding, budget, config.ArtifactsMaxMB())
	}
	return nil
}

// replacedBytes is how much of an incoming artefact's size is already counted,
// because storing it will replace a row that is there.
func (a *Artifacts) replacedBytes(ctx context.Context, projectID int64, incoming domain.Artifact) (int64, error) {
	filter := ports.ArtifactFilter{Limit: maxReplacementCandidates}
	if incoming.DebugID != "" {
		filter.DebugID = incoming.DebugID
	} else {
		filter.Name = incoming.Name
		filter.Dist = incoming.Dist
	}

	existing, err := a.repo.List(ctx, projectID, filter)
	if err != nil {
		return 0, err
	}
	for index := range existing {
		candidate := &existing[index]
		if incoming.DebugID != "" {
			if candidate.DebugID == incoming.DebugID && candidate.Kind == incoming.Kind {
				return candidate.Size, nil
			}
			continue
		}
		if candidate.DebugID == "" && candidate.Name == incoming.Name &&
			candidate.Dist == incoming.Dist && sameRelease(candidate.ReleaseID, incoming.ReleaseID) {
			return candidate.Size, nil
		}
	}
	return 0, nil
}

// maxReplacementCandidates bounds the lookup above. A debug id matches at most
// two rows and a url at most a handful; a wider page would only be a way for a
// pathological project to make one upload read its whole artefact table.
const maxReplacementCandidates = 16

func sameRelease(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// List returns a project's artefacts.
func (a *Artifacts) List(
	ctx context.Context, projectID int64, filter ports.ArtifactFilter,
) ([]domain.Artifact, error) {
	if _, err := a.projects.Find(ctx, projectID); err != nil {
		return nil, err
	}
	return a.repo.List(ctx, projectID, filter)
}

// Budget reports what a project holds and what it may hold.
//
// Both numbers together, because either alone is unreadable: "180 MB" means
// nothing without the ceiling, and a ceiling means nothing without the usage.
// It is the answer to the only question a failing upload raises.
func (a *Artifacts) Budget(ctx context.Context, projectID int64) (used, budget int64, err error) {
	if _, err := a.projects.Find(ctx, projectID); err != nil {
		return 0, 0, err
	}
	used, err = a.repo.UsedBytes(ctx, projectID)
	if err != nil {
		return 0, 0, err
	}
	config, err := a.config.ProjectConfig(ctx, projectID)
	if err != nil {
		return 0, 0, err
	}
	return used, config.ArtifactsBudgetBytes(), nil
}

// Delete removes one artefact.
func (a *Artifacts) Delete(ctx context.Context, projectID, artifactID int64) error {
	if _, err := a.projects.Find(ctx, projectID); err != nil {
		return err
	}
	return a.repo.Delete(ctx, projectID, artifactID)
}

// Sweep removes what nothing will ever ask for again: chunks whose upload was
// abandoned, and artefacts with no release to be deleted alongside.
//
// It is called by the retention pass rather than by a job of its own. Artefact
// expiry is the same promise on the same clock as event expiry — a server
// nobody has to tend — and a second timer for it would be a second thing that
// can be off.
func (a *Artifacts) Sweep(ctx context.Context, batch int) (int64, error) {
	now := a.clock.Now()
	chunks, err := a.chunks.PruneChunksBefore(ctx, now.Add(-domain.ChunkRetention), batch)
	if err != nil {
		return chunks, err
	}
	orphans, err := a.repo.PruneOrphansBefore(ctx, now.Add(-domain.OrphanArtifactRetention), batch)
	return chunks + orphans, err
}

func sha1Hex(content []byte) string {
	sum := sha1.Sum(content) //nolint:gosec // the protocol's hash; see the import comment.
	return hex.EncodeToString(sum[:])
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
