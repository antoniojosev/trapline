package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	mcpadapter "github.com/antoniojosev/trapline/internal/adapters/mcp"
)

// runMCP serves the Model Context Protocol over stdin and stdout.
//
// This is how an agent on the operator's own machine reaches the product:
// the client launches `trapline mcp` as a subprocess and talks to it down a
// pipe, so there is no port to pick, no TLS and no second credential — the
// one in `TRAPLINE_TOKEN` is the whole of it.
//
// It is a **client** of the REST API and not a shortcut into the database,
// which is the decision ADR 022 exists to record. Two things follow, and both
// are the point: it can serve an installation running on another machine,
// which is what makes it usable from a laptop against a VPS; and it cannot
// do anything the API cannot, including skip a scope check, because every
// tool call is an HTTP request the server authorises exactly as it authorises
// one from curl.
//
// The same table is served over HTTP by the server itself at `POST /mcp`.
// There is one table (adapters/mcp/tools.go), and the gate asserts the two
// transports list it identically.
func runMCP(c *context_, args []string) int {
	flags := newFlagSet(c, "mcp")
	// The remote flags, minus --json: this command's stdout is a protocol
	// stream and not a result, so there is no output format to choose.
	url := flags.String("url", "", "server base URL (env TRAPLINE_URL)")
	token := flags.String("token", "", "API token (env TRAPLINE_TOKEN)")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}

	baseURL := strings.TrimRight(resolve(*url, c.env, "TRAPLINE_URL", defaultURL), "/")
	credential := resolve(*token, c.env, "TRAPLINE_TOKEN", "")
	if credential == "" {
		// Refused here rather than on the first tool call. An MCP client
		// reports a server that exits at startup as "failed to connect",
		// which is a message an operator can act on; a server that connects
		// and then answers "invalid credentials" to every tool looks like a
		// broken product.
		return c.fail(ErrNoToken)
	}

	// Nothing may reach stdout except protocol messages: a client parses that
	// stream as JSON-RPC, and one stray log line is a parse error that
	// presents as the server having crashed. Logging goes to stderr, where
	// the client shows it as the server's own output.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	caller := mcpadapter.NewHTTPCaller(baseURL, credential, apiPath(""))
	server := mcpadapter.New(caller, credential, Version)

	if err := server.RunStdio(c.ctx); err != nil {
		return c.fail(fmt.Errorf("serving MCP over stdio: %w", err))
	}
	return ExitOK
}
