// Package host runs mcp-host's engine inside a Substrate service. It does not call
// mcphost.Run (which owns its own listener and signal handling); it composes the engine's public
// parts: bootstrap.BuildRegistry spawns and supervises the logical servers, serving builds each
// one's MCP server and Streamable HTTP handler, and this package hands the handlers to httphost
// as guarded mounts with one scope per logical server.
package host

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	libauth "github.com/hollis-labs/libs/security/auth"
	libsecrets "github.com/hollis-labs/libs/security/secrets"

	"github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/bootstrap"
	mcpconfig "github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/config"
	"github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/registry"
	"github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/serving"

	"github.com/hollis-labs/substrate/modules/health"
	"github.com/hollis-labs/substrate/modules/httphost"

	"github.com/hollis-labs/station/internal/settings"
)

// Resolver resolves a credential reference. The secrets module's resolver satisfies it.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (libsecrets.Resolved, error)
}

// Checks is where the host registers one health check per logical server. The health module's
// registry satisfies it.
type Checks interface {
	Add(name string, c health.Check)
}

// Host is the running engine: the registry of logical servers and the mounts that serve them.
type Host struct {
	reg    *registry.Registry
	order  []string
	mounts []httphost.Mount
	log    *slog.Logger
}

// Start resolves credential references in every process-mode server's env (through res, into a
// copy: the config is never changed), spawns the logical servers, and builds one guarded mount per
// HTTP-served server. Each mount's path is registered on table with scope <app>.<id>, so a mount
// can never be served without a scope (an unmapped mount is refused by the guard in any case).
func Start(ctx context.Context, app string, cfg *mcpconfig.Config, res Resolver, table *libauth.ScopeTable, checks Checks, log *slog.Logger) (*Host, error) {
	if log == nil {
		log = slog.Default()
	}
	resolved, err := resolveEnv(ctx, cfg, res)
	if err != nil {
		return nil, err
	}
	reg, err := bootstrap.BuildRegistry(ctx, resolved, log)
	if err != nil {
		return nil, fmt.Errorf("host: %w", err)
	}
	h := &Host{reg: reg, log: log}
	for _, ls := range cfg.LogicalServers {
		h.order = append(h.order, ls.ID)
		if checks != nil {
			if entry, ok := reg.Get(ls.ID); ok {
				checks.Add("mcp:"+ls.ID, cachedCheck(func(ctx context.Context) error {
					cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
					if _, err := entry.Transport.ListTools(cctx); err != nil {
						return fmt.Errorf("logical server %s is not answering: %w", ls.ID, err)
					}
					return nil
				}, healthTTL, time.Now))
			}
		}
		if ls.Serve == nil || ls.Serve.HTTP == nil {
			continue
		}
		entry, ok := reg.Get(ls.ID)
		if !ok {
			h.Close(ctx)
			return nil, fmt.Errorf("host: logical server %q missing from the registry after start", ls.ID)
		}
		serve := *ls.Serve.HTTP
		serve.BearerTokens = nil // the guard replaces per-server bearer tokens (settings refuses them)
		h.mounts = append(h.mounts, httphost.Mount{Pattern: serve.Path, Handler: serving.MountHTTP(serving.BuildMCPServer(entry), &serve)})
		if table != nil {
			table.Route("", serve.Path, settings.Scope(app, ls.ID))
		}
		log.Info("station: logical server mounted", "id", ls.ID, "path", serve.Path, "scope", settings.Scope(app, ls.ID))
	}
	return h, nil
}

// Mounts are the guarded mounts to pass to httphost.Options.
func (h *Host) Mounts() []httphost.Mount { return h.mounts }

// Close stops the logical servers, all at once, and waits for them. mcp-host's own stop sequence per
// child is: close the session, wait, SIGTERM, wait, SIGKILL (10 seconds at most), and registry
// removal blocks on a call in flight. Close is bounded by ctx: when ctx ends it returns the ids still
// stopping and leaves them to the process supervisor. It is safe to call twice.
func (h *Host) Close(ctx context.Context) (unfinished []string) {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		pending = map[string]bool{}
	)
	var ids []string
	for _, id := range h.order {
		if _, ok := h.reg.Get(id); ok {
			ids = append(ids, id)
			pending[id] = true // filled completely before any goroutine starts
		}
	}
	for _, id := range ids {
		wg.Go(func() {
			h.reg.Remove(id)
			h.log.Info("station: logical server stopped", "id", id)
			mu.Lock()
			delete(pending, id)
			mu.Unlock()
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		for id := range pending {
			unfinished = append(unfinished, id)
		}
		slices.Sort(unfinished)
		h.log.Warn("station: logical servers still stopping at the deadline", "ids", unfinished)
		return unfinished
	}
}

func resolveEnv(ctx context.Context, cfg *mcpconfig.Config, res Resolver) (*mcpconfig.Config, error) {
	out := *cfg
	out.LogicalServers = slices.Clone(cfg.LogicalServers)
	for i := range out.LogicalServers {
		ls := &out.LogicalServers[i]
		if ls.Process == nil {
			continue
		}
		p := *ls.Process
		p.Env = make(map[string]string, len(ls.Process.Env))
		for k, v := range ls.Process.Env {
			if !libsecrets.IsRef(v) {
				p.Env[k] = v
				continue
			}
			if res == nil {
				return nil, fmt.Errorf("host: logical_servers[%d] (%s): env %s is a credential reference but no resolver is configured", i, ls.ID, k)
			}
			r, err := res.Resolve(ctx, v)
			if err != nil {
				return nil, fmt.Errorf("host: logical_servers[%d] (%s): env %s: %w", i, ls.ID, k, err)
			}
			p.Env[k] = r.Secret.Reveal()
		}
		ls.Process = &p
	}
	return &out, nil
}
