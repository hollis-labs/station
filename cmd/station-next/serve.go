package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/auth"
	"github.com/hollis-labs/substrate/modules/health"
	"github.com/hollis-labs/substrate/modules/httphost"
	"github.com/hollis-labs/substrate/modules/identity"
	"github.com/hollis-labs/substrate/modules/secrets"

	"github.com/hollis-labs/station/internal/host"
	"github.com/hollis-labs/station/internal/hostguard"
	"github.com/hollis-labs/station/internal/settings"
)

// shutdownGrace bounds each shutdown step after the signal: in-flight HTTP requests get this long
// to finish, then the logical servers get this long to stop. A supervisor stop timeout must
// exceed shutdownGrace + childStopTimeout.
const shutdownGrace = 5 * time.Second

// childStopTimeout bounds stopping the logical servers: mcp-host closes a child's session, waits,
// sends SIGTERM, waits, then SIGKILL (5s each), so this must exceed 10s for a busy child to be killed
// rather than orphaned. Total shutdown is at most shutdownGrace + childStopTimeout.
const childStopTimeout = 12 * time.Second

// station is what the commands share: the started framework app. ready, when set, learns the
// bound address (tests listen on port 0). event, when set, records the shutdown steps in order.
type station struct {
	app   *module.App
	ready func(net.Addr)
	event func(string)
}

func (s *station) note(what string) {
	if s.event != nil {
		s.event(what)
	}
}

func (s *station) serveCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the configured logical MCP servers behind identity-issued keys",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return s.serve(ctx) },
	}
}

func (s *station) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the station: config block without spawning anything",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := settings.Parse(s.app.Config[settings.Key])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "station: OK, %d logical server(s) configured\n", len(cfg.MCP.LogicalServers))
			return err
		},
	}
}

func (s *station) listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print every configured logical server without spawning or dialing anything",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := settings.Parse(s.app.Config[settings.Key])
			if err != nil {
				return err
			}
			for _, sum := range cfg.MCP.Summarize() {
				tools := "(discovered when served)"
				if sum.ToolNames != nil {
					tools = strings.Join(sum.ToolNames, ", ")
				}
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%-14s %-20s %-10s scope: %s tools: %s\n", sum.ID, sum.Name, sum.Transport, settings.Scope(s.app.Name, sum.ID), tools); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// serve starts the logical servers, mounts them behind the guard and serves until ctx ends.
// Shutdown order is explicit: stop accepting and drain HTTP (Serve returns), then stop the
// logical servers, then the framework modules stop (the caller's deferred Stop).
func (s *station) serve(ctx context.Context) error {
	app := s.app
	if err := hostguard.RequireLoopbackBind(listenAddr(app)); err != nil {
		return err
	}
	cfg, err := settings.Parse(app.Config[settings.Key])
	if err != nil {
		return err
	}
	id := identity.From(app)
	resolver := secrets.Resolver(app)
	if id == nil || resolver == nil {
		return errors.New("the identity and secrets modules must be started")
	}
	h, err := host.Start(ctx, app.Name, cfg.MCP, resolver, auth.ScopeTable(app), health.From(app), slog.Default())
	if err != nil {
		return err
	}
	handler, err := httphost.Handler(app, httphost.Options{Mounts: h.Mounts()})
	if err != nil {
		h.Close(context.Background())
		return err
	}
	// Outermost first: the Host check, then the session cookie becomes the bearer the guard
	// verifies, then the host (guard, mounts).
	handler = hostguard.Handler(cfg.AllowedHosts, id.Middleware(handler))

	serveErr := httphost.Serve(ctx, app, handler, httphost.ServeOptions{
		GracePeriod: shutdownGrace,
		Ready: func(addr net.Addr) {
			slog.Info("station listening", "addr", addr.String())
			if s.ready != nil {
				s.ready(addr)
			}
		},
	})
	s.note("http-drained")
	if errors.Is(serveErr, context.DeadlineExceeded) {
		// A request outlived the grace period and was cut: a normal end for a stop signal, not a failure.
		slog.Warn("station: requests still in flight at the shutdown deadline were cut", "grace", shutdownGrace.String())
		serveErr = nil
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), childStopTimeout)
	defer cancel()
	if left := h.Close(closeCtx); len(left) > 0 {
		s.note("logical-servers-timeout")
	} else {
		s.note("logical-servers-stopped")
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
