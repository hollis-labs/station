// Command clock-plugin is the other of Station's two starter virtual
// MCPs: a lightweight inprocess-mode plugin speaking
// plugin-sdk/subprocess's JSON-RPC dialect, never implementing MCP
// itself. mcp-host performs the one real MCP handshake on its behalf.
//
// Its tool catalog is declared in ../../station.yaml's inprocess.tools
// block, not here — mcp-host never sends mcp/list_tools to a running
// plugin (see github.com/hollis-labs/mcp-host's transport/inprocess
// package doc), which is what makes plugin-sdk's own documented
// subprocess.Serve helper usable here.
//
// An inprocess-mode starting point: replace this tool with whatever
// Station's first real Go-native capability turns out to be.
package main

import (
	"context"
	"encoding/json"
	"os"
	"time"

	plugin "github.com/hollis-labs/plugin-sdk"
	sdksub "github.com/hollis-labs/plugin-sdk/subprocess"
)

type clockPlugin struct{}

func (clockPlugin) Init(ctx context.Context, params sdksub.InitParams) (sdksub.InitResult, error) {
	return sdksub.InitResult{
		ID: "clock-plugin", Name: "Clock Plugin", Version: "0.1.0",
		Description: "Reports the current time.",
		Protocol:    sdksub.ProtocolVersion,
	}, nil
}

func (clockPlugin) Load(ctx context.Context) (sdksub.LoadResult, error) {
	return sdksub.LoadResult{}, nil
}

func (clockPlugin) Unload(ctx context.Context) error { return nil }

func (clockPlugin) Health(ctx context.Context) (sdksub.HealthStatus, error) {
	return sdksub.HealthStatus{OK: true}, nil
}

func (clockPlugin) MCPCallTool(ctx context.Context, req sdksub.MCPCallRequest) (sdksub.MCPCallResult, error) {
	if req.ToolName != "now" {
		return sdksub.MCPCallResult{}, plugin.ErrNotFound("unknown tool " + req.ToolName)
	}
	payload, err := json.Marshal(map[string]any{"now": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return sdksub.MCPCallResult{}, err
	}
	return sdksub.MCPCallResult{Content: payload}, nil
}

func main() {
	if err := sdksub.Serve(clockPlugin{}); err != nil {
		os.Exit(1)
	}
}
