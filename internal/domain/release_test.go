package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewRelease(t *testing.T) {
	release, err := NewRelease(1, " app@1.0.0\n", testNow)
	if err != nil {
		t.Fatalf("NewRelease: %v", err)
	}
	if release.Version != "app@1.0.0" {
		t.Errorf("Version = %q, want the trimmed form", release.Version)
	}
	if !release.CreatedAt.Equal(testNow) {
		t.Errorf("CreatedAt = %v, want %v", release.CreatedAt, testNow)
	}
	if release.DateReleased != nil || release.FirstEventAt != nil {
		t.Error("a new release should be neither finalised nor seen")
	}
}

func TestNewReleaseRejectsWhatCannotBeStoredOrAddressed(t *testing.T) {
	cases := []struct {
		name      string
		projectID int64
		version   string
		wantErr   error
	}{
		{name: "no project", projectID: 0, version: "1.0.0", wantErr: ErrInvalidProject},
		{name: "empty", projectID: 1, version: "   ", wantErr: ErrInvalidRelease},
		{name: "over long", projectID: 1, version: strings.Repeat("a", MaxVersionLen+1), wantErr: ErrInvalidRelease},
		{name: "newline inside", projectID: 1, version: "app@1.0\n.0", wantErr: ErrInvalidRelease},
		// A slash would make a release that can be created and then never
		// addressed again, because the version is a path segment everywhere.
		{name: "slash", projectID: 1, version: "feature/x", wantErr: ErrInvalidRelease},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewRelease(testCase.projectID, testCase.version, testNow); !errors.Is(err, testCase.wantErr) {
				t.Errorf("error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

func TestFinalizeKeepsTheOriginalDate(t *testing.T) {
	// A retried deploy script must not be able to rewrite when something
	// actually went out.
	release, err := NewRelease(1, "app@1.0.0", testNow)
	if err != nil {
		t.Fatalf("NewRelease: %v", err)
	}

	first := release.Finalize(testNow)
	second := first.Finalize(testNow.Add(72 * time.Hour))

	if first.DateReleased == nil {
		t.Fatal("Finalize did not set a date")
	}
	if !second.DateReleased.Equal(*first.DateReleased) {
		t.Errorf("a second finalize moved the date to %v", second.DateReleased)
	}
	// Value semantics: the receiver is never mutated.
	if release.DateReleased != nil {
		t.Error("Finalize mutated its receiver")
	}
}

func TestCleanCommitsAssignsOrderAndDropsWhatCannotBeStored(t *testing.T) {
	commits, err := CleanCommits([]Commit{
		{SHA: " abc ", Message: "  fix the checkout  ", AuthorName: "Ana"},
		{SHA: "", Message: "an entry with no sha"},
		{SHA: "abc", Message: "the same sha again"},
		{SHA: "def", Files: []CommitFile{
			{Path: " src/app.go ", ChangeType: "m"},
			{Path: "src/app.go", ChangeType: "A"},
			{Path: "   "},
		}},
	})
	if err != nil {
		t.Fatalf("CleanCommits: %v", err)
	}

	if len(commits) != 2 {
		t.Fatalf("kept %d commits, want 2 (the empty sha and the duplicate go)", len(commits))
	}
	if commits[0].SHA != "abc" || commits[0].Message != "fix the checkout" {
		t.Errorf("commit not normalised: %+v", commits[0])
	}
	if commits[0].Ordinal != 0 || commits[1].Ordinal != 1 {
		t.Errorf("ordinals = %d,%d, want 0,1", commits[0].Ordinal, commits[1].Ordinal)
	}
	if len(commits[1].Files) != 1 {
		t.Fatalf("kept %d files, want 1 — the duplicate path and the empty one go", len(commits[1].Files))
	}
	if commits[1].Files[0].ChangeType != ChangeModified {
		t.Errorf("change type = %q, want the normalised M", commits[1].Files[0].ChangeType)
	}
}

func TestCleanCommitsRefusesAnUnboundedSet(t *testing.T) {
	commits := make([]Commit, MaxCommitsPerRelease+1)
	if _, err := CleanCommits(commits); !errors.Is(err, ErrInvalidRelease) {
		t.Errorf("error = %v, want ErrInvalidRelease", err)
	}
}

func TestCleanCommitsTruncatesRatherThanRejects(t *testing.T) {
	// A long commit message is a style, not an attack. Refusing the deploy
	// annotation over it would be the product being precious.
	commits, err := CleanCommits([]Commit{{
		SHA:     "abc",
		Message: strings.Repeat("é", MaxCommitMessageLen+50),
	}})
	if err != nil {
		t.Fatalf("CleanCommits: %v", err)
	}
	if count := len([]rune(commits[0].Message)); count > MaxCommitMessageLen {
		t.Errorf("message kept %d runes, want at most %d", count, MaxCommitMessageLen)
	}
	// Truncation on a rune boundary, or the JSON encoder mangles it later.
	if !isValidUTF8(commits[0].Message) {
		t.Error("truncation produced invalid UTF-8")
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestValidChangeType(t *testing.T) {
	cases := map[ChangeType]ChangeType{
		"A": ChangeAdded, "a": ChangeAdded,
		"D": ChangeDeleted, "d": ChangeDeleted,
		"M": ChangeModified, "": ChangeModified, "whatever": ChangeModified,
	}
	for input, want := range cases {
		if got := ValidChangeType(input); got != want {
			t.Errorf("ValidChangeType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNewDeploy(t *testing.T) {
	deploy, err := NewDeploy(7, " production ", testNow)
	if err != nil {
		t.Fatalf("NewDeploy: %v", err)
	}
	if deploy.Environment != "production" || deploy.ReleaseID != 7 {
		t.Errorf("deploy = %+v", deploy)
	}
	if deploy.FinishedAt == nil || !deploy.FinishedAt.Equal(testNow) {
		t.Errorf("FinishedAt = %v, want %v", deploy.FinishedAt, testNow)
	}
}

func TestNewDeployRejectsIncompleteRecords(t *testing.T) {
	if _, err := NewDeploy(0, "production", testNow); !errors.Is(err, ErrInvalidRelease) {
		t.Errorf("a deploy of no release: %v", err)
	}
	if _, err := NewDeploy(1, "  ", testNow); !errors.Is(err, ErrInvalidRelease) {
		t.Errorf("a deploy to nowhere: %v", err)
	}
	if _, err := NewDeploy(1, strings.Repeat("e", MaxTitleLen+1), testNow); !errors.Is(err, ErrInvalidRelease) {
		t.Errorf("an unbounded environment name: %v", err)
	}
}
