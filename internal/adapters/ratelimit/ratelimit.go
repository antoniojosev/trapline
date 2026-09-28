// Package ratelimit decides whether an event may be ingested.
//
// It answers two questions at once, deliberately: is this category switched on
// for this project, and is the project currently over its burst threshold. The
// ingest path should not have to know which of the two refused it, because the
// action it takes is the same either way — tell the SDK to stop sending this
// category (ADR 005).
package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/ports"
)

// configTTL is how long a project's configuration is trusted without
// re-reading it.
//
// A cache is not an optimisation here, it is a requirement: the limiter runs
// once per ingested event, and a database read per event would make the
// cheapest possible path — refusing something — cost the same as accepting it.
// Ten seconds is short enough that switching a category off takes effect while
// someone is still watching the screen where they switched it.
const configTTL = 10 * time.Second

var (
	_ ports.RateLimiter = (*Limiter)(nil)
	// The limiter is also the configuration store the rest of the application
	// uses. That is not layering for its own sake: it makes it impossible to
	// write configuration without dropping the cache that would otherwise
	// keep serving the old value.
	//
	// The previous shape had the writer call an invalidation callback it was
	// handed at assembly time. It worked in production and silently did not in
	// a test that assembled the stack slightly differently — which is the
	// failure mode of any rule enforced by remembering. Now the only way to
	// write is through the thing that caches.
	_ ports.ProjectConfigStore = (*Limiter)(nil)
)

// Limiter is an in-memory rate limiter with a short-lived config cache.
//
// In memory, and therefore per process, which is correct for a product whose
// architecture is a single binary on a single server (ADR 001). A shared store
// would add a dependency to buy coordination nobody needs.
type Limiter struct {
	store ports.ProjectConfigStore
	now   func() time.Time

	mu      sync.Mutex
	configs map[int64]cachedConfig
	buckets map[bucketKey]*bucket
}

type cachedConfig struct {
	config    domain.ProjectConfig
	expiresAt time.Time
}

type bucketKey struct {
	projectID int64
	category  engine.Category
}

// bucket is a fixed-window counter.
//
// A window rather than a token bucket because the thing being defended against
// is a runaway loop reporting an error per iteration, not a carefully paced
// attacker. A window is a counter and a timestamp; a token bucket is more
// arithmetic to reach the same answer for this shape of traffic.
type bucket struct {
	windowStart time.Time
	count       int
}

// New wraps a configuration store with caching and rate limiting.
func New(store ports.ProjectConfigStore) *Limiter {
	return &Limiter{
		store:   store,
		now:     time.Now,
		configs: map[int64]cachedConfig{},
		buckets: map[bucketKey]*bucket{},
	}
}

// Allow reports whether one event of this category may be accepted.
func (l *Limiter) Allow(ctx context.Context, projectID int64, category engine.Category) (bool, error) {
	config, err := l.configFor(ctx, projectID)
	if err != nil {
		return false, err
	}

	// A switched-off category costs nothing at all: no counter, no bucket, no
	// allocation. This is what makes "opt-in with zero cost when off" true
	// rather than aspirational.
	if !config.CategoryEnabled(string(category)) {
		return false, nil
	}

	return l.consume(projectID, category, config.Limit()), nil
}

func (l *Limiter) consume(projectID int64, category engine.Category, limit int) bool {
	now := l.now()
	key := bucketKey{projectID: projectID, category: category}

	l.mu.Lock()
	defer l.mu.Unlock()

	current, found := l.buckets[key]
	if !found || now.Sub(current.windowStart) >= time.Minute {
		l.buckets[key] = &bucket{windowStart: now, count: 1}
		return true
	}
	if current.count >= limit {
		return false
	}
	current.count++
	return true
}

// ProjectConfig reads a project's configuration, from cache when it is fresh.
func (l *Limiter) ProjectConfig(ctx context.Context, projectID int64) (domain.ProjectConfig, error) {
	return l.configFor(ctx, projectID)
}

// SetProjectConfig writes a project's configuration and drops what was cached,
// so a change takes effect on the next event rather than after the TTL.
func (l *Limiter) SetProjectConfig(ctx context.Context, projectID int64, config domain.ProjectConfig) error {
	if err := l.store.SetProjectConfig(ctx, projectID, config); err != nil {
		return err
	}
	l.Forget(projectID)
	return nil
}

func (l *Limiter) configFor(ctx context.Context, projectID int64) (domain.ProjectConfig, error) {
	now := l.now()

	l.mu.Lock()
	cached, found := l.configs[projectID]
	l.mu.Unlock()

	if found && now.Before(cached.expiresAt) {
		return cached.config, nil
	}

	config, err := l.store.ProjectConfig(ctx, projectID)
	if err != nil {
		return domain.ProjectConfig{}, err
	}

	l.mu.Lock()
	l.configs[projectID] = cachedConfig{config: config, expiresAt: now.Add(configTTL)}
	l.mu.Unlock()

	return config, nil
}

// Forget drops a project's cached configuration and its counters. Writes go
// through SetProjectConfig, which calls this; it is exported for the rare case
// of a change made outside the application, such as an edited database.
func (l *Limiter) Forget(projectID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.configs, projectID)
	for key := range l.buckets {
		if key.projectID == projectID {
			delete(l.buckets, key)
		}
	}
}

// Sweep drops buckets whose window has long passed.
//
// Called opportunistically rather than from a timer: this product does not
// start a goroutine for a subsystem that can ride along on work already
// happening (ADR 005). Without it, a project that reported once and went quiet
// would keep a bucket forever.
func (l *Limiter) Sweep() {
	cutoff := l.now().Add(-5 * time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()
	for key, current := range l.buckets {
		if current.windowStart.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
	for projectID, cached := range l.configs {
		if cached.expiresAt.Before(cutoff) {
			delete(l.configs, projectID)
		}
	}
}
