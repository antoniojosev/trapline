package ratelimit

import (
	"fmt"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func newIPLimiter(limit, maxEntries int) (*IPLimiter, *time.Time) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	limiter := NewIP(limit, IPWindow, maxEntries)
	limiter.now = func() time.Time { return now }
	return limiter, &now
}

func mustAddr(t *testing.T, raw string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return addr
}

func TestAnAddressIsRefusedOnceItPassesItsCeiling(t *testing.T) {
	limiter, _ := newIPLimiter(3, 16)
	addr := mustAddr(t, "203.0.113.9")

	for attempt := 1; attempt <= 3; attempt++ {
		allowed, retryAfter := limiter.Allow(addr)
		if !allowed {
			t.Fatalf("attempt %d refused below the ceiling", attempt)
		}
		if retryAfter != 0 {
			t.Errorf("attempt %d was allowed but asked the caller to wait %s", attempt, retryAfter)
		}
	}

	allowed, retryAfter := limiter.Allow(addr)
	if allowed {
		t.Fatal("the fourth request passed a ceiling of three")
	}
	// A Retry-After of zero tells a client to come straight back, which is
	// the one answer that makes a limit worse than none.
	if retryAfter <= 0 {
		t.Errorf("Retry-After = %s, want a positive wait", retryAfter)
	}
	if retryAfter > IPWindow+time.Second {
		t.Errorf("Retry-After = %s, longer than the window itself", retryAfter)
	}
}

func TestTheWindowTurnsOver(t *testing.T) {
	limiter, now := newIPLimiter(1, 16)
	addr := mustAddr(t, "203.0.113.9")

	if allowed, _ := limiter.Allow(addr); !allowed {
		t.Fatal("the first request was refused")
	}
	if allowed, _ := limiter.Allow(addr); allowed {
		t.Fatal("the second request passed a ceiling of one")
	}

	*now = now.Add(IPWindow)
	if allowed, _ := limiter.Allow(addr); !allowed {
		t.Error("the window did not turn over; a limit that never resets is a ban")
	}
	if tracked := limiter.Tracked(); tracked != 1 {
		t.Errorf("Tracked = %d after a window turned over on one address, want 1", tracked)
	}
}

func TestAddressesAreCountedSeparatelyButNotWithinAnIPv6Subnet(t *testing.T) {
	limiter, _ := newIPLimiter(1, 16)

	if allowed, _ := limiter.Allow(mustAddr(t, "203.0.113.9")); !allowed {
		t.Fatal("the first address was refused")
	}
	if allowed, _ := limiter.Allow(mustAddr(t, "203.0.113.10")); !allowed {
		t.Error("a second, unrelated address was refused; the counter is not per address")
	}

	// Rotating inside a /64 costs an attacker nothing, so it must buy them
	// nothing either.
	if allowed, _ := limiter.Allow(mustAddr(t, "2001:db8:1:2::1")); !allowed {
		t.Fatal("the first v6 address was refused")
	}
	if allowed, _ := limiter.Allow(mustAddr(t, "2001:db8:1:2::dead:beef")); allowed {
		t.Error("a second address from the same /64 got its own budget")
	}
	if allowed, _ := limiter.Allow(mustAddr(t, "2001:db8:1:3::1")); !allowed {
		t.Error("a different /64 was refused someone else's budget")
	}
}

func TestAnInvalidAddressIsNotRefused(t *testing.T) {
	limiter, _ := newIPLimiter(1, 16)
	for attempt := 0; attempt < 5; attempt++ {
		if allowed, _ := limiter.Allow(netip.Addr{}); !allowed {
			t.Fatal("a request whose address could not be determined was refused; " +
				"that drops traffic for a reason nobody can act on")
		}
	}
	if tracked := limiter.Tracked(); tracked != 0 {
		t.Errorf("Tracked = %d; an address that does not exist should not occupy an entry", tracked)
	}
}

func TestAZeroLimitDisablesTheCeilingWithoutRemovingTheLimiter(t *testing.T) {
	// The benchmark harness runs this way: the limiter still executes, so its
	// cost stays inside the throughput figure, but it never becomes the thing
	// the gate is measuring.
	limiter, _ := newIPLimiter(0, 16)
	addr := mustAddr(t, "203.0.113.9")
	for attempt := 0; attempt < 1000; attempt++ {
		if allowed, _ := limiter.Allow(addr); !allowed {
			t.Fatalf("attempt %d refused by a limiter with no ceiling", attempt)
		}
	}
	if tracked := limiter.Tracked(); tracked != 0 {
		t.Errorf("Tracked = %d; a disabled limiter should not be building a map", tracked)
	}
}

