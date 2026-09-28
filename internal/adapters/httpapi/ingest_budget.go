package httpapi

import (
	"errors"
	"io"
	"sync"
)

// The memory an ingest request may hold, and the total every in-flight ingest
// request may hold between them.
//
// The second number is the one that was missing, and its absence is the fourth
// instance of the same mistake in this repository's history. Argon2id was
// budgeted for one hash; the zstd encoder reserved a window per core on the
// machine that measured it; `/login` had a per-attempt cost and no ceiling on
// attempts, which turned a 30 MB footprint into 1.03 GB under 200 concurrent
// logins (ADR 023). Ingest had the same shape: every per-request bound was in
// place and correct — 20 MiB per envelope, 4 MiB per item, 100 items — and
// nothing bounded how many requests could hold those bounds at once.
//
// Measured before this existed, against this branch: 128 requests at
// concurrency 32, each carrying a 25 KiB gzip bomb, took resident memory to
// 649 MB. The same flood in zstd — 1 179 bytes per request, 151 KiB of upload
// in total — reached 1.46 GB. That is a 9 900× amplification of the attacker's
// bandwidth and 48× the published footprint, from an endpoint that is public
// by design and cannot require real authentication. Quadrupling the flood
// quadrupled the figure, which is the part that matters: there was no ceiling
// to quote at all (ADR 039, docs/benchmarks/footprint.md).
const (
	// ingestArenaBytes is the whole budget, shared by every request in flight.
	//
	// It is a ceiling and not a reservation: an idle server allocates none of
	// it. What it buys is that the answer to "what does a flood cost" stops
	// being "however many attackers there are, times 20 MiB" and becomes this
	// number plus the collector's slack — which is the only form in which a
	// memory figure can honestly be published.
	//
	// 32 MiB, and the floor is not a matter of taste: maxIngestBody promises
	// that a 20 MiB envelope is accepted, so an arena that could not admit one
	// would turn a documented limit into a lie. That is 20 MiB plus room for
	// ordinary traffic to keep flowing beside it — a normal SDK envelope is a
	// few kilobytes and charges a single chunk, so the remaining 12 MiB admits
	// nearly two hundred real clients at once, far past the 100 ev/s this
	// product is designed for (ADR 001).
	//
	// With it, the same two floods peak at 121 MB and 93 MB, and quadrupling
	// either one moves the figure by 4% and 30% rather than by 400%.
	ingestArenaBytes = 32 << 20

	// ingestArenaChunk is how much a reader claims at a time.
	//
	// Fine enough that a 4 KiB envelope is charged 64 KiB rather than its
	// whole 20 MiB ceiling, coarse enough that a 20 MiB body takes 320 trips
	// through a mutex rather than five thousand.
	ingestArenaChunk = 64 << 10
)

// errIngestBusy means the shared budget is exhausted.
//
// A sentinel, because it travels out through the envelope parser — which
// wraps whatever a reader hands it — and the handler has to be able to tell it
// apart from "this body is malformed" to answer differently.
var errIngestBusy = errors.New("ingest memory budget exhausted")

// ingestArena is a counting semaphore denominated in bytes.
//
// A mutex and an integer rather than a buffered channel: the units are bytes,
// a holder takes a different amount from the one beside it, and it releases
// everything at once. A channel of tokens would make every one of those three
// things a loop.
type ingestArena struct {
	mutex     sync.Mutex
	remaining int64
	total     int64
}

func newIngestArena(total int64) *ingestArena {
	return &ingestArena{remaining: total, total: total}
}

// take removes n from the budget, or reports that it could not.
//
// It never waits. Waiting in front of a memory budget is a way of holding the
// memory anyway, just later — the same reasoning that made the authentication
// semaphore refuse rather than queue (throttle.go).
func (a *ingestArena) take(n int64) bool {
	if n <= 0 {
		return true
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if a.remaining < n {
		return false
	}
	a.remaining -= n
	return true
}

func (a *ingestArena) give(n int64) {
	if n <= 0 {
		return
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.remaining += n
	if a.remaining > a.total {
		// Only reachable from a double release, which is a bug in this file
		// rather than a runtime condition. Clamping keeps the budget from
		// growing without bound if one ever ships.
		a.remaining = a.total
	}
}

// free is what is left.
//
// Only the tests read it today. Exposing it through `doctor` or
// `GET /system/jobs` would be worth doing — an operator seeing "the ingest
// budget is at 2%" learns something no other number tells them — but it is a
// new operation and ADR 006 says a new operation lands in all four clients at
// once, which is a change of its own rather than a line here.
func (a *ingestArena) free() int64 {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.remaining
}

// arenaHold is one request's claim on the arena.
//
// Everything it took is returned by release, once, whatever happened in
// between — which is why the handler defers it rather than returning it on
// each path.
type arenaHold struct {
	arena *ingestArena
	held  int64
	once  sync.Once
}

// hold reserves a request's fixed cost — the decompressor's working set —
// before the decompressor exists.
//
// Before, not after: a zstd decoder allocates its history buffer from the
// window the *frame* declares, so by the time the first byte is readable the
// memory is already resident. A budget checked afterwards would be a budget
// checked once the damage was done.
func (a *ingestArena) hold(upfront int64) (*arenaHold, bool) {
	if !a.take(upfront) {
		return nil, false
	}
	return &arenaHold{arena: a, held: upfront}, true
}

func (h *arenaHold) release() {
	h.once.Do(func() {
		h.arena.give(h.held)
		h.held = 0
	})
}

// wrap charges the arena for what the request actually produces.
//
// The fixed cost above covers the decompressor; this covers the bytes, which
// the envelope parser retains item by item for the life of the request. A
// reader that is never read costs only the fixed cost, which is what makes a
// refused request cheap.
func (h *arenaHold) wrap(r io.Reader) io.Reader {
	return &arenaReader{r: r, hold: h}
}

type arenaReader struct {
	r    io.Reader
	hold *arenaHold
	// slack is how many already-charged bytes are still unspent.
	slack int64
}

func (a *arenaReader) Read(p []byte) (int, error) {
	if a.slack <= 0 {
		if !a.hold.arena.take(ingestArenaChunk) {
			return 0, errIngestBusy
		}
		a.hold.held += ingestArenaChunk
		a.slack = ingestArenaChunk
	}
	if int64(len(p)) > a.slack {
		p = p[:a.slack]
	}
	n, err := a.r.Read(p)
	a.slack -= int64(n)
	return n, err //nolint:wrapcheck // io.Reader contract: pass errors through untouched.
}

// Close releases the decompressor underneath, if there is one. The arena is
// not released here: the handler owns that, because the bytes stay alive in
// the parsed envelope after the body has been closed.
func (a *arenaReader) Close() error {
	if closer, ok := a.r.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			return err //nolint:wrapcheck // io.Closer contract: the decompressor's own error.
		}
	}
	return nil
}
