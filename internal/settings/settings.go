// Package settings reads the app's own `station:` config block. The logical_servers list keeps
// mcp-host's schema; this package adds the checks the Substrate shape needs and refuses a bad
// value at start with a message that names the key.
package settings

import (
	"fmt"
	"regexp"
	"strings"

	mcpconfig "github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/config"
	"gopkg.in/yaml.v3"
)

// Key is the app's block in the config.
const Key = "station"

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Config is the parsed block.
type Config struct {
	// MCP is mcp-host's own configuration of the logical servers.
	MCP *mcpconfig.Config
	// AllowedHosts are extra Host names served besides loopback (a local TLS proxy's name).
	AllowedHosts []string
}

// Parse validates the raw block (app.Config["station"]).
func Parse(raw any) (*Config, error) {
	block, _ := raw.(map[string]any)
	if block == nil {
		return nil, fmt.Errorf("station: logical_servers: at least one logical server is required")
	}
	for k := range block {
		if k != "logical_servers" && k != "allowed_hosts" {
			return nil, fmt.Errorf("station.%s: unknown key (known: logical_servers, allowed_hosts)", k)
		}
	}
	out := &Config{}
	if v, ok := block["allowed_hosts"]; ok {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("station.allowed_hosts: want a list of host names")
		}
		for i, h := range list {
			s, ok := h.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("station.allowed_hosts[%d]: want a host name", i)
			}
			out.AllowedHosts = append(out.AllowedHosts, s)
		}
	}
	b, err := yaml.Marshal(map[string]any{"logical_servers": block["logical_servers"]})
	if err != nil {
		return nil, fmt.Errorf("station.logical_servers: %w", err)
	}
	cfg, err := mcpconfig.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("station.%w", err)
	}
	paths := map[string]string{}
	for i, ls := range cfg.LogicalServers {
		at := fmt.Sprintf("station.logical_servers[%d] (%s)", i, ls.ID)
		if !idPattern.MatchString(ls.ID) {
			return nil, fmt.Errorf("%s: id must be lower-case letters, digits and hyphens (it names the scope station.<id>)", at)
		}
		if ls.Serve == nil {
			continue
		}
		if ls.Serve.Stdio {
			return nil, fmt.Errorf("%s: serve.stdio is not ported yet (open decision: docs/conversion-notes.md); serve over http", at)
		}
		h := ls.Serve.HTTP
		if h == nil {
			continue
		}
		if len(h.BearerTokens) > 0 {
			return nil, fmt.Errorf("%s: serve.http.bearer_tokens was removed; callers present identity-issued keys with scope station.%s", at, ls.ID)
		}
		if !strings.HasPrefix(h.Path, "/") || h.Path == "/" || strings.HasPrefix(h.Path, "/api") || h.Path == "/healthz" || h.Path == "/mcp" {
			return nil, fmt.Errorf("%s: serve.http.path %q must start with / and not be /, /api..., /healthz or /mcp", at, h.Path)
		}
		if other, dup := paths[h.Path]; dup {
			return nil, fmt.Errorf("%s: serve.http.path %q is already used by %s", at, h.Path, other)
		}
		paths[h.Path] = ls.ID
	}
	out.MCP = cfg
	return out, nil
}

// Scope is the scope that grants access to one logical server.
func Scope(app, id string) string { return app + "." + id }
