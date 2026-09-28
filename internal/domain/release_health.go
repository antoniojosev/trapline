package domain

import (
	"fmt"
	"strings"
	"time"
)

// DefaultSessionWindowSize is how many sessions may be in flight at once.
//
// Fifty thousand, and the number is a memory budget rather than a guess about
// traffic: an entry is four short strings and three machine words, so a full
// window is a few megabytes — affordable inside the 30 MB resting footprint
// this product promises, and reached only by an installation receiving tens of
// sessions per second with none of them ending.
//
// Past it precision degrades and stability does not (ADR 008). That direction
// is the decision: the alternative is a map that grows with whatever a public
// endpoint sends it, which is not a degraded crash-free rate but a process
// that dies during the incident it was bought to report on.
const DefaultSessionWindowSize = 50_000

// SessionTTL is how long a session may go without an update before the window
// gives up waiting for its ending.
//
// An hour, which is longer than any healthy session takes to report its end
// and short enough that a client that vanished — a killed process, a closed
// laptop, a mobile app the OS reclaimed — is counted rather than remembered
// forever. What is counted at that point is what is known: it started, and it
// did not say how it ended.
const SessionTTL = time.Hour

// SessionStatus is the state an SDK reports a session in.
//
// Four values, which is the whole protocol vocabulary. `errored` is
// deliberately absent: the wire carries an error *count*, not an error status,
// and inventing a fifth status here would make the decoder accept something no
// SDK sends.
type SessionStatus string

// The four session states.
const (
	// SessionOK is a session still running. Not terminal: it is what an
	// `init` update says, and what a heartbeat repeats.
	SessionOK SessionStatus = "ok"
	// SessionExited is a session that ended normally.
	SessionExited SessionStatus = "exited"
	// SessionCrashed is a session that ended in an unhandled error. The
	// numerator of the crash-free rate, and the reason this subsystem exists.
	SessionCrashed SessionStatus = "crashed"
	// SessionAbnormal is a session that stopped without saying how — a killed
	// process, a tab closed mid-flight. Kept apart from crashed because
	// counting a force-quit as a crash is how a crash-free rate stops meaning
	// anything.
	SessionAbnormal SessionStatus = "abnormal"
)

// ParseSessionStatus reads a status off the wire.
//
// Anything unrecognised is `ok`, never an error. A session update is not a
// thing a user reads; it is one tick of a counter, and refusing the whole
// update because a future SDK spelled its status differently would put a hole
// in a release's numbers for a field this product barely uses (ADR 002).
func ParseSessionStatus(raw string) SessionStatus {
	switch SessionStatus(strings.ToLower(strings.TrimSpace(raw))) {
	case SessionExited:
		return SessionExited
	case SessionCrashed:
		return SessionCrashed
	case SessionAbnormal:
		return SessionAbnormal
	default:
		return SessionOK
	}
}

// Terminal reports whether this status ends a session.
func (s SessionStatus) Terminal() bool {
	return s == SessionExited || s == SessionCrashed || s == SessionAbnormal
}

// rank orders the statuses by how bad they are, so that two updates about one
// session merge without depending on which arrived first.
//
// Updates do arrive out of order — an SDK batches, a queue reorders, a mobile
// client flushes yesterday's buffer — and a session that both crashed and
// exited crashed. Taking the last update to arrive would make the crash-free
// rate depend on network timing, which is the kind of number that is different
// every time somebody reloads.
func (s SessionStatus) rank() int {
	switch s {
	case SessionCrashed:
		return 3
	case SessionAbnormal:
		return 2
	case SessionExited:
		return 1
	default:
		return 0
	}
}

