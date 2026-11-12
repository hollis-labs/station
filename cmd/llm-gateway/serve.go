package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/auth"
	"github.com/hollis-labs/substrate/modules/database"
	"github.com/hollis-labs/substrate/modules/httphost"
	"github.com/hollis-labs/substrate/modules/identity"
	"github.com/hollis-labs/substrate/modules/secrets"

	"github.com/hollis-labs/station/internal/hostguard"
	"github.com/hollis-labs/station/internal/llmgateway"
)

// shutdownGrace bounds draining in-flight requests after the signal (a stream that outlives it is
// cut); closeTimeout bounds the stops after that (the async ledger flushes what it holds). A
// supervisor stop timeout must exceed shutdownGrace + closeTimeout.
const (
	shutdownGrace = 15 * time.Second
	closeTimeout  = 10 * time.Second
)

// relayWired says whether the relay routes (chat, responses, messages, embeddings, systemone,
// models, catalog, usage) are served. Their error bodies are OpenAI- or Anthropic-shaped and
// httphost rewrites every error body into its flat shape (docs/conversion-notes.md, Gap 11), so
// they stay unmounted until the framework has a per-route opt-out and Station pins it.
const relayWired = false

// gateway is what the commands share: the started framework app. ready, when set, learns the bound
// address (tests listen on port 0). event, when set, records the shutdown steps in order.
type gateway struct {
	app   *module.App
	ready func(net.Addr)
	event func(string)
}

func (g *gateway) note(what string) {
	if g.event != nil {
		g.event(what)
	}
}

func (g *gateway) serveCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the gateway behind identity-issued keys",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return g.serve(ctx) },
	}
}

var errNoModules = errors.New("this command needs every module, but the command line was read as another command: serve must be the first word, and global flags that take a value are not supported")

// start loads the active revision and starts the gateway from it.
func (g *gateway) start(ctx context.Context) (*llmgateway.Service, int64, error) {
	st := database.From(g.app)
	res := secrets.Resolver(g.app)
	if st == nil || res == nil {
		return nil, 0, errNoModules
	}
	ms := llmgateway.NewManagementStore(st)
	rev, err := ms.Active(ctx)
	if errors.Is(err, llmgateway.ErrNoActiveRevision) {
		return nil, 0, errors.New("there is no active gateway revision: stage one (`llm-gateway gateway-store stage`) and activate it (`gateway-store activate`), or copy the old data with `gateway-store migrate-from`")
	}
	if err != nil {
		return nil, 0, err
	}
	svc, err := llmgateway.Start(ctx, newHost(slog.Default(), res), rev.Document.Gateway,
		llmgateway.Options{Store: st, Management: ms, RunningRevision: rev.ID})
	if err != nil {
		return nil, 0, err
	}
	return svc, rev.ID, nil
}

func (g *gateway) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Assemble the active revision without listening: resolves secrets, contacts metadata services, opens the ledger",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if _, err := parseSettings(g.app.Config[settingsKey]); err != nil {
				return err
			}
			svc, rev, err := g.start(c.Context())
			if err != nil {
				return err
			}
			defer func() { _ = svc.Close(context.Background()) }()
			_, err = fmt.Fprintf(c.OutOrStdout(), "llm-gateway: OK, revision %d, %d route(s)\n", rev, len(svc.Routes()))
			return err
		},
	}
}

// serve starts the gateway from the active revision, mounts its routes behind the guard and serves
// until ctx ends. Shutdown order is explicit: stop accepting and drain HTTP (Serve returns), then
// close the gateway (ledger flush, catalog refresh), then the framework modules stop (the
// caller's deferred Stop).
func (g *gateway) serve(ctx context.Context) error {
	app := g.app
	if err := hostguard.RequireLoopbackBind(listenAddr(app)); err != nil {
		return err
	}
	cfg, err := parseSettings(app.Config[settingsKey])
	if err != nil {
		return err
	}
	id := identity.From(app)
	if id == nil {
		return errNoModules
	}
	svc, rev, err := g.start(ctx)
	if err != nil {
		return err
	}
	slog.Info("llm-gateway: gateway configuration loaded", "revision", rev)

	table := auth.ScopeTable(app)
	mux := http.NewServeMux()
	skipped := 0
	for _, rt := range svc.Routes() {
		if rt.Raw && !relayWired {
			skipped++
			continue
		}
		table.Route(rt.Method, llmgateway.RoutePrefix+rt.Path, rt.Scope)
		mux.Handle(rt.Method+" "+llmgateway.RoutePrefix+rt.Path, rt.Limited())
	}
	if skipped > 0 {
		slog.Warn("llm-gateway: relay routes are not mounted until the framework's raw-error opt-out is pinned", "routes", skipped)
	}
	handler, err := httphost.Handler(app, httphost.Options{Routes: mux})
	if err != nil {
		_ = svc.Close(context.Background())
		return err
	}
	// Outermost first: the Host check, then the session cookie becomes the bearer the guard
	// verifies, then request ids and the Anthropic x-api-key bridge, then the host (guard, routes).
	handler = hostguard.Handler(cfg.AllowedHosts, id.Middleware(llmgateway.WithRequestID(llmgateway.APIKeyBridge(handler))))

	serveErr := httphost.Serve(ctx, app, handler, httphost.ServeOptions{
		GracePeriod: shutdownGrace,
		Ready: func(addr net.Addr) {
			slog.Info("llm-gateway listening", "addr", addr.String())
			if g.ready != nil {
				g.ready(addr)
			}
		},
	})
	g.note("http-drained")
	if errors.Is(serveErr, context.DeadlineExceeded) {
		// A request outlived the grace period and was cut: a normal end for a stop signal, not a failure.
		slog.Warn("llm-gateway: requests still in flight at the shutdown deadline were cut", "grace", shutdownGrace.String())
		serveErr = nil
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := svc.Close(closeCtx); err != nil {
		slog.Warn("llm-gateway: stopping the gateway failed", "err", err)
		g.note("gateway-close-failed")
	} else {
		g.note("gateway-closed")
	}
	return serveErr
}

// listenAddr is the address httphost will bind: config httphost.addr, else the module default.
func listenAddr(app *module.App) string {
	cfg, _ := app.Config["httphost"].(map[string]any)
	if a, _ := cfg["addr"].(string); a != "" {
		return a
	}
	return "127.0.0.1:8080"
}
