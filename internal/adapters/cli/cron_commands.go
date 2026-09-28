package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// runMonitors dispatches the monitor subcommands.
//
// One verb with a kind after it — `monitors cron …`, `monitors uptime …` —
// rather than a top-level `crons` and a top-level `uptime`, because a monitor
// is one concept with two implementations: a job that is supposed to check in,
// and a URL that is supposed to answer. An operator asking "what is this
// installation watching" should have one place to look, and the answer should
// not depend on which of the two they remembered the name of. That is also why
// there is one dispatcher and not two: the second `monitors` verb would have
// shadowed the first, and Go would not have said a word about it.
func runMonitors(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "monitors: expected cron, uptime or status-page")
		return ExitUsage
	}
	switch args[0] {
	case "cron":
		return runCronMonitors(c, args[1:])
	case "uptime":
		return runUptime(c, args[1:])
	case "status-page":
		return runStatusPage(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "monitors: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

func runCronMonitors(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "monitors cron: expected add, list, show, update, remove or checkins")
		return ExitUsage
	}
	switch args[0] {
	case "add":
		return runCronAdd(c, args[1:])
	case "list":
		return runCronList(c, args[1:])
	case "show":
		return runCronShow(c, args[1:])
	case "update":
		return runCronUpdate(c, args[1:])
	case "remove":
		return runCronRemove(c, args[1:])
	case "checkins":
		return runCronCheckIns(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "monitors cron: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type monitorPayload struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Slug      string `json:"slug"`
	PingKey   string `json:"ping_key"`
	// PingURL is the line somebody actually pastes into a crontab, which is
	// why the CLI prints it rather than the key on its own.
	PingURL              string     `json:"ping_url"`
	ScheduleType         string     `json:"schedule_type"`
	Schedule             string     `json:"schedule"`
	Timezone             string     `json:"timezone"`
	CheckinMarginSeconds int        `json:"checkin_margin_s"`
	MaxRuntimeSeconds    int        `json:"max_runtime_s"`
	Status               string     `json:"status"`
	LastCheckinAt        *time.Time `json:"last_checkin_at,omitempty"`
	NextExpectedAt       *time.Time `json:"next_expected_at,omitempty"`
	Enabled              bool       `json:"enabled"`
	CreatedAt            time.Time  `json:"created_at"`
}

type checkInPayload struct {
	ID          int64      `json:"id"`
	MonitorID   int64      `json:"monitor_id"`
	CheckInID   string     `json:"checkin_id,omitempty"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	DurationMS  int64      `json:"duration_ms"`
	Environment string     `json:"environment,omitempty"`
}

// cronPath is the REST path for one project's cron monitors.
func cronPath(projectID int64, rest ...string) string {
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/monitors/cron"
	for _, part := range rest {
		path += "/" + part
	}
	return path
}

func runCronAdd(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors cron add")
	projectID := flags.Int64("project", 0, "project id (required)")
	slug := flags.String("slug", "", "the identity an SDK will send (required)")
	schedule := flags.String("schedule", "", `a crontab expression or "<n> <unit>" (required)`)
	scheduleType := flags.String("type", "crontab", "crontab or interval")
	timezone := flags.String("timezone", "", "IANA zone the schedule is read in (default UTC)")
	margin := flags.Int("margin", 0, "seconds a run may be late before it counts as missed")
	maxRuntime := flags.Int("max-runtime", 0, "seconds a started run may go without finishing")
	disabled := flags.Bool("disabled", false, "create it without watching it")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *slug == "" || *schedule == "" {
		fmt.Fprintln(c.stderr, "monitors cron add: -project, -slug and -schedule are required")
		fmt.Fprintln(c.stderr, `  e.g. -project 1 -slug nightly-backup -schedule "0 3 * * *" -timezone America/Caracas`)
		return ExitUsage
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	enabled := !*disabled

	var monitor monitorPayload
	if err := client.do(c.ctx, http.MethodPost, cronPath(*projectID), map[string]any{
		"slug":             *slug,
		"schedule":         *schedule,
		"schedule_type":    *scheduleType,
		"timezone":         *timezone,
		"checkin_margin_s": *margin,
		"max_runtime_s":    *maxRuntime,
		"enabled":          enabled,
	}, &monitor); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, monitor, fmt.Sprintf(
		"monitor %d created (%s, %s)\n  ping: curl -fsS %s",
		monitor.ID, monitor.Slug, monitor.Schedule, monitor.PingURL))
}

func runCronList(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors cron list")
	projectID := flags.Int64("project", 0, "project id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "monitors cron list: -project is required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var page struct {
		Monitors []monitorPayload `json:"monitors"`
	}
	if err := client.do(c.ctx, http.MethodGet, cronPath(*projectID), nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Monitors) == 0 {
		return c.emit(*remote.asJSON, page, "no cron monitors")
	}

	var text strings.Builder
	for index := range page.Monitors {
		monitor := &page.Monitors[index]
		state := monitor.Status
		if !monitor.Enabled {
			state += " (disabled)"
		}
		fmt.Fprintf(&text, "%-4d %-24s %-10s %-18s %s\n",
			monitor.ID, monitor.Slug, state, monitor.Schedule, monitor.Timezone)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

func runCronShow(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors cron show")
	projectID := flags.Int64("project", 0, "project id (required)")
	id := flags.Int64("id", 0, "monitor id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors cron show: -project and -id are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var monitor monitorPayload
	if err := client.do(c.ctx, http.MethodGet,
		cronPath(*projectID, strconv.FormatInt(*id, 10)), nil, &monitor); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, monitor, renderMonitor(&monitor))
}

func renderMonitor(monitor *monitorPayload) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s (%d)\n", monitor.Slug, monitor.ID)
	fmt.Fprintf(&text, "  status:    %s\n", monitor.Status)
	fmt.Fprintf(&text, "  schedule:  %s (%s, %s)\n", monitor.Schedule, monitor.ScheduleType, monitor.Timezone)
	fmt.Fprintf(&text, "  margin:    %ds\n", monitor.CheckinMarginSeconds)
	fmt.Fprintf(&text, "  runtime:   %ds\n", monitor.MaxRuntimeSeconds)
	if monitor.LastCheckinAt != nil {
		fmt.Fprintf(&text, "  last:      %s\n", monitor.LastCheckinAt.Format(time.RFC3339))
	}
	if monitor.NextExpectedAt != nil {
		fmt.Fprintf(&text, "  next:      %s\n", monitor.NextExpectedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&text, "  enabled:   %t\n", monitor.Enabled)
	fmt.Fprintf(&text, "  ping:      curl -fsS %s", monitor.PingURL)
	return text.String()
}

func runCronUpdate(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors cron update")
	projectID := flags.Int64("project", 0, "project id (required)")
	id := flags.Int64("id", 0, "monitor id (required)")
	schedule := flags.String("schedule", "", "a new schedule")
	scheduleType := flags.String("type", "", "crontab or interval")
	timezone := flags.String("timezone", "", "a new IANA zone")
	margin := flags.Int("margin", -1, "seconds a run may be late (-1 leaves it alone)")
	maxRuntime := flags.Int("max-runtime", -1, "seconds a run may take (-1 leaves it alone)")
	enable := flags.Bool("enable", false, "start watching it")
	disable := flags.Bool("disable", false, "stop watching it")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors cron update: -project and -id are required")
		return ExitUsage
	}
	if *enable && *disable {
		fmt.Fprintln(c.stderr, "monitors cron update: -enable and -disable contradict each other")
		return ExitUsage
	}

	// Only what was actually asked for is sent. A body that carried every
	// field would turn "switch this off" into "switch this off and reset
	// everything else to whatever the flag defaults are".
	body := map[string]any{}
	if *schedule != "" {
		body["schedule"] = *schedule
	}
	if *scheduleType != "" {
		body["schedule_type"] = *scheduleType
	}
	if *timezone != "" {
		body["timezone"] = *timezone
	}
	if *margin >= 0 {
		body["checkin_margin_s"] = *margin
	}
	if *maxRuntime >= 0 {
		body["max_runtime_s"] = *maxRuntime
	}
	if *enable || *disable {
		body["enabled"] = *enable
	}
	if len(body) == 0 {
		fmt.Fprintln(c.stderr, "monitors cron update: nothing to change")
		return ExitUsage
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	var monitor monitorPayload
	if err := client.do(c.ctx, http.MethodPut,
		cronPath(*projectID, strconv.FormatInt(*id, 10)), body, &monitor); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, monitor, renderMonitor(&monitor))
}

func runCronRemove(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors cron remove")
	projectID := flags.Int64("project", 0, "project id (required)")
	id := flags.Int64("id", 0, "monitor id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors cron remove: -project and -id are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	if err := client.do(c.ctx, http.MethodDelete,
		cronPath(*projectID, strconv.FormatInt(*id, 10)), nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, map[string]any{"deleted": *id},
		fmt.Sprintf("monitor %d removed", *id))
}

func runCronCheckIns(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors cron checkins")
	projectID := flags.Int64("project", 0, "project id (required)")
	id := flags.Int64("id", 0, "monitor id (required)")
	limit := flags.Int("limit", 0, "how many to show (default 50)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *id <= 0 {
		fmt.Fprintln(c.stderr, "monitors cron checkins: -project and -id are required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := cronPath(*projectID, strconv.FormatInt(*id, 10), "checkins")
	if *limit > 0 {
		path += "?limit=" + strconv.Itoa(*limit)
	}
	var page struct {
		CheckIns []checkInPayload `json:"checkins"`
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.CheckIns) == 0 {
		return c.emit(*remote.asJSON, page, "no check-ins")
	}

	var text strings.Builder
	for index := range page.CheckIns {
		checkIn := &page.CheckIns[index]
		finished := "running"
		if checkIn.FinishedAt != nil {
			finished = fmt.Sprintf("%dms", checkIn.DurationMS)
		}
		fmt.Fprintf(&text, "%-24s %-12s %s\n",
			checkIn.StartedAt.Format(time.RFC3339), checkIn.Status, finished)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}
