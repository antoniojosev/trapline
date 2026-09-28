package sourcemap

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// mapOfSize builds a parsed map priced at roughly the requested number of
// bytes, by embedding that much source. Going through Parse rather than
// constructing a Map by hand keeps the price the cache charges the same price
// a real upload would produce.
func mapOfSize(t *testing.T, bytes int) *Map {
	t.Helper()
	document := fmt.Sprintf(`{"version":3,"sources":["a.js"],"names":[],`+
		`"sourcesContent":[%q],"mappings":"AAAA"}`, strings.Repeat("a", bytes))
	parsed, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("building a %d byte map: %v", bytes, err)
	}
	return parsed
}

func TestCacheRemembersAndReturns(t *testing.T) {
	cache := NewCache(1 << 20)
	parsed := mapOfSize(t, 1000)

	if _, known := cache.Get("a"); known {
		t.Error("an empty cache claimed to know a key")
	}

	cache.Put("a", parsed)
	got, known := cache.Get("a")
	if !known || got != parsed {
		t.Errorf("Get after Put = %v, known=%v; want the map back", got, known)
	}
}

// TestCacheRemembersMisses is what keeps an installation whose pipeline never
// uploads a map from querying the database once per event forever. The
// difference between "no map" and "no answer yet" is the whole point of the
// two return values.
func TestCacheRemembersMisses(t *testing.T) {
	cache := NewCache(1 << 20)
	cache.Put("missing", nil)

	got, known := cache.Get("missing")
	if !known {
		t.Fatal("a remembered miss was reported as unknown, so it would be looked up again")
	}
	if got != nil {
		t.Errorf("a remembered miss returned %v, want nil", got)
	}
	if cache.Bytes() <= 0 {
		t.Error("a remembered miss is charged nothing, so a flood of unknown ids grows without bound")
	}
}

// TestCacheEvictsToStayInsideItsBudget is the bound the whole design rests on.
// A cache of "some maps" whose size is chosen by whoever uploads is a memory
// leak with a nice name (ADR 018).
func TestCacheEvictsToStayInsideItsBudget(t *testing.T) {
	const budget = 50_000
	cache := NewCache(budget)

	for index := range 20 {
		cache.Put(strconv.Itoa(index), mapOfSize(t, 10_000))
		if cache.Bytes() > budget {
			t.Fatalf("after %d entries the cache holds %d bytes, budget is %d",
				index+1, cache.Bytes(), budget)
		}
	}
	if cache.Len() == 0 {
		t.Error("the cache evicted everything; it is meant to keep what fits")
	}
	if _, known := cache.Get("0"); known {
		t.Error("the oldest entry survived twenty insertions into a cache that fits four")
	}
}

// TestCacheEvictsTheLeastRecentlyUsed. Least *recently used*, not least
// recently written: a map that keeps answering frames is the one worth keeping,
// and an insertion-ordered cache would throw out the hottest bundle first.
func TestCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	cache := NewCache(30_000)
	cache.Put("hot", mapOfSize(t, 10_000))
	cache.Put("cold", mapOfSize(t, 10_000))

	if _, known := cache.Get("hot"); !known {
		t.Fatal("the entry meant to stay hot was not there to touch")
	}
	cache.Put("new", mapOfSize(t, 10_000))

	if _, known := cache.Get("hot"); !known {
		t.Error("the recently used entry was evicted")
	}
	if _, known := cache.Get("cold"); known {
		t.Error("the least recently used entry survived")
	}
}

// TestAMapLargerThanTheBudgetIsNotCached. Storing it would evict everything
// else to make room for something that cannot be kept, and the caller already
// holds the parsed map it just built.
func TestAMapLargerThanTheBudgetIsNotCached(t *testing.T) {
	cache := NewCache(5_000)
	cache.Put("kept", mapOfSize(t, 1_000))
	cache.Put("enormous", mapOfSize(t, 100_000))

	if _, known := cache.Get("enormous"); known {
		t.Error("a map larger than the whole budget was cached")
	}
	if _, known := cache.Get("kept"); !known {
		t.Error("an oversized insertion evicted an entry that fitted")
	}
}

// TestReplacingAKeyDoesNotDoubleCount. A second upload of the same debug id is
// a rebuild of the same file; if the old entry's bytes were not released the
// budget would drift up on every deploy.
func TestReplacingAKeyDoesNotDoubleCount(t *testing.T) {
	cache := NewCache(1 << 20)
	cache.Put("a", mapOfSize(t, 10_000))
	first := cache.Bytes()
	cache.Put("a", mapOfSize(t, 10_000))

	if cache.Bytes() != first {
		t.Errorf("replacing a key left %d bytes charged, was %d", cache.Bytes(), first)
	}
	if cache.Len() != 1 {
		t.Errorf("replacing a key left %d entries, want 1", cache.Len())
	}
}

// TestCacheIsSafeUnderConcurrentUse. It is read from the ingest path, which is
// several goroutines by construction, and `make check` runs the suite under
// -race precisely so this is not a hope.
func TestCacheIsSafeUnderConcurrentUse(t *testing.T) {
	cache := NewCache(200_000)
	parsed := mapOfSize(t, 1_000)

	var waiting sync.WaitGroup
	for worker := range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for round := range 200 {
				key := strconv.Itoa((worker*round)%50 + worker)
				cache.Put(key, parsed)
				cache.Get(key)
				cache.Bytes()
			}
		}()
	}
	waiting.Wait()

	if cache.Bytes() > 200_000 {
		t.Errorf("concurrent use left %d bytes charged, budget is 200000", cache.Bytes())
	}
}

// TestDefaultBudget pins the documented number, because ADR 018 states it and
// a default that drifts from its ADR is a document nobody can rely on.
func TestDefaultBudget(t *testing.T) {
	if DefaultCacheBytes != 64<<20 {
		t.Errorf("DefaultCacheBytes = %d, ADR 018 says 64 MB", DefaultCacheBytes)
	}
	cache := NewCache(0)
	if cache.maxBytes != DefaultCacheBytes {
		t.Errorf("NewCache(0) budget = %d, want the default", cache.maxBytes)
	}
}
