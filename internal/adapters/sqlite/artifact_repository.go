package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/compression"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var (
	_ ports.ArtifactRepository = (*ArtifactRepository)(nil)
	_ ports.ChunkStore         = (*ArtifactRepository)(nil)
)

// defaultArtifactPage is how many artefacts a listing returns when the caller
// names no limit. A bundle is two files, a build is a handful of bundles, so a
// hundred rows is several deploys' worth on one screen.
const defaultArtifactPage = 100

// maxArtifactPage bounds what a caller may ask for. Rows are small — the bytes
// are deliberately not selected — but an unbounded page is still a way to make
// this server allocate as much as somebody asks it to.
const maxArtifactPage = 1000

// ArtifactRepository stores uploaded scripts and source maps, and the chunks
// they arrive in.
//
// One type for both because they are two halves of one operation: chunks come
// in, get assembled, and become artefacts in the same request. Splitting them
// would mean two objects wired separately in order to be used together, and
// the sweep that expires the staging area would sit in a different file from
// the one that expires what the staging area produced.
type ArtifactRepository struct {
	db *DB
}

// NewArtifactRepository wires the repository to an open database.
func NewArtifactRepository(db *DB) *ArtifactRepository {
	return &ArtifactRepository{db: db}
}

// Store writes one artefact, replacing a previous upload of the same identity.
//
// "The same identity" is two different things depending on how the artefact is
// addressed, and the schema says so with two partial unique indexes: a file
// with a debug id is identified by (project, debug id, kind), and one without
// by (project, release, dist, url). Re-uploading a build must replace; two
// different builds of the same file must both survive, because an event from
// the older one still needs its map.
//
// So this is a delete-then-insert rather than an UPSERT: the two indexes
// cannot both be named in one ON CONFLICT clause, and picking one would make
// re-uploading through the other path duplicate silently.
func (r *ArtifactRepository) Store(
	ctx context.Context, artifact domain.Artifact, content []byte,
) (domain.Artifact, error) {
	if err := artifact.Validate(); err != nil {
		return domain.Artifact{}, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if artifact.DebugID != "" {
		_, err = tx.ExecContext(ctx,
			"DELETE FROM artifacts WHERE project_id = ? AND debug_id = ? AND kind = ?",
			artifact.ProjectID, artifact.DebugID, string(artifact.Kind))
	} else {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM artifacts
			  WHERE project_id = ? AND IFNULL(release_id, 0) = ? AND dist = ? AND name = ?
			    AND debug_id = ''`,
			artifact.ProjectID, releaseKey(artifact.ReleaseID), artifact.Dist, artifact.Name)
	}
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("replacing the previous upload: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts
		   (project_id, release_id, dist, debug_id, bundle_debug_id, name, kind,
		    sourcemap_ref, sha256, size, codec, blob, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ProjectID, artifact.ReleaseID, artifact.Dist, artifact.DebugID,
		artifact.BundleDebugID, artifact.Name, string(artifact.Kind), artifact.SourceMapRef,
		artifact.SHA256, artifact.Size, compression.Codec, compression.Compress(content),
		formatTime(artifact.CreatedAt))
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("storing the artifact: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("reading the artifact id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Artifact{}, fmt.Errorf("committing the artifact: %w", err)
	}

	artifact.ID = id
	return artifact, nil
}

// releaseKey is how a nullable release id is compared in the unique index.
// SQLite treats two NULLs in a unique index as distinct, so the index uses
// IFNULL(release_id, 0) and every query against it has to agree.
func releaseKey(releaseID *int64) int64 {
	if releaseID == nil {
		return 0
	}
	return *releaseID
}

const artifactColumns = `id, project_id, release_id, dist, debug_id, bundle_debug_id,
	name, kind, sourcemap_ref, sha256, size, created_at`

// List returns a project's artefacts, newest first, without their bytes.
func (r *ArtifactRepository) List(
	ctx context.Context, projectID int64, filter ports.ArtifactFilter,
) ([]domain.Artifact, error) {
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = defaultArtifactPage
	case limit > maxArtifactPage:
		limit = maxArtifactPage
	}

	query := strings.Builder{}
	query.WriteString("SELECT " + artifactColumns + " FROM artifacts WHERE project_id = ?")
	arguments := []any{projectID}

	if filter.Release != "" {
		// Joined by version rather than by id because that is what the caller
		// holds. A subquery instead of a JOIN so a version that names no
		// release returns nothing rather than everything.
		query.WriteString(
			" AND release_id = (SELECT id FROM releases WHERE project_id = ? AND version = ?)")
		arguments = append(arguments, projectID, filter.Release)
	}
	if filter.Dist != "" {
		query.WriteString(" AND dist = ?")
		arguments = append(arguments, filter.Dist)
	}
	if filter.DebugID != "" {
		query.WriteString(" AND debug_id = ?")
		arguments = append(arguments, filter.DebugID)
	}
	if filter.Name != "" {
		query.WriteString(" AND name = ?")
		arguments = append(arguments, domain.ArtifactURL(filter.Name))
	}
	// By id and not by created_at: two files of one bundle are stored in the
	// same millisecond, and a listing that reordered them between two reads
	// would make a cursor impossible later.
	query.WriteString(" ORDER BY id DESC LIMIT ?")
	arguments = append(arguments, limit)

	rows, err := r.db.QueryContext(ctx, query.String(), arguments...)
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	artifacts := []domain.Artifact{}
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	return artifacts, nil
}

// ByDebugID is the first lookup of ADR 018.
func (r *ArtifactRepository) ByDebugID(
	ctx context.Context, projectID int64, debugID string, kind domain.ArtifactKind,
) (domain.Artifact, []byte, error) {
	if debugID == "" {
		// An empty debug id is not an identity: every legacy upload carries
		// one, and matching on it would hand back an arbitrary artefact.
		return domain.Artifact{}, nil, domain.ErrArtifactNotFound
	}
	return r.one(ctx,
		"SELECT "+artifactColumns+", codec, blob FROM artifacts "+
			"WHERE project_id = ? AND debug_id = ? AND kind = ?",
		projectID, debugID, string(kind))
}

// ByReleaseURL is the second.
func (r *ArtifactRepository) ByReleaseURL(
	ctx context.Context, projectID, releaseID int64, dist, name string,
) (domain.Artifact, []byte, error) {
	return r.one(ctx,
		"SELECT "+artifactColumns+", codec, blob FROM artifacts "+
			"WHERE project_id = ? AND release_id = ? AND dist = ? AND name = ? "+
			"ORDER BY id DESC LIMIT 1",
		projectID, releaseID, dist, domain.ArtifactURL(name))
}

func (r *ArtifactRepository) one(
	ctx context.Context, query string, arguments ...any,
) (domain.Artifact, []byte, error) {
	row := r.db.QueryRowContext(ctx, query, arguments...)

	var (
		artifact  domain.Artifact
		releaseID sql.NullInt64
		kind      string
		createdAt string
		codec     string
		blob      []byte
	)
	err := row.Scan(&artifact.ID, &artifact.ProjectID, &releaseID, &artifact.Dist,
		&artifact.DebugID, &artifact.BundleDebugID, &artifact.Name, &kind,
		&artifact.SourceMapRef, &artifact.SHA256, &artifact.Size, &createdAt, &codec, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Artifact{}, nil, domain.ErrArtifactNotFound
	}
	if err != nil {
		return domain.Artifact{}, nil, fmt.Errorf("reading the artifact: %w", err)
	}
	if err := finishArtifact(&artifact, releaseID, kind, createdAt); err != nil {
		return domain.Artifact{}, nil, err
	}

	// Read from the row rather than assumed, so a future change of codec is a
	// row this build refuses by name instead of a blob it feeds to the wrong
	// decompressor. Same rule as `events`.
	if codec != compression.Codec {
		return domain.Artifact{}, nil, fmt.Errorf("%w: artifact %d uses unknown codec %q",
			ErrSchema, artifact.ID, codec)
	}
	content, err := compression.Decompress(blob)
	if err != nil {
		return domain.Artifact{}, nil, fmt.Errorf("decompressing artifact %d: %w", artifact.ID, err)
	}
	return artifact, content, nil
}

// Delete removes one artefact.
func (r *ArtifactRepository) Delete(ctx context.Context, projectID, artifactID int64) error {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM artifacts WHERE project_id = ? AND id = ?", projectID, artifactID)
	if err != nil {
		return fmt.Errorf("deleting artifact %d: %w", artifactID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deleting artifact %d: %w", artifactID, err)
	}
	if affected == 0 {
		return domain.ErrArtifactNotFound
	}
	return nil
}

// HasArtifacts reports whether a project holds any artefact at all.
//
// One indexed existence check standing in for every lookup symbolication
// would otherwise make: an installation that never uploaded a source map must
// not pay a read per JavaScript event, which is what ADR 005 means by a
// switched-off subsystem costing nothing.
func (r *ArtifactRepository) HasArtifacts(ctx context.Context, projectID int64) (bool, error) {
	var present int
	err := r.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM artifacts WHERE project_id = ?)", projectID).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("checking whether project %d has artifacts: %w", projectID, err)
	}
	return present == 1, nil
}

// UsedBytes is what a project holds, uncompressed.
//
// Summed rather than kept in a counter column: a counter would be a second
// copy of a fact the rows already carry, and it would drift the first time a
// delete cascaded from a release without going through this repository — which
// is exactly what ON DELETE CASCADE does.
func (r *ArtifactRepository) UsedBytes(ctx context.Context, projectID int64) (int64, error) {
	var used int64
	err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(size), 0) FROM artifacts WHERE project_id = ?", projectID).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("measuring the project's artifacts: %w", err)
	}
	return used, nil
}

// PruneOrphansBefore deletes artefacts that have no release to die with.
//
// Only those: an artefact uploaded against a release is removed by the
// schema's cascade when the release goes, which is the retention rule of
// ADR 018 and needs no sweep. This is the other half — a bundle uploaded by
// debug id alone names no release anywhere, so nothing will ever delete it.
func (r *ArtifactRepository) PruneOrphansBefore(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM artifacts WHERE id IN (
		   SELECT id FROM artifacts
		    WHERE release_id IS NULL AND created_at < ?
		    ORDER BY created_at LIMIT ?)`,
		formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("sweeping orphan artifacts: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweeping orphan artifacts: %w", err)
	}
	return deleted, nil
}

// PutChunk stores one piece of a bundle.
func (r *ArtifactRepository) PutChunk(
	ctx context.Context, checksum string, content []byte, at time.Time,
) error {
	// DO NOTHING and not an update: the key is the hash of the content, so a
	// row that is already there holds the same bytes. Rewriting it would only
	// move its created_at, which would keep an abandoned upload alive every
	// time somebody else uploaded a build that happens to share a chunk.
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO artifact_chunks (checksum, bytes, size, created_at)
		 VALUES (?, ?, ?, ?) ON CONFLICT (checksum) DO NOTHING`,
		checksum, compression.Compress(content), len(content), formatTime(at))
	if err != nil {
		return fmt.Errorf("storing chunk %s: %w", checksum, err)
	}
	return nil
}

// MissingChunks reports which of these the store does not hold.
func (r *ArtifactRepository) MissingChunks(
	ctx context.Context, checksums []string,
) ([]string, error) {
	missing := []string{}
	for _, checksum := range checksums {
		var present int
		err := r.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM artifact_chunks WHERE checksum = ?", checksum).Scan(&present)
		if err != nil {
			return nil, fmt.Errorf("looking for chunk %s: %w", checksum, err)
		}
		if present == 0 {
			missing = append(missing, checksum)
		}
	}
	return missing, nil
}

// Assemble concatenates the named chunks in the order given.
//
// The order is the client's and it is what the bundle's checksum was computed
// over, so it is honoured literally — including a checksum that appears twice,
// which is what an archive with two identical blocks produces.
func (r *ArtifactRepository) Assemble(ctx context.Context, checksums []string) ([]byte, error) {
	var whole []byte
	for _, checksum := range checksums {
		var stored []byte
		err := r.db.QueryRowContext(ctx,
			"SELECT bytes FROM artifact_chunks WHERE checksum = ?", checksum).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: chunk %s", domain.ErrArtifactNotFound, checksum)
		}
		if err != nil {
			return nil, fmt.Errorf("reading chunk %s: %w", checksum, err)
		}
		content, err := compression.Decompress(stored)
		if err != nil {
			return nil, fmt.Errorf("decompressing chunk %s: %w", checksum, err)
		}
		whole = append(whole, content...)
	}
	return whole, nil
}

// PruneChunksBefore expires the staging area.
func (r *ArtifactRepository) PruneChunksBefore(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM artifact_chunks WHERE checksum IN (
		   SELECT checksum FROM artifact_chunks WHERE created_at < ? ORDER BY created_at LIMIT ?)`,
		formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("sweeping chunks: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweeping chunks: %w", err)
	}
	return deleted, nil
}

