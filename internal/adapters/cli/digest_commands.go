package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// runDigest is the weekly report from a terminal.
//
// Two subcommands, because they are two different things: one shows what the
// report says and the other decides when it is sent. The first is what
// somebody runs before configuring anywhere to send it to, which is the order
// people actually do this in — see the thing, then decide who gets it
// (ADR 006: the CLI is a first-class client, not a thinner one).
func runDigest(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "digest: expected preview or schedule")
		return ExitUsage
	}
	switch args[0] {
	case "preview":
		return runDigestPreview(c, args[1:])
	case "schedule":
		return runDigestSchedule(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "digest: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

// digestSchedulePayload mirrors the schedule endpoint's body.
type digestSchedulePayload struct {
	Weekday  string `json:"weekday"`
	Hour     int    `json:"hour"`
	Timezone string `json:"timezone"`
}

// digestPreviewPayload mirrors the preview endpoint's response.
type digestPreviewPayload struct {
	Schedule digestSchedulePayload `json:"schedule"`
	Text     string                `json:"text"`
	Report   struct {
		Covers   digestWindowPayload    `json:"covers"`
		Previous digestWindowPayload    `json:"previous"`
		Projects []digestProjectPayload `json:"projects"`
	} `json:"report"`
}

type digestWindowPayload struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type digestProjectPayload struct {
	ID              int64 `json:"id"`
	Events          int64 `json:"events"`
	PreviousEvents  int64 `json:"previous_events"`
	NewIssues       int64 `json:"new_issues"`
	RegressedIssues int64 `json:"regressed_issues"`
}

// runDigestPreview renders the report without sending it.
func runDigestPreview(c *context_, args []string) int {
	flags := newFlagSet(c, "digest preview")
	projectID := flags.Int64("project", 0, "limit the report to one project (default: every project)")
	at := flags.String("at", "", "the scheduled moment to report as of, RFC 3339 (default: now)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *at != "" {
		if _, err := time.Parse(time.RFC3339, *at); err != nil {
			fmt.Fprintf(c.stderr, "digest preview: -at must be an RFC 3339 instant: %v\n", err)
			return ExitUsage
		}
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	request := map[string]any{}
	if *projectID > 0 {
		request["project_id"] = *projectID
	}
	if *at != "" {
		request["at"] = *at
	}

	var preview digestPreviewPayload
	if err := client.do(c.ctx, http.MethodPost, "/digest/preview", request, &preview); err != nil {
		return c.fail(err)
	}

	// The rendered text is the whole point of the text output: what a person
	// wants from `digest preview` is to read the mail. --json carries the
	// numbers as fields as well, for the client that is a program.
	return c.emit(*remote.asJSON, preview, strings.TrimRight(preview.Text, "\n"))
}

// runDigestSchedule shows or sets when the report goes out.
func runDigestSchedule(c *context_, args []string) int {
	flags := newFlagSet(c, "digest schedule")
	day := flags.String("day", "", "the day it goes out: monday … sunday (setting it changes the schedule)")
	hour := flags.Int("hour", -1, "the hour it goes out, 0–23 UTC")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	// Naming either half is what makes this a write. Naming only one is
	// rejected rather than merged with what is stored: "Tuesday, at whatever
	// hour it was" reads like it means something and is a good way to move a
	// report to an hour nobody chose.
	changing := *day != "" || *hour >= 0
	if changing && (*day == "" || *hour < 0) {
		fmt.Fprintln(c.stderr, "digest schedule: set both -day and -hour, or neither to show the current schedule")
		return ExitUsage
	}

	var schedule digestSchedulePayload
	if changing {
		body := digestSchedulePayload{Weekday: *day, Hour: *hour}
		if err := client.do(c.ctx, http.MethodPut, "/system/settings/digest", body, &schedule); err != nil {
			return c.fail(err)
		}
	} else if err := client.do(c.ctx, http.MethodGet, "/system/settings/digest", nil, &schedule); err != nil {
		return c.fail(err)
	}

	return c.emit(*remote.asJSON, schedule,
		fmt.Sprintf("%s at %02d:00 %s", schedule.Weekday, schedule.Hour, schedule.Timezone))
}
