package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func observation(at time.Time) Observation {
	return Observation{At: at, Level: LevelError, Title: "ValueError: bad", Culprit: "myapp.views in checkout"}
}

func newTestIssue(t *testing.T) Issue {
	t.Helper()
	issue, err := NewIssue(1, "abc123", observation(testNow))
	if err != nil {
		t.Fatalf("building issue: %v", err)
	}
	return issue
}

func TestNewIssue(t *testing.T) {
	issue := newTestIssue(t)

	if issue.Status != StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", issue.Status)
	}
	if issue.Times != 1 {
		t.Errorf("Times = %d, want 1", issue.Times)
	}
	if !issue.FirstSeen.Equal(testNow) || !issue.LastSeen.Equal(testNow) {
		t.Errorf("first/last seen = %v/%v", issue.FirstSeen, issue.LastSeen)
	}
	// The algorithm version travels with the issue so a future change is a
	// migration with a plan, not a silent rewrite of everyone's history.
	if issue.GroupingVersion != GroupingVersion {
		t.Errorf("GroupingVersion = %d, want %d", issue.GroupingVersion, GroupingVersion)
	}
}

func TestNewIssueRejections(t *testing.T) {
	cases := map[string]struct {
		projectID   int64
		fingerprint string
		at          time.Time
	}{
		"no project":     {0, "abc", testNow},
		"no fingerprint": {1, "", testNow},
		"no timestamp":   {1, "abc", time.Time{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewIssue(tc.projectID, tc.fingerprint, Observation{At: tc.at})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, ErrInvalidIssue) && !errors.Is(err, ErrInvalidProject) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

func TestObserveCountsAndMovesLastSeen(t *testing.T) {
	issue := newTestIssue(t)
	later := testNow.Add(time.Hour)

	updated, regressed := issue.Observe(observation(later))

	if regressed {
		t.Error("an unresolved issue reported a regression")
	}
	if updated.Times != 2 {
		t.Errorf("Times = %d, want 2", updated.Times)
	}
	if !updated.LastSeen.Equal(later) {
		t.Errorf("LastSeen = %v, want %v", updated.LastSeen, later)
	}
	if !updated.FirstSeen.Equal(testNow) {
		t.Errorf("FirstSeen moved to %v", updated.FirstSeen)
	}
	// Value semantics: observing must not mutate the caller's copy.
	if issue.Times != 1 {
		t.Error("Observe mutated the receiver")
	}
}

func TestLateEventsDoNotMoveLastSeenBackwards(t *testing.T) {
	// A mobile client that was offline, or a retry after an outage, sends
	// events with old timestamps. Letting last-seen move backwards would make
	// an actively firing issue look stale.
	issue := newTestIssue(t)
	recent, _ := issue.Observe(observation(testNow.Add(time.Hour)))

	late, _ := recent.Observe(observation(testNow.Add(-24 * time.Hour)))

	if !late.LastSeen.Equal(testNow.Add(time.Hour)) {
		t.Errorf("LastSeen = %v, want it to stay at the most recent event", late.LastSeen)
	}
	// First-seen, on the other hand, should move back: the issue really did
	// start earlier than we knew.
	if !late.FirstSeen.Equal(testNow.Add(-24 * time.Hour)) {
		t.Errorf("FirstSeen = %v, want it to move back", late.FirstSeen)
	}
	if late.Times != 3 {
		t.Errorf("Times = %d, want 3 — a late event still counts", late.Times)
	}
}

func TestResolvedIssueRegressesOnANewEvent(t *testing.T) {
	// This is the single most valuable signal the product gives: you thought
	// it was fixed and it is back.
	issue := newTestIssue(t).Resolve(testNow)

	updated, regressed := issue.Observe(observation(testNow.Add(time.Hour)))

	if !regressed {
		t.Error("a new event on a resolved issue did not report a regression")
	}
	if updated.Status != StatusUnresolved {
		t.Errorf("Status = %q, want unresolved after a regression", updated.Status)
	}
}

func TestIgnoredIssueCountsButDoesNotReopen(t *testing.T) {
	// The count is how someone notices they were wrong to ignore it.
	// Reopening would make "ignore" mean nothing.
	issue := newTestIssue(t).Ignore()

	updated, regressed := issue.Observe(observation(testNow.Add(time.Hour)))

	if regressed {
		t.Error("an ignored issue reported a regression")
	}
	if updated.Status != StatusIgnored {
		t.Errorf("Status = %q, want it to stay ignored", updated.Status)
	}
	if updated.Times != 2 {
		t.Errorf("Times = %d, want the event still counted", updated.Times)
	}
}

func TestOnlyTheFirstRegressionIsReported(t *testing.T) {
	// Once reopened, later events are just occurrences. Reporting a regression
	// on each would fire an alert per event during an incident.
	issue := newTestIssue(t).Resolve(testNow)

	reopened, first := issue.Observe(observation(testNow.Add(time.Hour)))
	_, second := reopened.Observe(observation(testNow.Add(2 * time.Hour)))

	if !first || second {
		t.Errorf("regressions reported = %v then %v, want true then false", first, second)
	}
}

func TestLevelTracksTheWorstSeen(t *testing.T) {
	// An issue that has ever been fatal must not be quietly downgraded
	// because the next occurrence was reported as a warning.
	issue := newTestIssue(t)

	worse, _ := issue.Observe(Observation{At: testNow.Add(time.Minute), Level: LevelFatal})
	if worse.Level != LevelFatal {
		t.Errorf("Level = %q, want fatal", worse.Level)
	}

	milder, _ := worse.Observe(Observation{At: testNow.Add(2 * time.Minute), Level: LevelWarning})
	if milder.Level != LevelFatal {
		t.Errorf("Level = %q, want it to stay fatal", milder.Level)
	}
}

func TestTitleDoesNotFlicker(t *testing.T) {
	// Two occurrences that differ only in an interpolated value must not make
	// the issue's name change every time one arrives.
	issue := newTestIssue(t)

	updated, _ := issue.Observe(Observation{At: testNow.Add(time.Minute), Title: "ValueError: otro valor"})

	if updated.Title != "ValueError: bad" {
		t.Errorf("Title = %q, want the original", updated.Title)
	}
}

func TestTitleFillsInWhenMissing(t *testing.T) {
	issue, err := NewIssue(1, "abc", Observation{At: testNow})
	if err != nil {
		t.Fatalf("building issue: %v", err)
	}
	updated, _ := issue.Observe(Observation{At: testNow.Add(time.Minute), Title: "por fin un título"})
	if updated.Title != "por fin un título" {
		t.Errorf("Title = %q", updated.Title)
	}
}

func TestReleaseTracksTheLatest(t *testing.T) {
	issue := newTestIssue(t)

	withRelease, _ := issue.Observe(Observation{At: testNow.Add(time.Minute), Release: "v2.0.0"})
	if withRelease.LastRelease != "v2.0.0" {
		t.Errorf("LastRelease = %q", withRelease.LastRelease)
	}

	// An event with no release must not erase what we knew.
	withoutRelease, _ := withRelease.Observe(Observation{At: testNow.Add(2 * time.Minute)})
	if withoutRelease.LastRelease != "v2.0.0" {
		t.Errorf("LastRelease = %q, want it kept", withoutRelease.LastRelease)
	}
}

func TestALateEventDoesNotRewriteTheRelease(t *testing.T) {
	// Found by the panel: the issue header showed one release while the newest
	// occurrence below it showed another, because the issue followed ingestion
	// order and the event list followed occurrence order. A client that was
	// offline delivers an old event from an old build, and that must not
	// relabel an issue that has since been seen on a newer one.
	issue := newTestIssue(t)

	current, _ := issue.Observe(Observation{At: testNow.Add(time.Hour), Release: "v2.0.0"})
	late, _ := current.Observe(Observation{At: testNow.Add(-24 * time.Hour), Release: "v1.0.0"})

	if late.LastRelease != "v2.0.0" {
		t.Errorf("LastRelease = %q, want the release of the newest occurrence", late.LastRelease)
	}
	if late.Times != 3 {
		t.Errorf("Times = %d, want the late event still counted", late.Times)
	}
}

func TestStatusTransitions(t *testing.T) {
	issue := newTestIssue(t)

	if got := issue.Resolve(testNow).Status; got != StatusResolved {
		t.Errorf("Resolve -> %q", got)
	}
	if got := issue.Ignore().Status; got != StatusIgnored {
		t.Errorf("Ignore -> %q", got)
	}
	if got := issue.Resolve(testNow).Reopen().Status; got != StatusUnresolved {
		t.Errorf("Reopen -> %q", got)
	}
	// Value semantics again: none of these may mutate the receiver.
	if issue.Status != StatusUnresolved {
		t.Error("a transition mutated the receiver")
	}
}

func TestValidStatus(t *testing.T) {
	for _, status := range []IssueStatus{StatusUnresolved, StatusResolved, StatusIgnored} {
		if !status.ValidStatus() {
			t.Errorf("%q reported invalid", status)
		}
	}
	for _, status := range []IssueStatus{"", "muted", "RESOLVED"} {
		if status.ValidStatus() {
			t.Errorf("%q reported valid", status)
		}
	}
}

func TestTitleIsBoundedAndUTF8Safe(t *testing.T) {
	// Cutting on a byte boundary would split a multi-byte character into
	// invalid UTF-8, which a JSON encoder then mangles.
	long := strings.Repeat("ñ", MaxTitleLen+50)
	issue, err := NewIssue(1, "abc", Observation{At: testNow, Title: long})
	if err != nil {
		t.Fatalf("building issue: %v", err)
	}
	if len([]rune(issue.Title)) > MaxTitleLen+1 {
		t.Errorf("title is %d runes, want at most %d plus an ellipsis", len([]rune(issue.Title)), MaxTitleLen)
	}
	for _, r := range issue.Title {
		if r == '�' {
			t.Fatal("the title was cut mid-character and contains a replacement rune")
		}
	}
}

func TestTitleWhitespaceIsCollapsed(t *testing.T) {
	issue, err := NewIssue(1, "abc", Observation{At: testNow, Title: "  Value\n\tError:   bad  "})
	if err != nil {
		t.Fatalf("building issue: %v", err)
	}
	if issue.Title != "Value Error: bad" {
		t.Errorf("Title = %q", issue.Title)
	}
}
