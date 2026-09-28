package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// recordAndRegister files an event, which is the single call the ingest path
// makes. The release, the counters and the verdict on a resolved issue all
// come out of that one transaction (ADR 032); the answer it returns is the
// one the stored row already agrees with.
func recordAndRegister(
	t *testing.T, _ *ReleaseRepository, issues *IssueRepository,
	projectID int64, fingerprint, release string, at time.Time,
) (issueID int64, regressed bool) {
	t.Helper()

	stored, err := issues.RecordEvent(context.Background(),
		releaseInput(projectID, fingerprint, release, at))
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	return stored.Issue.ID, stored.Regressed
}

func TestTheWholeNextReleaseStoryInStorage(t *testing.T) {
	// The reason this feature exists, end to end against the real database:
	// resolve, let the old build keep emitting, then deploy something that is
	// still broken.
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	issueID, _ := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow)

	if _, err := repo.Resolve(ctx, projectID, issueID, testNow, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	resolution, err := repo.Resolution(ctx, projectID, issueID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if !resolution.ResolveNextRelease || resolution.ResolvedInRelease != "app@1.0.0" {
		t.Fatalf("resolution = %+v", resolution)
	}
	if resolution.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q, want the release the issue was born in", resolution.FirstRelease)
	}

	// The pod that has not been redeployed yet.
	_, regressed := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow.Add(time.Minute))
	if regressed {
		t.Fatal("an event from the resolved release was reported as a regression")
	}
	resolution, err = repo.Resolution(ctx, projectID, issueID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.Status != domain.StatusResolved {
		t.Errorf("Status = %q, want it to stay resolved", resolution.Status)
	}
	if resolution.SeenInResolvedReleaseCount != 1 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want 1", resolution.SeenInResolvedReleaseCount)
	}
	if resolution.Regressions != 0 {
		t.Errorf("Regressions = %d, want 0", resolution.Regressions)
	}

	// The new build, still broken.
	_, regressed = recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.1", testNow.Add(time.Hour))
	if !regressed {
		t.Fatal("the fix did not work and the product did not say so")
	}
	resolution, err = repo.Resolution(ctx, projectID, issueID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.Status != domain.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", resolution.Status)
	}
	if resolution.Regressions != 1 {
		t.Errorf("Regressions = %d, want 1", resolution.Regressions)
	}
	if resolution.ResolveNextRelease || resolution.ResolvedInRelease != "" {
		t.Error("a reopened issue kept its pin")
	}
	if resolution.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q, want it unchanged by the regression", resolution.FirstRelease)
	}
	// The suppression count survives, because "three arrived from the old
	// build before it really came back" is what somebody reading a regression
	// wants to know.
	if resolution.SeenInResolvedReleaseCount != 1 {
		t.Errorf("SeenInResolvedReleaseCount = %d, want the suppressed one kept", resolution.SeenInResolvedReleaseCount)
	}
}

