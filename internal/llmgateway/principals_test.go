package llmgateway

import (
	"strings"
	"testing"
)

func TestUnmatchedPrincipals(t *testing.T) {
	cfg := Config{
		Policy: PolicyConfig{Rules: []RuleConfig{
			{ID: "any"},
			{ID: "old-client", Principal: "jev-client"},
			{ID: "all-keys", Principal: "key:*"},
			{ID: "one-key", Principal: "key:k_abc"},
			{ID: "star", Principal: "*"},
			{ID: "users", Principal: "user:*"},
		}},
		Budgets: []BudgetConfig{{ID: "ci", Principal: "ci-*"}, {ID: "all"}},
	}
	got := strings.Join(UnmatchedPrincipals(cfg, []string{"key:k_abc", "key:k_def"}), "\n")
	for _, want := range []string{`policy rule "old-client"`, `budget "ci"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	for _, not := range []string{`"any"`, `"all-keys"`, `"one-key"`, `"all"`, "k_abc"} {
		if strings.Contains(got, not) {
			t.Errorf("unexpected %s in:\n%s", not, got)
		}
	}
	// `*` and `user:*` can match a signed-in user, so they are never reported; named keys and old names are.
	if got := UnmatchedPrincipals(cfg, nil); len(got) != 4 {
		t.Errorf("with no keys every named principal is unmatched: %v", got)
	}
}
