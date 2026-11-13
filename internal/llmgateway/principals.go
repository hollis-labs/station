package llmgateway

import (
	"fmt"
	"path"
)

// UnmatchedPrincipals returns, for every policy rule and budget of cfg that names a principal, a line
// when none of the given principal ids (the `key:<id>` of each issued, live key) matches its glob.
// Rules and budgets match the caller's principal id, and callers are identity keys, so a rule written
// for an old client name never matches again and nothing says so: an allow falls to the default
// effect, a budget stops applying. An empty principal matches every caller and is not listed. The
// lines carry rule and budget ids and patterns, never key ids.
func UnmatchedPrincipals(cfg Config, principals []string) []string {
	var out []string
	check := func(kind, id, pattern string) {
		if pattern == "" {
			return
		}
		for _, p := range principals {
			if ok, err := path.Match(pattern, p); err == nil && ok {
				return
			}
		}
		out = append(out, fmt.Sprintf("%s %q names principal %q, which no issued key can produce", kind, id, pattern))
	}
	for _, r := range cfg.Policy.Rules {
		check("policy rule", r.ID, r.Principal)
	}
	for _, b := range cfg.Budgets {
		check("budget", b.ID, b.Principal)
	}
	return out
}
