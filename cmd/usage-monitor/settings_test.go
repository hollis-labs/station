package main

import "testing"

// Substrate delivers every number as a float64, and YAML writes one of a million or more in exponent form:
// an unquoted account or source id must reach the monitor as the digits written, not "1.048576e+06".
func TestParseSettingsKeepsLargeBareNumbersInIDs(t *testing.T) {
	cfg, err := parseSettings(map[string]any{
		"accounts": []any{map[string]any{"id": float64(1048576), "provider": "anthropic", "kind": "subscription", "label": "x", "enabled": true}},
		"sources": []any{
			map[string]any{"id": float64(2097152), "account_id": float64(1048576), "collector": "push", "principal": "key:k_1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Accounts[0].ID; got != "1048576" {
		t.Fatalf("account id = %q", got)
	}
	if got := cfg.Sources[0].ID; got != "2097152" {
		t.Fatalf("source id = %q", got)
	}
	if got := cfg.Sources[0].AccountID; got != "1048576" {
		t.Fatalf("source account_id = %q", got)
	}
}
