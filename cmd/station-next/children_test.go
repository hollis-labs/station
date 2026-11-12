package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/secrets"
)

// The three production child shapes, started the way production starts them, with stand-in values
// only: avatar (a binary with an env block of paths and settings plus a token reference), design-suite
// (a Python entry point with a provider key reference) and tripo (a shell wrapper with arguments).
func TestProductionChildShapes(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fakechild")
	if out, err := exec.Command("go", "build", "-o", fake, "github.com/hollis-labs/station/cmd/station-next/testdata/fakechild").CombinedOutput(); err != nil {
		t.Fatalf("build fakechild: %v\n%s", err, out)
	}
	t.Setenv("FAKECHILD", fake) // the tripo wrapper execs it; a wrapper in production execs its own binary

	const avatarToken, providerKey = "stand-in-hadron-token-0001", "stand-in-provider-key-0002"
	servers := fmt.Sprintf(`    - id: avatar
      name: Avatar
      transport: process
      process:
        command: %[1]q
        env:
          WORKFLOW_DIR: /srv/workflows
          HADRON_URL: http://127.0.0.1:1
          BUDGET_USD: "5"
          HADRON_TOKEN: file://stand-in/hadron-token
      serve: {http: {path: /avatar}}
    - id: design-suite
      name: Design suite
      transport: process
      process:
        command: python3
        args: [%[2]q]
        env:
          PROVIDER_KEY: file://stand-in/provider-key
      serve: {http: {path: /design-suite}}
    - id: tripo
      name: Tripo
      transport: process
      process:
        command: %[3]q
        args: [mcp, --toolset, full]
      serve: {http: {path: /tripo}}
`, fake, mustAbs(t, "testdata/design-suite.py"), mustAbs(t, "testdata/tripo-wrapper.sh"))

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	base, stop := startedWith(t, servers, func(ctx context.Context, app *module.App) {
		r := secrets.Resolver(app)
		for ref, v := range map[string]string{"file://stand-in/hadron-token": avatarToken, "file://stand-in/provider-key": providerKey} {
			if err := r.Set(ctx, ref, strings.NewReader(v)); err != nil {
				t.Fatalf("provision %s: %v", ref, err)
			}
		}
	})

	owner := newSession(t, base)
	owner.want(200, "POST", "/api/setup", map[string]any{"display_name": "Owner", "username": "owner", "password": "a long test passphrase"})
	k := owner.want(201, "POST", "/api/identity/keys", map[string]any{"name": "all", "scopes": []string{"station.*"}})
	var key string
	for _, v := range k {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "key_") {
			key = s
		}
	}
	if key == "" {
		t.Fatalf("no key issued: %v", keys(k))
	}

	sha8 := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:8] }
	probe := func(path, names string) map[string]any {
		t.Helper()
		if code, body := mcp(t, base, path, key, initialize); code != 200 {
			t.Fatalf("%s initialize = %d %s", path, code, body)
		}
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"probe","arguments":{"names":%q}}}`, names)
		code, resp := mcp(t, base, path, key, body)
		if code != 200 {
			t.Fatalf("%s tools/call = %d %s", path, code, resp)
		}
		// The response is an SSE event: find the data line, then the tool's text content.
		var rpc struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		for _, line := range strings.Split(resp, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				_ = json.Unmarshal([]byte(data), &rpc)
			}
		}
		if len(rpc.Result.Content) == 0 {
			t.Fatalf("%s: no content in %s", path, resp)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &out); err != nil {
			t.Fatalf("%s: %v in %s", path, err, rpc.Result.Content[0].Text)
		}
		return out
	}
	envOf := func(out map[string]any, name string) map[string]any {
		m, _ := out["env"].(map[string]any)[name].(map[string]any)
		return m
	}

	// avatar: plain settings arrive as written; the token reference arrives resolved.
	av := probe("/avatar", "WORKFLOW_DIR,BUDGET_USD,HADRON_TOKEN,HADRON_URL,NOT_SET")
	if e := envOf(av, "HADRON_TOKEN"); e["set"] != true || e["len"] != float64(len(avatarToken)) || e["sha8"] != sha8(avatarToken) {
		t.Fatalf("avatar token not resolved to the stand-in: %v", e)
	}
	if e := envOf(av, "WORKFLOW_DIR"); e["sha8"] != sha8("/srv/workflows") {
		t.Fatalf("avatar plain env: %v", e)
	}
	if e := envOf(av, "NOT_SET"); e["set"] != false {
		t.Fatalf("an unset name reads as set: %v", e)
	}
	// design-suite: a Python entry point; the provider key arrives resolved.
	ds := probe("/design-suite", "PROVIDER_KEY")
	if ds["python"] != true {
		t.Fatalf("design-suite stand-in did not run under python: %v", ds)
	}
	if e := envOf(ds, "PROVIDER_KEY"); e["set"] != true || e["sha8"] != sha8(providerKey) {
		t.Fatalf("design-suite key not resolved to the stand-in: %v", e)
	}
	// tripo: a shell wrapper that passes its arguments on (mcp --toolset full).
	tr := probe("/tripo", "")
	if got, _ := json.Marshal(tr["args"]); string(got) != `["mcp","--toolset","full"]` {
		t.Fatalf("tripo wrapper arguments = %s", got)
	}
	// A child never sees another child's secret.
	if e := envOf(probe("/tripo", "PROVIDER_KEY,HADRON_TOKEN"), "PROVIDER_KEY"); e["set"] != false {
		t.Fatalf("tripo sees design-suite's key: %v", e)
	}

	stop()
	for _, v := range []string{avatarToken, providerKey} {
		if strings.Contains(logs.String(), v) {
			t.Fatalf("a stand-in secret value appears in the log")
		}
	}
	if childAliveAt(t, fake) {
		t.Fatal("a child outlived serve")
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func childAliveNamed(t *testing.T, name string) bool {
	t.Helper()
	out, _ := exec.Command("pgrep", "-x", name).Output()
	return strings.TrimSpace(string(out)) != ""
}

// childAliveAt reports whether a process started from exactly this binary path is running, so a test is
// not confused by children another test or an earlier run left behind.
func childAliveAt(t *testing.T, path string) bool {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", "^"+path).Output()
	return strings.TrimSpace(string(out)) != ""
}