// SessionUpdate is one thing an SDK said about one session.
//
// Not a session: an update of one. That distinction is the reason this
// subsystem has a window at all — the SDK sends an `init` and, later, an
// ending, and knowing those are the same session is state (ADR 008).
type SessionUpdate struct {
	// ID is the session identity, unique within a project.
	ID        string
	ProjectID int64
	// Release is required. A session that cannot be attributed to a release
	// answers no question this table exists for.
	Release string
	// Environment is kept as sent, empty included.
	Environment string
	// Started is when the session began, and it decides the bucket — not when
	// this update arrived. A session that began at 13:58 and crashed at 14:03
	// is a fact about 13:00's release, not about 14:00's.
	Started time.Time
	Status  SessionStatus
	// Errors is how many errors the session has reported so far. Any number
	// above zero makes it errored, once, however many arrive.
	Errors int64
}

// Validate rejects an update that cannot be counted.
func (u SessionUpdate) Validate() error {
	switch {
	case strings.TrimSpace(u.ID) == "":
		return fmt.Errorf("%w: a session update needs a session id", ErrInvalidSessionUpdate)
	case u.ProjectID <= 0:
		return fmt.Errorf("%w: a session update needs a project", ErrInvalidSessionUpdate)
	case strings.TrimSpace(u.Release) == "":
		return fmt.Errorf("%w: a session update needs a release", ErrInvalidSessionUpdate)
	case u.Started.IsZero():
		return fmt.Errorf("%w: a session update needs a start time", ErrInvalidSessionUpdate)
	}
	return nil
}

// HealthKey is one row of session_hourly: whose sessions, from which release,
// in which environment, in which hour.
type HealthKey struct {
	ProjectID   int64
	Release     string
	Environment string
	Hour        string
}

// SessionCounts are the four disjoint counters of one bucket.
//
// Disjoint, exactly as the protocol's own aggregate item is: a session is
// counted once, under the worst thing that happened to it. Overlapping
// counters would allow `crashed + errored > started`, which is a number nobody
// can interpret.
type SessionCounts struct {
	Started  int64 `json:"started"`
	Errored  int64 `json:"errored"`
	Crashed  int64 `json:"crashed"`
	Abnormal int64 `json:"abnormal"`
}

// Add sums two buckets.
func (c SessionCounts) Add(other SessionCounts) SessionCounts {
	return SessionCounts{
		Started:  c.Started + other.Started,
		Errored:  c.Errored + other.Errored,
		Crashed:  c.Crashed + other.Crashed,
		Abnormal: c.Abnormal + other.Abnormal,
	}
}

// Empty reports whether there is nothing here worth writing.
func (c SessionCounts) Empty() bool {
	return c.Started == 0 && c.Errored == 0 && c.Crashed == 0 && c.Abnormal == 0
}

// Healthy is the sessions that ended with nothing wrong.
func (c SessionCounts) Healthy() int64 {
	healthy := c.Started - c.Errored - c.Crashed - c.Abnormal
	if healthy < 0 {
		// Only reachable from counters written by an older build or edited by
		// hand. Reporting a negative count of healthy sessions would be worse
		// than reporting none.
		return 0
	}
	return healthy
}

// CrashFreeRate is the one number this whole subsystem exists to produce, and
// whether it means anything.
//
// The second return is not politeness. A bucket with no sessions has no
// crash-free rate, and reporting 100% for it would tell a reader their release
// is perfect at the exact moment the truth is that nothing has reported in —
// which is the more alarming of the two situations and the one that would be
// hidden.
func (c SessionCounts) CrashFreeRate() (rate float64, known bool) {
	if c.Started <= 0 {
		return 0, false
	}
	return 1 - float64(c.Crashed)/float64(c.Started), true
}

