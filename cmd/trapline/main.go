// Command trapline is the whole product: server, CLI and, later, the embedded
// web UI and MCP server, in one binary.
package main

import (
	"os"

	"github.com/antoniojosev/trapline/internal/adapters/cli"
)

func main() {
	os.Exit(cli.Main())
}
