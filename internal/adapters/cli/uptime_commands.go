package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func runUptime(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr,
			"monitors uptime: expected add, list, show, remove, enable, disable, results or daily")
		return ExitUsage
	}
	switch args[0] {
	case "add":
		return runUptimeAdd(c, args[1:])
	case "list":
		return runUptimeList(c, args[1:])
	case "show":
		return runUptimeShow(c, args[1:])
	case "remove":
		return runUptimeRemove(c, args[1:])
	case "enable":
		return runUptimeSwitch(c, args[1:], true)
	case "disable":
		return runUptimeSwitch(c, args[1:], false)
	case "results":
		return runUptimeResults(c, args[1:])
	case "daily":
		return runUptimeDaily(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "monitors uptime: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

// uptimeMonitorPayload is a monitor as the API returns it.
type uptimeMonitorPayload struct {
	ID                    int64      `json:"id"`
	ProjectID             int64      `json:"project_id"`
	Name                  string     `json:"name"`
	URL                   string     `json:"url"`
	Method                string     `json:"method"`
	IntervalSeconds       int        `json:"interval_s"`
	TimeoutSeconds        int        `json:"timeout_s"`
	ExpectedStatusMin     int        `json:"expected_status_min"`
	ExpectedStatusMax     int        `json:"expected_status_max"`
	ExpectedBodySubstring string     `json:"expected_body_substring"`
	FollowRedirects       bool       `json:"follow_redirects"`
	AllowPrivate          bool       `json:"allow_private"`
	Public                bool       `json:"public"`
	Enabled               bool       `json:"enabled"`
	Status                string     `json:"status"`
	ConsecutiveFailures   int        `json:"consecutive_failures"`
	LastCheckedAt         *time.Time `json:"last_checked_at"`
	NextCheckAt           time.Time  `json:"next_check_at"`
	LastStatusChangeAt    *time.Time `json:"last_status_change_at"`
	CreatedAt             time.Time  `json:"created_at"`
}

func runUptimeAdd(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors uptime add")
	project := flags.Int64("project", 0, "project id (required)")
	name := flags.String("name", "", "what to call it (required)")
	// -target and not -url: every remote command already has a -url, and it
	// means the server this CLI talks to. Two flags with one name is a panic
	// at registration, and two flags with one meaning is worse — somebody
	// would eventually point a monitor at their own trapline.
	target := flags.String("target", "", "the http or https url to check (required)")
	method := flags.String("method", "", "GET or HEAD (default GET)")
	interval := flags.Int("interval", 0, "seconds between checks, at least 30 (default 60)")
	timeout := flags.Int("timeout", 0, "seconds to wait for an answer (default 10)")
	statusMin := flags.Int("status-min", 0, "lowest status code that counts as healthy (default 200)")
	statusMax := flags.Int("status-max", 0, "highest status code that counts as healthy (default 299)")
	contains := flags.String("contains", "", "text the response body must contain")
	follow := flags.Bool("follow-redirects", true, "follow 3xx responses, re-checking each hop")
	allowPrivate := flags.Bool("allow-private", false,
		"let this monitor reach a private address; the server must also run with -uptime-allow-private")
	public := flags.Bool("public", false, "show this monitor on the public status page")
	disabled := flags.Bool("disabled", false, "create it switched off")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *project <= 0 || *name == "" || *target == "" {
		fmt.Fprintln(c.stderr, "monitors uptime add: -project, -name and -target are required")
		fmt.Fprintln(c.stderr, "  e.g. -project 1 -name api -target https://api.example.com/health")
		return ExitUsage
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	body := map[string]any{
		"name":             *name,
		"url":              *target,
		"follow_redirects": *follow,
		"allow_private":    *allowPrivate,
		"public":           *public,
		"enabled":          !*disabled,
	}
	// Only what was asked for is sent, so the server's defaults stay the one
	// place they are written down. A CLI that filled them in itself would be
	// a second copy to keep in step.
	if *method != "" {
		body["method"] = strings.ToUpper(*method)
	}
	if *interval > 0 {
		body["interval_s"] = *interval
	}
	if *timeout > 0 {
		body["timeout_s"] = *timeout
	}
	if *statusMin > 0 {
		body["expected_status_min"] = *statusMin
	}
	if *statusMax > 0 {
		body["expected_status_max"] = *statusMax
	}
	if *contains != "" {
		body["expected_body_substring"] = *contains
	}

	var monitor uptimeMonitorPayload
	if err := client.do(c.ctx, http.MethodPost,
		"/projects/"+strconv.FormatInt(*project, 10)+"/monitors/uptime", body, &monitor); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, monitor,
		fmt.Sprintf("monitor %d created (%s, every %ds)", monitor.ID, monitor.Name, monitor.IntervalSeconds))
}

func runUptimeList(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors uptime list")
	project := flags.Int64("project", 0, "project id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *project <= 0 {
		fmt.Fprintln(c.stderr, "monitors uptime list: -project is required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var page struct {
		Monitors []uptimeMonitorPayload `json:"monitors"`
	}
	if err := client.do(c.ctx, http.MethodGet,
		"/projects/"+strconv.FormatInt(*project, 10)+"/monitors/uptime", nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Monitors) == 0 {
		return c.emit(*remote.asJSON, page, "no monitors")
	}

	var text strings.Builder
	for index := range page.Monitors {
		monitor := &page.Monitors[index]
		state := monitor.Status
		if !monitor.Enabled {
			state = "disabled"
		}
		fmt.Fprintf(&text, "%-4d %-8s %-24s %s\n", monitor.ID, state, monitor.Name, monitor.URL)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

func runUptimeShow(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors uptime show")
	id := flags.Int64("id", 0, "monitor id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors uptime show: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var monitor uptimeMonitorPayload
	if err := client.do(c.ctx, http.MethodGet, monitorPath(*id), nil, &monitor); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, monitor,
		fmt.Sprintf("%s is %s (%s %s, every %ds)",
			monitor.Name, monitor.Status, monitor.Method, monitor.URL, monitor.IntervalSeconds))
}

func runUptimeRemove(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors uptime remove")
	id := flags.Int64("id", 0, "monitor id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors uptime remove: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	if err := client.do(c.ctx, http.MethodDelete, monitorPath(*id), nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, map[string]any{"id": *id, "removed": true},
		fmt.Sprintf("monitor %d removed", *id))
}

func runUptimeSwitch(c *context_, args []string, enabled bool) int {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	flags := newFlagSet(c, "monitors uptime "+verb)
	id := flags.Int64("id", 0, "monitor id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintf(c.stderr, "monitors uptime %s: -id is required and must be positive\n", verb)
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var monitor uptimeMonitorPayload
	if err := client.do(c.ctx, http.MethodPost, monitorPath(*id)+"/enabled",
		map[string]any{"enabled": enabled}, &monitor); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, monitor, fmt.Sprintf("monitor %d %sd", monitor.ID, verb))
}

// checkPayload is one check as the API returns it.
type checkPayload struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int       `json:"latency_ms"`
	Error      string    `json:"error"`
}

func runUptimeResults(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors uptime results")
	id := flags.Int64("id", 0, "monitor id (required)")
	limit := flags.Int("limit", 0, "how many checks to show, newest first (default 50)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors uptime results: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := monitorPath(*id) + "/results"
	if *limit > 0 {
		path += "?limit=" + strconv.Itoa(*limit)
	}
	var page struct {
		Results []checkPayload `json:"results"`
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Results) == 0 {
		return c.emit(*remote.asJSON, page, "no checks yet")
	}

	var text strings.Builder
	for _, result := range page.Results {
		verdict := "ok"
		if !result.OK {
			verdict = "FAIL"
		}
		fmt.Fprintf(&text, "%s %-4s %3d %5dms %s\n",
			result.At.Format(time.RFC3339), verdict, result.StatusCode, result.LatencyMS, result.Error)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

// dayPayload is one day of the roll-up.
type dayPayload struct {
	Day           string  `json:"day"`
	Checks        int64   `json:"checks"`
	Failures      int64   `json:"failures"`
	Uptime        float64 `json:"uptime"`
	MeanLatencyMS int64   `json:"mean_latency_ms"`
}

func runUptimeDaily(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors uptime daily")
	id := flags.Int64("id", 0, "monitor id (required)")
	days := flags.Int("days", 0, "how many days back to read (default 90)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors uptime daily: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := monitorPath(*id) + "/daily"
	if *days > 0 {
		path += "?days=" + strconv.Itoa(*days)
	}
	var page struct {
		Days []dayPayload `json:"days"`
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Days) == 0 {
		return c.emit(*remote.asJSON, page, "no history yet")
	}

	var text strings.Builder
	for _, day := range page.Days {
		fmt.Fprintf(&text, "%s %6.2f%% %4d checks %4d failed %5dms\n",
			day.Day, day.Uptime, day.Checks, day.Failures, day.MeanLatencyMS)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

func monitorPath(id int64) string {
	return "/monitors/uptime/" + strconv.FormatInt(id, 10)
}
