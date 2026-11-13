package usagemonitor

import "fmt"

// UnmatchedPushPrincipals returns a line for every push source whose principal is not one of the given
// principal ids (the `key:<id>` of each issued, live key). A push is accepted only from the principal
// the source names, exactly; a source copied from the old configuration still names a client name,
// and its reporter would get source_forbidden. The lines carry source ids and the configured
// principal, never key ids.
func UnmatchedPushPrincipals(cfg Config, principals []string) []string {
	var out []string
	for _, s := range cfg.Sources {
		if s.Collector != "push" {
			continue
		}
		found := false
		for _, p := range principals {
			if p == s.Principal {
				found = true
				break
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("push source %q names principal %q, which no issued key can produce", s.ID, s.Principal))
		}
	}
	return out
}
