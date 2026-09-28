package usecase

import (
	"sync"
	"sync/atomic"

	"github.com/antoniojosev/trapline/internal/domain"
)

// IssueEventKind names what happened to an issue.
//
// The three are the whole vocabulary of the live feed, and they are the same
// three sentences the panel already writes on a row: this is new, this came
// back, this happened again. A fourth would need a reader who acts on it
// differently, and there is none.
type IssueEventKind string

// The kinds a live subscriber can receive.
const (
	// IssueEventNew is an issue whose first event just arrived.
	IssueEventNew IssueEventKind = "issue.new"
	// IssueEventRegressed is a resolved issue that a new event reopened —
	// the single most valuable signal the product produces, and the reason
	// this feed exists at all rather than a polling interval.
	IssueEventRegressed IssueEventKind = "issue.regressed"
	// IssueEventUpdated is another occurrence of something already open.
	IssueEventUpdated IssueEventKind = "issue.updated"
)

// IssueEvent is one thing that happened, addressed to whoever is watching the
// project it happened in.
type IssueEvent struct {
	Kind  IssueEventKind
	Issue domain.Issue
}

// subscriberBuffer is how far behind a reader may fall before it starts
// losing events.
//
// Sixty-four is roughly a second of a busy project at the throughput the
// product promises, which is far more slack than a browser needs and far less
// memory than a queue that never drops. What it buys is the guarantee below:
// ingestion never waits for a reader.
const subscriberBuffer = 64

// Broadcaster fans issue lifecycle events out to whoever is watching a
// project right now.
//
// Two properties decide the design, and both point the same way:
//
//   - **A subscriber must never be able to slow ingestion down.** A browser on
//     a train, a laptop that went to sleep with a tab open — these are normal,
//     and a send that blocked on one of them would hold the ingest path open
//     behind it. So the sends are non-blocking and a full buffer drops.
//     Dropping is safe because of what the panel does with these: the banner
//     is a prompt to refetch, and the refetch reads the database, so the worst
//     a dropped event costs is a number in the banner being low.
//
//   - **Nothing is persisted.** This is a notification that something changed,
//     not a record of it — the record is the issue row, and it is already
//     written by the time anything is published here. A subscriber that was
//     not connected missed nothing it cannot read back.
type Broadcaster struct {
	mu          sync.RWMutex
	next        int64
	subscribers map[int64]subscriber

	// watching is the subscriber count, kept outside the mutex so the common
	// case — an installation nobody has a panel open on — costs one atomic
	// load per ingested event instead of a lock.
	watching atomic.Int64
}

type subscriber struct {
	projectID int64
	events    chan IssueEvent
}

// NewBroadcaster returns a broadcaster with no subscribers.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subscribers: map[int64]subscriber{}}
}

// Subscribe returns a channel of the events for one project and the function
// that stops it.
//
// The cancel function is idempotent and must be called: it is what closes the
// channel and removes the subscription, and a caller that forgets leaks both.
func (b *Broadcaster) Subscribe(projectID int64) (events <-chan IssueEvent, cancel func()) {
	b.mu.Lock()
	b.next++
	id := b.next
	channel := make(chan IssueEvent, subscriberBuffer)
	b.subscribers[id] = subscriber{projectID: projectID, events: channel}
	b.watching.Store(int64(len(b.subscribers)))
	b.mu.Unlock()

	var once sync.Once
	return channel, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, id)
			b.watching.Store(int64(len(b.subscribers)))
			b.mu.Unlock()
			// Closed after it is unreachable from Publish, under the same
			// lock discipline, so a send can never race with the close.
			close(channel)
		})
	}
}

// Publish delivers an event to every subscriber watching its project.
//
// Safe on a nil receiver, because a stack assembled without a feed is a
// perfectly good stack: the caller on the ingest path should not have to ask
// whether anybody is listening.
//
// The event is taken by pointer and copied once per subscriber. It carries a
// whole domain.Issue, and this runs on the ingest path: a value parameter
// would copy it again on every call, including the overwhelmingly common one
// where there are no subscribers and the copy is discarded immediately.
func (b *Broadcaster) Publish(projectID int64, event *IssueEvent) {
	if b == nil || b.watching.Load() == 0 {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subscribers {
		if sub.projectID != projectID {
			continue
		}
		select {
		case sub.events <- *event:
		default:
			// The reader is behind. Dropping is deliberate: see the type's
			// documentation. Not logged, because a slow reader on a busy
			// project would write a line per event.
		}
	}
}

// Watching is how many subscriptions are open. It exists for the tests and
// for the diagnostics, not for the hot path, which reads the counter directly.
func (b *Broadcaster) Watching() int {
	if b == nil {
		return 0
	}
	return int(b.watching.Load())
}
