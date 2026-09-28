package ratelimit

import (
	"net/netip"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/clientip"
)

// IPWindow is the period a per-address ceiling is counted over.
//
// A minute, like the per-project limiter, so the two numbers an operator sees
// are in the same unit and can be compared without arithmetic.
const IPWindow = time.Minute

// IPLimiter counts requests per client address, in a map that cannot grow
// without bound.
//
// It is deliberately a second limiter rather than a mode of the one next to
// it. That one answers "is this category switched on for this project, and is
// the project over its burst threshold" — configuration and product behaviour,
// whose refusal the protocol has a header for. This one answers "has this
// address asked for too much", which is a defence, and whose refusal must not
// use that header: telling an SDK to stop sending errors for an hour because
// its address had a burst would turn a defence into data loss (ADR 023).
//
// Sharing one type would have meant one set of keys, one map to bound and one
// answer shape for two questions that only look alike.
type IPLimiter struct {
	limit      int
	window     time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	buckets map[netip.Prefix]ipBucket
}

// ipBucket is a fixed-window counter, stored by value.
//
// By value rather than behind a pointer because a flood creates entries as
// fast as it can, and one heap allocation per never-seen-again address is a
// cost the attacker chooses and this process pays.
type ipBucket struct {
	windowStart time.Time
	count       int
}

// NewIP returns a limiter allowing limit requests per address per window,
// holding at most maxEntries addresses.
//
// A limit of zero or less means no limiting at all, which exists for the
// benchmark harness: the gate measures the ingest path, and a ceiling below
// the throughput being measured would turn the gate into a measurement of this
// file. The limiter still runs, so its cost stays inside the figure.
func NewIP(limit int, window time.Duration, maxEntries int) *IPLimiter {
	if window <= 0 {
		window = IPWindow
	}
	if maxEntries <= 0 {
		maxEntries = 1
	}
	return &IPLimiter{
		limit:      limit,
		window:     window,
		maxEntries: maxEntries,
		now:        time.Now,
		buckets:    make(map[netip.Prefix]ipBucket),
	}
}

// Allow reports whether one request from addr may proceed, and if not, how
// long the caller should be told to wait.
//
// The retry hint is the remainder of the current window rounded up to a whole
// second, because Retry-After has no finer unit and rounding down would invite
// a client back before the window has actually turned over.
func (l *IPLimiter) Allow(addr netip.Addr) (allowed bool, retryAfter time.Duration) {
	if l.limit <= 0 {
		return true, 0
	}
	if !addr.IsValid() {
		// No address means no per-address decision. Refusing here would drop
		// traffic for a reason nobody can act on; the per-project limiter and
		// the body cap still apply.
		return true, 0
	}

	key := clientip.Bucket(addr)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, found := l.buckets[key]
	if !found || now.Sub(bucket.windowStart) >= l.window {
		if !found && len(l.buckets) >= l.maxEntries {
			l.evictLocked(now)
		}
		l.buckets[key] = ipBucket{windowStart: now, count: 1}
		return true, 0
	}

	if bucket.count >= l.limit {
		remaining := l.window - now.Sub(bucket.windowStart)
		return false, remaining.Round(time.Second) + time.Second
	}

	bucket.count++
	l.buckets[key] = bucket
	return true, 0
}

// evictLocked makes room for one new address.
//
// It probes a handful of entries rather than sweeping the map. A full sweep
// would be O(n) on every insert once the map is full — which is exactly the
// state an attacker rotating addresses puts it in, so the cleanup would become
// the amplification. Probing prefers an expired entry and otherwise drops an
// arbitrary one; Go randomises where a map range starts, so "arbitrary" is not
// a position an attacker can aim at.
//
// Dropping a live entry loses precision: that address starts a fresh window
// early. That is the accepted trade — precision, never stability (ADR 008) —
// and under a rotation attack the entries being dropped are overwhelmingly the
// attacker's own.
func (l *IPLimiter) evictLocked(now time.Time) {
	const probe = 8

	var (
		victim      netip.Prefix
		haveVictim  bool
		probedCount int
	)
	for key, bucket := range l.buckets {
		if now.Sub(bucket.windowStart) >= l.window {
			delete(l.buckets, key)
			return
		}
		if !haveVictim {
			victim, haveVictim = key, true
		}
		probedCount++
		if probedCount >= probe {
			break
		}
	}
	if haveVictim {
		delete(l.buckets, victim)
	}
}

// Tracked reports how many addresses the limiter is currently holding. It
// exists for the test that proves the map is bounded, and for the diagnostics
// a future `doctor` check will want.
func (l *IPLimiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Limit reports the configured ceiling, for the startup log and for tests that
// need to know how many requests it takes to reach it.
func (l *IPLimiter) Limit() int { return l.limit }
