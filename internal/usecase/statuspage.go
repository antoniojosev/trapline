package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// StatusPage builds the public page for a project, and remembers it for a short while.
//
// It is a use case and not a handler helper because the page has real rules —
// which monitors show, what a day with no checks means, what counts as an
// incident — and those belong where they can be tested without a socket
// (ADR 006: the same answer is what the panel's preview reads).
type StatusPage struct {
	projects ports.ProjectRepository
	config   ports.ProjectConfigStore
	uptime   ports.UptimeRepository
	settings ports.SettingsStore
	clock    ports.Clock

	// mu guards cached. A page is read by anyone on the internet and built
	// from several queries; without this, a link posted somewhere busy turns
	// every reader into a fan-out of database work (ADR 017).
	mu     sync.Mutex
	cached map[string]cachedStatusPage
}

type cachedStatusPage struct {
	page      domain.StatusPage
	expiresAt time.Time
}

// NewStatusPage wires the use case.
func NewStatusPage(
	projects ports.ProjectRepository,
	config ports.ProjectConfigStore,
	uptime ports.UptimeRepository,
	settings ports.SettingsStore,
	clock ports.Clock,
) *StatusPage {
	return &StatusPage{
		projects: projects, config: config, uptime: uptime,
		settings: settings, clock: clock,
		cached: make(map[string]cachedStatusPage),
	}
}

// ErrStatusPageDisabled means the project exists but has not published a page.
//
// Distinct from "no such project" inside this package and deliberately not
// distinct to the caller on the wire: the handler answers 404 for both,
// because telling an anonymous reader that a project exists but keeps its
// status private is a fact they were not given.
var ErrStatusPageDisabled = errors.New("status page not enabled")

// Page returns the page for a project slug, from the cache when it is fresh.
func (s *StatusPage) Page(ctx context.Context, slug string) (domain.StatusPage, error) {
	now := s.clock.Now().UTC()

	s.mu.Lock()
	if hit, ok := s.cached[slug]; ok && now.Before(hit.expiresAt) {
		s.mu.Unlock()
		return hit.page, nil
	}
	s.mu.Unlock()

	page, err := s.build(ctx, slug, now)
	if err != nil {
		// Only a page that was actually rendered is remembered. A miss is not
		// cached on purpose: the key is chosen by whoever types the URL, so
		// caching misses would let an anonymous caller grow a map in this
		// server's memory one made-up slug at a time. What bounds the cost of
		// those requests is the per-address limit, not this map.
		return domain.StatusPage{}, err
	}

	s.mu.Lock()
	s.cached[slug] = cachedStatusPage{page: page, expiresAt: now.Add(domain.StatusPageTTL)}
	// The map is bounded by the number of projects that have published a
	// page, which is small, but expired entries of deleted projects would
	// otherwise stay forever. Sweeping here costs one pass every thirty
	// seconds per project and needs no job.
	for key, entry := range s.cached {
		if !now.Before(entry.expiresAt) {
			delete(s.cached, key)
		}
	}
	s.mu.Unlock()
	return page, nil
}

// Invalidate drops a project's cached page.
//
// Called when the operator changes what the page says, so the panel's own
// save does not appear to have done nothing for thirty seconds — which is the
// shape of bug report that ends with somebody deciding the setting is broken.
func (s *StatusPage) Invalidate(slug string) {
	s.mu.Lock()
	delete(s.cached, slug)
	s.mu.Unlock()
}

// InvalidateAll drops every cached page, for a change that affects all of them
// (the installation-wide title and description).
func (s *StatusPage) InvalidateAll() {
	s.mu.Lock()
	clear(s.cached)
	s.mu.Unlock()
}

func (s *StatusPage) build(ctx context.Context, slug string, now time.Time) (domain.StatusPage, error) {
	project, err := s.projects.FindBySlug(ctx, slug)
	if err != nil {
		return domain.StatusPage{}, err
	}
	config, err := s.config.ProjectConfig(ctx, project.ID)
	if err != nil {
		return domain.StatusPage{}, err
	}
	if !config.StatusPage.Enabled {
		return domain.StatusPage{}, fmt.Errorf("%w: %s", ErrStatusPageDisabled, slug)
	}

	monitors, err := s.uptime.ListMonitors(ctx, &project.ID)
	if err != nil {
		return domain.StatusPage{}, err
	}

	from := now.Truncate(24*time.Hour).AddDate(0, 0, -(domain.StatusPageDays - 1))
	shown := make([]domain.StatusPageMonitor, 0, len(monitors))
	for i := range monitors {
		monitor := &monitors[i]
		if !monitor.Public {
			continue
		}
		days, err := s.uptime.DailyUptime(ctx, monitor.ID, from, now)
		if err != nil {
			return domain.StatusPage{}, err
		}
		shown = append(shown, domain.NewStatusPageMonitor(monitor, days, now))
	}

	return domain.NewStatusPage(s.Settings(ctx), project.Name, shown, now), nil
}

// Settings reads the installation-wide title and description.
//
// A stored value that no longer validates is treated as absent, the way the
// digest treats an unreadable schedule: the page is public and must render.
func (s *StatusPage) Settings(ctx context.Context) domain.StatusPageSettings {
	raw, err := s.settings.Setting(ctx, domain.StatusPageSettingsKey)
	if err != nil {
		return domain.StatusPageSettings{}
	}
	return domain.DecodeStatusPageSettings(raw)
}

// SetSettings writes the title and description.
func (s *StatusPage) SetSettings(ctx context.Context, settings domain.StatusPageSettings) (domain.StatusPageSettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.StatusPageSettings{}, err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return domain.StatusPageSettings{}, fmt.Errorf("encoding status page settings: %w", err)
	}
	if err := s.settings.SetSetting(ctx, domain.StatusPageSettingsKey, encoded); err != nil {
		return domain.StatusPageSettings{}, err
	}
	s.InvalidateAll()
	return settings, nil
}
