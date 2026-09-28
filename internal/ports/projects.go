package ports

import (
	"context"

	"github.com/antoniojosev/trapline/internal/domain"
)

// ProjectRepository stores projects and their DSN keys.
//
// Projects and keys share one port rather than having one each because they
// share an invariant: a project without an active key can never receive an
// event, so it is not a meaningful state to be able to persist. Splitting
// them would let a caller create half of a project.
type ProjectRepository interface {
	// Create persists a project together with its first key, atomically.
	// The store assigns the project id and binds the key to it, returning
	// both as saved.
	Create(ctx context.Context, project domain.Project, key domain.Key) (domain.Project, domain.Key, error)

	// FindByID returns domain.ErrProjectNotFound if there is no such project.
	FindByID(ctx context.Context, id int64) (domain.Project, error)

	// FindBySlug resolves a project by the name a deploy tool puts in a URL,
	// and returns domain.ErrProjectNotFound when nothing holds that slug
	// (ADR 013).
	FindBySlug(ctx context.Context, slug string) (domain.Project, error)

	// List returns every project, oldest first.
	List(ctx context.Context) ([]domain.Project, error)

	// Delete removes a project and everything belonging to it. Deleting a
	// project that does not exist returns domain.ErrProjectNotFound.
	Delete(ctx context.Context, id int64) error

	// AddKey binds an already minted key to an existing project, for
	// rotation. It returns domain.ErrTooManyActiveKeys when the project
	// already holds the maximum, so the check cannot be lost to a race
	// between reading the count and writing the key.
	AddKey(ctx context.Context, projectID int64, key domain.Key) (domain.Key, error)

	// ActiveKeys returns a project's usable keys, oldest first.
	ActiveKeys(ctx context.Context, projectID int64) ([]domain.Key, error)

	// FindActiveKey resolves a public key for ingest authentication. This is
	// the hottest read in the product: every accepted envelope calls it.
	// Returns domain.ErrKeyNotFound when the key is unknown or revoked.
	FindActiveKey(ctx context.Context, publicKey string) (domain.Key, error)

	// RevokeKey marks a key unusable. Revoking an already revoked key is not
	// an error: a retried command must be safe.
	RevokeKey(ctx context.Context, publicKey string) error
}
