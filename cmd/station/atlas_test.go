package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/station/internal/atlas/atlastest"
)

// These tests drive the real atlas-server binary the way Station runs it: as a
// process-mode logical server, reached by a real MCP client through Station
// itself. Tesseract is a fake, so no daemon is needed. What they establish is
// what unit tests cannot: that the tools, their descriptions, annotations and
// errors survive Station's bridge intact, over both of the ways Station serves
// a logical server.

// atlasConfig writes a station.yaml with atlas as the only logical server,
// served as given (the `serve:` block's body), and returns its path.
func atlasConfig(t *testing.T, tesseractURL, serve string) string {
	t.Helper()
	atlasBin := buildBinary(t, "../../plugins/atlas-server", "atlas-server")
	config := fmt.Sprintf(`
logical_servers:
  - id: atlas
    name: Atlas
    transport: process
    process:
      command: %q
      env:
        TESSERACT_URL: %q
        ATLAS_CACHE_TTL: "0"
    serve:
%s
`, atlasBin, tesseractURL, serve)
	path := filepath.Join(t.TempDir(), "station.yaml")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func atlasFake(t *testing.T) *atlastest.Fake {
	t.Helper()
	return atlastest.New(t,
		atlastest.Record("question", "ATLAS-Q-001",
			atlastest.Data(map[string]any{"question": "Q?", "why_it_matters": "x", "what_would_answer_it": "y"}),
			atlastest.State(map[string]any{"status": "open"})),
		atlastest.Record("question", "ATLAS-Q-002", atlastest.State(map[string]any{"status": "answered"})),
	)
}

func TestAtlasServerThroughStationStdio(t *testing.T) {
	fake := atlasFake(t)
	stationBin := buildStationBinary(t)
	configPath := atlasConfig(t, fake.URL(), "      stdio: true")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stationCmd := exec.CommandContext(ctx, stationBin, "-config", configPath)
	stationCmd.Stderr = os.Stderr
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "atlas-integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: stationCmd}, nil)
	if err != nil {
		t.Fatalf("connect to station: %v", err)
	}
	defer session.Close()

	checkAtlasSession(t, ctx, session)
}

func TestAtlasServerThroughStationHTTP(t *testing.T) {
	fake := atlasFake(t)
	stationBin := buildStationBinary(t)
	configPath := atlasConfig(t, fake.URL(), "      http:\n        path: /atlas")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stationCmd := exec.CommandContext(ctx, stationBin, "-config", configPath, "-http-addr", addr)
	stationCmd.Stderr = os.Stderr
	if err := stationCmd.Start(); err != nil {
		t.Fatalf("start station: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = stationCmd.Wait() })

	url := "http://" + addr + "/atlas"
	waitForHTTPReachable(t, ctx, url)
	session := connectHTTP(t, ctx, url)
	defer session.Close()

	checkAtlasSession(t, ctx, session)
}

func checkAtlasSession(t *testing.T, ctx context.Context, session *sdkmcp.ClientSession) {
	t.Helper()

	listed, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	byName := map[string]*sdkmcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"atlas_guide", "atlas_get", "atlas_list", "atlas_search"} {
		tool := byName[name]
		if tool == nil {
			t.Errorf("%s is not served through Station", name)
			continue
		}
		if tool.Description == "" || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s lost its description or read-only annotation crossing the bridge: %+v", name, tool)
		}
	}

	text := func(res *sdkmcp.CallToolResult) string {
		t.Helper()
		if len(res.Content) == 0 {
			t.Fatal("empty content")
		}
		tc, ok := res.Content[0].(*sdkmcp.TextContent)
		if !ok {
			t.Fatalf("content[0] = %T", res.Content[0])
		}
		return tc.Text
	}

	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "atlas_list", Arguments: map[string]any{"class": "question", "state": "open"}})
	if err != nil || res.IsError {
		t.Fatalf("atlas_list: err=%v res=%+v", err, res)
	}
	var list struct {
		Total int `json:"total"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(text(res)), &list); err != nil {
		t.Fatalf("atlas_list result is not the documented shape: %v\n%s", err, text(res))
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != "ATLAS-Q-001" {
		t.Errorf("atlas_list = %+v, want only the open question", list)
	}

	// An agent's mistake comes back as a tool error it can act on, not a
	// protocol failure: the code, the way forward and the guide all survive.
	res, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "atlas_get", Arguments: map[string]any{"id": "ATLAS-Q-099"}})
	if err != nil {
		t.Fatalf("atlas_get on a missing key returned a protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("atlas_get on a missing key: IsError=false, %s", text(res))
	}
	for _, want := range []string{"not_found", "atlas_list class=question", "atlas_guide"} {
		if !strings.Contains(text(res), want) {
			t.Errorf("error %q lacks %q", text(res), want)
		}
	}

	guide, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "atlas_guide"})
	if err != nil || guide.IsError || !strings.Contains(text(guide), "ATLAS-Q-NNN") {
		t.Errorf("atlas_guide through Station: err=%v %s", err, text(guide))
	}
}
