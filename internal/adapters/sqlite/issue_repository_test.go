package sqlite

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

func newIssueRepo(t *testing.T) (repo *IssueRepository, projectID int64) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	return NewIssueRepository(db), project.ID
}

func recordInput(projectID int64, fingerprint string, at time.Time) *ports.RecordEventInput {
	return &ports.RecordEventInput{
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Observation: domain.Observation{
			At:      at,
			Level:   domain.LevelError,
			Title:   "ValueError: invalid amount",
			Culprit: "myapp.views in checkout",
			Release: "v1.0.0",
		},
		EventID:     "9ec79c33ec9942ab8353589fcb2e04dc",
		ReceivedAt:  at,
		Environment: "production",
		Message:     "invalid amount",
		Tags:        map[string]string{"server": "web-01", "browser": "firefox"},
		Payload:     []byte(`{"exception":{"values":[{"type":"ValueError"}]}}`),
	}
}

func TestFirstEventCreatesTheIssue(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	result, err := repo.RecordEvent(ctx, recordInput(projectID, "abc123", testNow))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	if !result.New {
		t.Error("the first event did not report creating the issue")
	}
	if result.Regressed {
		t.Error("a brand new issue reported a regression")
	}
	if result.Issue.ID <= 0 || result.Issue.Times != 1 {
		t.Errorf("issue = %+v", result.Issue)
	}
	if result.Issue.GroupingVersion != domain.GroupingVersion {
		t.Errorf("GroupingVersion = %d", result.Issue.GroupingVersion)
	}
}

func TestRepeatedEventsAccumulateIntoOneIssue(t *testing.T) {
	// This is the product working: ten thousand occurrences are one issue.
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	for i := range 5 {
		result, err := repo.RecordEvent(ctx, recordInput(projectID, "abc123", testNow.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatalf("recording %d: %v", i, err)
		}
		if i > 0 && result.New {
			t.Errorf("event %d created a second issue", i)
		}
	}

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(page.Issues))
	}
	if page.Issues[0].Times != 5 {
		t.Errorf("Times = %d, want 5", page.Issues[0].Times)
	}
	if !page.Issues[0].LastSeen.Equal(testNow.Add(4 * time.Minute)) {
		t.Errorf("LastSeen = %v", page.Issues[0].LastSeen)
	}
}

func TestDifferentFingerprintsAreDifferentIssues(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	for _, fingerprint := range []string{"abc", "def"} {
		if _, err := repo.RecordEvent(ctx, recordInput(projectID, fingerprint, testNow)); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Issues) != 2 {
		t.Errorf("got %d issues, want 2", len(page.Issues))
	}
}

func TestAnEventOnAResolvedIssueRegresses(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	first, err := repo.RecordEvent(ctx, recordInput(projectID, "abc123", testNow))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	if err := repo.SetStatus(ctx, projectID, first.Issue.ID, domain.StatusResolved); err != nil {
		t.Fatalf("resolving: %v", err)
	}

	result, err := repo.RecordEvent(ctx, recordInput(projectID, "abc123", testNow.Add(time.Hour)))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	if !result.Regressed {
		t.Error("an event on a resolved issue did not report a regression")
	}
	if result.Issue.Status != domain.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", result.Issue.Status)
	}
}

func TestConcurrentEventsOfTheSameErrorProduceOneIssue(t *testing.T) {
	// A read-then-write outside a transaction would duplicate the issue under
	// load, and a duplicated issue is a split counter — the number the product
	// exists to give, silently wrong.
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	const events = 25
	var wg sync.WaitGroup
	errs := make(chan error, events)

	for i := range events {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := repo.RecordEvent(ctx, recordInput(projectID, "concurrent", testNow.Add(time.Duration(i)*time.Second))); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("recording concurrently: %v", err)
	}

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("got %d issues from concurrent events of one error, want 1", len(page.Issues))
	}
	if page.Issues[0].Times != events {
		t.Errorf("Times = %d, want %d — a counter was lost to a race", page.Issues[0].Times, events)
	}
}

func TestListFilters(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	first, err := repo.RecordEvent(ctx, recordInput(projectID, "abc", testNow))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	second := recordInput(projectID, "def", testNow.Add(time.Minute))
	second.Observation.Title = "TypeError: not callable"
	if _, err := repo.RecordEvent(ctx, second); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if err := repo.SetStatus(ctx, projectID, first.Issue.ID, domain.StatusResolved); err != nil {
		t.Fatalf("resolving: %v", err)
	}

	t.Run("by status", func(t *testing.T) {
		page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Status: domain.StatusUnresolved})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 1 || page.Issues[0].Title != "TypeError: not callable" {
			t.Errorf("issues = %+v", page.Issues)
		}
	})

	t.Run("by title, case-insensitively", func(t *testing.T) {
		page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Query: "VALUEerror"})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 1 || page.Issues[0].Title != "ValueError: invalid amount" {
			t.Errorf("issues = %+v", page.Issues)
		}
	})

	t.Run("by culprit", func(t *testing.T) {
		// Searching for a filename is what people actually do, and the
		// filename lives in the culprit rather than the title.
		page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Query: "views"})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 2 {
			t.Errorf("got %d issues searching the culprit, want both", len(page.Issues))
		}
	})

	t.Run("newest first", func(t *testing.T) {
		page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if len(page.Issues) != 2 || page.Issues[0].Fingerprint != "def" {
			t.Errorf("order = %+v", page.Issues)
		}
	})
}

