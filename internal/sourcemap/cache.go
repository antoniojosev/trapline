package sourcemap

import (
	"container/list"
	"sync"
)

// DefaultCacheBytes is the budget a cache gets when nobody chose one.
//
// Sixty-four megabytes is two to three real bundles' worth of parsed mappings
// and embedded sources, which is enough for one deploy of one application to
// stay hot, and it sits inside the product's declared ceiling of 80 MB at peak
// rather than on top of it (ADR 018).
const DefaultCacheBytes = 64 << 20

// missBytes is what a remembered miss is charged.
//
// A miss holds no map, but it is not free and it must not be: without a price
// a flood of events naming distinct debug ids would grow the index without
// bound, which is the same unbounded map the ingest path has already been bitten
// by once. Half a kilobyte is generous for a key and a list node, and it means
// the budget caps remembered misses at roughly a hundred thousand of them.
const missBytes = 512

// Cache is a size-bounded LRU of parsed source maps.
//
// Bounded by bytes rather than by entries, because entries are not comparable:
// a map for a small vendor chunk is tens of kilobytes and one for an
// application bundle with embedded sources is tens of megabytes. A cache of
// "200 maps" is a cache whose memory use is decided by whoever uploads, which
// is exactly the shape of the leak this product has already paid for once —
// the zstd encoder that reserved a window per core and blew the 30 MB resting
// budget before a single event arrived.
//
// It remembers misses as well as hits. A miss is what an event naming a debug
// id nobody uploaded produces, and without remembering it every such event
// would go to the database again; a browser SDK sending a thousand events a
// minute from a deploy whose maps were never uploaded is the ordinary case,
// not the exotic one.
type Cache struct {
	maxBytes int64

	mu    sync.Mutex
	bytes int64
	// order is most-recently-used first, so eviction pops the back.
	order   *list.List
	entries map[string]*list.Element
}

// entry is one cached lookup. A nil parsed map is a remembered miss.
type entry struct {
	key    string
	parsed *Map
	size   int64
}

// NewCache builds a cache with a byte budget. A budget of zero or less means
// DefaultCacheBytes.
func NewCache(maxBytes int64) *Cache {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	return &Cache{
		maxBytes: maxBytes,
		order:    list.New(),
		entries:  make(map[string]*list.Element),
	}
}

// Get returns what is remembered for a key: the parsed map, whether anything
// is remembered at all, and — when something is — whether it was a miss.
//
// Three-valued rather than two, because "no map" and "no answer" lead to
// different work. The first is settled and costs nothing; the second means
// somebody has to go and read a blob out of the database.
func (c *Cache) Get(key string) (parsed *Map, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, found := c.entries[key]
	if !found {
		return nil, false
	}
	c.order.MoveToFront(element)
	held, ok := element.Value.(*entry)
	if !ok {
		return nil, false
	}
	return held.parsed, true
}

// Put remembers a parsed map under a key. A nil map remembers a miss.
//
// A map larger than the whole budget is not cached and not an error: it is
// used once by the caller that just parsed it and then dropped. Storing it
// would evict everything else to make room for something that cannot be kept.
func (c *Cache) Put(key string, parsed *Map) {
	size := int64(missBytes)
	if parsed != nil {
		size = parsed.Size() + missBytes
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, found := c.entries[key]; found {
		if held, ok := existing.Value.(*entry); ok {
			c.bytes -= held.size
		}
		c.order.Remove(existing)
		delete(c.entries, key)
	}
	if size > c.maxBytes {
		return
	}

	c.entries[key] = c.order.PushFront(&entry{key: key, parsed: parsed, size: size})
	c.bytes += size

	for c.bytes > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			// Cannot happen while bytes is positive, and if it somehow did,
			// looping forever with the lock held would take the server down
			// rather than merely use too much memory.
			c.bytes = 0
			break
		}
		evicted, ok := c.order.Remove(oldest).(*entry)
		if !ok {
			// Nothing else puts anything in this list, so this cannot
			// happen; dropping the entry rather than panicking keeps a
			// corrupted cache from taking the server with it.
			continue
		}
		delete(c.entries, evicted.key)
		c.bytes -= evicted.size
	}
}

// Bytes is how much the cache is currently holding, by its own accounting.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Len is how many keys the cache remembers, misses included.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
