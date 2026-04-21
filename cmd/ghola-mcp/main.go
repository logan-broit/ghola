// Command ghola-mcp is the MCP stdio bridge to the ghola local
// service. Claude Code spawns it; tool calls are translated into
// HTTP POSTs against the ghola daemon on localhost:7421. All state
// lives in the daemon's sietch + Pipeline A.
//
// Configuration (env vars):
//
//   GHOLA_ADDR   URL of the ghola daemon   (http://localhost:7421)
//
// Wire up in Claude Code via:
//
//   claude mcp add ghola /path/to/ghola-mcp
package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"

	ghmcp "github.com/logan-broit/ghola/internal/mcp"
)

func main() {
	addr := os.Getenv("GHOLA_ADDR")
	if addr == "" {
		addr = "http://localhost:7421"
	}

	s := server.NewMCPServer("ghola", "0.1.0",
		server.WithToolCapabilities(true))
	ghmcp.Register(s, ghmcp.Config{BaseURL: addr})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "ghola-mcp: %v\n", err)
		os.Exit(1)
	}
}