func TestIssuesAreScopedToTheirProject(t *testing.T) {
	// A query that can return another project's issue is one refactor away
	// from being an authorisation bug.
	db := openTemp(t)
	projects := NewProjectRepository(db)
	mine, _ := createProject(t, projects, "mine")
	theirs, _ := createProject(t, projects, "theirs")
	repo := NewIssueRepository(db)
	ctx := context.Background()

	result, err := repo.RecordEvent(ctx, recordInput(mine.ID, "abc", testNow))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	if _, err := repo.FindByID(ctx, theirs.ID, result.Issue.ID); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Errorf("another project could read the issue: %v", err)
	}
	if err := repo.SetStatus(ctx, theirs.ID, result.Issue.ID, domain.StatusResolved); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Errorf("another project could resolve the issue: %v", err)
	}
}

func TestEventPayloadRoundTrips(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	input := recordInput(projectID, "abc", testNow)
	result, err := repo.RecordEvent(ctx, input)
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	events, err := repo.LatestEvents(ctx, result.Issue.ID, 10)
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	// The whole payload is kept so the detail view and the issue bundle have
	// everything; the columns beside it are what queries actually read.
	if !bytes.Equal(events[0].Payload, input.Payload) {
		t.Errorf("payload changed in storage: %q", events[0].Payload)
	}
	if events[0].Environment != "production" || events[0].Release != "v1.0.0" {
		t.Errorf("event = %+v", events[0])
	}
}

func TestTagsAccumulate(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	for range 3 {
		if _, err := repo.RecordEvent(ctx, recordInput(projectID, "abc", testNow)); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
	other := recordInput(projectID, "abc", testNow)
	other.Tags = map[string]string{"server": "web-02"}
	if _, err := repo.RecordEvent(ctx, other); err != nil {
		t.Fatalf("recording: %v", err)
	}

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	tags, err := repo.Tags(ctx, page.Issues[0].ID)
	if err != nil {
		t.Fatalf("reading tags: %v", err)
	}

	servers := tags["server"]
	if len(servers) != 2 {
		t.Fatalf("server values = %+v, want two", servers)
	}
	// Most common first, so a detail page leads with the answer.
	if servers[0].Value != "web-01" || servers[0].Count != 3 {
		t.Errorf("top server = %+v, want web-01 with 3", servers[0])
	}
}

func TestRetentionDeletesInBatches(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	old := testNow.Add(-100 * 24 * time.Hour)
	for i := range 10 {
		input := recordInput(projectID, "abc", old.Add(time.Duration(i)*time.Minute))
		input.ReceivedAt = old
		if _, err := repo.RecordEvent(ctx, input); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
	if _, err := repo.RecordEvent(ctx, recordInput(projectID, "abc", testNow)); err != nil {
		t.Fatalf("recording a recent event: %v", err)
	}

	cutoff := testNow.Add(-24 * time.Hour)

	// Batched so a sweep never holds one long write transaction and stalls
	// ingestion. The caller loops until a pass deletes nothing.
	deleted, err := repo.DeleteEventsBefore(ctx, projectID, cutoff, 4)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if deleted != 4 {
		t.Errorf("deleted %d, want the batch limit of 4", deleted)
	}

	var total int64
	for {
		batch, err := repo.DeleteEventsBefore(ctx, projectID, cutoff, 4)
		if err != nil {
			t.Fatalf("sweeping: %v", err)
		}
		if batch == 0 {
			break
		}
		total += batch
	}
	if total+deleted != 10 {
		t.Errorf("deleted %d old events in total, want 10", total+deleted)
	}

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	events, err := repo.LatestEvents(ctx, page.Issues[0].ID, 100)
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("got %d events after the sweep, want the recent one kept", len(events))
	}
}

func TestSetStatusRejectsUnknownStatus(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()
	result, err := repo.RecordEvent(ctx, recordInput(projectID, "abc", testNow))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	if err := repo.SetStatus(ctx, projectID, result.Issue.ID, "muted"); !errors.Is(err, domain.ErrInvalidIssue) {
		t.Errorf("error = %v, want ErrInvalidIssue", err)
	}
}

// TestRetentionKeepsAnEventAfterTheCutoff is the expensive half of the
// timestamp bug (ADR 033), pinned as a test.
//
// The sweep compares stored text, so while the layout dropped trailing zeros
// an event on an exact second read as *later* than one with a fraction: a
// cutoff of 10:00:00.500000000Z was greater than the string "…10:00:01Z", and
// an event a whole half-second inside the keep-window was deleted. There is no
// symptom to notice afterwards — the row is simply gone — which is why the
// only defence is a test that says the surviving event survives.
func TestRetentionKeepsAnEventAfterTheCutoff(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	// The cutoff lands on an exact second, which is not a contrivance: a
	// keep-window is a whole number of days subtracted from a swept-at instant,
	// and every SDK that rounds sends exact seconds too.
	cutoff := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		received time.Time
		survives bool
	}{
		{"a second before the cutoff", cutoff.Add(-time.Second), false},
		{"half a second before the cutoff", cutoff.Add(-500 * time.Millisecond), false},
		{"one nanosecond before the cutoff", cutoff.Add(-time.Nanosecond), false},
		{"the cutoff itself", cutoff, true},
		// The one the old layout deleted. "…10:00:00.500000000Z" sorted before
		// "…10:00:00Z" because '.' is less than 'Z', so this event was half a
		// second inside the keep-window and swept anyway.
		{"half a second after the cutoff", cutoff.Add(500 * time.Millisecond), true},
		{"one nanosecond after the cutoff", cutoff.Add(time.Nanosecond), true},
		{"a second after the cutoff", cutoff.Add(time.Second), true},
	}

	for i, tc := range cases {
		input := recordInput(projectID, "fp-"+strconv.Itoa(i), tc.received)
		input.ReceivedAt = tc.received
		if _, err := repo.RecordEvent(ctx, input); err != nil {
			t.Fatalf("recording %s: %v", tc.name, err)
		}
	}

	for {
		deleted, err := repo.DeleteEventsBefore(ctx, projectID, cutoff, 100)
		if err != nil {
			t.Fatalf("sweeping: %v", err)
		}
		if deleted == 0 {
			break
		}
	}

	survivors := map[time.Time]bool{}
	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 100})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, issue := range page.Issues {
		events, err := repo.LatestEvents(ctx, issue.ID, 100)
		if err != nil {
			t.Fatalf("reading events: %v", err)
		}
		for _, event := range events {
			survivors[event.ReceivedAt] = true
		}
	}

	for _, tc := range cases {
		if survivors[tc.received] != tc.survives {
			verb := "was deleted"
			if !tc.survives {
				verb = "survived"
			}
			t.Errorf("the event %s (%s) %s", tc.name, formatTime(tc.received), verb)
		}
	}
}

