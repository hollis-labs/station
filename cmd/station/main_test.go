package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gmcpserver "github.com/hollis-labs/go-mcp/server"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixtureEnvVar makes the test binary act as a trivial standalone MCP
// server instead of running tests — the real station binary spawns this
// as its process-mode child, exercising mcp-host's real spawn path
// end to end rather than a mocked one.
const fixtureEnvVar = "STATION_TEST_FIXTURE_ECHO"

func TestMain(m *testing.M) {
	if os.Getenv(fixtureEnvVar) != "" {
		runFixtureServer()
		return
	}
	os.Exit(m.Run())
}

func runFixtureServer() {
	srv := gmcpserver.NewServer("station-fixture", "test")
	srv.RegisterTool(gmcpserver.Tool{
		Name:            "ping",
		Description:     "Echoes back the given message.",
		InputSchema:     gmcpserver.InputSchema(gmcpserver.StringProp("message", "message to echo", false)),
		ReadOnlyHint:    true,
		DestructiveHint: false,
		IdempotentHint:  true,
		OpenWorldHint:   false,
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			msg, _ := args["message"].(string)
			return map[string]any{"pong": fmt.Sprintf("pong:%s", msg)}, nil
		},
	})
	_ = srv.Run(context.Background())
	os.Exit(0)
}

// buildStationBinary compiles the real station binary (this package)
// into a temp directory and returns its path.
func buildStationBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "station")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build station: %v\n%s", err, out)
	}
	return binPath
}

// TestStdioEndToEnd proves the whole stack — mcp-host (as an external
// dependency) plus Station's own wiring — end to end: the real station
// binary, reading a real config file, spawns a real process-mode child
// and serves that logical server over its own stdio to a real MCP
// client — a genuine tools/list + tools/call round trip.
func TestStdioEndToEnd(t *testing.T) {
	binPath := buildStationBinary(t)

	configPath := filepath.Join(t.TempDir(), "station.yaml")
	configYAML := fmt.Sprintf(`
logical_servers:
  - id: echo
    name: Echo Server
    transport: process
    process:
      command: %q
      env:
        %s: "1"
    serve:
      stdio: true
`, os.Args[0], fixtureEnvVar)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stationCmd := exec.CommandContext(ctx, binPath, "-config", configPath)
	stationCmd.Stderr = os.Stderr // surface station's own logs on test failure

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "station-integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: stationCmd}, nil)
	if err != nil {
		t.Fatalf("connect to station over stdio: %v", err)
	}
	defer session.Close()

	listRes, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listRes.Tools) != 1 || listRes.Tools[0].Name != "ping" {
		t.Fatalf("tools/list = %+v, want one tool named ping", listRes.Tools)
	}

	callRes, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "ping",
		Arguments: map[string]any{"message": "integration"},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("tools/call result IsError=true: %+v", callRes)
	}
	if len(callRes.Content) == 0 {
		t.Fatal("tools/call: empty content")
	}
	text, ok := callRes.Content[0].(*sdkmcp.TextContent)
	if !ok || text.Text == "" {
		t.Fatalf("tools/call content[0] = %+v, want non-empty text", callRes.Content[0])
	}
	t.Logf("tools/call ping result: %s", text.Text)
}
