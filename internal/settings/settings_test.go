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
