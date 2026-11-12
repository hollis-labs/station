package main

import (
	"fmt"
	"strings"
)

// settingsKey is the app's own block in the config: the unit's listener-side settings. The gateway
// configuration (providers, models, policy, budgets, ledger) is a revision in the service database.
const settingsKey = "llm-gateway"

type settings struct {
	// AllowedHosts are extra Host names served besides loopback (a local TLS proxy's name).
	AllowedHosts []string
}

func parseSettings(raw any) (settings, error) {
	var out settings
	block, _ := raw.(map[string]any)
	for k := range block {
		if k != "allowed_hosts" {
			return out, fmt.Errorf("%s.%s: unknown key (known: allowed_hosts)", settingsKey, k)
		}
	}
	v, ok := block["allowed_hosts"]
	if !ok || v == nil {
		return out, nil
	}
	list, ok := v.([]any)
	if !ok {
		return out, fmt.Errorf("%s.allowed_hosts: want a list of host names", settingsKey)
	}
	for i, h := range list {
		s, ok := h.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return out, fmt.Errorf("%s.allowed_hosts[%d]: want a host name", settingsKey, i)
		}
		out.AllowedHosts = append(out.AllowedHosts, s)
	}
	return out, nil
}
