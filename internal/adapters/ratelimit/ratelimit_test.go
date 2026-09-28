package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
)

type stubSource struct {
	mu      sync.Mutex
	config  domain.ProjectConfig
	reads   int
	failing error
}

// SetProjectConfig makes the stub a full ports.ProjectConfigStore. The limiter
// is the store the application writes through, so a stub that only reads could
// not stand in for the real one.
func (s *stubSource) SetProjectConfig(_ context.Context, _ int64, config domain.ProjectConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = config
	return nil
}

func (s *stubSource) ProjectConfig(context.Context, int64) (domain.ProjectConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failing != nil {
		return domain.ProjectConfig{}, s.failing
	}
	return s.config, nil
}

func (s *stubSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func newLimiter(config domain.ProjectConfig) (*Limiter, *stubSource, *time.Time) {
	source := &stubSource{config: config}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	limiter := New(source)
	limiter.now = func() time.Time { return now }
	return limiter, source, &now
}

func TestDisabledCategoriesAreRefusedForFree(t *testing.T) {
	// The minimum profile: a fresh project takes errors and nothing else.
	limiter, _, _ := newLimiter(domain.ProjectConfig{})
	ctx := context.Background()

	allowed, err := limiter.Allow(ctx, 1, engine.CategoryError)
	if err != nil || !allowed {
		t.Fatalf("errors were refused: allowed=%v err=%v", allowed, err)
	}

	for _, category := range []engine.Category{engine.CategoryTransaction, engine.CategorySession, engine.CategoryCheckIn} {
		t.Run(string(category), func(t *testing.T) {
			allowed, err := limiter.Allow(ctx, 1, category)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allowed {
				t.Errorf("%q was accepted on a default project", category)
			}
		})
	}

	// A switched-off category must cost nothing at all: no counter, no
	// bucket. This is what makes "zero cost when off" true rather than
	// aspirational.
	limiter.mu.Lock()
	buckets := len(limiter.buckets)
	limiter.mu.Unlock()
	if buckets != 1 {
		t.Errorf("%d buckets allocated; a disabled category allocated state", buckets)
	}
}

func TestOptingInACategory(t *testing.T) {
	limiter, _, _ := newLimiter(domain.ProjectConfig{EnabledCategories: []string{"error", "transaction"}})

	allowed, err := limiter.Allow(context.Background(), 1, engine.CategoryTransaction)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("an explicitly enabled category was refused")
	}
}

func TestTheLimitHolds(t *testing.T) {
	// The usual cause of a spike is not an attacker, it is someone's retry
	// loop reporting an error per iteration.
	limiter, _, _ := newLimiter(domain.ProjectConfig{RateLimitPerMinute: 5})
	ctx := context.Background()

	for i := range 5 {
		allowed, err := limiter.Allow(ctx, 1, engine.CategoryError)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Fatalf("event %d was refused below the limit", i)
		}
	}

	allowed, err := limiter.Allow(ctx, 1, engine.CategoryError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("the limit was exceeded")
	}
}

