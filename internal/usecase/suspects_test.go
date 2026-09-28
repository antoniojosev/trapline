package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
)

// suspectIssues is the issue half of the attribution: one issue and the
// occurrences stored under it.
//
// Its own fake rather than the shared one, because this is the only use case
// that reads a stored payload back, and the shared fake answers LatestEvents
// with nothing on purpose.
type suspectIssues struct {
	issue  domain.Issue
	events []ports.StoredEvent
	err    error
	asked  int
}

func (s *suspectIssues) RecordEvent(context.Context, *ports.RecordEventInput) (ports.RecordEventResult, error) {
	return ports.RecordEventResult{}, errors.New("not used")
}

func (s *suspectIssues) List(context.Context, ports.IssueFilter) (ports.IssuePage, error) {
	return ports.IssuePage{}, nil
}

func (s *suspectIssues) CountsByStatus(context.Context, int64) (map[domain.IssueStatus]int64, error) {
	return nil, nil
}

func (s *suspectIssues) FindByID(_ context.Context, _, issueID int64) (domain.Issue, error) {
	if s.err != nil {
		return domain.Issue{}, s.err
	}
	if issueID != s.issue.ID {
		return domain.Issue{}, domain.ErrIssueNotFound
	}
	return s.issue, nil
}

func (s *suspectIssues) SetStatus(context.Context, int64, int64, domain.IssueStatus) error {
	return nil
}

func (s *suspectIssues) LatestEvents(_ context.Context, _ int64, limit int) ([]ports.StoredEvent, error) {
	s.asked = limit
	return s.events, nil
}

func (s *suspectIssues) Tags(context.Context, int64) (map[string][]ports.TagCount, error) {
	return nil, nil
}

func (s *suspectIssues) DeleteEventsBefore(context.Context, int64, time.Time, int) (int64, error) {
	return 0, nil
}

// suspectResolution answers with whichever first release a test needs.
type suspectResolution struct {
	first string
	err   error
}

func (s *suspectResolution) Resolve(context.Context, int64, int64, time.Time, bool) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (s *suspectResolution) SetStatus(context.Context, int64, int64, domain.IssueStatus) error {
	return nil
}

func (s *suspectResolution) Resolution(context.Context, int64, int64) (ports.IssueResolution, error) {
	if s.err != nil {
		return ports.IssueResolution{}, s.err
	}
	return ports.IssueResolution{FirstRelease: s.first}, nil
}

// storedEventWith encodes an occurrence the way ingestion stores one, so the
// use case decodes exactly what production hands it.
func storedEventWith(t *testing.T, release string, frames []sentry.Frame) ports.StoredEvent {
	t.Helper()
	event := &sentry.Event{
		EventID:   strings.Repeat("a", 32),
		Timestamp: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		Release:   release,
		Exceptions: []sentry.Exception{{
			Type:       "TypeError",
			Value:      "total is not a function",
			Stacktrace: &sentry.Stacktrace{Frames: frames},
		}},
	}
	payload, err := sentry.EncodeEvent(event)
	if err != nil {
		t.Fatalf("encoding the fixture event: %v", err)
	}
	return ports.StoredEvent{ID: 1, EventID: event.EventID, Release: release, Payload: payload}
}

// suspectsUnderTest wires the use case over fakes and a release set.
func suspectsUnderTest(
	t *testing.T, firstRelease string, commits []domain.Commit, events []ports.StoredEvent,
) (*Suspects, *suspectIssues) {
	t.Helper()
	issues := &suspectIssues{
		issue:  domain.Issue{ID: 7, ProjectID: 1, Title: "TypeError"},
		events: events,
	}
	releases := newFakeReleases()
	if firstRelease != "" {
		release, _, err := releases.Create(context.Background(),
			domain.Release{ProjectID: 1, Version: firstRelease})
		if err != nil {
			t.Fatalf("seeding the release: %v", err)
		}
		if commits != nil {
			cleaned, err := domain.CleanCommits(commits)
			if err != nil {
				t.Fatalf("cleaning the commit set: %v", err)
			}
			if err := releases.SetCommits(context.Background(), release.ID, cleaned); err != nil {
				t.Fatalf("seeding the commits: %v", err)
			}
		}
	}
	return NewSuspects(issues, releases, &suspectResolution{first: firstRelease}), issues
}

func fileCommit(sha, message string, paths ...string) domain.Commit {
	files := make([]domain.CommitFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, domain.CommitFile{Path: path, ChangeType: domain.ChangeModified})
	}
	return domain.Commit{SHA: sha, Message: message, Files: files}
}

