package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// runAlerts dispatches the alerting subcommands.
//
// They exist at the same moment as the API and not afterwards, because the
// first thing anybody does with a new alerting subsystem is configure it from
// the machine it runs on, over ssh, before the panel has ever been opened
// (ADR 006). The channel configuration arrives as JSON on -config rather than
// as a flag per field: five channel types with five different shapes would
// otherwise be twenty flags, nineteen of which are wrong for whatever you are
// doing.
func runAlerts(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "alerts: expected channels, rules or log")
		return ExitUsage
	}
	switch args[0] {
	case "channels":
		return runAlertChannels(c, args[1:])
	case "rules":
		return runAlertRules(c, args[1:])
	case "log":
		return runAlertLog(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "alerts: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

func runAlertChannels(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "alerts channels: expected add, list, test or remove")
		return ExitUsage
	}
	switch args[0] {
	case "add":
		return runAlertChannelAdd(c, args[1:])
	case "list":
		return runAlertChannelList(c, args[1:])
	case "test":
		return runAlertChannelTest(c, args[1:])
	case "remove":
		return runAlertChannelRemove(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "alerts channels: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type channelPayload struct {
	ID        int64          `json:"id"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	Config    map[string]any `json:"config"`
	Digest    bool           `json:"digest"`
	CreatedAt time.Time      `json:"created_at"`
}

func runAlertChannelAdd(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts channels add")
	channelType := flags.String("type", "", "telegram, slack, discord, webhook or email (required)")
	name := flags.String("name", "", "what to call it (required)")
	config := flags.String("config", "", "the channel's configuration as JSON (required)")
	digest := flags.Bool("digest", false, "also send the weekly digest here")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *channelType == "" || *name == "" || *config == "" {
		fmt.Fprintln(c.stderr, "alerts channels add: -type, -name and -config are required")
		fmt.Fprintln(c.stderr, `  e.g. -type slack -config '{"url":"https://hooks.slack.com/services/…"}'`)
		return ExitUsage
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(*config), &parsed); err != nil {
		return c.fail(fmt.Errorf("-config is not JSON: %w", err))
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var channel channelPayload
	if err := client.do(c.ctx, http.MethodPost, "/alerts/channels", map[string]any{
		"type": *channelType, "name": *name, "config": parsed, "digest": *digest,
	}, &channel); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, channel,
		fmt.Sprintf("channel %d created (%s, %s)", channel.ID, channel.Name, channel.Type))
}

func runAlertChannelList(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts channels list")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var page struct {
		Channels []channelPayload `json:"channels"`
	}
	if err := client.do(c.ctx, http.MethodGet, "/alerts/channels", nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Channels) == 0 {
		return c.emit(*remote.asJSON, page, "no channels")
	}

	var text strings.Builder
	for _, channel := range page.Channels {
		fmt.Fprintf(&text, "%-4d %-9s %s\n", channel.ID, channel.Type, channel.Name)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

func runAlertChannelTest(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts channels test")
	id := flags.Int64("id", 0, "channel id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "alerts channels test: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	// A refused delivery comes back as a status the client turns into an
	// error, and that error already carries what the far end said. Reporting
	// it as a failed command rather than as a successful command with a sad
	// payload is what makes `alerts channels test` usable in a script.
	if err := client.do(c.ctx, http.MethodPost,
		"/alerts/channels/"+strconv.FormatInt(*id, 10)+"/test", nil, &result); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, result, "delivered")
}

func runAlertChannelRemove(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts channels remove")
	id := flags.Int64("id", 0, "channel id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "alerts channels remove: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	if err := client.do(c.ctx, http.MethodDelete,
		"/alerts/channels/"+strconv.FormatInt(*id, 10), nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, map[string]any{"status": "deleted", "id": *id},
		fmt.Sprintf("channel %d removed", *id))
}

func runAlertRules(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "alerts rules: expected add, list, remove or test")
		return ExitUsage
	}
	switch args[0] {
	case "add":
		return runAlertRuleAdd(c, args[1:])
	case "list":
		return runAlertRuleList(c, args[1:])
	case "remove":
		return runAlertRuleRemove(c, args[1:])
	case "test":
		return runAlertRuleTest(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "alerts rules: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type rulePayload struct {
	ID             int64          `json:"id"`
	ProjectID      *int64         `json:"project_id"`
	Name           string         `json:"name"`
	Trigger        map[string]any `json:"trigger"`
	ChannelIDs     []int64        `json:"channel_ids"`
	SilenceSeconds int            `json:"silence_seconds"`
	Enabled        bool           `json:"enabled"`
}

func runAlertRuleAdd(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts rules add")
	name := flags.String("name", "", "what to call it (required)")
	projectID := flags.Int64("project", 0, "project id, or 0 for every project")
	trigger := flags.String("trigger", "",
		`the condition as JSON, e.g. '{"kind":"new_issue"}' or '{"kind":"issue_spike","window_s":3600,"min_count":10,"factor":3}' (required)`)
	channels := flags.String("channels", "", "comma-separated channel ids (required)")
	silence := flags.Int("silence", 0, "seconds of quiet after firing about one subject, 0 for the default")
	disabled := flags.Bool("disabled", false, "create the rule switched off")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" || *trigger == "" || *channels == "" {
		fmt.Fprintln(c.stderr, "alerts rules add: -name, -trigger and -channels are required")
		return ExitUsage
	}

	var parsedTrigger map[string]any
	if err := json.Unmarshal([]byte(*trigger), &parsedTrigger); err != nil {
		return c.fail(fmt.Errorf("-trigger is not JSON: %w", err))
	}
	ids, err := parseIDList(*channels)
	if err != nil {
		return c.fail(err)
	}

	body := map[string]any{
		"name":            *name,
		"trigger":         parsedTrigger,
		"channel_ids":     ids,
		"silence_seconds": *silence,
		"enabled":         !*disabled,
	}
	// Omitted rather than sent as zero: a rule with no project covers every
	// project, and 0 is not a project id.
	if *projectID > 0 {
		body["project_id"] = *projectID
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	var rule rulePayload
	if err := client.do(c.ctx, http.MethodPost, "/alerts/rules", body, &rule); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, rule, fmt.Sprintf("rule %d created (%s)", rule.ID, rule.Name))
}

func parseIDList(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		id, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a channel id", trimmed)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no channel ids in %q", raw)
	}
	return ids, nil
}

func runAlertRuleList(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts rules list")
	projectID := flags.Int64("project", 0, "only the rules covering this project")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := "/alerts/rules"
	if *projectID > 0 {
		path += "?project_id=" + strconv.FormatInt(*projectID, 10)
	}
	var page struct {
		Rules []rulePayload `json:"rules"`
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Rules) == 0 {
		return c.emit(*remote.asJSON, page, "no rules")
	}

	var text strings.Builder
	for _, rule := range page.Rules {
		scope := "all projects"
		if rule.ProjectID != nil {
			scope = "project " + strconv.FormatInt(*rule.ProjectID, 10)
		}
		state := "enabled"
		if !rule.Enabled {
			state = "disabled"
		}
		kind, _ := rule.Trigger["kind"].(string)
		fmt.Fprintf(&text, "%-4d %-14s %-16s %-14s %s\n", rule.ID, kind, scope, state, rule.Name)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}

func runAlertRuleRemove(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts rules remove")
	id := flags.Int64("id", 0, "rule id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "alerts rules remove: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}
	if err := client.do(c.ctx, http.MethodDelete,
		"/alerts/rules/"+strconv.FormatInt(*id, 10), nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, map[string]any{"status": "deleted", "id": *id},
		fmt.Sprintf("rule %d removed", *id))
}

// runAlertRuleTest sends a synthetic alert through every channel a rule names.
//
// It exits 1 when any channel failed, and prints one line per channel either
// way. "It works except for Discord" is the answer somebody needs, and a
// command that reported only the first failure would hide it.
func runAlertRuleTest(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts rules test")
	id := flags.Int64("id", 0, "rule id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "alerts rules test: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var report struct {
		OK      bool `json:"ok"`
		Results []struct {
			ChannelID int64  `json:"channel_id"`
			Name      string `json:"name"`
			Type      string `json:"type"`
			OK        bool   `json:"ok"`
			Error     string `json:"error"`
		} `json:"results"`
	}
	if err := client.do(c.ctx, http.MethodPost,
		"/alerts/rules/"+strconv.FormatInt(*id, 10)+"/test", nil, &report); err != nil {
		return c.fail(err)
	}

	var text strings.Builder
	for _, result := range report.Results {
		marker := "ok  "
		detail := result.Type
		if !result.OK {
			marker, detail = "FAIL", result.Error
		}
		fmt.Fprintf(&text, "%s  %-4d %-20s %s\n", marker, result.ChannelID, result.Name, detail)
	}
	c.emit(*remote.asJSON, report, strings.TrimRight(text.String(), "\n"))
	if !report.OK {
		return ExitError
	}
	return ExitOK
}

type notificationPayload struct {
	ID            int64      `json:"id"`
	RuleID        int64      `json:"rule_id"`
	ChannelID     int64      `json:"channel_id"`
	SubjectKey    string     `json:"subject_key"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastError     string     `json:"last_error"`
	CreatedAt     time.Time  `json:"created_at"`
	SentAt        *time.Time `json:"sent_at"`
	Payload       struct {
		Event string `json:"event"`
		Rule  string `json:"rule"`
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"payload"`
}

// runAlertLog reads the delivery log, and can put a row back in the queue.
//
// The log is the answer to the two questions this subsystem produces: "why did
// I get this" and, far more often, "why did I not". A dead row with its last
// error is what turns the second one from a mystery into a typo somebody can
// see.
func runAlertLog(c *context_, args []string) int {
	flags := newFlagSet(c, "alerts log")
	status := flags.String("status", "", "only pending, sent, failed or dead")
	limit := flags.Int("limit", 0, "how many rows to return")
	retry := flags.Int64("retry", 0, "put this notification back at the front of the queue")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	if *retry > 0 {
		var notification notificationPayload
		if err := client.do(c.ctx, http.MethodPost,
			"/alerts/notifications/"+strconv.FormatInt(*retry, 10)+"/retry", nil, &notification); err != nil {
			return c.fail(err)
		}
		return c.emit(*remote.asJSON, notification,
			fmt.Sprintf("notification %d queued again", notification.ID))
	}

	path := "/alerts/notifications"
	query := make([]string, 0, 2)
	if *status != "" {
		query = append(query, "status="+*status)
	}
	if *limit > 0 {
		query = append(query, "limit="+strconv.Itoa(*limit))
	}
	if len(query) > 0 {
		path += "?" + strings.Join(query, "&")
	}

	var page struct {
		Notifications []notificationPayload `json:"notifications"`
	}
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Notifications) == 0 {
		return c.emit(*remote.asJSON, page, "no notifications")
	}

	var text strings.Builder
	// Indexed rather than ranged by value: a row carries its whole rendered
	// payload.
	for index := range page.Notifications {
		notification := &page.Notifications[index]
		detail := notification.Payload.Title
		if notification.LastError != "" {
			detail = notification.LastError
		}
		fmt.Fprintf(&text, "%-5d %-8s ch%-3d %-13s %s\n",
			notification.ID, notification.Status, notification.ChannelID,
			notification.Payload.Event, detail)
	}
	return c.emit(*remote.asJSON, page, strings.TrimRight(text.String(), "\n"))
}
