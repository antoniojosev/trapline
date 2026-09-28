package ports

import (
	"context"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
)

// RateLimiter decides whether a category may be ingested right now.
//
// It is a port rather than a concrete limiter because the answer combines two
// different things: whether the subsystem is switched on for this project at
// all, and whether it is currently over a burst threshold. The first is
// configuration and the second is state, and the ingest path should not have
// to know which one refused it.
type RateLimiter interface {
	// Allow reports whether one event of this category may be accepted.
	// Refusing must be cheaper than accepting, or a flood costs the same
	// either way and the limit protects nothing.
	Allow(ctx context.Context, projectID int64, category engine.Category) (bool, error)
}

// ProjectConfigStore reads and writes per-project ingest configuration.
//
// Separate from ProjectRepository because it has a different access pattern:
// this is read on the hot ingest path, behind a cache, while the rest of the
// repository is read by an operator looking at a panel.
type ProjectConfigStore interface {
	ProjectConfig(ctx context.Context, projectID int64) (domain.ProjectConfig, error)
	SetProjectConfig(ctx context.Context, projectID int64, config domain.ProjectConfig) error
}