// SessionWindow is the bounded in-memory state that turns a stream of session
// updates into counters (ADR 008).
//
// It is a plain data structure with no clock, no lock and no storage: every
// method takes the time it needs as an argument, and the caller owns both the
// mutex and the writing. That is what makes the awkward parts — the LRU, the
// TTL, the merge of two updates about one session — testable without a
// database and without waiting for anything.
//
// The bound is a hard ceiling on entries, and reaching it costs precision
// rather than stability: the least recently touched session is settled with
// what is known about it and evicted. An unbounded map here would be a public
// endpoint deciding this process's memory.
type SessionWindow struct {
	capacity int
	ttl      time.Duration

	entries map[sessionRef]*windowEntry
	// The LRU list, newest at head. An intrusive list rather than a heap or a
	// timestamp scan: eviction has to be O(1) on the ingest path, which is
	// where this is called from once per session update.
	head, tail *windowEntry

	// pending is what has been settled but not yet written down. It is
	// deliberately a second map rather than a write per settlement: a session
	// ending is a hot-path event and a per-session write would be a row per
	// session with extra steps.
	pending map[HealthKey]SessionCounts

	settled int64
	evicted int64
	expired int64
}

// sessionRef identifies a session. Scoped by project, because a session id is
// chosen by an SDK and two projects choosing the same one is not a collision
// anybody should have to think about.
type sessionRef struct {
	projectID int64
	id        string
}

type windowEntry struct {
	ref      sessionRef
	key      HealthKey
	status   SessionStatus
	errored  bool
	lastSeen time.Time

	prev, next *windowEntry
}

// NewSessionWindow builds a window. A capacity or TTL that is not positive
// falls back to the default, so a misconfigured installation gets the bounded
// behaviour rather than an unbounded one.
func NewSessionWindow(capacity int, ttl time.Duration) *SessionWindow {
	if capacity <= 0 {
		capacity = DefaultSessionWindowSize
	}
	if ttl <= 0 {
		ttl = SessionTTL
	}
	return &SessionWindow{
		capacity: capacity,
		ttl:      ttl,
		entries:  make(map[sessionRef]*windowEntry),
		pending:  make(map[HealthKey]SessionCounts),
	}
}

// Capacity is the ceiling on sessions in flight.
func (w *SessionWindow) Capacity() int { return w.capacity }

// TTL is how long a silent session is remembered.
func (w *SessionWindow) TTL() time.Duration { return w.ttl }

// InFlight is how many sessions are being tracked right now.
func (w *SessionWindow) InFlight() int { return len(w.entries) }

// Evicted is how many sessions were settled early because the window was
// full. A non-zero value is the measurable form of "precision was sacrificed",
// and is reported rather than merely logged so that an operator can see it
// without going through a log file (ADR 008).
func (w *SessionWindow) Evicted() int64 { return w.evicted }

// Expired is how many sessions were settled because they went quiet for
// longer than the TTL.
func (w *SessionWindow) Expired() int64 { return w.expired }

// Settled is how many sessions have reached a verdict, ever.
func (w *SessionWindow) Settled() int64 { return w.settled }

// Observe folds one update into the window.
//
// A terminal update settles the session immediately: its verdict is known and
// nothing further can change it, so keeping the entry around would be paying
// memory for a fact already decided.
func (w *SessionWindow) Observe(update SessionUpdate, now time.Time) error {
	if err := update.Validate(); err != nil {
		return err
	}

	ref := sessionRef{projectID: update.ProjectID, id: update.ID}
	entry, found := w.entries[ref]
	if !found {
		entry = &windowEntry{
			ref: ref,
			key: HealthKey{
				ProjectID:   update.ProjectID,
				Release:     update.Release,
				Environment: update.Environment,
				Hour:        HourBucket(update.Started),
			},
		}
		w.entries[ref] = entry
		w.pushFront(entry)
	}

	if update.Status.rank() > entry.status.rank() {
		entry.status = update.Status
	}
	if update.Errors > 0 {
		entry.errored = true
	}
	entry.lastSeen = now
	w.touch(entry)

	if entry.status.Terminal() {
		w.settle(entry)
		return nil
	}

	// Only after the insert, so the entry that arrived is never the one thrown
	// away: a window at capacity that discarded the newest update would stop
	// counting new sessions entirely under load, which is the opposite of
	// degrading gracefully.
	for len(w.entries) > w.capacity {
		w.settle(w.tail)
		w.evicted++
	}
	return nil
}

