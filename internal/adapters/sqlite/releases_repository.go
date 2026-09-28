package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Compile-time proof that this adapter satisfies both ports.
var (
	_ ports.ReleaseRepository         = (*ReleaseRepository)(nil)
	_ ports.IssueResolutionRepository = (*ReleaseRepository)(nil)
)

// DefaultReleasePageSize is how many releases a listing returns when the
// caller does not say.
const DefaultReleasePageSize = 50

// MaxReleasePageSize bounds what a caller may ask for.
const MaxReleasePageSize = 200

// ReleaseRepository stores releases and the resolution columns of `issues`.
//
// What it does not do is the ingest path. An event's release, and what that
// event does to a resolved issue, are written by RecordEvent in the same
// transaction as the event itself (ADR 032). This type owns the deliberate
// half: a release created by a deploy tool, its commits and deploys, and a
// person resolving or reopening an issue by hand.
type ReleaseRepository struct {
	db *DB
}

// NewReleaseRepository wires the repository to an open database.
func NewReleaseRepository(db *DB) *ReleaseRepository {
	return &ReleaseRepository{db: db}
}

// Ensure records that a release exists and that an event arrived from it.
//
// An event naming an unknown release is the commonest way a release comes into
// existence: most installations never run a deploy tool at all, and a product
// that only knew about releases somebody remembered to register would know
// about almost none of them. On the ingest path that upsert happens inside
// RecordEvent's transaction (ADR 032); this is the same statement for a caller
// that has no transaction of its own.
func (r *ReleaseRepository) Ensure(ctx context.Context, projectID int64, version string, at time.Time) error {
	version, err := domain.CleanVersion(version)
	if err != nil {
		return err
	}
	return ensureRelease(ctx, r.db, projectID, version, at)
}

// execer is whatever can run a statement: the pool, or one transaction.
//
// It exists so the release upsert is written once and used from both, rather
// than copied into the ingest path where the copy would quietly drift from
// the original.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureRelease is the upsert itself, on an already-validated version.
//
// One statement, because it runs once per ingested event that names a release.
// The timestamps widen rather than overwrite: events arrive late, and a
// release's first sighting must not move forward because a mobile client
// uploaded a crash from yesterday this morning.
func ensureRelease(ctx context.Context, db execer, projectID int64, version string, at time.Time) error {
	stamp := formatTime(at)
	_, err := db.ExecContext(ctx, `
		INSERT INTO releases (project_id, version, created_at, first_event_at, last_event_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project_id, version) DO UPDATE SET
			first_event_at = MIN(COALESCE(first_event_at, excluded.first_event_at), excluded.first_event_at),
			last_event_at  = MAX(COALESCE(last_event_at,  excluded.last_event_at),  excluded.last_event_at)`,
		projectID, version, stamp, stamp, stamp)
	if err != nil {
		return fmt.Errorf("recording release %q: %w", version, err)
	}
	return nil
}