func TestNonSemverReleasesAreOrderedByFirstSight(t *testing.T) {
	// Two git shas. Nothing about the strings says which came first.
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	issueID, _ := recordAndRegister(t, repo, issues, projectID, "abc", "a3f9c1e", testNow)
	if _, err := repo.Resolve(ctx, projectID, issueID, testNow, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// An event from the same build: expected, not a regression.
	if _, regressed := recordAndRegister(t, repo, issues, projectID, "abc", "a3f9c1e", testNow.Add(time.Minute)); regressed {
		t.Error("the same sha reopened the issue")
	}
	// A build first seen after the resolution: a regression.
	if _, regressed := recordAndRegister(t, repo, issues, projectID, "abc", "b7d2f04", testNow.Add(time.Hour)); !regressed {
		t.Error("a build first seen after the fix did not reopen the issue")
	}
}

func TestPlainResolveReopensOnTheVerySameRelease(t *testing.T) {
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	issueID, _ := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow)
	if _, err := repo.Resolve(ctx, projectID, issueID, testNow, false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	_, regressed := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow.Add(time.Minute))
	if !regressed {
		t.Error("a plain resolve stopped reopening")
	}
	resolution, err := repo.Resolution(ctx, projectID, issueID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.Regressions != 1 {
		t.Errorf("Regressions = %d, want 1", resolution.Regressions)
	}
}

func TestIgnoringForgetsAPinThatNoLongerApplies(t *testing.T) {
	// Otherwise a later event would still be measured against a release
	// nobody is waiting for.
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	issueID, _ := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow)
	if _, err := repo.Resolve(ctx, projectID, issueID, testNow, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := repo.SetStatus(ctx, projectID, issueID, domain.StatusIgnored); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	resolution, err := repo.Resolution(ctx, projectID, issueID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.Status != domain.StatusIgnored {
		t.Errorf("Status = %q", resolution.Status)
	}
	if resolution.ResolveNextRelease || resolution.ResolvedAt != nil {
		t.Error("an ignored issue kept its resolution")
	}
}

func TestManualReopenIsNotCountedAsARegression(t *testing.T) {
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	issueID, _ := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow)
	if _, err := repo.Resolve(ctx, projectID, issueID, testNow, false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := repo.SetStatus(ctx, projectID, issueID, domain.StatusUnresolved); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	resolution, err := repo.Resolution(ctx, projectID, issueID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.Regressions != 0 {
		t.Errorf("Regressions = %d, want 0 — a person saying they were wrong is not a regression", resolution.Regressions)
	}
}

func TestSetStatusRejectsWhatIsNotAStatus(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	err := repo.SetStatus(context.Background(), projectID, 1, domain.IssueStatus("archived"))
	if !errors.Is(err, domain.ErrInvalidIssue) {
		t.Errorf("error = %v, want ErrInvalidIssue", err)
	}
}

func TestResolutionOfAnIssueThatIsNotThere(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()

	if _, err := repo.Resolution(ctx, projectID, 999); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Errorf("Resolution: %v, want ErrIssueNotFound", err)
	}
	if _, err := repo.Resolve(ctx, projectID, 999, testNow, true); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Errorf("Resolve: %v, want ErrIssueNotFound", err)
	}
	if err := repo.SetStatus(ctx, projectID, 999, domain.StatusIgnored); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Errorf("SetStatus: %v, want ErrIssueNotFound", err)
	}
}

func TestAnOrdinaryEventLeavesTheResolutionColumnsAlone(t *testing.T) {
	// The common path: another occurrence of something already open. It is
	// not a regression, and it must not write a resolution nobody declared —
	// this runs once per ingested event, and a stray column here is what
	// would make "has this ever been fixed?" answer wrongly.
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	first, err := issues.RecordEvent(ctx, releaseInput(projectID, "abc", "app@1.0.0", testNow))
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	again, err := issues.RecordEvent(ctx,
		releaseInput(projectID, "abc", "app@1.0.0", testNow.Add(time.Minute)))
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if again.Regressed || again.New {
		t.Errorf("second event = %+v, want neither new nor a regression", again)
	}

	resolution, err := repo.Resolution(ctx, projectID, first.Issue.ID)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.Status != domain.StatusUnresolved || resolution.ResolvedAt != nil ||
		resolution.Regressions != 0 || resolution.SeenInResolvedReleaseCount != 0 {
		t.Errorf("resolution = %+v, want it untouched", resolution)
	}
	if resolution.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q, want the release the issue was born in", resolution.FirstRelease)
	}
}

func TestAnEventWithNoReleaseDoesNotReopenAPinnedIssue(t *testing.T) {
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	issueID, _ := recordAndRegister(t, repo, issues, projectID, "abc", "app@1.0.0", testNow)
	if _, err := repo.Resolve(ctx, projectID, issueID, testNow, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if _, regressed := recordAndRegister(t, repo, issues, projectID, "abc", "", testNow.Add(time.Minute)); regressed {
		t.Error("an event with no release at all reopened a pinned issue")
	}
}
