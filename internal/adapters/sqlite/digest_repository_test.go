package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/ports"
)

// newDigestRepos builds the three repositories the weekly report needs over
// one database: one writes the buckets and the transitions inside the ingest
// transaction, one reads them back, and one resolves an issue so it has
// something to come back from.
func newDigestRepos(t *testing.T) (*IssueRepository, *ReleaseRepository, *DigestRepository, int64) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	return NewIssueRepository(db), NewReleaseRepository(db), NewDigestRepository(db), project.ID
}

func TestNewIssuesAreTheOnesFirstSeenInTheWindow(t *testing.T) {
	issues, _, digests, projectID := newDigestRepos(t)
	ctx := context.Background()

	// Born before the window, and noisy inside it. It is not new.
	record(t, issues, projectID, "old", at(8), nil)
	record(t, issues, projectID, "old", at(11), nil)
	record(t, issues, projectID, "old", at(11), nil)
	// Born inside it.
	record(t, issues, projectID, "fresh", at(10), nil)
	record(t, issues, projectID, "fresh", at(11), nil)

	counts, err := digests.NewIssues(ctx, projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the new issues: %v", err)
	}
	if counts.Total != 1 {
		t.Fatalf("counted %d new issues, want 1", counts.Total)
	}
	if len(counts.Issues) != 1 || counts.Issues[0].Issue.Fingerprint != "fresh" {
		t.Fatalf("the list is %+v", counts.Issues)
	}
	if counts.Issues[0].Count != 2 {
		t.Errorf("the new issue counted %d events in the window, want 2", counts.Issues[0].Count)
	}
}

// TestNewIssuesCountsOnlyTheWindowsEvents is the difference between an issue's
// life and the week being reported on. Times would say four; the digest has to
// say two.
func TestNewIssuesCountsOnlyTheWindowsEvents(t *testing.T) {
	issues, _, digests, projectID := newDigestRepos(t)

	record(t, issues, projectID, "fresh", at(10), nil)
	record(t, issues, projectID, "fresh", at(10), nil)
	record(t, issues, projectID, "fresh", at(15), nil)
	record(t, issues, projectID, "fresh", at(16), nil)

	counts, err := digests.NewIssues(context.Background(), projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the new issues: %v", err)
	}
	if counts.Issues[0].Count != 2 {
		t.Errorf("counted %d events in the window, want 2 (Times is %d)",
			counts.Issues[0].Count, counts.Issues[0].Issue.Times)
	}
}

// TestRegressedIssuesAreTheOnesThatCameBackInTheWindow is the query the
// regressed_at column exists for. Before it, the only honest answers required
// reading events.
func TestRegressedIssuesAreTheOnesThatCameBackInTheWindow(t *testing.T) {
	issues, releases, digests, projectID := newDigestRepos(t)
	ctx := context.Background()

	// One issue, resolved, then reopened by an event inside the window.
	result := record(t, issues, projectID, "returns", at(9), nil)
	if _, err := releases.Resolve(ctx, projectID, result.Issue.ID, at(9), false); err != nil {
		t.Fatalf("resolving: %v", err)
	}
	back := record(t, issues, projectID, "returns", at(11), nil)
	if !back.Regressed {
		t.Fatal("the event did not reopen the issue, so there is nothing to report")
	}

	// And one that never went away, so the query has something to exclude.
	record(t, issues, projectID, "never left", at(10), nil)

	counts, err := digests.RegressedIssues(ctx, projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the regressions: %v", err)
	}
	if counts.Total != 1 {
		t.Fatalf("counted %d regressions, want 1", counts.Total)
	}
	if counts.Issues[0].Issue.Fingerprint != "returns" {
		t.Errorf("the regression is %q", counts.Issues[0].Issue.Fingerprint)
	}
}

// TestARegressionOutsideTheWindowIsNotThisWeeksNews is the whole reason the
// column exists rather than "unresolved and has regressed before": an issue
// that came back in March and has been noisy ever since must not be reported
// as this week's regression.
func TestARegressionOutsideTheWindowIsNotThisWeeksNews(t *testing.T) {
	issues, releases, digests, projectID := newDigestRepos(t)
	ctx := context.Background()

	result := record(t, issues, projectID, "old news", at(1), nil)
	if _, err := releases.Resolve(ctx, projectID, result.Issue.ID, at(1), false); err != nil {
		t.Fatalf("resolving: %v", err)
	}
	// It came back long before the window…
	record(t, issues, projectID, "old news", at(2), nil)
	// …and has been loud inside it ever since.
	record(t, issues, projectID, "old news", at(10), nil)
	record(t, issues, projectID, "old news", at(11), nil)

	counts, err := digests.RegressedIssues(ctx, projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the regressions: %v", err)
	}
	if counts.Total != 0 {
		t.Errorf("an old regression was reported as this week's: %+v", counts.Issues)
	}
}

