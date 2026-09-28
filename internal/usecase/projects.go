// Package usecase holds the application's operations.
//
// A use case orchestrates: it validates through the domain, calls ports, and
// returns domain values. It knows nothing about HTTP, SQL, JSON or terminals
// — that is what makes the same operation serve the REST API, the web UI, the
// CLI and the MCP server without any of them re-implementing it (ADR 006).
package usecase

import (
	"context"
	"fmt"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Projects is the set of operations on projects and their DSN keys.
type Projects struct {
	repo   ports.ProjectRepository
	config ports.ProjectConfigStore
	clock  ports.Clock
	origin domain.Origin
}

// NewProjects wires the use case.
func NewProjects(
	repo ports.ProjectRepository,
	config ports.ProjectConfigStore,
	clock ports.Clock,
	origin domain.Origin,
) *Projects {
	return &Projects{repo: repo, config: config, clock: clock, origin: origin}
}

// ProjectView is a project together with everything a caller needs to act on
// it: its active keys and the DSN string for each.
//
// The DSN is computed here rather than stored because it is derived from the
// key plus the configured public origin. Persisting it would mean every
// stored DSN goes stale the day the installation moves to a real domain.
type ProjectView struct {
	Project domain.Project
	Keys    []KeyView
}

// KeyView is one key and the DSN built from it.
type KeyView struct {
	Key domain.Key
	DSN domain.DSN
}

// PrimaryDSN is the DSN a user should copy: the oldest active key, which is
// the one already deployed. During a rotation a project has two, and handing
// back the newest would suggest switching before the old one is retired.
func (v ProjectView) PrimaryDSN() (domain.DSN, bool) {
	if len(v.Keys) == 0 {
		return domain.DSN{}, false
	}
	return v.Keys[0].DSN, true
}

// Create makes a project with its first key and returns it ready to use.
func (p *Projects) Create(ctx context.Context, name string) (ProjectView, error) {
	now := p.clock.Now()

	project, err := domain.NewProject(name, now)
	if err != nil {
		return ProjectView{}, err
	}
	key, err := domain.NewKey(now)
	if err != nil {
		return ProjectView{}, fmt.Errorf("minting the first key: %w", err)
	}

	saved, savedKey, err := p.repo.Create(ctx, project, key)
	if err != nil {
		return ProjectView{}, err
	}
	return p.view(saved, []domain.Key{savedKey}), nil
}

// List returns every project with its active keys.
func (p *Projects) List(ctx context.Context) ([]ProjectView, error) {
	projects, err := p.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	views := make([]ProjectView, 0, len(projects))
	for _, project := range projects {
		keys, err := p.repo.ActiveKeys(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, p.view(project, keys))
	}
	return views, nil
}

// Get returns one project with its active keys.
func (p *Projects) Get(ctx context.Context, id int64) (ProjectView, error) {
	project, err := p.repo.FindByID(ctx, id)
	if err != nil {
		return ProjectView{}, err
	}
	keys, err := p.repo.ActiveKeys(ctx, id)
	if err != nil {
		return ProjectView{}, err
	}
	return p.view(project, keys), nil
}

// Find returns a project without its keys.
//
// Separate from Get because most callers that need a project's name do not
// need its DSNs: an issue listing names the project it is showing, and paying
// a second query per page for keys nobody renders is a cost that only grows
// with how often the screen is refreshed. Keys are also the one part of a
// project that is a credential, so not fetching them where they are not shown
// is worth a method of its own.
func (p *Projects) Find(ctx context.Context, id int64) (domain.Project, error) {
	return p.repo.FindByID(ctx, id)
}

// Delete removes a project and everything belonging to it.
func (p *Projects) Delete(ctx context.Context, id int64) error {
	return p.repo.Delete(ctx, id)
}

// RotateKey issues an additional key for a project, leaving the existing one
// active. Rotation is two steps on purpose: issue, deploy, then revoke. A
// single "replace the key" call would break every running deployment at the
// instant it returned.
func (p *Projects) RotateKey(ctx context.Context, projectID int64) (KeyView, error) {
	key, err := domain.NewKey(p.clock.Now())
	if err != nil {
		return KeyView{}, fmt.Errorf("minting key: %w", err)
	}
	saved, err := p.repo.AddKey(ctx, projectID, key)
	if err != nil {
		return KeyView{}, err
	}
	return p.keyView(saved), nil
}

// RevokeKey retires a key. It is idempotent.
func (p *Projects) RevokeKey(ctx context.Context, publicKey string) error {
	return p.repo.RevokeKey(ctx, publicKey)
}

func (p *Projects) view(project domain.Project, keys []domain.Key) ProjectView {
	views := make([]KeyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, p.keyView(key))
	}
	return ProjectView{Project: project, Keys: views}
}

func (p *Projects) keyView(key domain.Key) KeyView {
	return KeyView{Key: key, DSN: p.origin.DSNFor(key)}
}

// AuthenticateKey resolves a DSN public key for ingestion.
//
// The project id from the request path is checked against the key's own
// project rather than trusted. They come from different places — the path is
// free text, the key is a credential — and letting the path decide would mean
// any valid key could write into any project.
func (p *Projects) AuthenticateKey(ctx context.Context, publicKey string, pathProjectID int64) (domain.Key, error) {
	key, err := p.repo.FindActiveKey(ctx, publicKey)
	if err != nil {
		return domain.Key{}, err
	}
	if key.ProjectID != pathProjectID {
		return domain.Key{}, domain.ErrKeyNotFound
	}
	return key, nil
}

// Config reads a project's ingest configuration.
func (p *Projects) Config(ctx context.Context, projectID int64) (domain.ProjectConfig, error) {
	return p.config.ProjectConfig(ctx, projectID)
}

// SetConfig writes a project's ingest configuration.
//
// Whether that invalidates a cache is the store's business, not this use
// case's: the store it is given is the one that caches, so there is no way to
// write without the cache learning about it.
func (p *Projects) SetConfig(ctx context.Context, projectID int64, config domain.ProjectConfig) error {
	return p.config.SetProjectConfig(ctx, projectID, config)
}
