package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// sessionCounts mirrors the four disjoint counters the API returns.
type sessionCounts struct {
	Started  int64 `json:"started"`
	Errored  int64 `json:"errored"`
	Crashed  int64 `json:"crashed"`
	Abnormal int64 `json:"abnormal"`
}

// windowPayload is what the API says about the in-memory window it counted in.
//
// It is printed rather than dropped, because it carries the one caveat this
// subsystem owes its reader: the window lives in memory, so the newest hour
// can move after a restart (ADR 008). A terminal that showed the number and
// swallowed the sentence would be the CLI being less honest than the API.
type windowPayload struct {
	InFlight int    `json:"in_flight"`
	Capacity int    `json:"capacity"`
	Evicted  int64  `json:"evicted"`
	Expired  int64  `json:"expired"`
	Note     string `json:"note"`
}

type healthPointPayload struct {
	Hour string `json:"hour"`
	sessionCounts
	CrashFreeRate *float64 `json:"crash_free_rate"`
}

type releaseHealthPayload struct {
	Project projectRef        `json:"project"`
	Range   statsRangePayload `json:"range"`
	Release string            `json:"release"`
	sessionCounts
	Healthy       int64                `json:"healthy"`
	CrashFreeRate *float64             `json:"crash_free_rate"`
	Series        []healthPointPayload `json:"series"`
	Window        windowPayload        `json:"window"`
}

type releaseSummaryPayload struct {
	Release string `json:"release"`
	sessionCounts
	Healthy       int64    `json:"healthy"`
	CrashFreeRate *float64 `json:"crash_free_rate"`
	FirstSeen     string   `json:"first_seen"`
	LastSeen      string   `json:"last_seen"`
}

type projectHealthPayload struct {
	Project projectRef        `json:"project"`
	Range   statsRangePayload `json:"range"`
	sessionCounts
	Healthy       int64                   `json:"healthy"`
	CrashFreeRate *float64                `json:"crash_free_rate"`
	Releases      []releaseSummaryPayload `json:"releases"`
	Window        windowPayload           `json:"window"`
}

// runReleasesHealth reports crash-free rates, for one release or for all.
//
// One verb with an optional -version rather than two, unlike `transactions`:
// both answers are the same table with the same columns, and the version only
// decides whether it has one row or several. Splitting them would make
// somebody learn two commands to read one number.
func runReleasesHealth(c *context_, args []string) int {
	flags := newFlagSet(c, "releases health")
	projectID := flags.Int64("project", 0, "project id (required)")
	version := flags.String("version", "", "a single release; omit for every release in the range")
	from := flags.String("from", "", "start of the range: 2006-01-02, 2006-01-02T15 or RFC 3339 (default: 24h ago)")
	to := flags.String("to", "", "end of the range, same formats (default: now)")
	limit := flags.Int("limit", 0, "how many releases to return when no version is given")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "releases health: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	if *version != "" {
		var payload releaseHealthPayload
		path := releasesPath(*projectID, *version) + "/health" + rangeQuery(*from, *to, nil)
		if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
			return c.fail(err)
		}
		return c.emit(*remote.asJSON, payload, renderReleaseHealth(&payload))
	}

	extra := map[string]string{}
	if *limit > 0 {
		extra["limit"] = strconv.Itoa(*limit)
	}
	var payload projectHealthPayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/health" +
		rangeQuery(*from, *to, extra)
	if err := client.do(c.ctx, http.MethodGet, path, nil, &payload); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, payload, renderProjectHealth(&payload))
}

func renderReleaseHealth(payload *releaseHealthPayload) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s — %s over %d hours\n",
		payload.Project.Name, payload.Release, payload.Range.Hours)
	fmt.Fprintf(&text, "%s crash-free · %d sessions: %d healthy, %d errored, %d crashed, %d abnormal\n",
		renderCrashFree(payload.CrashFreeRate), payload.Started, payload.Healthy,
		payload.Errored, payload.Crashed, payload.Abnormal)

	// Only the hours that saw something. The series carries every hour of the
	// range so a chart can draw the quiet ones as gaps rather than as a line
	// between two spikes, but a terminal printing twenty-four empty rows is
	// twenty-four rows nobody reads.
	shown := 0
	for index := range payload.Series {
		point := &payload.Series[index]
		if point.Started == 0 {
			continue
		}
		if shown == 0 {
			text.WriteString("\n")
		}
		shown++
		fmt.Fprintf(&text, "  %s  %6d sessions  %5d crashed  %s\n",
			point.Hour, point.Started, point.Crashed, renderCrashFree(point.CrashFreeRate))
	}
	if shown == 0 {
		text.WriteString("\nno sessions in this range\n")
	}

	text.WriteString("\n" + renderWindow(&payload.Window) + "\n")
	return strings.TrimRight(text.String(), "\n")
}

func renderProjectHealth(payload *projectHealthPayload) string {
	if len(payload.Releases) == 0 {
		return "no sessions in this range\n\n" + renderWindow(&payload.Window)
	}

	var text strings.Builder
	fmt.Fprintf(&text, "%s — %d sessions over %d hours, %s crash-free\n\n",
		payload.Project.Name, payload.Started, payload.Range.Hours,
		renderCrashFree(payload.CrashFreeRate))

	for index := range payload.Releases {
		release := &payload.Releases[index]
		fmt.Fprintf(&text, "  %-28s %8d sessions  %5d crashed  %s  last seen %s\n",
			truncate(release.Release, 28), release.Started, release.Crashed,
			renderCrashFree(release.CrashFreeRate), release.LastSeen)
	}

	text.WriteString("\n" + renderWindow(&payload.Window))
	return text.String()
}

// renderCrashFree prints the rate, or says there is none.
//
// A release with no sessions has no crash-free rate, and printing 100% for it
// would tell somebody their release is perfect at the moment the truth is that
// nothing has reported in — the more alarming of the two, and the one that
// would be hidden.
func renderCrashFree(rate *float64) string {
	if rate == nil {
		return "     —"
	}
	return fmt.Sprintf("%5.2f%%", *rate*100)
}

func renderWindow(window *windowPayload) string {
	line := fmt.Sprintf("%d sessions in flight of %d; %s",
		window.InFlight, window.Capacity, window.Note)
	if window.Evicted > 0 {
		// Said only when it happened, and said plainly. A full window trades
		// precision for stability by design (ADR 008), but somebody reading a
		// number that was rounded off by a ceiling should be told so where
		// they are reading it.
		line += fmt.Sprintf("\n%d sessions were settled early because the window was full, "+
			"so these counts are a lower bound; raise -session-window", window.Evicted)
	}
	return line
}

// truncate bounds a column so one long release version does not shift every
// other row of the table.
func truncate(value string, width int) string {
	if len(value) <= width {
		return value
	}
	return value[:width-1] + "…"
}