// Create registers a release explicitly, as a deploy tool does.
//
// Creating one that already exists is not an error and does not overwrite:
// a pipeline step that runs twice, or a deploy annotation that arrives after
// the first error did, must not reset what is already known about a release.
func (r *ReleaseRepository) Create(
	ctx context.Context, release domain.Release,
) (domain.Release, bool, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO releases (project_id, version, created_at, date_released)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (project_id, version) DO NOTHING`,
		release.ProjectID, release.Version, formatTime(release.CreatedAt),
		nullableTime(release.DateReleased))
	if err != nil {
		return domain.Release{}, false, fmt.Errorf("creating release %q: %w", release.Version, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return domain.Release{}, false, fmt.Errorf("reading insert result: %w", err)
	}

	saved, err := r.Find(ctx, release.ProjectID, release.Version)
	if err != nil {
		return domain.Release{}, false, err
	}
	return saved, affected > 0, nil
}

const releaseColumns = `id, project_id, version, created_at, date_released,
	first_event_at, last_event_at, commit_count`

// Find returns one release.
//
// The project id is part of the lookup rather than checked afterwards: a
// release belongs to a project, and a query that can return another project's
// release is one refactor away from being an authorisation bug.
func (r *ReleaseRepository) Find(
	ctx context.Context, projectID int64, version string,
) (domain.Release, error) {
	version, err := domain.CleanVersion(version)
	if err != nil {
		return domain.Release{}, err
	}
	row := r.db.QueryRowContext(ctx,
		"SELECT "+releaseColumns+" FROM releases WHERE project_id = ? AND version = ?",
		projectID, version)

	release, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Release{}, fmt.Errorf("%w: %s", domain.ErrReleaseNotFound, version)
	}
	if err != nil {
		return domain.Release{}, fmt.Errorf("reading release: %w", err)
	}
	return release, nil
}

// List returns a project's releases, most recently discovered first.
//
// Ordered by id rather than by version, because ordering by version is not a
// thing SQL can do for identifiers that are not versions, and doing it in Go
// after the fact would need every release in memory to produce one page.
func (r *ReleaseRepository) List(
	ctx context.Context, projectID int64, limit int,
) ([]domain.Release, error) {
	if limit <= 0 {
		limit = DefaultReleasePageSize
	}
	if limit > MaxReleasePageSize {
		limit = MaxReleasePageSize
	}

	rows, err := r.db.QueryContext(ctx,
		"SELECT "+releaseColumns+" FROM releases WHERE project_id = ? ORDER BY id DESC LIMIT ?",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing releases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var releases []domain.Release
	for rows.Next() {
		release, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning release: %w", err)
		}
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating releases: %w", err)
	}
	return releases, nil
}

// Finalize records when a release was declared shipped.
func (r *ReleaseRepository) Finalize(
	ctx context.Context, projectID int64, version string, at time.Time,
) (domain.Release, error) {
	release, err := r.Find(ctx, projectID, version)
	if err != nil {
		return domain.Release{}, err
	}
	finalized := release.Finalize(at)
	if finalized.DateReleased == release.DateReleased {
		// Already finalised, and the domain kept the original date.
		return finalized, nil
	}
	if _, err := r.db.ExecContext(ctx,
		"UPDATE releases SET date_released = ? WHERE id = ?",
		nullableTime(finalized.DateReleased), finalized.ID,
	); err != nil {
		return domain.Release{}, fmt.Errorf("finalizing release: %w", err)
	}
	return finalized, nil
}

// SetCommits replaces a release's commit set, paths included.
//
// Replacing rather than appending, because a deploy tool sends the whole set
// each time and a re-run of the same pipeline step must not double it. One
// transaction, so a release is never left holding half of two commit sets.
func (r *ReleaseRepository) SetCommits(ctx context.Context, releaseID int64, commits []domain.Commit) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM release_commit_files WHERE release_id = ?", releaseID); err != nil {
		return fmt.Errorf("clearing commit files: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM release_commits WHERE release_id = ?", releaseID); err != nil {
		return fmt.Errorf("clearing commits: %w", err)
	}

	for index := range commits {
		commit := &commits[index]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO release_commits
				(release_id, sha, message, author_name, author_email, timestamp, repository, ordinal)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			releaseID, commit.SHA, commit.Message, commit.AuthorName, commit.AuthorEmail,
			nullableTime(commit.Timestamp), commit.Repository, commit.Ordinal,
		); err != nil {
			return fmt.Errorf("recording commit %s: %w", commit.SHA, err)
		}
		for _, file := range commit.Files {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO release_commit_files (release_id, sha, path, change_type)
				VALUES (?, ?, ?, ?)`,
				releaseID, commit.SHA, file.Path, string(file.ChangeType),
			); err != nil {
				return fmt.Errorf("recording changed file %s: %w", file.Path, err)
			}
		}
	}

	// Denormalised so a release listing can show "12 commits" without a
	// correlated count per row.
	if _, err := tx.ExecContext(ctx,
		"UPDATE releases SET commit_count = ? WHERE id = ?", len(commits), releaseID,
	); err != nil {
		return fmt.Errorf("updating commit count: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing commits: %w", err)
	}
	return nil
}

