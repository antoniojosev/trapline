package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runIssues dispatches the issue subcommands.
//
// These are the commands an agent uses: list what is broken, read one in full,
// mark it resolved after fixing it. They exist in the CLI at the same time as
// in the panel rather than afterwards, because a feature that reaches the CLI
// late is a feature the agent-first story does not actually have (ADR 006).
func runIssues(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "issues: expected list, show, bundle, suspects, resolve, ignore or reopen")
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return runIssuesList(c, args[1:])
	case "show":
		return runIssuesShow(c, args[1:])
	case "bundle":
		return runIssuesBundle(c, args[1:])
	case "suspects":
		return runIssuesSuspects(c, args[1:])
	case "resolve":
		return runIssueStatus(c, "resolve", args[1:], "resolved")
	case "ignore":
		return runIssueStatus(c, "ignore", args[1:], "ignored")
	case "reopen":
		return runIssueStatus(c, "reopen", args[1:], "unresolved")
	default:
		fmt.Fprintf(c.stderr, "issues: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type issuePayload struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	Fingerprint string    `json:"fingerprint"`
	Title       string    `json:"title"`
	Culprit     string    `json:"culprit"`
	Level       string    `json:"level"`
	Status      string    `json:"status"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Times       int64     `json:"times"`
	LastRelease string    `json:"last_release,omitempty"`
}

func runIssuesList(c *context_, args []string) int {
	flags := newFlagSet(c, "issues list")
	projectID := flags.Int64("project", 0, "project id (required)")
	status := flags.String("status", "", "filter by status: unresolved, resolved, ignored")
	query := flags.String("q", "", "filter by title or culprit")
	environment := flags.String("environment", "", "filter by environment")
	release := flags.String("release", "", "filter by release")
	limit := flags.Int("limit", 0, "how many issues to return")
	cursor := flags.String("cursor", "", "continue from a previous page")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "issues list: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/issues"
	filters := map[string]string{
		"status":      *status,
		"q":           *query,
		"environment": *environment,
		"release":     *release,
		"cursor":      *cursor,
	}
	if *limit > 0 {
		filters["limit"] = strconv.Itoa(*limit)
	}
	if params := issueQuery(filters); params != "" {
		path += "?" + params
	}

	var page issuePagePayload
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Issues) == 0 {
		return c.emit(*remote.asJSON, page, "no issues")
	}

	var text strings.Builder
	for index := range page.Issues {
		issue := &page.Issues[index]
		fmt.Fprintf(&text, "%d\t%s\t%s\t%d×\t%s\n",
			issue.ID, issue.Status, issue.Title, issue.Times, issue.Culprit)
	}
	// The counts and the cursor are printed rather than dropped: a list that
	// silently shows the first page of many is a list somebody trusts as
	// complete.
	fmt.Fprintf(&text, "\n%d unresolved, %d resolved, %d ignored",
		page.Counts["unresolved"], page.Counts["resolved"], page.Counts["ignored"])
	if page.NextCursor != "" {
		fmt.Fprintf(&text, "\nmore: -cursor %s", page.NextCursor)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

// issuePagePayload mirrors the API's list response.
type issuePagePayload struct {
	Issues     []issuePayload   `json:"issues"`
	NextCursor string           `json:"next_cursor"`
	Counts     map[string]int64 `json:"counts"`
}

func issueQuery(values map[string]string) string {
	var params []string
	for name, value := range values {
		if value != "" {
			params = append(params, name+"="+url.QueryEscape(value))
		}
	}
	sort.Strings(params)
	return strings.Join(params, "&")
}

// runIssuesShow prints one issue in full.
//
// The JSON form is the issue bundle in embryo: everything an agent needs to
// fix the bug without asking a follow-up question — the stacktrace, the tags,
// the release, the recent occurrences.
func runIssuesShow(c *context_, args []string) int {
	flags := newFlagSet(c, "issues show")
	projectID := flags.Int64("project", 0, "project id (required)")
	issueID := flags.Int64("issue", 0, "issue id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *issueID <= 0 {
		fmt.Fprintln(c.stderr, "issues show: -project and -issue are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var detail struct {
		issuePayload
		FirstRelease               string     `json:"first_release"`
		ResolvedAt                 *time.Time `json:"resolved_at"`
		ResolvedInRelease          string     `json:"resolved_in_release"`
		ResolveNextRelease         bool       `json:"resolve_next_release"`
		Regressions                int64      `json:"regressions"`
		RegressedInRelease         string     `json:"regressed_in_release"`
		SeenInResolvedReleaseCount int64      `json:"seen_in_resolved_release_count"`
		Events                     []struct {
			OccurredAt  time.Time `json:"occurred_at"`
			Level       string    `json:"level"`
			Release     string    `json:"release"`
			Environment string    `json:"environment"`
		} `json:"events"`
		Tags map[string][]struct {
			Value string `json:"value"`
			Count int64  `json:"count"`
		} `json:"tags"`
	}
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/issues/" + strconv.FormatInt(*issueID, 10)
	if err := client.do(c.ctx, http.MethodGet, path, nil, &detail); err != nil {
		return c.fail(err)
	}

	if *remote.asJSON {
		// Re-fetched raw so the payloads travel through untouched: an agent
		// wants the whole stored event, not this command's summary of it.
		var raw any
		if err := client.do(c.ctx, http.MethodGet, path, nil, &raw); err != nil {
			return c.fail(err)
		}
		return c.emit(true, raw, "")
	}

	var text strings.Builder
	fmt.Fprintf(&text, "#%d  %s\n%s\n\n", detail.ID, detail.Status, detail.Title)
	fmt.Fprintf(&text, "  culprit    %s\n", detail.Culprit)
	fmt.Fprintf(&text, "  level      %s\n", detail.Level)
	fmt.Fprintf(&text, "  seen       %d times, %s to %s\n",
		detail.Times, detail.FirstSeen.Format(time.RFC3339), detail.LastSeen.Format(time.RFC3339))
	if detail.LastRelease != "" {
		fmt.Fprintf(&text, "  release    %s\n", detail.LastRelease)
	}
	if detail.FirstRelease != "" {
		fmt.Fprintf(&text, "  first in   %s\n", detail.FirstRelease)
	}
	if detail.ResolveNextRelease {
		// The line an agent reads to know why a resolved issue is still
		// receiving events without being a regression.
		fmt.Fprintf(&text, "  resolved   in %s, waiting for the next release\n", detail.ResolvedInRelease)
	}
	if detail.SeenInResolvedReleaseCount > 0 {
		fmt.Fprintf(&text, "  suppressed %d events from the resolved release\n",
			detail.SeenInResolvedReleaseCount)
	}
	if detail.Regressions > 0 {
		fmt.Fprintf(&text, "  regressed  %d times, most recently in %s\n",
			detail.Regressions, detail.RegressedInRelease)
	}
	for key, values := range detail.Tags {
		if len(values) > 0 {
			fmt.Fprintf(&text, "  %-10s %s (%d)\n", key, values[0].Value, values[0].Count)
		}
	}
	fmt.Fprintf(&text, "\n  %d recent events\n", len(detail.Events))

	return c.emit(false, detail, strings.TrimRight(text.String(), "\n"))
}

// runIssueStatus backs resolve, ignore and reopen.
//
// `verb` and `status` are both parameters and they are not the same word: the
// verb is the command, the status is what the API is told. The verb arrives as
// a literal from each case rather than as `args[0]`, which is the same string
// — the switch above matched on it — but is user input, and handing user input
// to a function that builds an HTTP request makes gosec's taint analysis
// right to complain about a URL nobody can actually influence. Deriving
// the usage line from the status printed "Usage of issues resolved:" and
// "Usage of issues unresolved:" — a help message naming two commands that do
// not exist, in the one place somebody looks when they already got the
// invocation wrong.
func runIssueStatus(c *context_, verb string, args []string, status string) int {
	flags := newFlagSet(c, "issues "+verb)
	projectID := flags.Int64("project", 0, "project id (required)")
	issueID := flags.Int64("issue", 0, "issue id (required)")
	// Only meaningful for resolve, and declared only there: a --next-release
	// on `issues ignore` would be a flag whose only possible outcome is an
	// error, which is a worse experience than not having it.
	var nextRelease *bool
	if status == "resolved" {
		nextRelease = flags.Bool("next-release", false,
			"fixed, and the fix ships next: only a newer release counts as a regression")
	}
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *issueID <= 0 {
		fmt.Fprintln(c.stderr, "issues: -project and -issue are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := "/projects/" + strconv.FormatInt(*projectID, 10) +
		"/issues/" + strconv.FormatInt(*issueID, 10) + "/status"
	body := map[string]any{"status": status}
	text := status
	if nextRelease != nil && *nextRelease {
		body["in_next_release"] = true
		text = "resolved in next release"
	}
	if err := client.do(c.ctx, http.MethodPost, path, body, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, body, text)
}

// runIssuesBundle prints one issue as the document an agent reads.
//
// The same bytes the MCP tool `get_issue_bundle` returns and the same bytes
// `GET .../bundle` serves, because all three are the one endpoint (ADR 006).
// It exists as a command so that the agent-first story does not require MCP:
// `trapline issues bundle -project 1 -issue 42` piped into a prompt is the
// whole workflow, on a machine where nobody has configured anything.
func runIssuesBundle(c *context_, args []string) int {
	flags := newFlagSet(c, "issues bundle")
	projectID := flags.Int64("project", 0, "project id (required)")
	issueID := flags.Int64("issue", 0, "issue id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *issueID <= 0 {
		fmt.Fprintln(c.stderr, "issues bundle: -project and -issue are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := "/projects/" + strconv.FormatInt(*projectID, 10) +
		"/issues/" + strconv.FormatInt(*issueID, 10) + "/bundle"
	document, err := client.fetch(c.ctx, path, "text/markdown")
	if err != nil {
		return c.fail(err)
	}

	if *remote.asJSON {
		// --json wraps the document rather than restructuring it. The point
		// of the bundle is that it is already prose; a JSON form that split
		// it back into fields would be a third shape of the same answer, and
		// the one caller who wants fields has `issues show --json` (ADR 006).
		return c.emit(true, bundlePayload{
			ProjectID: *projectID,
			IssueID:   *issueID,
			Bundle:    string(document),
		}, "")
	}

	// Written through untouched, not through emit: this command's output *is*
	// the server's body, and the trailing newline emit would add or trim is
	// the difference between `trapline issues bundle > issue.md` producing
	// the same file the endpoint serves and producing one that differs by a
	// byte. The gate diffs them, so the byte matters.
	fmt.Fprint(c.stdout, string(document))
	return ExitOK
}

// bundlePayload is the --json form of a document.
type bundlePayload struct {
	ProjectID int64  `json:"project_id"`
	IssueID   int64  `json:"issue_id"`
	Bundle    string `json:"bundle"`
}

// runIssuesSuspects asks which change probably caused an issue.
//
// A command of its own rather than more lines under `issues show`, for the
// reason the endpoint is separate: it reads a release's whole commit set and
// decodes a stored payload, and it is the one answer on that screen that is a
// guess. An agent that wants the stacktrace should not pay for the guess, and
// one that wants the guess should be able to tell it failing apart from the
// issue failing (ADR 019).
func runIssuesSuspects(c *context_, args []string) int {
	flags := newFlagSet(c, "issues suspects")
	projectID := flags.Int64("project", 0, "project id (required)")
	issueID := flags.Int64("issue", 0, "issue id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *issueID <= 0 {
		fmt.Fprintln(c.stderr, "issues suspects: -project and -issue are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var report suspectsPayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) +
		"/issues/" + strconv.FormatInt(*issueID, 10) + "/suspects"
	if err := client.do(c.ctx, http.MethodGet, path, nil, &report); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, report, report.text())
}

// suspectsPayload is the answer, and the reasons it may be empty.
type suspectsPayload struct {
	Release  string `json:"release"`
	Suspects []struct {
		ID         string  `json:"id"`
		Message    string  `json:"message"`
		AuthorName string  `json:"author_name"`
		Score      float64 `json:"score"`
		Reasons    []struct {
			Path       string `json:"path"`
			Type       string `json:"type"`
			FrameDepth int    `json:"frame_depth"`
		} `json:"reasons"`
	} `json:"suspects"`
	Commits []struct {
		ID         string `json:"id"`
		Message    string `json:"message"`
		AuthorName string `json:"author_name"`
	} `json:"commits"`
	CommitCount  int    `json:"commit_count"`
	Warning      string `json:"warning"`
	Symbolicated bool   `json:"symbolicated"`
}

// text renders the answer for a person, warning included.
//
// The warning is printed on stdout with the rest rather than on stderr,
// because it is the answer when there are no suspects — and an answer that
// only reaches a terminal nobody is reading is the same as no answer at all.
func (s suspectsPayload) text() string {
	var text strings.Builder
	if s.Release != "" {
		fmt.Fprintf(&text, "first seen in %s, %d commits\n", s.Release, s.CommitCount)
	}

	for index, suspect := range s.Suspects {
		fmt.Fprintf(&text, "\n%d. %s  %s\n", index+1, shortSHA(suspect.ID), firstLine(suspect.Message))
		if suspect.AuthorName != "" {
			fmt.Fprintf(&text, "   by %s\n", suspect.AuthorName)
		}
		for _, reason := range suspect.Reasons {
			fmt.Fprintf(&text, "   touched %s (%s), frame #%d\n",
				reason.Path, reason.Type, reason.FrameDepth+1)
		}
	}

	if len(s.Suspects) == 0 {
		for _, commit := range s.Commits {
			fmt.Fprintf(&text, "   %s  %s\n", shortSHA(commit.ID), firstLine(commit.Message))
		}
		if len(s.Commits) < s.CommitCount {
			fmt.Fprintf(&text, "   … and %d more\n", s.CommitCount-len(s.Commits))
		}
	}
	if s.Warning != "" {
		fmt.Fprintf(&text, "\n%s\n", s.Warning)
	}
	return strings.TrimRight(text.String(), "\n")
}

// shortSHA is what anybody actually types into `git show`.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// firstLine is a commit message's subject; a list wants the subject.
func firstLine(message string) string {
	if cut := strings.IndexByte(message, '\n'); cut >= 0 {
		return message[:cut]
	}
	return message
}