func TestTheWindowResets(t *testing.T) {
	limiter, _, now := newLimiter(domain.ProjectConfig{RateLimitPerMinute: 2})
	ctx := context.Background()

	for range 3 {
		if _, err := limiter.Allow(ctx, 1, engine.CategoryError); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	*now = now.Add(time.Minute)

	allowed, err := limiter.Allow(ctx, 1, engine.CategoryError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("the window did not reset after a minute")
	}
}

func TestProjectsHaveTheirOwnBudgets(t *testing.T) {
	// One noisy project must not be able to silence another's errors.
	limiter, _, _ := newLimiter(domain.ProjectConfig{RateLimitPerMinute: 2})
	ctx := context.Background()

	for range 3 {
		if _, err := limiter.Allow(ctx, 1, engine.CategoryError); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	allowed, err := limiter.Allow(ctx, 2, engine.CategoryError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("one project exhausting its budget blocked another")
	}
}

func TestConfigIsCached(t *testing.T) {
	// A database read per event would make refusing something cost the same
	// as accepting it, and the limit would protect nothing.
	limiter, source, now := newLimiter(domain.ProjectConfig{})
	ctx := context.Background()

	for range 50 {
		if _, err := limiter.Allow(ctx, 1, engine.CategoryError); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if reads := source.readCount(); reads != 1 {
		t.Errorf("%d config reads for 50 events, want 1", reads)
	}

	// Short enough that switching a category off takes effect while someone
	// is still looking at the screen where they switched it.
	*now = now.Add(configTTL + time.Second)
	if _, err := limiter.Allow(ctx, 1, engine.CategoryError); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reads := source.readCount(); reads != 2 {
		t.Errorf("%d config reads after the TTL expired, want 2", reads)
	}
}

func TestWritingConfigDropsTheCache(t *testing.T) {
	// The invalidation is not the caller's job any more: writing goes through
	// the thing that caches, so there is no assembly in which a change fails
	// to take effect. That used to depend on remembering a callback, and a
	// test that assembled the stack differently silently lost it.
	limiter, source, _ := newLimiter(domain.ProjectConfig{})
	ctx := context.Background()

	allowed, err := limiter.Allow(ctx, 1, engine.CategoryTransaction)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("transactions were accepted on a default project")
	}

	if err := limiter.SetProjectConfig(ctx, 1, domain.ProjectConfig{
		EnabledCategories: []string{"error", "transaction"},
	}); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	allowed, err = limiter.Allow(ctx, 1, engine.CategoryTransaction)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("the change did not take effect on the next event")
	}
	if reads := source.readCount(); reads < 2 {
		t.Errorf("%d config reads; the cache was not dropped by the write", reads)
	}
}

func TestForgetTakesEffectImmediately(t *testing.T) {
	limiter, source, _ := newLimiter(domain.ProjectConfig{})
	ctx := context.Background()

	if _, err := limiter.Allow(ctx, 1, engine.CategoryError); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	limiter.Forget(1)
	if _, err := limiter.Allow(ctx, 1, engine.CategoryError); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reads := source.readCount(); reads != 2 {
		t.Errorf("%d config reads, want the cache to have been dropped", reads)
	}
}

func TestAConfigFailureIsReported(t *testing.T) {
	// Distinct from "refused": the ingest handler answers differently for a
	// server problem than for a category that is switched off.
	source := &stubSource{failing: errors.New("database is gone")}
	limiter := New(source)

	if _, err := limiter.Allow(context.Background(), 1, engine.CategoryError); err == nil {
		t.Error("a config failure was reported as a plain refusal")
	}
}

func TestSweepDropsStaleState(t *testing.T) {
	// Without it, a project that reported once and went quiet keeps a bucket
	// forever — a slow leak on a product that advertises a small footprint.
	limiter, _, now := newLimiter(domain.ProjectConfig{})
	ctx := context.Background()

	for projectID := range int64(10) {
		if _, err := limiter.Allow(ctx, projectID+1, engine.CategoryError); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	*now = now.Add(time.Hour)
	limiter.Sweep()

	limiter.mu.Lock()
	buckets, configs := len(limiter.buckets), len(limiter.configs)
	limiter.mu.Unlock()

	if buckets != 0 || configs != 0 {
		t.Errorf("after a sweep: %d buckets, %d configs; want none", buckets, configs)
	}
}

func TestConcurrentAllowsRespectTheLimit(t *testing.T) {
	limiter, _, _ := newLimiter(domain.ProjectConfig{RateLimitPerMinute: 100})
	ctx := context.Background()

	const attempts = 500
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, err := limiter.Allow(ctx, 1, engine.CategoryError)
			if err == nil && allowed {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != 100 {
		t.Errorf("%d of %d concurrent events accepted, want exactly the limit of 100", accepted, attempts)
	}
}