// Commits returns a release's commits in the order they were sent.
func (r *ReleaseRepository) Commits(ctx context.Context, releaseID int64) ([]domain.Commit, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT sha, message, author_name, author_email, timestamp, repository, ordinal
		FROM release_commits WHERE release_id = ? ORDER BY ordinal`, releaseID)
	if err != nil {
		return nil, fmt.Errorf("listing commits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	commits := []domain.Commit{}
	index := map[string]int{}
	for rows.Next() {
		var (
			commit    domain.Commit
			timestamp sql.NullString
		)
		if err := rows.Scan(&commit.SHA, &commit.Message, &commit.AuthorName,
			&commit.AuthorEmail, &timestamp, &commit.Repository, &commit.Ordinal); err != nil {
			return nil, fmt.Errorf("scanning commit: %w", err)
		}
		if commit.Timestamp, err = parseNullableTime(timestamp); err != nil {
			return nil, err
		}
		index[commit.SHA] = len(commits)
		commits = append(commits, commit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating commits: %w", err)
	}
	if len(commits) == 0 {
		return nil, nil
	}

	// The paths in a second query rather than a join: a join repeats every
	// commit column once per changed file, and a release of 200 commits each
	// touching 20 files would carry the message text 4000 times over the
	// wire from SQLite for no reason.
	fileRows, err := r.db.QueryContext(ctx,
		"SELECT sha, path, change_type FROM release_commit_files WHERE release_id = ? ORDER BY path",
		releaseID)
	if err != nil {
		return nil, fmt.Errorf("listing changed files: %w", err)
	}
	defer func() { _ = fileRows.Close() }()

	for fileRows.Next() {
		var (
			sha  string
			file domain.CommitFile
		)
		if err := fileRows.Scan(&sha, &file.Path, &file.ChangeType); err != nil {
			return nil, fmt.Errorf("scanning changed file: %w", err)
		}
		if position, found := index[sha]; found {
			commits[position].Files = append(commits[position].Files, file)
		}
	}
	if err := fileRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating changed files: %w", err)
	}
	return commits, nil
}

// AddDeploy records one release going out to one environment.
func (r *ReleaseRepository) AddDeploy(ctx context.Context, deploy domain.Deploy) (domain.Deploy, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO deploys (release_id, environment, name, url, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		deploy.ReleaseID, deploy.Environment, deploy.Name, deploy.URL,
		nullableTime(deploy.StartedAt), nullableTime(deploy.FinishedAt))
	if err != nil {
		return domain.Deploy{}, fmt.Errorf("recording deploy: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Deploy{}, fmt.Errorf("reading assigned deploy id: %w", err)
	}
	deploy.ID = id
	return deploy, nil
}

// Deploys returns a release's deploys, most recent first.
func (r *ReleaseRepository) Deploys(ctx context.Context, releaseID int64) ([]domain.Deploy, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, release_id, environment, name, url, started_at, finished_at
		FROM deploys WHERE release_id = ? ORDER BY id DESC`, releaseID)
	if err != nil {
		return nil, fmt.Errorf("listing deploys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var deploys []domain.Deploy
	for rows.Next() {
		var (
			deploy   domain.Deploy
			started  sql.NullString
			finished sql.NullString
		)
		if err := rows.Scan(&deploy.ID, &deploy.ReleaseID, &deploy.Environment,
			&deploy.Name, &deploy.URL, &started, &finished); err != nil {
			return nil, fmt.Errorf("scanning deploy: %w", err)
		}
		if deploy.StartedAt, err = parseNullableTime(started); err != nil {
			return nil, err
		}
		if deploy.FinishedAt, err = parseNullableTime(finished); err != nil {
			return nil, err
		}
		deploys = append(deploys, deploy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating deploys: %w", err)
	}
	return deploys, nil
}

// Stats counts what a release did to a project's issues, and how many events
// it produced.
//
// The two issue counts come from indexed columns on `issues`; the event total
// comes from the hourly aggregates (ADR 010). Nothing here touches the event
// table, which is the rule the whole storage design rests on (ADR 001): a
// release page that scanned events would get slower every day and would start
// lying the moment retention deleted the old ones.
func (r *ReleaseRepository) Stats(
	ctx context.Context, projectID int64, version string,
) (ports.ReleaseStats, error) {
	var stats ports.ReleaseStats

	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM issues WHERE project_id = ? AND first_release = ?",
		projectID, version,
	).Scan(&stats.NewIssues); err != nil {
		return ports.ReleaseStats{}, fmt.Errorf("counting new issues: %w", err)
	}

	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM issues WHERE project_id = ? AND regressed_in_release = ?",
		projectID, version,
	).Scan(&stats.RegressedIssues); err != nil {
		return ports.ReleaseStats{}, fmt.Errorf("counting regressed issues: %w", err)
	}

	// One release's events, not the project's: the bucket rows are keyed by
	// the dimension's value, and summing without that predicate would report
	// every deploy's traffic on every release's page — a number that looks
	// plausible and is wrong, which is the worst kind.
	//
	// SUM over no rows is NULL, which is a release nobody has sent an event
	// from yet. That is a zero, not an error and not an absence.
	var total sql.NullInt64
	if err := r.db.QueryRowContext(ctx, `
		SELECT SUM(count) FROM project_hourly_dims
		WHERE project_id = ? AND dim = ? AND value = ?`,
		projectID, string(domain.DimensionRelease), version,
	).Scan(&total); err != nil {
		return ports.ReleaseStats{}, fmt.Errorf("summing release events: %w", err)
	}
	stats.Events = total.Int64
	return stats, nil
}

