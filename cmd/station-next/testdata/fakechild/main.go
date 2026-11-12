// Command fakechild stands in for a production child server in tests. It is a real stdio MCP
// server with one tool, probe, that reports facts about its own environment and arguments without
// ever returning a value: for each requested env name whether it is set, its length and the first
// eight hex digits of its SHA-256, plus its argument list.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	gmcpserver "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

func main() {
	srv := gmcpserver.NewServer("fakechild", "0.1.0")
	srv.RegisterTool(gmcpserver.Tool{
		Name:         "probe",
		Description:  "Reports facts about env names and arguments, never values.",
		InputSchema:  gmcpserver.InputSchema(gmcpserver.StringProp("names", "comma separated env names", false)),
		ReadOnlyHint: true,
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			out := map[string]any{}
			names, _ := args["names"].(string)
			for _, n := range strings.Split(names, ",") {
				n = strings.TrimSpace(n)
				if n == "" {
					continue
				}
				v, ok := os.LookupEnv(n)
				sum := sha256.Sum256([]byte(v))
				out[n] = map[string]any{"set": ok && v != "", "len": len(v), "sha8": hex.EncodeToString(sum[:])[:8]}
			}
			return map[string]any{"env": out, "args": os.Args[1:]}, nil
		},
	})
	srv.RegisterTool(gmcpserver.Tool{
		Name:         "sleep",
		Description:  "Waits for the given number of milliseconds, then answers.",
		InputSchema:  gmcpserver.InputSchema(gmcpserver.StringProp("ms", "milliseconds", true)),
		ReadOnlyHint: true,
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			ms, _ := strconv.Atoi(fmt.Sprint(args["ms"]))
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
			case <-ctx.Done():
			}
			return map[string]any{"slept_ms": ms}, nil
		},
	})
	if err := srv.Run(context.Background()); err != nil {
		log.Fatalf("fakechild: %v", err)
	}
}
