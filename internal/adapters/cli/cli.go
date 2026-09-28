// Package cli is the command-line adapter, and the process entry point.
//
// It is a first-class client of the same API as the web UI and the MCP server,
// never a thinner one (ADR 006). The contract it must keep, because an agent
// is a primary consumer:
//
//   - --json on every command
//   - no interactive prompts, ever
//   - stable, documented exit codes
//   - idempotent commands
//
// Commands split into two kinds. Those that operate on the installation's file
// — serve, backup, token — open the database directly, because they are what
// you run on the box. Everything else talks to a running server over HTTP, so
// the CLI can administer an installation it is not hosting.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Exit codes. These are part of the public contract: an agent branches on
// them, so they may be added to but never reassigned.
const (
	// ExitOK means the command succeeded.
	ExitOK = 0
	// ExitError means the command failed at runtime.
	ExitError = 1
	// ExitUsage means the arguments were wrong; nothing was attempted.
	ExitUsage = 2
)

// Version is the build version, overridden at link time.
var Version = "dev"

// Env is the environment lookup, replaced in tests.
type Env func(string) string

// command is one CLI verb.
type command struct {
	name    string
	summary string
	run     func(*context_, []string) int
}

// context_ carries what every command needs. Named with a trailing underscore
// to leave the identifier "context" free for the standard package, which
// almost every command also uses.
type context_ struct {
	ctx    context.Context
	env    Env
	stdout io.Writer
	stderr io.Writer
}

// Run executes one invocation and returns the process exit code. Streams and
// environment are arguments so tests exercise the real entry point rather than
// a rearranged copy of it.
func Run(ctx context.Context, args []string, env Env, stdout, stderr io.Writer) int {
	cmdCtx := &context_{ctx: ctx, env: env, stdout: stdout, stderr: stderr}

	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
	}

	switch args[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return ExitOK
	case "version":
		return runVersion(cmdCtx, args[1:])
	}

	for _, cmd := range commands() {
		if cmd.name == args[0] {
			return cmd.run(cmdCtx, args[1:])
		}
	}

	fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
	usage(stderr)
	return ExitUsage
}

func commands() []command {
	return []command{
		{"serve", "Run the server", runServe},
		{"backup", "Write a consistent copy of the database", runBackup},
		{"retention", "Delete events past their keep-window, now", runRetention},
		{"downsample", "Fold closed minute buckets into their hours, now", runDownsample},
		{"token", "Manage API tokens (local: needs the database)", runToken},
		{"projects", "List, create and delete projects", runProjects},
		{"keys", "Rotate and revoke DSN keys", runKeys},
		{"issues", "List, read and triage issues", runIssues},
		{"stats", "Events per hour, loudest issues, and breakdowns", runStats},
		{"releases", "Register releases, their commits and their deploys", runReleases},
		{"artifacts", "Upload, list and delete source maps and the scripts they resolve", runArtifacts},
		{"config", "Show and change a project's ingest profile", runConfig},
		{"transactions", "What is slow, what is busy, and what is failing", runTransactions},
		{"traces", "One request's waterfall, span by span", runTraces},
		{"alerts", "Channels, rules and the delivery log", runAlerts},
		{"monitors", "What should have run and did not, and what stopped answering", runMonitors},
		{"digest", "Preview the weekly report and set when it goes out", runDigest},
		{"mcp", "Serve the Model Context Protocol over stdio, for an agent", runMCP},
		{"doctor", "Diagnose an installation", runDoctor},
	}
}

// fail reports an error on stderr and returns the runtime exit code. Errors
// never go to stdout: an agent parses stdout, and a diagnostic mixed into it
// would corrupt the output it is trying to read.
func (c *context_) fail(err error) int {
	fmt.Fprintf(c.stderr, "%v\n", err)
	return ExitError
}

// emit writes a result, as JSON when asked and as text otherwise.
func (c *context_) emit(asJSON bool, payload any, text string) int {
	if asJSON {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return c.fail(fmt.Errorf("encoding output: %w", err))
		}
		fmt.Fprintf(c.stdout, "%s\n", encoded)
		return ExitOK
	}
	if text != "" {
		fmt.Fprintln(c.stdout, text)
	}
	return ExitOK
}

// newFlagSet builds a flag set that reports errors on stderr and never exits
// the process itself, so Run stays in control of the exit code.
func newFlagSet(c *context_, name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	return flags
}

func runVersion(c *context_, args []string) int {
	flags := newFlagSet(c, "version")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	return c.emit(*asJSON, map[string]string{"version": Version}, Version)
}

// resolve returns the first non-empty of a flag, an environment variable and a
// default. Flags win over the environment: a stale exported variable must not
// override what the operator just typed.
func resolve(flagValue string, env Env, envKey, fallback string) string {
	if flagValue != "" {
		return flagValue
	}
	if value := env(envKey); value != "" {
		return value
	}
	return fallback
}

func usage(w io.Writer) {
	var b strings.Builder
	b.WriteString("trapline — error tracking in a single binary\n\nUsage:\n  trapline <command> [flags]\n\nCommands:\n")
	for _, cmd := range commands() {
		fmt.Fprintf(&b, "  %-10s %s\n", cmd.name, cmd.summary)
	}
	b.WriteString("  version    Print the version\n  help       Print this message\n")
	b.WriteString(`
Every command accepts --json and never prompts.
Exit codes: 0 success, 1 runtime error, 2 usage error.

Server commands (serve, backup, token, retention, downsample) read the database directly:
  -db <path>            or TRAPLINE_DB

Remote commands (projects, keys, issues, stats, releases, artifacts, alerts, config,
digest, monitors, transactions, traces, mcp, doctor) talk to a running server:
  -url <base-url>       or TRAPLINE_URL     (default http://127.0.0.1:9000)
  -token <api-token>    or TRAPLINE_TOKEN

mcp is a remote command too, and the one whose stdout is not a result: it speaks
JSON-RPC on stdin and stdout for an agent that launched it. Point a client at the
command "trapline" with the argument "mcp" and TRAPLINE_URL/TRAPLINE_TOKEN in its
environment. A server serves the same tools at POST /mcp for an agent elsewhere.

doctor --quick is the exception: it reads the database directly and makes no
network call. It is what the container healthcheck runs.
`)
	fmt.Fprint(w, b.String())
}

// stdEnv is os.Getenv, used by main.
func stdEnv(key string) string { return os.Getenv(key) }

// Main is the process entry point, kept here so cmd/trapline stays a
// three-line file whose only job is to exist.
func Main() int {
	return Run(context.Background(), os.Args[1:], stdEnv, os.Stdout, os.Stderr)
}

// unwrapUsage reports whether an error came from flag parsing, which already
// printed its own message.
func unwrapUsage(err error) bool {
	return errors.Is(err, flag.ErrHelp)
}