// TestAnEventFromTheResolvedReleaseDoesNotMoveTheTimestamp covers the other
// branch of the same UPDATE: an occurrence from the build already known to be
// broken moves the resolution columns without being a regression, and it must
// leave the previous regression's timestamp alone.
func TestAnEventFromTheResolvedReleaseDoesNotMoveTheTimestamp(t *testing.T) {
	issues, releases, digests, projectID := newDigestRepos(t)
	ctx := context.Background()

	result := record(t, issues, projectID, "rollout", at(1), nil)
	// Resolved "in the next release", so events from v1.0.0 are expected.
	if _, err := releases.Resolve(ctx, projectID, result.Issue.ID, at(1), true); err != nil {
		t.Fatalf("resolving: %v", err)
	}

	// A pod that has not been redeployed, reporting from the old build inside
	// the window. Counted, not a regression.
	record(t, issues, projectID, "rollout", at(10), nil)

	counts, err := digests.RegressedIssues(ctx, projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the regressions: %v", err)
	}
	if counts.Total != 0 {
		t.Errorf("an expected event from the resolved release was reported as a regression: %+v",
			counts.Issues)
	}
}

func TestDigestListsAreCutToLengthButTheTotalIsNot(t *testing.T) {
	issues, _, digests, projectID := newDigestRepos(t)

	for index := range 9 {
		fingerprint := string(rune('a' + index))
		// Each one louder than the last, so the cut is provably by volume.
		for repeat := 0; repeat <= index; repeat++ {
			record(t, issues, projectID, fingerprint, at(10), nil)
		}
	}

	counts, err := digests.NewIssues(context.Background(), projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the new issues: %v", err)
	}
	if counts.Total != 9 {
		t.Errorf("the total is %d, want 9 — a digest that prints the length of its own list lies", counts.Total)
	}
	if len(counts.Issues) != 5 {
		t.Fatalf("the list has %d rows, want 5", len(counts.Issues))
	}
	if counts.Issues[0].Issue.Fingerprint != "i" {
		t.Errorf("the loudest issue is %q, want the one with nine events", counts.Issues[0].Issue.Fingerprint)
	}
}

func TestDigestQueriesAreScopedToTheirProject(t *testing.T) {
	db := openTemp(t)
	projects := NewProjectRepository(db)
	mine, _ := createProject(t, projects, "mine")
	theirs, _ := createProject(t, projects, "theirs")
	issues, digests := NewIssueRepository(db), NewDigestRepository(db)

	record(t, issues, theirs.ID, "not yours", at(10), nil)

	counts, err := digests.NewIssues(context.Background(), mine.ID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the new issues: %v", err)
	}
	if counts.Total != 0 {
		t.Errorf("another project's issues leaked into the report: %+v", counts.Issues)
	}
}

// TestNewIssuesSurviveTheDeletionOfTheirEvents is the promise of ADR 010, for
// this query: the digest still names what appeared after retention has taken
// the payloads.
func TestNewIssuesSurviveTheDeletionOfTheirEvents(t *testing.T) {
	issues, _, digests, projectID := newDigestRepos(t)
	ctx := context.Background()

	record(t, issues, projectID, "fresh", at(10), nil)
	if _, err := issues.DeleteEventsBefore(ctx, projectID, at(23), 1000); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	counts, err := digests.NewIssues(ctx, projectID, windowOver(t, 9, 12), 5)
	if err != nil {
		t.Fatalf("reading the new issues: %v", err)
	}
	if counts.Total != 1 {
		t.Errorf("the report lost an issue when its events were deleted")
	}
	if counts.Issues[0].Count != 1 {
		t.Errorf("the count came from the events rather than the buckets: %d", counts.Issues[0].Count)
	}
}

func TestDigestLimitsAreBounded(t *testing.T) {
	issues, _, digests, projectID := newDigestRepos(t)
	record(t, issues, projectID, "one", at(10), nil)

	for _, limit := range []int{0, -1, MaxDigestIssues + 1000} {
		if _, err := digests.NewIssues(context.Background(), projectID, windowOver(t, 9, 12), limit); err != nil {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}

// TestRegressedAtIsWrittenAsTheOccurrenceTime, not the arrival time — the same
// rule last_seen follows, because a late event describes when it happened.
func TestRegressedAtIsWrittenAsTheOccurrenceTime(t *testing.T) {
	issues, releases, _, projectID := newDigestRepos(t)
	ctx := context.Background()

	result := record(t, issues, projectID, "returns", at(9), nil)
	if _, err := releases.Resolve(ctx, projectID, result.Issue.ID, at(9), false); err != nil {
		t.Fatalf("resolving: %v", err)
	}

	occurred := at(11)
	record(t, issues, projectID, "returns", occurred, func(input *ports.RecordEventInput) {
		// Arrived hours after it happened, the way a mobile client's backlog
		// does.
		input.ReceivedAt = at(20)
	})

	var stored string
	if err := issues.db.QueryRowContext(ctx,
		"SELECT regressed_at FROM issues WHERE project_id = ? AND fingerprint = ?",
		projectID, "returns").Scan(&stored); err != nil {
		t.Fatalf("reading regressed_at: %v", err)
	}
	parsed, err := parseTime(stored)
	if err != nil {
		t.Fatalf("parsing regressed_at: %v", err)
	}
	if !parsed.Equal(occurred.UTC().Truncate(time.Nanosecond)) {
		t.Errorf("regressed_at is %s, want the occurrence time %s",
			parsed.Format(time.RFC3339Nano), occurred.Format(time.RFC3339Nano))
	}
}
