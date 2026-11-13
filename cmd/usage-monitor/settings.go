package main

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/station/internal/cfgnum"
	"github.com/hollis-labs/station/internal/usagemonitor"
)

// settingsKey is the app's own block in the config.
const settingsKey = "usage-monitor"

type settings struct {
	usagemonitor.Config
	// AllowedHosts are extra Host names served besides loopback (a local TLS proxy's name).
	AllowedHosts []string
}

// parseSettings validates the raw block (app.Config["usage-monitor"]). Unknown keys are refused.
func parseSettings(raw any) (settings, error) {
	var out settings
	block, _ := raw.(map[string]any)
	if v, ok := block["allowed_hosts"]; ok && v != nil {
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
	}
	rest := map[string]any{}
	for k, v := range block {
		if k != "allowed_hosts" {
			rest[k] = v
		}
	}
	b, err := yaml.Marshal(cfgnum.Integers(rest))
	if err != nil {
		return out, fmt.Errorf("%s: %w", settingsKey, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&out.Config); err != nil && err.Error() != "EOF" {
		return out, fmt.Errorf("%s: %w", settingsKey, err)
	}
	return out, nil
}
