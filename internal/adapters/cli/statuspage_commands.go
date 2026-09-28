package cli

import (
	"fmt"
	"net/http"
	"strings"
)

// runStatusPage reads and writes the public page's heading.
//
// Under `monitors` because that is what the page shows, and next to the two
// families for the reason ADR 037 gives: an operator asking "what is this
// installation watching, and what does the world see of it" should find the
// answer in one place. Whether a given project publishes at all is part of
// that project's configuration and lives in `config set -status-page`.
func runStatusPage(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "monitors status-page: expected show or set")
		return ExitUsage
	}
	switch args[0] {
	case "show":
		return runStatusPageShow(c, args[1:])
	case "set":
		return runStatusPageSet(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "monitors status-page: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type statusPagePayload struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

const statusPagePath = "/system/settings/status-page"

func runStatusPageShow(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors status-page show")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var settings statusPagePayload
	if err := client.do(c.ctx, http.MethodGet, statusPagePath, nil, &settings); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, settings, renderStatusPage(&settings))
}

func renderStatusPage(settings *statusPagePayload) string {
	var text strings.Builder
	title := settings.Title
	if title == "" {
		// Said rather than left blank: an empty line here reads like a bug,
		// and what actually happens is that each page falls back to its own
		// project's name.
		title = `(unset — each page uses its project's name)`
	}
	fmt.Fprintf(&text, "title        %s\n", title)
	description := settings.Description
	if description == "" {
		description = "(none)"
	}
	fmt.Fprintf(&text, "description  %s", description)
	return text.String()
}

// runStatusPageSet writes both fields.
//
// Both together, because they are one heading and the server takes them that
// way (PUT, not PATCH). An omitted flag is an empty value here rather than
// "leave it alone", and that is stated in the flag help: the alternative is a
// command that cannot clear a title somebody regrets.
func runStatusPageSet(c *context_, args []string) int {
	flags := newFlagSet(c, "monitors status-page set")
	title := flags.String("title", "",
		"heading on every public status page; empty falls back to the project's name")
	description := flags.String("description", "",
		"line under the heading; empty means none. Both flags are written together")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	body := statusPagePayload{Title: *title, Description: *description}
	var settings statusPagePayload
	if err := client.do(c.ctx, http.MethodPut, statusPagePath, body, &settings); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, settings, renderStatusPage(&settings))
}
