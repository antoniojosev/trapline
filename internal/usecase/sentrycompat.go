package usecase

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/antoniojosev/trapline/internal/domain"
)

// This file holds the two lookups the sentry-cli compatibility surface needs
// and nothing else does (ADR 013). They are here rather than folded into
// projects.go and releases.go so that the emulated surface can be read — and
// removed — as one piece.

// ResolveProject finds a project by the reference a deploy tool put in a URL.
//
// A numeric reference is an id and never a slug. That order is not arbitrary:
// the id is the identifier the protocol already forces on every installation
// (it is in the ingest path of every DSN), so it is the one that cannot be
// taken away, and reading a number as anything else would make an id
// occasionally address a different project. The domain refuses to derive a
// slug that is only digits for the same reason.
func (p *Projects) ResolveProject(ctx context.Context, reference string) (domain.Project, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return domain.Project{}, fmt.Errorf("%w: no project in the path", domain.ErrProjectNotFound)
	}
	if id, err := strconv.ParseInt(reference, 10, 64); err == nil && id > 0 {
		return p.repo.FindByID(ctx, id)
	}
	return p.repo.FindBySlug(ctx, reference)
}

// ProjectsWithVersion returns the projects that have a release of this
// version, oldest project first.
//
// It exists because half of what sentry-cli sends names no project at all.
// Setting a commit set and recording a deploy are addressed to the
// organisation — `PUT /organizations/{org}/releases/{version}/` — while
// creating and finalising the same release are addressed to the project. This
// installation has one organisation (ADR 013), so the only thing left to
// resolve the call with is the version itself.
//
// Every match is returned rather than the first, and the caller applies the
// change to all of them. In the API being emulated a release belongs to the
// organisation and spans projects, so a commit set attached to `app@1.0.0`
// attaches to `app@1.0.0` everywhere — and a caller that picked one project
// would silently drop the write for the others.
func (r *Releases) ProjectsWithVersion(ctx context.Context, version string) ([]domain.Project, error) {
	projects, err := r.projects.List(ctx)
	if err != nil {
		return nil, err
	}

	matched := make([]domain.Project, 0, 1)
	for _, project := range projects {
		_, err := r.repo.Find(ctx, project.ID, version)
		if err == nil {
			matched = append(matched, project)
			continue
		}
		if !errors.Is(err, domain.ErrReleaseNotFound) {
			return nil, err
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("%w: no project has a release %q", domain.ErrReleaseNotFound, version)
	}
	return matched, nil
}