// AddAggregate folds a `sessions` item — counts an SDK already added up
// itself — straight into the pending counters.
//
// It never touches the window, and that is not an optimisation: an aggregate
// carries no session ids at all, so there is nothing to deduplicate and no
// ending to wait for. It is already the thing the window exists to produce.
func (w *SessionWindow) AddAggregate(key HealthKey, counts SessionCounts) {
	if counts.Empty() {
		return
	}
	w.AddPending(key, counts)
	w.settled += counts.Started
}

// AddPending merges counts straight into what is waiting to be written.
//
// It exists for the one caller that already had a verdict and could not store
// it: a flush whose write failed puts its buckets back rather than dropping
// them, so a disk that was full for a minute costs a minute of latency in a
// chart instead of a hole in a release's numbers. The counters are
// commutative, so the next flush writes the sum and the row ends up the same.
func (w *SessionWindow) AddPending(key HealthKey, counts SessionCounts) {
	if counts.Empty() {
		return
	}
	w.pending[key] = w.pending[key].Add(counts)
}

// Expire settles every session that has gone quiet for longer than the TTL,
// and reports how many.
//
// A sweep rather than a timer per session: the window is walked once a minute
// by the flush, and fifty thousand map entries is microseconds. A heap ordered
// by deadline would be less work asymptotically and more state to get wrong.
func (w *SessionWindow) Expire(now time.Time) int {
	cutoff := now.Add(-w.ttl)
	count := 0
	for _, entry := range w.entries {
		if entry.lastSeen.After(cutoff) {
			continue
		}
		w.settle(entry)
		w.expired++
		count++
	}
	return count
}

// Drain hands over everything settled so far and empties the pending map.
//
// The sessions still in flight stay in flight: they have not reached a verdict
// and writing them now would count them as started twice, once here and once
// when they end.
func (w *SessionWindow) Drain() map[HealthKey]SessionCounts {
	if len(w.pending) == 0 {
		return nil
	}
	drained := w.pending
	w.pending = make(map[HealthKey]SessionCounts, len(drained))
	return drained
}

// Pending is how many buckets are waiting to be written.
func (w *SessionWindow) Pending() int { return len(w.pending) }

// settle records a session's verdict and removes it from the window.
func (w *SessionWindow) settle(entry *windowEntry) {
	if entry == nil {
		return
	}
	counts := SessionCounts{Started: 1}
	switch {
	case entry.status == SessionCrashed:
		counts.Crashed = 1
	case entry.status == SessionAbnormal:
		counts.Abnormal = 1
	case entry.errored:
		counts.Errored = 1
	}

	w.pending[entry.key] = w.pending[entry.key].Add(counts)
	w.settled++

	delete(w.entries, entry.ref)
	w.unlink(entry)
}

// pushFront puts an entry at the head of the LRU list.
func (w *SessionWindow) pushFront(entry *windowEntry) {
	entry.prev = nil
	entry.next = w.head
	if w.head != nil {
		w.head.prev = entry
	}
	w.head = entry
	if w.tail == nil {
		w.tail = entry
	}
}

// touch moves an entry back to the head.
func (w *SessionWindow) touch(entry *windowEntry) {
	if w.head == entry {
		return
	}
	w.unlink(entry)
	w.pushFront(entry)
}

// unlink removes an entry from the LRU list, leaving it detached.
func (w *SessionWindow) unlink(entry *windowEntry) {
	if entry.prev != nil {
		entry.prev.next = entry.next
	}
	if entry.next != nil {
		entry.next.prev = entry.prev
	}
	if w.head == entry {
		w.head = entry.next
	}
	if w.tail == entry {
		w.tail = entry.prev
	}
	entry.prev, entry.next = nil, nil
}
