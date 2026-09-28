package domain

import (
	"fmt"
	"strings"
	"time"
)

// MaxCommitMessageLen bounds a stored commit message. The first paragraph is
// what a suspect-commit line shows; a full essay in the body helps nobody
// reading an issue and is already in the repository it came from.
const MaxCommitMessageLen = 1000

// MaxCommitsPerRelease bounds one set-commits call. A release with more
// commits than this is a first deploy or a squashed year of history, and
// storing all of it would make the request the largest write the product
// accepts by two orders of magnitude.
const MaxCommitsPerRelease = 5000

// MaxPathLen bounds a changed file's path.
const MaxPathLen = 500

// Release is a version of the software that produced events.
//
// It is an entity rather than the string on an event because three questions
// need somewhere to live: when it was deployed, what went into it, and what
// broke because of it. A column on the event table can answer none of them.
type Release struct {
	ID        int64
	ProjectID int64
	// Version is the identifier the SDK sends, unique within a project.
	Version string
	// CreatedAt is when this installation first learned the version exists,
	// whether from a deploy tool or from the first event carrying it.
	CreatedAt time.Time
	// DateReleased is when it was declared finished. Nil until someone
	// finalises it, which is what tells "we are still deploying this" apart
	// from "this is live".
	DateReleased *time.Time
	// FirstEventAt and LastEventAt bound the events seen from this release.
	// Nil while it has produced none, which is the good case and worth being
	// able to see.
	FirstEventAt *time.Time
	LastEventAt  *time.Time
	// CommitCount is how many commits were associated with it.
	CommitCount int64
}

// NewRelease builds a release from a version string.
func NewRelease(projectID int64, version string, now time.Time) (Release, error) {
	if projectID <= 0 {
		return Release{}, fmt.Errorf("%w: project id must be positive", ErrInvalidProject)
	}
	version, err := CleanVersion(version)
	if err != nil {
		return Release{}, err
	}
	return Release{
		ProjectID: projectID,
		Version:   version,
		CreatedAt: now.UTC(),
	}, nil
}

// CleanVersion validates and normalises a version string.
//
// Whitespace is trimmed rather than rejected because it arrives from shell
// pipelines — `git describe` with a trailing newline is the classic — and a
// release that differs from another only by an invisible character is a
// duplicate nobody can see or fix.
func CleanVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	switch {
	case version == "":
		return "", fmt.Errorf("%w: version is required", ErrInvalidRelease)
	case len(version) > MaxVersionLen:
		return "", fmt.Errorf("%w: version exceeds %d characters", ErrInvalidRelease, MaxVersionLen)
	case strings.ContainsAny(version, "\n\r\t"):
		return "", fmt.Errorf("%w: version contains a control character", ErrInvalidRelease)
	case strings.Contains(version, "/"):
		// The version is a path segment in every REST route that names one.
		// Allowing a slash would mean a release that can be created and then
		// never addressed again.
		return "", fmt.Errorf("%w: version cannot contain a slash", ErrInvalidRelease)
	}
	return version, nil
}

// Finalize marks a release as shipped.
//
// Finalising an already finalised release keeps the original date: a retried
// deploy script must not be able to rewrite when something actually went out.
func (r Release) Finalize(at time.Time) Release {
	if r.DateReleased != nil {
		return r
	}
	released := at.UTC()
	r.DateReleased = &released
	return r
}

// ChangeType is what a commit did to a file.
type ChangeType string

// The change types the protocol's patch set uses.
const (
	// ChangeAdded is a file the commit created.
	ChangeAdded ChangeType = "A"
	// ChangeModified is a file the commit edited.
	ChangeModified ChangeType = "M"
	// ChangeDeleted is a file the commit removed.
	ChangeDeleted ChangeType = "D"
)

// ValidChangeType normalises a change type, defaulting to modified.
func ValidChangeType(change ChangeType) ChangeType {
	switch ChangeType(strings.ToUpper(string(change))) {
	case ChangeAdded:
		return ChangeAdded
	case ChangeDeleted:
		return ChangeDeleted
	case ChangeModified:
		return ChangeModified
	default:
		return ChangeModified
	}
}

