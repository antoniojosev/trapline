package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/antoniojosev/trapline/internal/config"
)

// defaultURL matches the server's default listen address, so the common case
// — administering the installation on this machine — needs no configuration.
const defaultURL = "http://" + config.DefaultAddr

// remoteFlags are the flags every command that talks to a server accepts.
type remoteFlags struct {
	url    *string
	token  *string
	asJSON *bool
}

func addRemoteFlags(flags interface {
	String(string, string, string) *string
	Bool(string, bool, string) *bool
}) remoteFlags {
	return remoteFlags{
		url:    flags.String("url", "", "server base URL (env TRAPLINE_URL)"),
		token:  flags.String("token", "", "API token (env TRAPLINE_TOKEN)"),
		asJSON: flags.Bool("json", false, "emit JSON"),
	}
}

func (r remoteFlags) client(c *context_) (*apiClient, error) {
	return newAPIClient(
		strings.TrimRight(resolve(*r.url, c.env, "TRAPLINE_URL", defaultURL), "/"),
		resolve(*r.token, c.env, "TRAPLINE_TOKEN", ""),
	)
}

// runProjects dispatches the project subcommands.
func runProjects(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "projects: expected list, create or delete")
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return runProjectsList(c, args[1:])
	case "create":
		return runProjectsCreate(c, args[1:])
	case "delete":
		return runProjectsDelete(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "projects: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

func runProjectsList(c *context_, args []string) int {
	flags := newFlagSet(c, "projects list")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var projects []projectPayload
	if err := client.do(c.ctx, http.MethodGet, "/projects", nil, &projects); err != nil {
		return c.fail(err)
	}
	if len(projects) == 0 {
		return c.emit(*remote.asJSON, []projectPayload{}, "no projects")
	}

	var text strings.Builder
	for _, project := range projects {
		// The slug is between the name and the DSN because it is the third
		// thing a pipeline needs and there is nowhere else to read it: it is
		// derived, not chosen, so nobody knows it until the server says so.
		fmt.Fprintf(&text, "%d\t%s\t%s\t%s\n", project.ID, project.Name, project.Slug, project.DSN)
	}
	return c.emit(*remote.asJSON, projects, strings.TrimRight(text.String(), "\n"))
}

func runProjectsCreate(c *context_, args []string) int {
	flags := newFlagSet(c, "projects create")
	name := flags.String("name", "", "project name (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" {
		fmt.Fprintln(c.stderr, "projects create: -name is required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var project projectPayload
	if err := client.do(c.ctx, http.MethodPost, "/projects", createProjectPayload{Name: *name}, &project); err != nil {
		return c.fail(err)
	}
	// The DSN alone in text mode: it is the one thing you came for, and it
	// makes `trapline projects create -name x` pipeable straight into a
	// config file or an env var.
	return c.emit(*remote.asJSON, project, project.DSN)
}

func runProjectsDelete(c *context_, args []string) int {
	flags := newFlagSet(c, "projects delete")
	id := flags.Int64("id", 0, "project id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *id <= 0 {
		fmt.Fprintln(c.stderr, "projects delete: -id is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := "/projects/" + strconv.FormatInt(*id, 10)
	if err := client.do(c.ctx, http.MethodDelete, path, nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, map[string]string{"status": "deleted"}, "deleted")
}

// runKeys dispatches the DSN key subcommands.
func runKeys(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "keys: expected rotate or revoke")
		return ExitUsage
	}
	switch args[0] {
	case "rotate":
		return runKeysRotate(c, args[1:])
	case "revoke":
		return runKeysRevoke(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "keys: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

func runKeysRotate(c *context_, args []string) int {
	flags := newFlagSet(c, "keys rotate")
	projectID := flags.Int64("project", 0, "project id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "keys rotate: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var key keyPayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/keys"
	if err := client.do(c.ctx, http.MethodPost, path, nil, &key); err != nil {
		return c.fail(err)
	}
	// Both keys stay live until the old one is revoked, and the text output
	// says so: rotation is not finished when this command returns.
	return c.emit(*remote.asJSON, key,
		key.DSN+"\n\nThe previous key still works. Deploy this one, then run:\n  trapline keys revoke -key <old-public-key>")
}

func runKeysRevoke(c *context_, args []string) int {
	flags := newFlagSet(c, "keys revoke")
	publicKey := flags.String("key", "", "public key to revoke (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *publicKey == "" {
		fmt.Fprintln(c.stderr, "keys revoke: -key is required")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	if err := client.do(c.ctx, http.MethodDelete, "/keys/"+*publicKey, nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, map[string]string{"status": "revoked"}, "revoked")
}
