// Command echo-server is a test fixture: a real, standalone MCP server speaking the protocol itself over stdio,
// with no dependency on Station or mcp-host. The tests spawn and supervise it as a process-mode logical server.
package main

import (
	"context"
	"log"

	gmcpserver "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

func main() {
	srv := gmcpserver.NewServer("echo-server", "0.1.0")
	srv.RegisterTool(gmcpserver.Tool{
		Name:        "echo",
		Description: "Echoes back the given message.",
		InputSchema: gmcpserver.InputSchema(gmcpserver.StringProp("message", "message to echo", true)),

		ReadOnlyHint:    true,
		DestructiveHint: false,
		IdempotentHint:  true,
		OpenWorldHint:   false,

		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			msg, _ := args["message"].(string)
			return map[string]any{"echo": msg}, nil
		},
	})

	if err := srv.Run(context.Background()); err != nil {
		log.Fatalf("echo-server: %v", err)
	}
}