// releaseRanks reads the first-sight rank of just the versions being
// compared.
//
// Two rows, not the project's whole release order: this runs inside the
// transaction that decides whether an event reopens an issue, and a query
// whose cost grows with how often somebody deploys does not belong there.
//
// A version missing from the result has never been seen, which the comparison
// reads as newer than anything that has (ADR 012). That is why the event's own
// release is upserted before this runs: by the time the two are compared, the
// build that is emitting right now is a release this installation knows about.
func releaseRanks(
	ctx context.Context, tx *sql.Tx, projectID int64, versions ...string,
) (domain.ReleaseOrder, error) {
	wanted := make([]any, 0, len(versions)+1)
	wanted = append(wanted, projectID)
	placeholders := ""
	for _, version := range versions {
		if version == "" {
			continue
		}
		if placeholders != "" {
			placeholders += ", "
		}
		placeholders += "?"
		wanted = append(wanted, version)
	}
	if placeholders == "" {
		return nil, nil
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT version, id FROM releases WHERE project_id = ? AND version IN ("+placeholders+")",
		wanted...)
	if err != nil {
		return nil, fmt.Errorf("reading release ranks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	order := domain.ReleaseOrder{}
	for rows.Next() {
		var (
			version string
			rank    int64
		)
		if err := rows.Scan(&version, &rank); err != nil {
			return nil, fmt.Errorf("scanning release rank: %w", err)
		}
		order[version] = rank
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating release ranks: %w", err)
	}
	return order, nil
}

// Order is the order a project's releases were first seen in.
//
// The map is the whole project's releases, which is bounded by how often
// somebody deploys and is read on the two cold paths that need it — a release
// listing and an issue being reopened. It is deliberately not read on the
// ingest path, where only two versions ever need comparing.
func (r *ReleaseRepository) Order(ctx context.Context, projectID int64) (domain.ReleaseOrder, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT version, id FROM releases WHERE project_id = ?", projectID)
	if err != nil {
		return nil, fmt.Errorf("reading release order: %w", err)
	}
	defer func() { _ = rows.Close() }()

	order := domain.ReleaseOrder{}
	for rows.Next() {
		var (
			version string
			rank    int64
		)
		if err := rows.Scan(&version, &rank); err != nil {
			return nil, fmt.Errorf("scanning release order: %w", err)
		}
		order[version] = rank
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating release order: %w", err)
	}
	return order, nil
}

func scanRelease(row interface{ Scan(...any) error }) (domain.Release, error) {
	var (
		release   domain.Release
		createdAt string
		released  sql.NullString
		first     sql.NullString
		last      sql.NullString
	)
	if err := row.Scan(&release.ID, &release.ProjectID, &release.Version, &createdAt,
		&released, &first, &last, &release.CommitCount); err != nil {
		return domain.Release{}, err //nolint:wrapcheck // the caller distinguishes sql.ErrNoRows.
	}

	var err error
	if release.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Release{}, err
	}
	if release.DateReleased, err = parseNullableTime(released); err != nil {
		return domain.Release{}, err
	}
	if release.FirstEventAt, err = parseNullableTime(first); err != nil {
		return domain.Release{}, err
	}
	if release.LastEventAt, err = parseNullableTime(last); err != nil {
		return domain.Release{}, err
	}
	return release, nil
}
