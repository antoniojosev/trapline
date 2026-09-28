package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Compile-time proof that this adapter satisfies the port.
var _ ports.ProjectRepository = (*ProjectRepository)(nil)

// ProjectRepository is the SQLite implementation of ports.ProjectRepository.
type ProjectRepository struct {
	db *DB
}

// NewProjectRepository wires the repository to an open database.
func NewProjectRepository(db *DB) *ProjectRepository {
	return &ProjectRepository{db: db}
}

// Create persists a project and its first key in one transaction. A project
// with no key could never receive an event, so committing one without the
// other would persist a state that has no meaning.
func (r *ProjectRepository) Create(
	ctx context.Context, project domain.Project, key domain.Key,
) (domain.Project, domain.Key, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, domain.Key{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The slug is settled here rather than in the domain because the only
	// thing that decides it is whether somebody already holds it, and that is
	// a fact about the store. Inside the transaction, so two `projects create`
	// racing cannot both read the same slug as free.
	slug, err := freeSlug(ctx, tx, project.Name)
	if err != nil {
		return domain.Project{}, domain.Key{}, err
	}
	project.Slug = slug

	result, err := tx.ExecContext(ctx,
		"INSERT INTO projects (name, slug, created_at) VALUES (?, ?, ?)",
		project.Name, project.Slug, formatTime(project.CreatedAt),
	)
	if err != nil {
		return domain.Project{}, domain.Key{}, fmt.Errorf("inserting project: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Project{}, domain.Key{}, fmt.Errorf("reading assigned project id: %w", err)
	}
	project.ID = id

	// The key is bound to the id only now, inside the transaction: the store
	// is what assigns the id, and the domain is what owns the binding rule.
	key, err = key.AssignTo(id)
	if err != nil {
		return domain.Project{}, domain.Key{}, err
	}
	if err := insertKey(ctx, tx, key); err != nil {
		return domain.Project{}, domain.Key{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Project{}, domain.Key{}, fmt.Errorf("committing project: %w", err)
	}
	return project, key, nil
}

// FindByID returns one project.
func (r *ProjectRepository) FindByID(ctx context.Context, id int64) (domain.Project, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, name, slug, created_at FROM projects WHERE id = ?", id)

	project, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, id)
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("reading project %d: %w", id, err)
	}
	return project, nil
}

// FindBySlug resolves the name a deploy tool put in a URL (ADR 013).
func (r *ProjectRepository) FindBySlug(ctx context.Context, slug string) (domain.Project, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, name, slug, created_at FROM projects WHERE slug = ?", slug)

	project, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, fmt.Errorf("%w: slug %q", domain.ErrProjectNotFound, slug)
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("reading project %q: %w", slug, err)
	}
	return project, nil
}

// freeSlug returns the first candidate for this name that nobody holds.
//
// It walks the domain's list rather than inventing suffixes of its own, so
// what a project ends up called is the same on every installation and is
// answerable without a database.
func freeSlug(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	candidates := domain.SlugCandidates(name)
	for _, candidate := range candidates {
		var taken int
		err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM projects WHERE slug = ?", candidate).Scan(&taken)
		if err != nil {
			return "", fmt.Errorf("checking the slug %q: %w", candidate, err)
		}
		if taken == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: %d projects already derive the slug %q; give this one a different name",
		domain.ErrInvalidProject, len(candidates), candidates[0])
}

// List returns every project, oldest first.
func (r *ProjectRepository) List(ctx context.Context) ([]domain.Project, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, name, slug, created_at FROM projects ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var projects []domain.Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating projects: %w", err)
	}
	return projects, nil
}

// Delete removes a project and, by cascade, its keys.
func (r *ProjectRepository) Delete(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, "DELETE FROM projects WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting project %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading delete result: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, id)
	}
	return nil
}

// AddKey binds a minted key to an existing project, for rotation.
//
// The active-key count is checked inside the same transaction as the insert.
// Checking it in the use case instead would leave a window where two
// concurrent rotations each see room for one more key and both write.
func (r *ProjectRepository) AddKey(
	ctx context.Context, projectID int64, key domain.Key,
) (domain.Key, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Key{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM projects WHERE id = ?", projectID).Scan(&exists); err != nil {
		return domain.Key{}, fmt.Errorf("checking project %d: %w", projectID, err)
	}
	if exists == 0 {
		return domain.Key{}, fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, projectID)
	}

	var active int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM project_keys WHERE project_id = ? AND revoked_at IS NULL",
		projectID).Scan(&active); err != nil {
		return domain.Key{}, fmt.Errorf("counting active keys: %w", err)
	}
	if active >= domain.MaxActiveKeys {
		return domain.Key{}, fmt.Errorf(
			"%w: project %d already has %d active keys; revoke one before issuing another",
			domain.ErrTooManyActiveKeys, projectID, active)
	}

	key, err = key.AssignTo(projectID)
	if err != nil {
		return domain.Key{}, err
	}
	if err := insertKey(ctx, tx, key); err != nil {
		return domain.Key{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Key{}, fmt.Errorf("committing key: %w", err)
	}
	return key, nil
}