// TestListingOrdersWithinTheSameSecond is the cheap half of the same bug, on
// the product's main screen: two issues whose last event lands in the same
// second, one on the second exactly and one with a fraction.
func TestListingOrdersWithinTheSameSecond(t *testing.T) {
	repo, projectID := newIssueRepo(t)
	ctx := context.Background()

	second := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	// Recorded oldest first, so the expected order is the reverse of this.
	moments := []time.Time{
		second,
		second.Add(time.Millisecond),
		second.Add(500 * time.Millisecond),
		second.Add(999_999_999 * time.Nanosecond),
		second.Add(time.Second),
	}
	for i, at := range moments {
		if _, err := repo.RecordEvent(ctx, recordInput(projectID, "fp-"+strconv.Itoa(i), at)); err != nil {
			t.Fatalf("recording %s: %v", at, err)
		}
	}

	page, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 10})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Issues) != len(moments) {
		t.Fatalf("listed %d issues, want %d", len(page.Issues), len(moments))
	}
	for i, issue := range page.Issues {
		want := moments[len(moments)-1-i]
		if !issue.LastSeen.Equal(want) {
			t.Errorf("position %d last seen %s, want %s", i, formatTime(issue.LastSeen), formatTime(want))
		}
	}

	// And the keyset cursor has to cut the list in the same place the ORDER BY
	// puts it, or paging skips or repeats rows around an exact second.
	first, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 2})
	if err != nil {
		t.Fatalf("listing the first page: %v", err)
	}
	rest, err := repo.List(ctx, ports.IssueFilter{ProjectID: projectID, Limit: 10, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("listing the second page: %v", err)
	}
	if len(first.Issues)+len(rest.Issues) != len(moments) {
		t.Errorf("paging saw %d issues over two pages, want %d",
			len(first.Issues)+len(rest.Issues), len(moments))
	}
	for i, issue := range rest.Issues {
		want := moments[len(moments)-1-(len(first.Issues)+i)]
		if !issue.LastSeen.Equal(want) {
			t.Errorf("second page position %d last seen %s, want %s",
				i, formatTime(issue.LastSeen), formatTime(want))
		}
	}
}
