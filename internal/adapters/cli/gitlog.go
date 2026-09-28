package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Reading a commit set out of a local repository.
//
// It exists because the whole of suspect-commit attribution rests on one
// thing: the *paths* a commit touched (ADR 019). `sentry-cli releases
// set-commits` sends them only with `--local`, and every pipeline that forgot
// that flag has a release page full of commits and no suspects — the failure
// this product should not require somebody else's tool to avoid (ADR 006).
//
// `git log --name-status` and nothing else. No GitHub integration, no clone,
// no credentials: the pipeline that deploys the code already has the checkout
// in front of it, which is the one place this information is free.

// gitFieldSeparator and gitRecordSeparator delimit the log output.
//
// ASCII unit and record separators, because a commit message contains
// newlines, tabs, quotes and — routinely — lines that look exactly like the
// `M<tab>path` entries that follow it. These two bytes are the only ones a
// commit message reliably does not contain.
const (
	gitFieldSeparator  = "\x1f"
	gitRecordSeparator = "\x1e"
)

// gitLogFormat lays out one commit: sha, author, email, ISO date, message,
// and a closing separator that tells the message apart from the file list.
//
// The trailing separator is the load-bearing part. Without it, a message
// whose last line reads `M	src/checkout.ts` would be parsed as a changed
// file, and the commit that "touched" it would be accused of a crash it had
// nothing to do with.
const gitLogFormat = gitRecordSeparator + "%H" + gitFieldSeparator + "%an" +
	gitFieldSeparator + "%ae" + gitFieldSeparator + "%aI" +
	gitFieldSeparator + "%B" + gitFieldSeparator

// defaultGitLogLimit bounds a range nobody bounded.
//
// A first deploy has no previous release to start from, so `-from` is often
// empty and the range is the whole history of the repository. A thousand
// commits is far more than any deploy actually ships and still an order of
// magnitude under the server's own ceiling (domain.MaxCommitsPerRelease), so
// the refusal a user hits is this flag rather than a 400 from a server.
const defaultGitLogLimit = 1000

// gitTimeout bounds the subprocess. A `git log` over a large repository is
// seconds; anything past a minute is a repository on a dead network mount or
// a pager waiting for a terminal that is not there.
const gitTimeout = time.Minute

// commitFilePayload is one changed path, in the protocol's vocabulary.
type commitFilePayload struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// commitPayload is one commit as the API takes it.
type commitPayload struct {
	ID          string              `json:"id"`
	Message     string              `json:"message,omitempty"`
	AuthorName  string              `json:"author_name,omitempty"`
	AuthorEmail string              `json:"author_email,omitempty"`
	Timestamp   *time.Time          `json:"timestamp,omitempty"`
	Repository  string              `json:"repository,omitempty"`
	PatchSet    []commitFilePayload `json:"patch_set,omitempty"`
}

// gitCommits reads a revision range out of a repository, paths included.
func gitCommits(ctx context.Context, repo, from, to string, limit int) ([]commitPayload, error) {
	revision, err := gitRevisionRange(from, to)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultGitLogLimit
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// `core.quotePath=false` so a path with an accent in it arrives as the
	// path and not as `"src/a\303\261o.ts"`, which would match nothing in a
	// stacktrace. `--no-pager` because a pager attached to a pipe is a hang.
	// The trailing `--` closes the revision list, so a revision that starts
	// with a dash cannot become an option — and gitRevisionRange refuses one
	// anyway, because defence in depth costs one `if` here.
	args := []string{
		"-C", repo, "--no-pager", "-c", "core.quotePath=false",
		"log", "--name-status", "--no-color",
		fmt.Sprintf("--max-count=%d", limit),
		"--format=" + gitLogFormat,
		revision, "--",
	}
	//nolint:gosec // The arguments are a path and two revisions the operator
	// typed, passed as argv and never through a shell; the range is validated
	// above and terminated by "--".
	command := exec.CommandContext(ctx, "git", args...)
	var stderr strings.Builder
	command.Stderr = &stderr

	output, err := command.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("reading commits from %s: git is not installed", repo)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("reading commits from %s: %s", repo, detail)
	}
	return parseGitLog(string(output)), nil
}

// gitRevisionRange turns two revisions into the range git takes.
func gitRevisionRange(from, to string) (string, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if to == "" {
		to = "HEAD"
	}
	for _, revision := range []string{from, to} {
		if strings.HasPrefix(revision, "-") {
			return "", fmt.Errorf("a revision cannot start with a dash: %q", revision)
		}
	}
	if from == "" {
		// No previous release to diff against — a first deploy. The window is
		// whatever --max-count allows, which is why that flag has a default
		// rather than being unbounded.
		return to, nil
	}
	return from + ".." + to, nil
}

// parseGitLog turns the log output into commits.
//
// Newest first, which is the order `git log` emits and the order every tool
// that sends a commit set means (domain.CleanCommits assigns the ordinal from
// it). Nothing is rejected here: a commit with no changed files is a merge,
// and dropping it would silently shorten a release's history.
func parseGitLog(output string) []commitPayload {
	records := strings.Split(output, gitRecordSeparator)
	commits := make([]commitPayload, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.SplitN(record, gitFieldSeparator, 6)
		if len(fields) < 6 {
			continue
		}

		commit := commitPayload{
			ID:          strings.TrimSpace(fields[0]),
			AuthorName:  strings.TrimSpace(fields[1]),
			AuthorEmail: strings.TrimSpace(fields[2]),
			Message:     strings.TrimSpace(fields[4]),
		}
		if commit.ID == "" {
			continue
		}
		if stamp, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[3])); err == nil {
			utc := stamp.UTC()
			commit.Timestamp = &utc
		}
		commit.PatchSet = parseNameStatus(fields[5])
		commits = append(commits, commit)
	}
	return commits
}

// parseNameStatus reads the `M<tab>path` block that follows one commit.
func parseNameStatus(block string) []commitFilePayload {
	var files []commitFilePayload
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		files = append(files, nameStatusEntry(parts)...)
	}
	return files
}

// nameStatusEntry turns one status line into the paths it means.
//
// A rename is two facts, not one: the old path stopped existing and the new
// one started. Recording only the new path would lose the half that matters
// when the crash is in an event from *before* the rename, whose stacktrace
// still names the old file.
func nameStatusEntry(parts []string) []commitFilePayload {
	status := parts[0]
	if status == "" {
		return nil
	}
	switch status[0] {
	case 'R':
		if len(parts) < 3 {
			return nil
		}
		return []commitFilePayload{
			{Path: parts[2], Type: "A"},
			{Path: parts[1], Type: "D"},
		}
	case 'C':
		// A copy creates the destination and leaves the source alone.
		if len(parts) < 3 {
			return nil
		}
		return []commitFilePayload{{Path: parts[2], Type: "A"}}
	case 'A', 'D', 'M':
		return []commitFilePayload{{Path: parts[1], Type: string(status[0])}}
	default:
		// T (type change), U (unmerged), X (a bug in git). Recorded as a
		// modification, which is what the domain would normalise them to
		// anyway, rather than dropped: the file was touched.
		return []commitFilePayload{{Path: parts[1], Type: "M"}}
	}
}
