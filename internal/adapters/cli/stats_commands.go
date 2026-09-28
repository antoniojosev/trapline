package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// runStats is the dashboard from a terminal.
//
// One command with flags rather than four subcommands, because the four
// questions differ only in what is being counted: the range is the same, the
// project is the same, and someone comparing "how many" with "which release"
// should not have to relearn the flags between the two. Which of the four runs
// is decided by whether -issue, -top or -by was named — in that order, from
// most specific to least (ADR 006: the CLI is a first-class client, not a
// thinner one).
func runStats(c *context_, args []string) int {
	flags := newFlagSet(c, "stats")
	projectID := flags.Int64("project", 0, "project id (required)")
	from := flags.String("from", "", "start of the range: 2006-01-02, 2006-01-02T15 or RFC 3339 (default: 24h ago)")
	to := flags.String("to", "", "end of the range, same formats (default: now)")
	by := flags.String("by", "", "break the range down by release or environment")
	top := flags.Int("top", 0, "instead of a series, the N issues with the most events in the range")
	issueID := flags.Int64("issue", 0, "instead of a project series, one issue's own series")
	issueIDs := flags.String("issues", "", "instead of one series, a sparkline for each of these issue ids (comma separated)")
	window := flags.String("range", "", "window for -issue: 24h or 14d")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "stats: -project is required and must be positive")
		return ExitUsage
	}
	if *issueID > 0 && (*by != "" || *top > 0) {
		fmt.Fprintln(c.stderr, "stats: -issue asks about one issue, so -by and -top do not apply")
		return ExitUsage
	}
	if *issueIDs != "" && (*issueID > 0 || *by != "" || *top > 0) {
		fmt.Fprintln(c.stderr, "stats: -issues names its own set of issues, so -issue, -by and -top do not apply")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	base := "/projects/" + strconv.FormatInt(*projectID, 10)
	switch {
	case *issueIDs != "":
		return runIssueSparklines(c, client, base, *issueIDs, *window, remote)
	case *issueID > 0:
		return runIssueStats(c, client, base, *issueID, *window, remote)
	case *top > 0:
		return runTopStats(c, client, base, *from, *to, *top, remote)
	case *by != "":
		return runBreakdownStats(c, client, base, *from, *to, *by, remote)
	default:
		return runSeriesStats(c, client, base, *from, *to, remote)
	}
}

// statsRangePayload mirrors the range every stats response echoes back.
type statsRangePayload struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Hours int       `json:"hours"`
}

type seriesPayload struct {
	Project projectRef        `json:"project"`
	Range   statsRangePayload `json:"range"`
	Total   int64             `json:"total"`
	Series  []struct {
		Hour    string           `json:"hour"`
		Count   int64            `json:"count"`
		ByLevel map[string]int64 `json:"by_level"`
	} `json:"series"`
	Levels map[string]int64 `json:"by_level"`
}

// projectRef is the smallest identification of a project a response carries.
type projectRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func runSeriesStats(c *context_, client *apiClient, base, from, to string, remote remoteFlags) int {
	var payload seriesPayload
	if err := client.do(c.ctx, http.MethodGet, base+"/stats"+rangeQuery(from, to, nil), nil, &payload); err != nil {
		return c.fail(err)
	}

	var text strings.Builder
	fmt.Fprintf(&text, "%s — %d events over %d hours\n\n", payload.Project.Name, payload.Total, payload.Range.Hours)
	for index := range payload.Series {
		point := &payload.Series[index]
		if point.Count == 0 {
			// Quiet hours are in the JSON, because a chart needs them, and out
			// of the text, because a terminal full of zeroes hides the hour
			// that was not one.
			continue
		}
		fmt.Fprintf(&text, "%s\t%d\t%s\n", point.Hour, point.Count, renderLevels(point.ByLevel))
	}
	if payload.Total == 0 {
		fmt.Fprintf(&text, "nothing in this range\n")
	}
	return c.emit(*remote.asJSON, payload, strings.TrimRight(text.String(), "\n"))
}

type topPayload struct {
	Project projectRef        `json:"project"`
	Range   statsRangePayload `json:"range"`
	Issues  []struct {
		issuePayload
		Count int64 `json:"count"`
	} `json:"issues"`
}

func runTopStats(c *context_, client *apiClient, base, from, to string, limit int, remote remoteFlags) int {
	var payload topPayload
	path := base + "/stats/top" + rangeQuery(from, to, map[string]string{"limit": strconv.Itoa(limit)})
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}
	if len(payload.Issues) == 0 {
		return c.emit(*remote.asJSON, payload, "nothing in this range")
	}

	var text strings.Builder
	for index := range payload.Issues {
		issue := &payload.Issues[index]
		fmt.Fprintf(&text, "%d\t%d×\t%s\t%s\n", issue.ID, issue.Count, issue.Title, issue.Culprit)
	}
	return c.emit(*remote.asJSON, payload, strings.TrimRight(text.String(), "\n"))
}

type breakdownPayload struct {
	Project projectRef        `json:"project"`
	Range   statsRangePayload `json:"range"`
	By      string            `json:"by"`
	Values  []struct {
		Value string `json:"value"`
		Count int64  `json:"count"`
	} `json:"values"`
}