// StagedBytes is how much the staging area holds right now.
func (r *ArtifactRepository) StagedBytes(ctx context.Context) (int64, error) {
	var staged int64
	err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(size), 0) FROM artifact_chunks").Scan(&staged)
	if err != nil {
		return 0, fmt.Errorf("measuring the staging area: %w", err)
	}
	return staged, nil
}

func scanArtifact(rows *sql.Rows) (domain.Artifact, error) {
	var (
		artifact  domain.Artifact
		releaseID sql.NullInt64
		kind      string
		createdAt string
	)
	err := rows.Scan(&artifact.ID, &artifact.ProjectID, &releaseID, &artifact.Dist,
		&artifact.DebugID, &artifact.BundleDebugID, &artifact.Name, &kind,
		&artifact.SourceMapRef, &artifact.SHA256, &artifact.Size, &createdAt)
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("reading an artifact row: %w", err)
	}
	if err := finishArtifact(&artifact, releaseID, kind, createdAt); err != nil {
		return domain.Artifact{}, err
	}
	return artifact, nil
}

func finishArtifact(
	artifact *domain.Artifact, releaseID sql.NullInt64, kind, createdAt string,
) error {
	if releaseID.Valid {
		id := releaseID.Int64
		artifact.ReleaseID = &id
	}
	artifact.Kind = domain.ArtifactKind(kind)
	parsed, err := parseTime(createdAt)
	if err != nil {
		return fmt.Errorf("reading artifact %d: %w", artifact.ID, err)
	}
	artifact.CreatedAt = parsed
	return nil
}
