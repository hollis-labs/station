// Command echo-server is one of Station's two starter virtual MCPs: a
// real, standalone MCP server speaking the protocol itself over stdio,
// with no dependency on Station or mcp-host — it doesn't know it will
// typically be spawned and supervised as a process-mode logical server
// (see ../../station.yaml).
//
// A process-mode starting point: replace this tool with whatever
// Station's first real capability turns out to be, in any language,
// reached over stdio or HTTP.
package main

import (
	"context"
	"log"

	gmcpserver "github.com/hollis-labs/go-mcp/server"
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
