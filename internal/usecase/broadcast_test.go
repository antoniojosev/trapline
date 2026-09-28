package usecase

import (
	"sync"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

func TestASubscriberOnlyHearsItsOwnProject(t *testing.T) {
	feed := NewBroadcaster()

	mine, stopMine := feed.Subscribe(1)
	defer stopMine()
	theirs, stopTheirs := feed.Subscribe(2)
	defer stopTheirs()

	feed.Publish(1, &IssueEvent{Kind: IssueEventNew, Issue: domain.Issue{ID: 10}})

	select {
	case event := <-mine:
		if event.Issue.ID != 10 {
			t.Fatalf("got issue %d, want 10", event.Issue.ID)
		}
	default:
		t.Fatal("the project's own subscriber heard nothing")
	}

	select {
	case event := <-theirs:
		t.Fatalf("another project's subscriber received issue %d; a feed that "+
			"leaks across projects leaks every issue title in the installation",
			event.Issue.ID)
	default:
	}
}

func TestUnsubscribingStopsTheChannelAndIsIdempotent(t *testing.T) {
	feed := NewBroadcaster()
	events, stop := feed.Subscribe(1)

	stop()
	// A second call must not panic on the already-closed channel: the handler
	// calls it from a defer and may also have called it on an error path.
	stop()

	if _, open := <-events; open {
		t.Fatal("the channel is still open after unsubscribing")
	}
	if feed.Watching() != 0 {
		t.Fatalf("%d subscriptions left behind", feed.Watching())
	}

	// Publishing to nobody must be safe, and is the shape of every
	// installation with no panel open.
	feed.Publish(1, &IssueEvent{Kind: IssueEventNew})
}

func TestASlowSubscriberIsDroppedRatherThanWaitedFor(t *testing.T) {
	feed := NewBroadcaster()
	events, stop := feed.Subscribe(1)
	defer stop()

	// Nothing reads while this loop runs. If a send ever blocked, this test
	// would hang rather than fail — which is exactly the failure being
	// prevented, only it would be happening on the ingest path in production.
	for index := range subscriberBuffer * 3 {
		feed.Publish(1, &IssueEvent{Kind: IssueEventUpdated, Issue: domain.Issue{ID: int64(index)}})
	}

	if got := len(events); got != subscriberBuffer {
		t.Fatalf("buffered %d events, want the buffer's %d", got, subscriberBuffer)
	}
}

func TestPublishingIsSafeOnANilBroadcaster(t *testing.T) {
	var feed *Broadcaster
	// The ingest path holds one of these whether or not anything mounted a
	// feed, and it must not have to ask.
	feed.Publish(1, &IssueEvent{Kind: IssueEventNew})
	if feed.Watching() != 0 {
		t.Fatal("a nil broadcaster claims subscribers")
	}
}

func TestConcurrentSubscribersAndPublishers(t *testing.T) {
	feed := NewBroadcaster()

	var waiting sync.WaitGroup
	for range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			events, stop := feed.Subscribe(1)
			defer stop()
			for range 4 {
				feed.Publish(1, &IssueEvent{Kind: IssueEventNew})
			}
			// Draining under the race detector is the point: a send racing a
			// close is the bug this arrangement exists to make impossible.
			for range len(events) {
				<-events
			}
		}()
	}
	waiting.Wait()

	if feed.Watching() != 0 {
		t.Fatalf("%d subscriptions survived their goroutines", feed.Watching())
	}
}

// The order matters, and it is severity of news: a regression outranks
// everything, because an issue coming back is the signal the product exists to
// produce and another occurrence of it is not.
func TestWhatAnEventDidToItsIssueDecidesTheKind(t *testing.T) {
	for name, test := range map[string]struct {
		result ports.RecordEventResult
		want   IssueEventKind
	}{
		"the first occurrence":   {ports.RecordEventResult{New: true}, IssueEventNew},
		"another occurrence":     {ports.RecordEventResult{}, IssueEventUpdated},
		"a resolved issue back":  {ports.RecordEventResult{Regressed: true}, IssueEventRegressed},
		"new and regressed both": {ports.RecordEventResult{New: true, Regressed: true}, IssueEventRegressed},
	} {
		t.Run(name, func(t *testing.T) {
			if got := issueEventKind(&test.result); got != test.want {
				t.Errorf("kind = %q, want %q", got, test.want)
			}
		})
	}
}
