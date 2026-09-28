package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// runReleases dispatches the release subcommands.
//
// These are the commands a deploy pipeline runs, which is why they exist in
// the CLI at the same moment as in the API rather than afterwards: a release
// that can only be registered by hand-writing a curl is a feature nobody
// puts in their pipeline (ADR 006).
func runReleases(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "releases: expected list, create, show, finalize, commits, deploys or health")
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return runReleasesList(c, args[1:])
	case "create":
		return runReleasesCreate(c, args[1:])
	case "show":
		return runReleasesShow(c, args[1:])
	case "finalize":
		return runReleasesFinalize(c, args[1:])
	case "commits":
		return runReleasesCommits(c, args[1:])
	case "deploys":
		return runReleasesDeploys(c, args[1:])
	case "health":
		return runReleasesHealth(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "releases: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type releasePayload struct {
	Version      string     `json:"version"`
	ProjectID    int64      `json:"project_id"`
	CreatedAt    time.Time  `json:"created_at"`
	DateReleased *time.Time `json:"date_released"`
	FirstEventAt *time.Time `json:"first_event_at"`
	LastEventAt  *time.Time `json:"last_event_at"`
	CommitCount  int64      `json:"commit_count"`
}

type releaseDetailPayload struct {
	releasePayload
	NewIssues       int64 `json:"new_issues"`
	RegressedIssues int64 `json:"regressed_issues"`
	Events          int64 `json:"events"`
	Commits         []struct {
		ID         string `json:"id"`
		Message    string `json:"message"`
		AuthorName string `json:"author_name"`
		PatchSet   []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"patch_set"`
	} `json:"commits"`
	Deploys []struct {
		Environment string     `json:"environment"`
		Name        string     `json:"name"`
		URL         string     `json:"url"`
		FinishedAt  *time.Time `json:"finished_at"`
	} `json:"deploys"`
}

// releasesPath builds the path of a project's releases, escaping the version.
//
// Escaped rather than interpolated: a version is a string somebody chose, and
// "myapp@1.0.0+build 7" is not a hypothetical.
func releasesPath(projectID int64, version string) string {
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/releases"
	if version != "" {
		path += "/" + url.PathEscape(version)
	}
	return path
}

func runReleasesList(c *context_, args []string) int {
	flags := newFlagSet(c, "releases list")
	projectID := flags.Int64("project", 0, "project id (required)")
	limit := flags.Int("limit", 0, "how many releases to return")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "releases list: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := releasesPath(*projectID, "")
	if *limit > 0 {
		path += "?limit=" + strconv.Itoa(*limit)
	}

	var page struct {
		Releases []releasePayload `json:"releases"`
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Releases) == 0 {
		return c.emit(*remote.asJSON, page, "no releases")
	}

	var text strings.Builder
	for index := range page.Releases {
		release := &page.Releases[index]
		state := "unreleased"
		if release.DateReleased != nil {
			state = release.DateReleased.Format(time.RFC3339)
		}
		fmt.Fprintf(&text, "%s\t%s\t%d commits\n", release.Version, state, release.CommitCount)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

func runReleasesCreate(c *context_, args []string) int {
	flags := newFlagSet(c, "releases create")
	projectID := flags.Int64("project", 0, "project id (required)")
	version := flags.String("version", "", "release version, e.g. myapp@1.4.0 (required)")
	released := flags.String("released", "", "mark it shipped at this RFC 3339 time")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *version == "" {
		fmt.Fprintln(c.stderr, "releases create: -project and -version are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	body := map[string]any{"version": *version}
	if *released != "" {
		at, err := time.Parse(time.RFC3339, *released)
		if err != nil {
			fmt.Fprintf(c.stderr, "releases create: -released must be RFC 3339: %v\n", err)
			return ExitUsage
		}
		body["date_released"] = at
	}

	var release releasePayload
	if err := client.do(c.ctx, http.MethodPost, releasesPath(*projectID, ""), body, &release); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, release, release.Version)
}

func runReleasesShow(c *context_, args []string) int {
	flags := newFlagSet(c, "releases show")
	projectID := flags.Int64("project", 0, "project id (required)")
	version := flags.String("version", "", "release version (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *version == "" {
		fmt.Fprintln(c.stderr, "releases show: -project and -version are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var detail releaseDetailPayload
	if err := client.do(c.ctx, http.MethodGet, releasesPath(*projectID, *version), nil, &detail); err != nil {
		return c.fail(err)
	}

	var text strings.Builder
	fmt.Fprintf(&text, "%s\n", detail.Version)
	if detail.DateReleased != nil {
		fmt.Fprintf(&text, "  released   %s\n", detail.DateReleased.Format(time.RFC3339))
	} else {
		fmt.Fprintf(&text, "  released   not yet\n")
	}
	fmt.Fprintf(&text, "  new        %d issues\n", detail.NewIssues)
	fmt.Fprintf(&text, "  regressed  %d issues\n", detail.RegressedIssues)
	fmt.Fprintf(&text, "  events     %d\n", detail.Events)
	fmt.Fprintf(&text, "  commits    %d\n", len(detail.Commits))
	for _, deploy := range detail.Deploys {
		fmt.Fprintf(&text, "  deploy     %s %s\n", deploy.Environment, deploy.Name)
	}
	return c.emit(*remote.asJSON, detail, strings.TrimRight(text.String(), "\n"))
}

func runReleasesFinalize(c *context_, args []string) int {
	flags := newFlagSet(c, "releases finalize")
	projectID := flags.Int64("project", 0, "project id (required)")
	version := flags.String("version", "", "release version (required)")
	released := flags.String("released", "", "the RFC 3339 time it shipped (default: now)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *version == "" {
		fmt.Fprintln(c.stderr, "releases finalize: -project and -version are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	body := map[string]any{}
	if *released != "" {
		at, err := time.Parse(time.RFC3339, *released)
		if err != nil {
			fmt.Fprintf(c.stderr, "releases finalize: -released must be RFC 3339: %v\n", err)
			return ExitUsage
		}
		body["date_released"] = at
	}

	var release releasePayload
	if err := client.do(c.ctx, http.MethodPut, releasesPath(*projectID, *version), body, &release); err != nil {
		return c.fail(err)
	}
	text := release.Version + " released"
	if release.DateReleased != nil {
		text += " at " + release.DateReleased.Format(time.RFC3339)
	}
	return c.emit(*remote.asJSON, release, text)
}

// runReleasesCommits associates a commit set with a release.
//
// Two ways in. `-repo` reads the commits out of a local checkout with `git log
// --name-status`, which is the one that matters: attribution is the
// intersection between the files a release changed and the files a stacktrace
// names, so a commit set without paths is a commit set that can never name a
// suspect (ADR 019). It is also the reason this exists rather than deferring
// to `sentry-cli releases set-commits --local`: a product whose flagship
// answer requires somebody else's tool, with a flag they have to know about,
// does not have that answer (ADR 006).
//
// Without `-repo`, the set arrives as JSON on a file or stdin, because it is a
// list of records and a pipeline that built it some other way already has it
// in that shape. Inventing a flag syntax for nested records would be a small
// language nobody asked for.
func runReleasesCommits(c *context_, args []string) int {
	flags := newFlagSet(c, "releases commits")
	projectID := flags.Int64("project", 0, "project id (required)")
	version := flags.String("version", "", "release version (required)")
	file := flags.String("file", "-", `JSON commits, "-" for stdin (ignored with -repo)`)
	repo := flags.String("repo", "", "read the commits from this git checkout instead")
	// Empty is legal and means the whole history, which is what a first deploy
	// has. It is also the one value that quietly weakens suspect commits: an
	// initial import created every file the stacktrace names, so it matches
	// more frames than the commit that actually broke something and outranks
	// it. Attribution is only as narrow as the range it is given (ADR 019),
	// and the flag is where somebody finds that out.
	from := flags.String("from", "",
		"the revision the range starts after — usually the previous release's sha;\n"+
			"\tempty means the whole history, which makes the initial commit a suspect")
	to := flags.String("to", "HEAD", "the revision the range ends at")
	limit := flags.Int("max", defaultGitLogLimit, "how many commits to read from -repo at most")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *version == "" {
		fmt.Fprintln(c.stderr, "releases commits: -project and -version are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	body, count, err := commitSetFrom(c, *repo, *from, *to, *limit, *file)
	if err != nil {
		return c.fail(err)
	}
	if *repo != "" && count == 0 {
		// A range that is empty is almost always a wrong one — `-from` naming
		// a revision that is already an ancestor of nothing, or the two
		// swapped. Sending it would replace a good commit set with nothing.
		return c.fail(fmt.Errorf(
			"no commits between %q and %q in %s; nothing was sent",
			*from, *to, *repo))
	}

	var result struct {
		Commits int `json:"commits"`
	}
	path := releasesPath(*projectID, *version) + "/commits"
	if err := client.do(c.ctx, http.MethodPost, path, body, &result); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, result, fmt.Sprintf("%d commits", result.Commits))
}

// commitSetFrom builds the request body from whichever source was named, and
// reports how many commits it holds.
func commitSetFrom(
	c *context_, repo, from, to string, limit int, file string,
) (body any, count int, err error) {
	if repo != "" {
		commits, err := gitCommits(c.ctx, repo, from, to, limit)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"commits": commits}, len(commits), nil
	}

	raw, err := readCommitSet(file)
	if err != nil {
		return nil, 0, err
	}
	// Decoded here rather than forwarded blind, so a malformed file is an
	// error from the command the operator ran and not a 400 from a server
	// they may not even be able to read the logs of.
	var decoded struct {
		Commits []json.RawMessage `json:"commits"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, 0, fmt.Errorf("reading the commit set: %w", err)
	}
	return decoded, len(decoded.Commits), nil
}

// maxCommitSetInput bounds what the CLI will read from standard input. It
// matches the server's own budget for the same body, so a set that would be
// refused is refused here, where the error names the file.
const maxCommitSetInput = 8 * 1024 * 1024

// readCommitSet reads a commit set from a file or standard input.
//
// A bare array is accepted as well as the documented object, because that is
// what `git log --format` pipelines produce and rejecting it would make the
// commonest input the one that does not work.
func readCommitSet(path string) ([]byte, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, maxCommitSetInput))
	} else {
		raw, err = os.ReadFile(path) //nolint:gosec // the operator names the file.
	}
	if err != nil {
		return nil, fmt.Errorf("reading commits: %w", err)
	}
	if trimmed := strings.TrimSpace(string(raw)); strings.HasPrefix(trimmed, "[") {
		return []byte(`{"commits":` + trimmed + `}`), nil
	}
	return raw, nil
}

func runReleasesDeploys(c *context_, args []string) int {
	flags := newFlagSet(c, "releases deploys")
	projectID := flags.Int64("project", 0, "project id (required)")
	version := flags.String("version", "", "release version (required)")
	environment := flags.String("environment", "", "where it went, e.g. production (required)")
	name := flags.String("name", "", "a label for this deploy, e.g. the pipeline id")
	// Named -link and not -url: every remote command already has a -url, and
	// it means the server to talk to. Two flags spelled the same on one
	// command is a panic at registration, which is how this was found.
	deployURL := flags.String("link", "", "a link back to the pipeline that deployed it")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *version == "" || *environment == "" {
		fmt.Fprintln(c.stderr, "releases deploys: -project, -version and -environment are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	body := map[string]any{"environment": *environment, "name": *name, "url": *deployURL}
	var deploy struct {
		ID          int64  `json:"id"`
		Environment string `json:"environment"`
	}
	path := releasesPath(*projectID, *version) + "/deploys"
	if err := client.do(c.ctx, http.MethodPost, path, body, &deploy); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, deploy, *version+" deployed to "+deploy.Environment)
}
