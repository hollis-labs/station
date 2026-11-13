package settings

import (
	"strings"
	"testing"
)

func block(servers ...map[string]any) map[string]any {
	list := make([]any, len(servers))
	for i, s := range servers {
		list[i] = s
	}
	return map[string]any{"logical_servers": list}
}

func server(id, path string, extra map[string]any) map[string]any {
	s := map[string]any{
		"id": id, "name": id, "transport": "process",
		"process": map[string]any{"command": "/bin/true"},
		"serve":   map[string]any{"http": map[string]any{"path": path}},
	}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

func TestParseAcceptsAndScopes(t *testing.T) {
	cfg, err := Parse(block(server("avatar", "/avatar", nil), server("design-suite", "/design-suite", nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCP.LogicalServers) != 2 || Scope("station", "design-suite") != "station.design-suite" {
		t.Fatalf("got %d servers, scope %q", len(cfg.MCP.LogicalServers), Scope("station", "design-suite"))
	}
}

func TestParseRefusesWithTheKeyNamed(t *testing.T) {
	withTokens := server("a", "/a", nil)
	withTokens["serve"] = map[string]any{"http": map[string]any{"path": "/a", "bearer_tokens": []any{"x"}}}
	stdio := server("s", "/s", nil)
	stdio["serve"] = map[string]any{"stdio": true}
	cases := map[string]struct {
		raw  any
		want string
	}{
		"nothing":        {nil, "logical_servers"},
		"unknown key":    {map[string]any{"logical_servers": block(server("a", "/a", nil))["logical_servers"], "bogus": 1}, "station.bogus"},
		"bad id":         {block(server("Bad_ID", "/a", nil)), "id must be"},
		"admin id":       {block(server("admin", "/a", nil)), "is reserved"},
		"admin no serve": {block(map[string]any{"id": "admin", "name": "A", "transport": "process", "process": map[string]any{"command": "/x"}}), "is reserved"},
		"bearer tokens":  {block(withTokens), "bearer_tokens was removed"},
		"stdio":          {block(stdio), "serve.stdio is not ported"},
		"reserved path":  {block(server("a", "/api/x", nil)), "serve.http.path"},
		"root path":      {block(server("a", "/", nil)), "serve.http.path"},
		"duplicate path": {block(server("a", "/p", nil), server("b", "/p", nil)), "already used by a"},
		"bad hosts":      {map[string]any{"logical_servers": block(server("a", "/a", nil))["logical_servers"], "allowed_hosts": "nope"}, "allowed_hosts"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(c.raw)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// Substrate delivers every number as a float64, and YAML writes one of a million or more in exponent form:
// an unquoted argument or environment value must still reach a process server as the digits written.
func TestParseKeepsLargeBareNumbersInArgsAndEnv(t *testing.T) {
	s := server("avatar", "/avatar", map[string]any{"process": map[string]any{
		"command": "/bin/true",
		"args":    []any{float64(1048576), "--port", float64(8080)},
		"env":     map[string]any{"MAX_BYTES": float64(1048576), "WORKERS": float64(4)},
	}})
	cfg, err := Parse(block(s))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.MCP.LogicalServers[0].Process
	if got := strings.Join(p.Args, " "); got != "1048576 --port 8080" {
		t.Fatalf("args = %q", got)
	}
	if p.Env["MAX_BYTES"] != "1048576" || p.Env["WORKERS"] != "4" {
		t.Fatalf("env = %v", p.Env)
	}
}