func TestSuspectsNamesTheCommitThatTouchedTheFailingFrame(t *testing.T) {
	// Frames arrive oldest first, so `checkout.ts` last is the call that
	// raised — which is the frame the top commit has to be attributed to.
	frames := []sentry.Frame{
		{Filename: "src/main.ts", Lineno: 3, InApp: true},
		{Filename: "src/checkout.ts", Lineno: 42, InApp: true, Raw: &sentry.RawFrame{
			Filename: "~/bundle.min.js", Lineno: 1, Colno: 3120,
		}},
	}
	commits := []domain.Commit{
		fileCommit("f1f1f1", "rewrite the totals", "src/checkout.ts"),
		fileCommit("a2a2a2", "bump a dependency", "package-lock.json"),
	}
	suspects, issues := suspectsUnderTest(t, "app@1.4.0",
		commits, []ports.StoredEvent{storedEventWith(t, "app@1.4.0", frames)})

	report, err := suspects.For(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if report.Release != "app@1.4.0" {
		t.Fatalf("blamed release %q, want the one the issue was first seen in", report.Release)
	}
	if len(report.Suspects) != 1 || report.Suspects[0].Commit.SHA != "f1f1f1" {
		t.Fatalf("got %+v, want exactly f1f1f1", report.Suspects)
	}
	if report.Suspects[0].Reasons[0].FrameDepth != 0 {
		t.Fatalf("the reason points at frame #%d, want the failing one",
			report.Suspects[0].Reasons[0].FrameDepth+1)
	}
	if !report.Symbolicated {
		t.Fatal("the report says nothing was symbolicated, but a frame carries its raw form")
	}
	if report.Warning != "" {
		t.Fatalf("an answer arrived with a warning: %q", report.Warning)
	}
	if report.CommitCount != 2 {
		t.Fatalf("counted %d commits, want 2", report.CommitCount)
	}
	if issues.asked != suspectEventLookback {
		t.Fatalf("read %d occurrences, want %d", issues.asked, suspectEventLookback)
	}
}

func TestSuspectsPrefersAnOccurrenceFromTheReleaseUnderSuspicion(t *testing.T) {
	// The newest occurrence is minified — it arrived before the maps were
	// uploaded, from a later deploy. The one from the release being blamed is
	// resolved, and it is the one that can be attributed.
	minified := storedEventWith(t, "app@2.0.0", []sentry.Frame{
		{Filename: "~/bundle.min.js", Lineno: 1, Colno: 9022},
	})
	resolved := storedEventWith(t, "app@1.4.0", []sentry.Frame{
		{Filename: "src/checkout.ts", Lineno: 42, Raw: &sentry.RawFrame{Filename: "~/bundle.min.js"}},
	})
	suspects, _ := suspectsUnderTest(t, "app@1.4.0",
		[]domain.Commit{fileCommit("f1f1f1", "rewrite the totals", "src/checkout.ts")},
		[]ports.StoredEvent{minified, resolved})

	report, err := suspects.For(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if len(report.Suspects) != 1 {
		t.Fatalf("got %d suspects, want the one the older occurrence names", len(report.Suspects))
	}
}

func TestSuspectsExplainsEveryEmptyAnswer(t *testing.T) {
	resolved := []sentry.Frame{{Filename: "src/checkout.ts", Lineno: 42,
		Raw: &sentry.RawFrame{Filename: "~/bundle.min.js"}}}
	minified := []sentry.Frame{{Filename: "~/bundle.min.js", Lineno: 1, Colno: 9022}}

	cases := []struct {
		name     string
		release  string
		commits  []domain.Commit
		frames   []sentry.Frame
		contains string
	}{
		{
			name:     "no release at all",
			release:  "",
			contains: "never carried a release",
		},
		{
			name:     "a release with no commit set",
			release:  "app@1.4.0",
			frames:   resolved,
			contains: "No commits are associated",
		},
		{
			name:    "commits without a patch set",
			release: "app@1.4.0",
			commits: []domain.Commit{{SHA: "f1f1f1", Message: "rewrite the totals"}},
			frames:  resolved,
			// The ADR's explicit case: list the commits, mark none, say why.
			contains: "without the files they changed",
		},
		{
			name:     "paths that match nothing",
			release:  "app@1.4.0",
			commits:  []domain.Commit{fileCommit("f1f1f1", "docs", "README.md")},
			frames:   resolved,
			contains: "None of the files this release changed appears",
		},
		{
			name:     "a stacktrace still minified",
			release:  "app@1.4.0",
			commits:  []domain.Commit{fileCommit("f1f1f1", "totals", "src/checkout.ts")},
			frames:   minified,
			contains: "still minified",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var events []ports.StoredEvent
			if test.frames != nil {
				events = []ports.StoredEvent{storedEventWith(t, test.release, test.frames)}
			}
			suspects, _ := suspectsUnderTest(t, test.release, test.commits, events)

			report, err := suspects.For(context.Background(), 1, 7)
			if err != nil {
				t.Fatalf("For: %v", err)
			}
			if len(report.Suspects) != 0 {
				t.Fatalf("got %+v, want nothing", report.Suspects)
			}
			if !strings.Contains(report.Warning, test.contains) {
				t.Fatalf("warning is %q, want it to mention %q", report.Warning, test.contains)
			}
			if test.commits != nil && len(report.Commits) != len(test.commits) {
				t.Fatalf("listed %d candidates, want the %d the release holds",
					len(report.Commits), len(test.commits))
			}
		})
	}
}

func TestSuspectsBoundsTheCandidateList(t *testing.T) {
	commits := make([]domain.Commit, 0, maxListedCommits+5)
	for index := 0; index < maxListedCommits+5; index++ {
		commits = append(commits, fileCommit(
			strings.Repeat("0", 5)+string(rune('a'+index%26))+string(rune('a'+index/26)),
			"noise", "docs/note.md"))
	}
	suspects, _ := suspectsUnderTest(t, "app@1.4.0", commits,
		[]ports.StoredEvent{storedEventWith(t, "app@1.4.0",
			[]sentry.Frame{{Filename: "src/checkout.ts", Lineno: 1}})})

	report, err := suspects.For(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if len(report.Commits) != maxListedCommits {
		t.Fatalf("listed %d commits, want the cap of %d", len(report.Commits), maxListedCommits)
	}
	if report.CommitCount != maxListedCommits+5 {
		t.Fatalf("counted %d commits, want the release's real total", report.CommitCount)
	}
}

func TestSuspectsRefusesAnIssueThatIsNotThere(t *testing.T) {
	suspects, _ := suspectsUnderTest(t, "app@1.4.0", nil, nil)

	if _, err := suspects.For(context.Background(), 1, 999); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Fatalf("got %v, want the issue-not-found sentinel so the transport answers 404", err)
	}
}

func TestSuspectsSurvivesAReleaseThatWasDeleted(t *testing.T) {
	// The resolution columns keep the version string; the release row can be
	// gone. That is a warning, not a 500.
	issues := &suspectIssues{issue: domain.Issue{ID: 7, ProjectID: 1}}
	suspects := NewSuspects(issues, newFakeReleases(), &suspectResolution{first: "app@9.9.9"})

	report, err := suspects.For(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if !strings.Contains(report.Warning, "not registered here") {
		t.Fatalf("warning is %q", report.Warning)
	}
	if report.Release != "app@9.9.9" {
		t.Fatalf("lost the release name: %q", report.Release)
	}
}

func TestSuspectsFallsBackToEveryFrameWhenNothingIsInApp(t *testing.T) {
	// Several browser SDKs never set in_app. Applying ADR 019's "in_app
	// frames" literally there would attribute nothing for the one platform
	// this phase exists for.
	frames := []sentry.Frame{
		{Filename: "src/main.ts", Lineno: 3},
		{Filename: "src/checkout.ts", Lineno: 42},
	}
	suspects, _ := suspectsUnderTest(t, "app@1.4.0",
		[]domain.Commit{fileCommit("f1f1f1", "totals", "src/checkout.ts")},
		[]ports.StoredEvent{storedEventWith(t, "app@1.4.0", frames)})

	report, err := suspects.For(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if len(report.Suspects) != 1 {
		t.Fatalf("got %d suspects, want one", len(report.Suspects))
	}
}

func TestSuspectsIgnoresLibraryFramesWhenAnythingIsInApp(t *testing.T) {
	// A vendored framework file must not be able to name a suspect when the
	// event says which frames are the application's.
	frames := []sentry.Frame{
		{Filename: "src/checkout.ts", Lineno: 42, InApp: true},
		{Filename: "node_modules/react-dom/index.js", Lineno: 900, InApp: false},
	}
	suspects, _ := suspectsUnderTest(t, "app@1.4.0", []domain.Commit{
		fileCommit("f1f1f1", "vendor", "node_modules/react-dom/index.js"),
		fileCommit("a2a2a2", "totals", "src/checkout.ts"),
	}, []ports.StoredEvent{storedEventWith(t, "app@1.4.0", frames)})

	report, err := suspects.For(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if len(report.Suspects) != 1 || report.Suspects[0].Commit.SHA != "a2a2a2" {
		t.Fatalf("got %+v, want only the commit on the application frame", report.Suspects)
	}
}

func TestSuspectsReportsAPayloadItCannotRead(t *testing.T) {
	issues := &suspectIssues{
		issue:  domain.Issue{ID: 7, ProjectID: 1},
		events: []ports.StoredEvent{{ID: 1, Release: "app@1.4.0", Payload: []byte("{not json")}},
	}
	releases := newFakeReleases()
	release, _, err := releases.Create(context.Background(),
		domain.Release{ProjectID: 1, Version: "app@1.4.0"})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := releases.SetCommits(context.Background(), release.ID,
		[]domain.Commit{fileCommit("f1f1f1", "totals", "src/checkout.ts")}); err != nil {
		t.Fatalf("seeding commits: %v", err)
	}
	suspects := NewSuspects(issues, releases, &suspectResolution{first: "app@1.4.0"})

	if _, err := suspects.For(context.Background(), 1, 7); err == nil {
		t.Fatal("a payload this build cannot decode came back as an empty answer")
	}
}
