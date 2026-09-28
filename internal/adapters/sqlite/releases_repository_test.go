package sqlite

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

func newReleaseRepo(t *testing.T) (*ReleaseRepository, *IssueRepository, int64) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	return NewReleaseRepository(db), NewIssueRepository(db), project.ID
}

func mustRelease(t *testing.T, projectID int64, version string) domain.Release {
	t.Helper()
	release, err := domain.NewRelease(projectID, version, testNow)
	if err != nil {
		t.Fatalf("building release: %v", err)
	}
	return release
}

func TestEnsureCreatesAReleaseNobodyRegistered(t *testing.T) {
	// The commonest way a release comes into existence: nobody ran a deploy
	// tool, an event simply arrived carrying a version.
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()

	if err := repo.Ensure(ctx, projectID, "app@1.0.0", testNow); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	release, err := repo.Find(ctx, projectID, "app@1.0.0")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if release.FirstEventAt == nil || !release.FirstEventAt.Equal(testNow) {
		t.Errorf("FirstEventAt = %v, want %v", release.FirstEventAt, testNow)
	}
	if release.DateReleased != nil {
		t.Error("a release discovered from an event should not be finalised")
	}
}

func TestEnsureWidensTheEventWindowWithoutDuplicating(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()

	later := testNow.Add(2 * time.Hour)
	for _, at := range []time.Time{testNow, later, testNow.Add(time.Hour)} {
		if err := repo.Ensure(ctx, projectID, "app@1.0.0", at); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}

	releases, err := repo.List(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("three events produced %d releases, want 1", len(releases))
	}
	if !releases[0].FirstEventAt.Equal(testNow) {
		t.Errorf("FirstEventAt = %v, want the earliest", releases[0].FirstEventAt)
	}
	if !releases[0].LastEventAt.Equal(later) {
		t.Errorf("LastEventAt = %v, want the latest", releases[0].LastEventAt)
	}
}