// CommitFile is one path a commit touched.
//
// The paths are stored, and not just the commit list, because they are the
// entire input to answering "which commit probably caused this issue": the
// intersection between the files in a stacktrace and the files a release
// changed. Recording them at set-commits time is what makes that answer a
// lookup later instead of a clone of the repository.
type CommitFile struct {
	Path       string
	ChangeType ChangeType
}

// Commit is one change that went into a release.
type Commit struct {
	SHA         string
	Message     string
	AuthorName  string
	AuthorEmail string
	// Timestamp is when the commit was authored. Nil when the sender did not
	// say, which several tools do not.
	Timestamp *time.Time
	// Repository is which repository it came from, for a release assembled
	// from more than one.
	Repository string
	// Ordinal preserves the order the sender listed them in, which is newest
	// first for every tool that sends them.
	Ordinal int
	// Files are the paths this commit touched, when the sender included them.
	Files []CommitFile
}

// CleanCommits validates and normalises a set of commits, assigning ordinals.
//
// Commits with no sha are dropped rather than rejected: a sender that
// includes an empty entry has a bug in its own pipeline, and failing the
// whole deploy annotation over it helps nobody. A duplicate sha keeps the
// first occurrence, because the storage key is (release, sha) and the
// alternative is a write that fails halfway.
func CleanCommits(commits []Commit) ([]Commit, error) {
	if len(commits) > MaxCommitsPerRelease {
		return nil, fmt.Errorf("%w: at most %d commits per release, got %d",
			ErrInvalidRelease, MaxCommitsPerRelease, len(commits))
	}

	cleaned := make([]Commit, 0, len(commits))
	seen := make(map[string]bool, len(commits))
	for _, commit := range commits {
		commit.SHA = strings.TrimSpace(commit.SHA)
		if commit.SHA == "" || seen[commit.SHA] {
			continue
		}
		seen[commit.SHA] = true

		commit.Message = truncate(strings.TrimSpace(commit.Message), MaxCommitMessageLen)
		commit.AuthorName = truncate(strings.TrimSpace(commit.AuthorName), MaxTitleLen)
		commit.AuthorEmail = truncate(strings.TrimSpace(commit.AuthorEmail), MaxTitleLen)
		commit.Repository = truncate(strings.TrimSpace(commit.Repository), MaxTitleLen)
		commit.Ordinal = len(cleaned)
		commit.Files = cleanFiles(commit.Files)
		cleaned = append(cleaned, commit)
	}
	return cleaned, nil
}

func cleanFiles(files []CommitFile) []CommitFile {
	if len(files) == 0 {
		return nil
	}
	cleaned := make([]CommitFile, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		path := truncate(strings.TrimSpace(file.Path), MaxPathLen)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		cleaned = append(cleaned, CommitFile{Path: path, ChangeType: ValidChangeType(file.ChangeType)})
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

// Deploy is one release going out to one environment.
type Deploy struct {
	ID          int64
	ReleaseID   int64
	Environment string
	// Name is the human label a deploy tool gives a run, e.g. a pipeline id.
	Name string
	// URL points back at the pipeline that did it.
	URL        string
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// NewDeploy builds a deploy record.
func NewDeploy(releaseID int64, environment string, now time.Time) (Deploy, error) {
	if releaseID <= 0 {
		return Deploy{}, fmt.Errorf("%w: release id must be positive", ErrInvalidRelease)
	}
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return Deploy{}, fmt.Errorf("%w: environment is required", ErrInvalidRelease)
	}
	if len(environment) > MaxTitleLen {
		return Deploy{}, fmt.Errorf("%w: environment exceeds %d characters", ErrInvalidRelease, MaxTitleLen)
	}
	finished := now.UTC()
	return Deploy{
		ReleaseID:   releaseID,
		Environment: environment,
		FinishedAt:  &finished,
	}, nil
}

// truncate cuts a string on a rune boundary, so a multi-byte character is
// never split into invalid UTF-8 that a JSON encoder would then mangle.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
