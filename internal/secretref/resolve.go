// Package secretref resolves credential-capability references
// (keychain://…, helper://…) that appear in a process-mode logical
// server's env map, before Station spawns it.
//
// Per adr_api_to_mcp_projection (Tesseract, project/atlas/knowledge/adr),
// decision 4: Station resolves a manifest's declared credential reference
// and hands the resolved value to the spawned process over its own env,
// keyed by name — never the other way around, and the spawned process
// (an api-projection interpreter, or anything else) never touches the
// credential store itself. This package makes that step generic rather
// than api-projection-specific: it resolves any reference-valued env
// entry on any process-mode logical server, using
// api-projection/credential's resolver — the same keychain:// / helper://
// scheme used elsewhere in the portfolio (Tether, Cerberus), under its own
// "api-projection" keychain service.
//
// A logical server whose env holds no references is left untouched. An
// unresolvable reference is a fatal config error — mcp-host's own
// mcphost.Run is never reached with a blank credential silently spawned.
package secretref

import (
	"context"
	"fmt"

	"github.com/hollis-labs/api-projection/credential"
	"github.com/hollis-labs/mcp-host/config"
)

// resolver is the seam credential.Resolver satisfies — narrowed to what
// this package needs, so tests can substitute a fake without touching the
// real OS keychain.
type resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// newResolver is overridden in tests.
var newResolver = func() resolver { return credential.NewResolver() }

// Resolve replaces every keychain:// / helper:// reference in cfg's
// process-mode logical servers' env maps with its resolved value, in
// place. Non-reference env values are left untouched.
func Resolve(ctx context.Context, cfg *config.Config) error {
	resolver := newResolver()
	for i := range cfg.LogicalServers {
		ls := &cfg.LogicalServers[i]
		if ls.Process == nil {
			continue
		}
		for key, value := range ls.Process.Env {
			if !credential.IsRef(value) {
				continue
			}
			resolved, err := resolver.Resolve(ctx, value)
			if err != nil {
				return fmt.Errorf("logical_servers[%d] (%s): env %s: %w", i, ls.ID, key, err)
			}
			ls.Process.Env[key] = resolved
		}
	}
	return nil
}
