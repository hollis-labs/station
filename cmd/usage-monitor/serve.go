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
	"github.com/hollis-labs/station/internal/usagemonitor"
)

// shutdownGrace bounds draining in-flight requests after the signal; collectorStop bounds waiting for
// the collectors (and an agy child) after that. A supervisor stop timeout must exceed both together.
const (
	shutdownGrace = 10 * time.Second
	collectorStop = 10 * time.Second
)

// monitor is what the commands share: the started framework app. ready, when set, learns the bound
// address (tests listen on port 0). event, when set, records the shutdown steps in order.
type monitor struct {
	app   *module.App
	ready func(net.Addr)
	event func(string)
}

func (m *monitor) note(what string) {
	if m.event != nil {
		m.event(what)
	}
}

var errNoModules = errors.New("this command needs every module, but the command line was read as another command: serve must be the first word, and global flags that take a value are not supported")

func (m *monitor) serveCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the usage monitor behind identity-issued keys and run its collectors",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return m.serve(ctx) },
	}
}

// start validates the configuration, records the bootstrap accounts and builds the monitor. It
// starts no collector.
func (m *monitor) start(ctx context.Context, readOnly bool) (*usagemonitor.Service, settings, error) {
	cfg, err := parseSettings(m.app.Config[settingsKey])
	if err != nil {
		return nil, cfg, err
	}
	st, res := database.From(m.app), secrets.Resolver(m.app)
	if st == nil || res == nil {
		return nil, cfg, errNoModules
	}
	svc, err := usagemonitor.Start(ctx, secretHost{log: slog.Default(), res: res}, cfg.Config, usagemonitor.Options{Store: st, ReadOnly: readOnly})
	return svc, cfg, err
}

func (m *monitor) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration against the existing database, writing nothing and fetching nothing",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			svc, _, err := m.start(c.Context(), true)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "usage-monitor: OK, %d route(s)\n", len(svc.Routes()))
			return err
		},
	}
}

// serve starts the monitor, mounts its routes behind the guard, runs its collectors and serves
// until ctx ends. Shutdown order is explicit: stop accepting and drain HTTP (Serve returns), wait
// for the collectors, then the framework modules stop (the caller's deferred Stop closes the
// database last).
func (m *monitor) serve(ctx context.Context) error {
	app := m.app
	if err := hostguard.RequireLoopbackBind(listenAddr(app)); err != nil {
		return err
	}
	id := identity.From(app)
	if id == nil {
		return errNoModules
	}
	svc, cfg, err := m.start(ctx, false)
	if err != nil {
		return err
	}

	table := auth.ScopeTable(app)
	mux := http.NewServeMux()
	for _, rt := range svc.Routes() {
		table.Route(rt.Method, usagemonitor.RoutePrefix+rt.Path, rt.Scope)
		mux.Handle(rt.Method+" "+usagemonitor.RoutePrefix+rt.Path, rt.Limited())
	}
	handler, err := httphost.Handler(app, httphost.Options{Routes: mux})
	if err != nil {
		return err
	}
	// Outermost first: the Host check, then the session cookie becomes the bearer the guard
	// verifies, then the host (guard, routes).
	handler = hostguard.Handler(cfg.AllowedHosts, id.Middleware(handler))

	runCtx, stopCollectors := context.WithCancel(ctx)
	defer stopCollectors()
	collectorsDone := make(chan struct{})
	go func() { svc.Run(runCtx); close(collectorsDone) }()

	serveErr := httphost.Serve(ctx, app, handler, httphost.ServeOptions{
		GracePeriod: shutdownGrace,
		Ready: func(addr net.Addr) {
			slog.Info("usage-monitor listening", "addr", addr.String())
			if m.ready != nil {
				m.ready(addr)
			}
		},
	})
	m.note("http-drained")
	if errors.Is(serveErr, context.DeadlineExceeded) {
		// A request outlived the grace period and was cut: a normal end for a stop signal, not a failure.
		slog.Warn("usage-monitor: requests still in flight at the shutdown deadline were cut", "grace", shutdownGrace.String())
		serveErr = nil
	}
	stopCollectors()
	select {
	case <-collectorsDone:
		m.note("collectors-stopped")
	case <-time.After(collectorStop):
		slog.Warn("usage-monitor: collectors did not stop in time", "wait", collectorStop.String())
		m.note("collectors-timeout")
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