// ActiveKeys returns a project's usable keys, oldest first.
func (r *ProjectRepository) ActiveKeys(ctx context.Context, projectID int64) ([]domain.Key, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT public_key, project_id, created_at, revoked_at
		FROM project_keys
		WHERE project_id = ? AND revoked_at IS NULL
		ORDER BY created_at, public_key`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing keys for project %d: %w", projectID, err)
	}
	defer func() { _ = rows.Close() }()

	var keys []domain.Key
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating keys: %w", err)
	}
	return keys, nil
}

// FindActiveKey resolves a public key for ingest authentication.
func (r *ProjectRepository) FindActiveKey(ctx context.Context, publicKey string) (domain.Key, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT public_key, project_id, created_at, revoked_at
		FROM project_keys
		WHERE public_key = ? AND revoked_at IS NULL`, publicKey)

	key, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Deliberately not echoing the key back: this error reaches an
		// unauthenticated caller on the public ingest endpoint.
		return domain.Key{}, domain.ErrKeyNotFound
	}
	if err != nil {
		return domain.Key{}, fmt.Errorf("reading key: %w", err)
	}
	return key, nil
}

// RevokeKey marks a key unusable. Revoking an unknown or already revoked key
// is a no-op, so a retried command is safe.
func (r *ProjectRepository) RevokeKey(ctx context.Context, publicKey string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_keys SET revoked_at = ?
		WHERE public_key = ? AND revoked_at IS NULL`,
		formatTime(nowUTC()), publicKey)
	if err != nil {
		return fmt.Errorf("revoking key: %w", err)
	}
	return nil
}

func insertKey(ctx context.Context, tx *sql.Tx, key domain.Key) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO project_keys (public_key, project_id, created_at) VALUES (?, ?, ?)",
		key.PublicKey, key.ProjectID, formatTime(key.CreatedAt))
	if err != nil {
		return fmt.Errorf("inserting key: %w", err)
	}
	return nil
}

// scanner is what *sql.Row and *sql.Rows have in common, so one scan
// function serves both the single-row and the iterating query.
type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (domain.Project, error) {
	var (
		project   domain.Project
		createdAt string
	)
	if err := row.Scan(&project.ID, &project.Name, &project.Slug, &createdAt); err != nil {
		return domain.Project{}, err
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.Project{}, err
	}
	project.CreatedAt = parsed
	return project, nil
}

func scanKey(row scanner) (domain.Key, error) {
	var (
		key       domain.Key
		createdAt string
		revokedAt sql.NullString
	)
	if err := row.Scan(&key.PublicKey, &key.ProjectID, &createdAt, &revokedAt); err != nil {
		return domain.Key{}, err
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.Key{}, err
	}
	key.CreatedAt = parsed

	if revokedAt.Valid {
		revoked, err := parseTime(revokedAt.String)
		if err != nil {
			return domain.Key{}, err
		}
		key.RevokedAt = &revoked
	}
	return key, nil
}

// ProjectConfig reads a project's ingest configuration.
//
// On the ingest path, so it returns the defaults rather than an error when a
// project has no configuration yet: a missing row of settings must not stop a
// project from receiving the errors it was created to receive.
func (r *ProjectRepository) ProjectConfig(ctx context.Context, projectID int64) (domain.ProjectConfig, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, "SELECT config FROM projects WHERE id = ?", projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectConfig{}, fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, projectID)
	}
	if err != nil {
		return domain.ProjectConfig{}, fmt.Errorf("reading project config: %w", err)
	}
	return domain.DecodeProjectConfig(raw), nil
}

// SetProjectConfig writes a project's ingest configuration.
func (r *ProjectRepository) SetProjectConfig(ctx context.Context, projectID int64, config domain.ProjectConfig) error {
	encoded, err := domain.EncodeProjectConfig(config)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, "UPDATE projects SET config = ? WHERE id = ?", encoded, projectID)
	if err != nil {
		return fmt.Errorf("writing project config: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading update result: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, projectID)
	}
	return nil
}
