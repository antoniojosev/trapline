package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// ArtifactFilter narrows a listing of a project's uploaded files.
//
// Every field is optional and they compose. The zero value lists the project's
// most recent artefacts, which is what somebody who has just run an upload and
// wants to know whether it arrived asks for.
type ArtifactFilter struct {
	// Release, when set, is the version whose artefacts are wanted. A version
	// rather than an id because that is what the caller has: a pipeline knows
	// `app@1.4.0`, not a row number.
	Release string
	// Dist narrows to one build of that release.
	Dist string
	// DebugID narrows to the script and map of one build.
	DebugID string
	// Name is the `~/…` url, matched exactly after normalisation.
	Name string
	// Limit bounds the page. Zero means the repository's default.
	Limit int
}

// ArtifactRepository stores and finds uploaded scripts and source maps.
//
// The two lookups are the two ways ADR 018 resolves a frame, and they are
// separate methods rather than one filtered query because they answer
// different questions and have different indexes behind them: ByDebugID is the
// modern path and is on the ingest hot path, ByReleaseURL is the legacy one.
type ArtifactRepository interface {
	// Store writes one artefact, replacing any previous upload of the same
	// identity. It returns what was stored, with its id and created_at.
	//
	// The content is passed beside the artefact rather than on it because the
	// bytes are the one part of an artefact nothing but this interface ever
	// touches: a listing must not carry a megabyte per row.
	Store(ctx context.Context, artifact domain.Artifact, content []byte) (domain.Artifact, error)

	// List returns a project's artefacts, newest first, without their bytes.
	List(ctx context.Context, projectID int64, filter ArtifactFilter) ([]domain.Artifact, error)

	// ByDebugID finds one build's file of a given kind — the first lookup of
	// ADR 018. The kind matters: a script and its map share a debug id, so
	// asking without one returns two rows and a resolver that took the first
	// would half the time be handed the minified script it was trying to
	// resolve (from the recording).
	ByDebugID(ctx context.Context, projectID int64, debugID string, kind domain.ArtifactKind,
	) (domain.Artifact, []byte, error)

	// ByReleaseURL finds an artefact by release, dist and normalised url — the
	// second lookup. The name must already be in `~/…` form; normalising it
	// is domain.ArtifactURL's job and doing it here would put the rule in two
	// places.
	ByReleaseURL(ctx context.Context, projectID, releaseID int64, dist, name string,
	) (domain.Artifact, []byte, error)

	// Delete removes one artefact, or returns domain.ErrArtifactNotFound.
	Delete(ctx context.Context, projectID, artifactID int64) error

	// HasArtifacts reports whether a project has any artefact at all.
	//
	// It exists so the ingest path can decide in one cached question whether
	// symbolication has anything to do, and skip every lookup when it does
	// not. Without it an installation that never uploaded a source map would
	// pay a database read per JavaScript event forever, which is the cost
	// ADR 005 says a switched-off subsystem must not have.
	HasArtifacts(ctx context.Context, projectID int64) (bool, error)

	// UsedBytes is what this project currently holds, uncompressed, which is
	// what its budget is counted in.
	UsedBytes(ctx context.Context, projectID int64) (int64, error)

	// PruneOrphansBefore deletes artefacts with no release that are older
	// than the cutoff, a bounded batch at a time. Artefacts that belong to a
	// release are not swept here: they are deleted with it, by the schema.
	PruneOrphansBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// ChunkStore holds the pieces of a bundle between the requests that upload
// them and the one that asks for them to be assembled.
//
// It is a port of its own, and not part of ArtifactRepository, because a chunk
// is not an artefact: it is a fragment of an archive, it belongs to no project
// — the endpoint that receives it names none (from the recording) — and it exists for
// hours rather than for a release's lifetime.
type ChunkStore interface {
	// PutChunk stores one chunk under the sha1 of its inflated content. A
	// chunk that is already there is left alone: the same bytes are the same
	// bytes, and two projects uploading builds that share a vendor chunk
	// should cost one row.
	PutChunk(ctx context.Context, checksum string, content []byte, at time.Time) error

	// MissingChunks returns the subset of these checksums the store does not
	// hold, preserving the caller's order.
	//
	// This is what makes an honest assembly possible: the alternative is
	// answering "ok" and finding out later, which is precisely the failure
	// the protocol makes cheap and expensive (from the recording).
	MissingChunks(ctx context.Context, checksums []string) ([]string, error)

	// Assemble concatenates the named chunks in the order given. The order is
	// the client's and it is significant: it is what the checksum of the
	// whole bundle was computed over.
	Assemble(ctx context.Context, checksums []string) ([]byte, error)

	// PruneChunksBefore deletes chunks that have been waiting longer than the
	// cutoff, a bounded batch at a time.
	PruneChunksBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)

	// StagedBytes is how much the staging area currently holds. It is the
	// installation-wide ceiling the chunk endpoint enforces, and it is
	// installation-wide because that endpoint is told no project (from the recording).
	StagedBytes(ctx context.Context) (int64, error)
}

// ReleaseIDLookup resolves a release version to the release itself.
//
// A one-method interface rather than taking ReleaseRepository whole, because
// symbolication needs exactly this: the legacy resolution path is keyed by
// release id and events carry a version string. Depending on the full
// repository would let a use case that has no business writing a deploy do so.
type ReleaseIDLookup interface {
	Find(ctx context.Context, projectID int64, version string) (domain.Release, error)
}