func TestCreateIsIdempotentAndDoesNotOverwrite(t *testing.T) {
	// A pipeline step that runs twice, or a deploy annotation that arrives
	// after the first error already created the release implicitly.
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()

	if err := repo.Ensure(ctx, projectID, "app@1.0.0", testNow); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	saved, created, err := repo.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created {
		t.Error("Create reported a release it did not create")
	}
	if saved.FirstEventAt == nil {
		t.Error("Create overwrote what was already known about the release")
	}

	_, created, err = repo.Create(ctx, mustRelease(t, projectID, "app@1.0.1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created {
		t.Error("a genuinely new release was not reported as created")
	}
}

func TestFindIsScopedToItsProject(t *testing.T) {
	// A query that can return another project's release is one refactor away
	// from being an authorisation bug.
	db := openTemp(t)
	repo := NewReleaseRepository(db)
	projects := NewProjectRepository(db)
	first, _ := createProject(t, projects, "one")
	second, _ := createProject(t, projects, "two")
	ctx := context.Background()

	if _, _, err := repo.Create(ctx, mustRelease(t, first.ID, "app@1.0.0")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := repo.Find(ctx, second.ID, "app@1.0.0"); !errors.Is(err, domain.ErrReleaseNotFound) {
		t.Errorf("error = %v, want ErrReleaseNotFound", err)
	}
}

func TestFinalizeKeepsTheFirstDate(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	if _, _, err := repo.Create(ctx, mustRelease(t, projectID, "app@1.0.0")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := repo.Finalize(ctx, projectID, "app@1.0.0", testNow)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	second, err := repo.Finalize(ctx, projectID, "app@1.0.0", testNow.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("Finalize again: %v", err)
	}

	if first.DateReleased == nil || !second.DateReleased.Equal(*first.DateReleased) {
		t.Errorf("a rerun moved the release date to %v", second.DateReleased)
	}
	reread, err := repo.Find(ctx, projectID, "app@1.0.0")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if reread.DateReleased == nil || !reread.DateReleased.Equal(*first.DateReleased) {
		t.Errorf("the stored date is %v, want %v", reread.DateReleased, first.DateReleased)
	}
}

func TestFinalizeOfAnUnknownRelease(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	if _, err := repo.Finalize(context.Background(), projectID, "app@9.9.9", testNow); !errors.Is(err, domain.ErrReleaseNotFound) {
		t.Errorf("error = %v, want ErrReleaseNotFound", err)
	}
}

func TestSetCommitsReplacesRatherThanAppends(t *testing.T) {
	// A deploy tool sends the whole set each time; appending would double it
	// on a pipeline rerun.
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	release, _, err := repo.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	authored := testNow.Add(-time.Hour)
	first := []domain.Commit{
		{SHA: "aaa", Message: "one", AuthorName: "Ana", Timestamp: &authored, Ordinal: 0,
			Files: []domain.CommitFile{{Path: "src/a.go", ChangeType: domain.ChangeModified}}},
		{SHA: "bbb", Message: "two", Ordinal: 1},
	}
	if err := repo.SetCommits(ctx, release.ID, first); err != nil {
		t.Fatalf("SetCommits: %v", err)
	}
	if err := repo.SetCommits(ctx, release.ID, first); err != nil {
		t.Fatalf("SetCommits again: %v", err)
	}

	commits, err := repo.Commits(ctx, release.ID)
	if err != nil {
		t.Fatalf("Commits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("a rerun left %d commits, want 2", len(commits))
	}
	if commits[0].SHA != "aaa" || commits[1].SHA != "bbb" {
		t.Errorf("commits came back out of order: %v, %v", commits[0].SHA, commits[1].SHA)
	}
	if commits[0].Timestamp == nil || !commits[0].Timestamp.Equal(authored) {
		t.Errorf("Timestamp = %v, want %v", commits[0].Timestamp, authored)
	}
	if len(commits[0].Files) != 1 || commits[0].Files[0].Path != "src/a.go" {
		t.Errorf("changed files = %+v", commits[0].Files)
	}
	if len(commits[1].Files) != 0 {
		t.Errorf("a commit with no patch set came back with %d files", len(commits[1].Files))
	}

	reread, err := repo.Find(ctx, projectID, "app@1.0.0")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if reread.CommitCount != 2 {
		t.Errorf("CommitCount = %d, want 2", reread.CommitCount)
	}
}

func TestSetCommitsToAnEmptySetClearsIt(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	release, _, err := repo.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SetCommits(ctx, release.ID, []domain.Commit{{SHA: "aaa"}}); err != nil {
		t.Fatalf("SetCommits: %v", err)
	}
	if err := repo.SetCommits(ctx, release.ID, nil); err != nil {
		t.Fatalf("SetCommits empty: %v", err)
	}

	commits, err := repo.Commits(ctx, release.ID)
	if err != nil {
		t.Fatalf("Commits: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("kept %d commits after clearing", len(commits))
	}
}

func TestDeploys(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	release, _, err := repo.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, environment := range []string{"staging", "production"} {
		deploy, err := domain.NewDeploy(release.ID, environment, testNow)
		if err != nil {
			t.Fatalf("NewDeploy: %v", err)
		}
		deploy.Name = "pipeline-42"
		if _, err := repo.AddDeploy(ctx, deploy); err != nil {
			t.Fatalf("AddDeploy: %v", err)
		}
	}

	deploys, err := repo.Deploys(ctx, release.ID)
	if err != nil {
		t.Fatalf("Deploys: %v", err)
	}
	if len(deploys) != 2 {
		t.Fatalf("got %d deploys, want 2", len(deploys))
	}
	// Most recent first: during an incident the last deploy is the one being
	// asked about.
	if deploys[0].Environment != "production" {
		t.Errorf("first deploy = %q, want the most recent", deploys[0].Environment)
	}
	if deploys[0].FinishedAt == nil || !deploys[0].FinishedAt.Equal(testNow) {
		t.Errorf("FinishedAt = %v", deploys[0].FinishedAt)
	}
}

func TestOrderIsFirstSightOrder(t *testing.T) {
	// The id is assigned when a release is first seen, which is exactly the
	// ordering the domain needs for identifiers that are not versions.
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	for _, version := range []string{"b7d2f04", "a3f9c1e"} {
		if err := repo.Ensure(ctx, projectID, version, testNow); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}

	order, err := repo.Order(ctx, projectID)
	if err != nil {
		t.Fatalf("Order: %v", err)
	}
	if order["b7d2f04"] >= order["a3f9c1e"] {
		t.Errorf("order = %v, want the first-seen sha to rank lower", order)
	}
	if domain.CompareReleases("a3f9c1e", "b7d2f04", order) != 1 {
		t.Error("the later-seen sha did not compare as newer")
	}
}

func TestStatsAttributeEachReleaseWhatItDid(t *testing.T) {
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	// An issue born in 1.0.0, resolved in the next release, then genuinely
	// regressing in 1.0.1.
	born, err := issues.RecordEvent(ctx, releaseInput(projectID, "abc", "app@1.0.0", testNow))
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if _, err := repo.Resolve(ctx, projectID, born.Issue.ID, testNow, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	regressing, err := issues.RecordEvent(ctx,
		releaseInput(projectID, "abc", "app@1.0.1", testNow.Add(time.Hour)))
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if !regressing.Regressed {
		t.Fatal("an event from a newer release was not a regression")
	}

	first, err := repo.Stats(ctx, projectID, "app@1.0.0")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if first.NewIssues != 1 || first.RegressedIssues != 0 {
		t.Errorf("1.0.0 stats = %+v, want 1 new and 0 regressed", first)
	}
	second, err := repo.Stats(ctx, projectID, "app@1.0.1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if second.NewIssues != 0 || second.RegressedIssues != 1 {
		t.Errorf("1.0.1 stats = %+v, want 0 new and 1 regressed", second)
	}
}

func TestReleaseEventCountsAreTheReleasesOwnAndNotTheProjects(t *testing.T) {
	// Two releases in the same project, with different traffic. The easy
	// mistake is to sum the project's buckets and report the same total on
	// every release page — a number that looks plausible on the release that
	// happens to be the only one, and is wrong the moment there are two.
	repo, issues, projectID := newReleaseRepo(t)
	ctx := context.Background()

	const (
		oldBuild = "app@1.0.0"
		newBuild = "app@1.0.1"
	)
	// Three events from the old build, spread over two hours so the count
	// also has to survive being split across buckets, and two from the new.
	for index, at := range []time.Time{testNow, testNow.Add(time.Minute), testNow.Add(time.Hour)} {
		if _, err := issues.RecordEvent(ctx,
			releaseInput(projectID, "boom"+strconv.Itoa(index%2), oldBuild, at)); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}
	for index := range 2 {
		if _, err := issues.RecordEvent(ctx,
			releaseInput(projectID, "boom0", newBuild, testNow.Add(time.Duration(index)*time.Minute))); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	older, err := repo.Stats(ctx, projectID, oldBuild)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if older.Events != 3 {
		t.Errorf("%s events = %d, want 3", oldBuild, older.Events)
	}
	newer, err := repo.Stats(ctx, projectID, newBuild)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if newer.Events != 2 {
		t.Errorf("%s events = %d, want 2", newBuild, newer.Events)
	}

	// A release registered by a deploy tool that has not seen an error yet
	// reports nothing rather than failing: SUM over no rows is NULL.
	if _, _, err := repo.Create(ctx, mustRelease(t, projectID, "app@2.0.0")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	quiet, err := repo.Stats(ctx, projectID, "app@2.0.0")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if quiet.Events != 0 {
		t.Errorf("events = %d, want 0 for a release nothing has been seen from", quiet.Events)
	}
}

// releaseInput is an event carrying a release.
func releaseInput(projectID int64, fingerprint, release string, at time.Time) *ports.RecordEventInput {
	input := recordInput(projectID, fingerprint, at)
	input.Observation.Release = release
	return input
}

func TestEnsureRejectsWhatIsNotAVersion(t *testing.T) {
	// The version is a path segment everywhere it is addressed, so a slash
	// would create a release nobody could ever read back.
	repo, _, projectID := newReleaseRepo(t)
	if err := repo.Ensure(context.Background(), projectID, "feature/x", testNow); !errors.Is(err, domain.ErrInvalidRelease) {
		t.Errorf("error = %v, want ErrInvalidRelease", err)
	}
	if _, err := repo.Find(context.Background(), projectID, "  "); !errors.Is(err, domain.ErrInvalidRelease) {
		t.Errorf("Find with no version: %v", err)
	}
}

func TestListIsBoundedWhateverTheCallerAsksFor(t *testing.T) {
	// A page nobody can read is a query somebody's dashboard runs in a loop.
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	for index := range MaxReleasePageSize + 5 {
		if err := repo.Ensure(ctx, projectID, "build-"+strconv.Itoa(index), testNow); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}

	releases, err := repo.List(ctx, projectID, 10_000)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(releases) != MaxReleasePageSize {
		t.Errorf("got %d releases, want the cap of %d", len(releases), MaxReleasePageSize)
	}
	// Newest first, so a listing opens on what was just deployed.
	if releases[0].Version != "build-"+strconv.Itoa(MaxReleasePageSize+4) {
		t.Errorf("first release = %q, want the most recently discovered", releases[0].Version)
	}
}

func TestCommitsAndDeploysOfAReleaseWithNeither(t *testing.T) {
	repo, _, projectID := newReleaseRepo(t)
	ctx := context.Background()
	release, _, err := repo.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	commits, err := repo.Commits(ctx, release.ID)
	if err != nil || len(commits) != 0 {
		t.Errorf("Commits = %v, %v", commits, err)
	}
	deploys, err := repo.Deploys(ctx, release.ID)
	if err != nil || len(deploys) != 0 {
		t.Errorf("Deploys = %v, %v", deploys, err)
	}
}