func TestTheMapDoesNotGrowWithTheAttackersImagination(t *testing.T) {
	// This is the test the limiter exists to survive. A map keyed by address
	// is unbounded by construction, and inside one IPv6 /64 an attacker gets
	// 2^64 keys for free — so the defence, left alone, is a cheaper way to
	// exhaust this process than the endpoint it protects.
	const (
		maxEntries = 512
		addresses  = 50_000
	)

	limiter, _ := newIPLimiter(10, maxEntries)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for index := range addresses {
		// A different /64 every time: the worst case, since anything narrower
		// would collapse into a key the limiter has already seen.
		addr := mustAddr(t, fmt.Sprintf("2001:db8:%x:%x::1", index>>16, index&0xffff))
		if allowed, _ := limiter.Allow(addr); !allowed {
			t.Fatalf("address %d was refused on its first request", index)
		}
		if tracked := limiter.Tracked(); tracked > maxEntries {
			t.Fatalf("after %d addresses the map holds %d entries, over the bound of %d",
				index+1, tracked, maxEntries)
		}
	}

	if tracked := limiter.Tracked(); tracked != maxEntries {
		t.Errorf("Tracked = %d after %d addresses, want the map full at %d",
			tracked, addresses, maxEntries)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	// A generous ceiling: the point is the shape of the growth, not a precise
	// figure. 50 000 unbounded entries would be megabytes; a bounded 512 is
	// tens of kilobytes plus whatever the test's own address parsing left
	// behind.
	const budget = 1 << 20
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > budget {
		t.Errorf("the heap grew %d bytes over %d distinct addresses with a bound of %d entries; "+
			"the map is not being evicted", grew, addresses, maxEntries)
	}

	// And it must still be limiting. An eviction policy that quietly stopped
	// counting would pass every assertion above.
	addr := mustAddr(t, "203.0.113.9")
	for attempt := 0; attempt < 10; attempt++ {
		if allowed, _ := limiter.Allow(addr); !allowed {
			t.Fatalf("attempt %d refused below the ceiling on a full map", attempt)
		}
	}
	if allowed, _ := limiter.Allow(addr); allowed {
		t.Error("a full map stopped enforcing the ceiling; precision may be sacrificed, enforcement may not")
	}
}

func TestExpiredEntriesAreThePreferredVictims(t *testing.T) {
	limiter, now := newIPLimiter(1, 4)

	for index := range 4 {
		limiter.Allow(mustAddr(t, fmt.Sprintf("203.0.113.%d", index)))
	}
	if tracked := limiter.Tracked(); tracked != 4 {
		t.Fatalf("Tracked = %d, want the map full at 4", tracked)
	}

	// Let every window lapse, then admit one new address. The eviction should
	// find an expired entry rather than throw out a live one.
	*now = now.Add(2 * IPWindow)
	if allowed, _ := limiter.Allow(mustAddr(t, "198.51.100.1")); !allowed {
		t.Fatal("a new address was refused on its first request")
	}
	if tracked := limiter.Tracked(); tracked > 4 {
		t.Errorf("Tracked = %d, over the bound", tracked)
	}
}

func TestConcurrentCallersShareOneCeiling(t *testing.T) {
	// The whole reason this is a lock and not an atomic per bucket: a flood is
	// concurrent by definition, and a limiter that only holds under sequential
	// access is not a limiter. Run under -race in `make check`.
	const (
		limit    = 50
		attempts = 400
	)
	limiter := NewIP(limit, IPWindow, 16)
	addr := netip.MustParseAddr("203.0.113.9")

	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if ok, _ := limiter.Allow(addr); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()

	if allowed != limit {
		t.Errorf("%d of %d concurrent requests were allowed, want exactly the ceiling of %d",
			allowed, attempts, limit)
	}
}

func TestTheShippedDefaultsAreWhatTheLimiterIsBuiltWith(t *testing.T) {
	// Cheap, and it catches the version of this change where the constants
	// were written, reasoned about at length, and then not used.
	ingest := NewIP(domain.DefaultIngestIPRateLimitPerMinute, IPWindow, domain.MaxIngestIPEntries)
	if ingest.Limit() != domain.DefaultIngestIPRateLimitPerMinute {
		t.Errorf("ingest limiter ceiling = %d, want %d", ingest.Limit(), domain.DefaultIngestIPRateLimitPerMinute)
	}
	auth := NewIP(domain.DefaultAuthRateLimitPerMinute, IPWindow, domain.MaxAuthIPEntries)
	if auth.Limit() != domain.DefaultAuthRateLimitPerMinute {
		t.Errorf("auth limiter ceiling = %d, want %d", auth.Limit(), domain.DefaultAuthRateLimitPerMinute)
	}
}

func TestNewIPRepairsNonsenseArguments(t *testing.T) {
	limiter := NewIP(1, 0, 0)
	if limiter.window != IPWindow {
		t.Errorf("window = %s with a zero argument, want the default %s", limiter.window, IPWindow)
	}
	if limiter.maxEntries < 1 {
		t.Errorf("maxEntries = %d, want at least one entry so the limiter can count at all", limiter.maxEntries)
	}
}