func runBreakdownStats(c *context_, client *apiClient, base, from, to, by string, remote remoteFlags) int {
	var payload breakdownPayload
	path := base + "/stats/breakdown" + rangeQuery(from, to, map[string]string{"by": by})
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}
	if len(payload.Values) == 0 {
		return c.emit(*remote.asJSON, payload, "nothing in this range")
	}

	var text strings.Builder
	for _, value := range payload.Values {
		fmt.Fprintf(&text, "%s\t%d\n", value.Value, value.Count)
	}
	return c.emit(*remote.asJSON, payload, strings.TrimRight(text.String(), "\n"))
}

type issueStatsPayload struct {
	IssueID int64             `json:"issue_id"`
	Window  string            `json:"range"`
	Range   statsRangePayload `json:"range_bounds"`
	Total   int64             `json:"total"`
	Series  []struct {
		Hour  string `json:"hour"`
		Count int64  `json:"count"`
	} `json:"series"`
}

func runIssueStats(c *context_, client *apiClient, base string, issueID int64, window string, remote remoteFlags) int {
	var payload issueStatsPayload
	path := base + "/issues/" + strconv.FormatInt(issueID, 10) + "/stats"
	if window != "" {
		path += "?range=" + window
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}

	var text strings.Builder
	fmt.Fprintf(&text, "issue %d — %d events over %s\n\n", payload.IssueID, payload.Total, payload.Window)
	for _, point := range payload.Series {
		if point.Count == 0 {
			continue
		}
		fmt.Fprintf(&text, "%s\t%d\n", point.Hour, point.Count)
	}
	if payload.Total == 0 {
		fmt.Fprintf(&text, "nothing in this range\n")
	}
	return c.emit(*remote.asJSON, payload, strings.TrimRight(text.String(), "\n"))
}

type sparklinesPayload struct {
	Project projectRef        `json:"project"`
	Window  string            `json:"range"`
	Range   statsRangePayload `json:"range_bounds"`
	Hours   []string          `json:"hours"`
	Issues  []struct {
		IssueID int64   `json:"issue_id"`
		Total   int64   `json:"total"`
		Counts  []int64 `json:"counts"`
	} `json:"issues"`
}

// runIssueSparklines is the shape of several issues at once, which is what a
// listing draws beside its rows.
//
// It is here rather than only in the panel because ADR 006 has no "endpoints
// the UI gets to itself": a question the product can answer is answerable from
// every client, or the API grows a private corner that nothing else can test.
func runIssueSparklines(
	c *context_, client *apiClient, base, issues, window string, remote remoteFlags,
) int {
	var payload sparklinesPayload
	query := map[string]string{"issues": issues}
	if window != "" {
		query["range"] = window
	}
	path := base + "/stats/series"
	if params := issueQuery(query); params != "" {
		path += "?" + params
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}

	var text strings.Builder
	for index := range payload.Issues {
		issue := &payload.Issues[index]
		fmt.Fprintf(&text, "%d\t%d\t%s\n", issue.IssueID, issue.Total, renderSparkline(issue.Counts))
	}
	if len(payload.Issues) == 0 {
		fmt.Fprintf(&text, "no issues named\n")
	}
	return c.emit(*remote.asJSON, payload, strings.TrimRight(text.String(), "\n"))
}

// sparklineBlocks are the eight heights a terminal can draw a bar at.
var sparklineBlocks = []rune("▁▂▃▄▅▆▇█")

// renderSparkline draws a series as one line of text.
//
// Scaled to the series' own maximum rather than to a shared one: this is read
// as the shape of one issue over time — is it climbing, did it stop — and a
// scale borrowed from the loudest issue on the page would flatten every other
// row to nothing. The number beside it is what compares two rows.
func renderSparkline(counts []int64) string {
	if len(counts) == 0 {
		return ""
	}
	var highest int64
	for _, count := range counts {
		if count > highest {
			highest = count
		}
	}
	drawn := make([]rune, 0, len(counts))
	for _, count := range counts {
		switch {
		case count == 0:
			// A space, not the shortest bar: an hour with nothing in it and
			// an hour with one event are different, and drawing them the same
			// is how a quiet night reads as a steady trickle.
			drawn = append(drawn, ' ')
		case highest <= 1:
			drawn = append(drawn, sparklineBlocks[len(sparklineBlocks)-1])
		default:
			step := int((count - 1) * int64(len(sparklineBlocks)-1) / highest)
			drawn = append(drawn, sparklineBlocks[step])
		}
	}
	return string(drawn)
}

// rangeQuery builds the query string every stats endpoint understands, leaving
// out what the caller did not name so the server applies its own default
// rather than this command inventing a second one.
func rangeQuery(from, to string, extra map[string]string) string {
	values := map[string]string{"from": from, "to": to}
	for name, value := range extra {
		values[name] = value
	}
	if params := issueQuery(values); params != "" {
		return "?" + params
	}
	return ""
}

// renderLevels prints an hour's severities in a fixed order, so two rows of a
// table line up instead of listing whatever the map iterated first.
func renderLevels(byLevel map[string]int64) string {
	order := []string{"fatal", "error", "warning", "info", "debug"}
	parts := make([]string, 0, len(byLevel))
	for _, level := range order {
		if count, present := byLevel[level]; present {
			parts = append(parts, fmt.Sprintf("%s=%d", level, count))
		}
	}
	return strings.Join(parts, " ")
}
