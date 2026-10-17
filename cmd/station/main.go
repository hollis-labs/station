// Command station hosts MCP logical servers and opt-in HTTP plugin APIs.
// Use -api-only with -api-config for an HTTP service without MCP processes.
// CLI subcommands are the operator surface; Station has no management API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	mcphost "github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host"
	"github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/config"

	"github.com/hollis-labs/station/internal/secretref"
	"github.com/hollis-labs/station/platform"
)

// version is a placeholder until this binary has a real release
// process; see Nanite's ldflags-injected equivalent for the eventual
// shape once Station has one.
const version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd, rest := splitSubcommand(args)
	switch cmd {
	case "serve":
		return runServe(rest)
	case "validate":
		return runValidate(rest)
	case "list":
		return runList(rest)
	case "secret":
		return runSecret(rest)
	case "client":
		return runClient(rest)
	case "version":
		fmt.Println("station " + version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "station: unknown subcommand %q\n\nUsage: station [serve|validate|list|secret|client|version] [flags]\n", cmd)
		return 2
	}
}

// splitSubcommand separates a leading subcommand word from the rest of
// args. No subcommand (a bare flag, or nothing at all) defaults to
// serve — `station -config x.yaml` keeps working exactly as it did
// before this file had subcommands.
func splitSubcommand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "serve", args
	}
	return args[0], args[1:]
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("station serve", flag.ContinueOnError)
	configPath := fs.String("config", "station.yaml", "path to the logical-server config file")
	httpAddr := fs.String("http-addr", ":8080", "address to serve HTTP-exposed logical servers on, if any are configured")
	apiConfig := fs.String("api-config", "", "path to the HTTP API config (api.yaml); empty serves MCP logical servers only")
	apiOnly := fs.Bool("api-only", false, "serve only the HTTP API; requires -api-config and does not load MCP configuration")
	stateDir := fs.String("state-dir", defaultStateDir(), "directory for plugin data and cache roots")
	logFormat := fs.String("log-format", "json", "log format: json or text")
	logLevel := fs.String("log-level", "info", "minimum log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "station: invalid -log-level %q\n", *logLevel)
		return 2
	}
	logging, err := platform.NewLogging(os.Stderr, *logFormat, level)
	if err != nil {
		fmt.Fprintln(os.Stderr, "station: logging setup failed")
		return 1
	}
	logger := logging.Logger

	cfg, err := loadMCPConfig(*configPath, *apiConfig, *apiOnly)
	if err != nil {
		logger.Error("station: failed to load config", "path", *configPath, "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resolve any keychain://…/helper://… credential references in a
	// process-mode logical server's env before spawning it — see
	// internal/secretref. A logical server with no reference-valued env
	// entries is unaffected.
	if cfg != nil {
		if err := secretref.Resolve(ctx, cfg); err != nil {
			logger.Error("station: failed to resolve credential references", "err", err)
			return 1
		}
	}

	factories, err := initFactories(cfg, *stateDir)
	if err != nil {
		logger.Error("station: failed to prepare plugin initialization", "err", err)
		return 1
	}
	app, err := loadAPIApp(ctx, *apiConfig, logging)
	if err != nil {
		logger.Error("station: failed to load API config", "path", *apiConfig, "err", err)
		return 1
	}
	if *apiOnly {
		if app == nil || !app.Enabled() {
			if app != nil {
				_ = app.Close(ctx)
			}
			logger.Error("station: -api-only requires an API listen address")
			return 1
		}
		if err := app.Run(ctx); err != nil {
			logger.Error("station: API failed", "err", err)
			return 1
		}
		return 0
	}

	// MCP hosting and the optional HTTP API run side by side; whichever
	// stops first (error or signal) takes the other down with it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() {
		results <- mcphost.Run(runCtx, cfg, mcphost.Options{Logger: logger, HTTPAddr: *httpAddr, InprocessInitFactories: factories})
	}()
	pending := 1
	if app != nil && app.Enabled() {
		for _, line := range app.Explain() {
			logger.Info("station: middleware", "chain", line)
		}
		pending++
		go func() { results <- app.Run(runCtx) }()
	}
	code := 0
	for ; pending > 0; pending-- {
		if err := <-results; err != nil {
			logger.Error("station: run failed", "err", err)
			code = 1
		}
		cancel()
	}
	return code
}

// runValidate checks configuration without listening or spawning MCP servers.
// API validation assembles plugins: it may resolve secrets, contact metadata
// services, and connect/migrate the configured ledger.
func runValidate(args []string) int {
	fs := flag.NewFlagSet("station validate", flag.ContinueOnError)
	configPath := fs.String("config", "station.yaml", "path to the logical-server config file")
	apiConfig := fs.String("api-config", "", "also validate the HTTP API config (api.yaml), assembling plugins without listening")
	apiOnly := fs.Bool("api-only", false, "validate only the HTTP API configuration")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadMCPConfig(*configPath, *apiConfig, *apiOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station: %s: invalid: %v\n", *configPath, err)
		return 1
	}
	if cfg != nil {
		fmt.Printf("station: %s: OK, %d logical server(s) configured\n", *configPath, len(cfg.LogicalServers))
	}
	if *apiConfig != "" {
		app, err := loadAPIApp(context.Background(), *apiConfig, validateLogging())
		if err != nil {
			fmt.Fprintf(os.Stderr, "station: %s: invalid: %v\n", *apiConfig, err)
			return 1
		}
		defer app.Close(context.Background()) //nolint:errcheck // releases connections opened while assembling
		if *apiOnly && !app.Enabled() {
			fmt.Fprintln(os.Stderr, "station: -api-only requires an API listen address")
			return 1
		}
		fmt.Printf("station: %s: OK, middleware chain:\n", *apiConfig)
		for _, line := range app.Explain() {
			fmt.Println("  " + line)
		}
	}
	return 0
}

func loadMCPConfig(path, apiPath string, apiOnly bool) (*config.Config, error) {
	if apiOnly {
		if apiPath == "" {
			return nil, fmt.Errorf("-api-only requires -api-config")
		}
		return nil, nil
	}
	return config.Load(path)
}

// runList prints every configured logical server without spawning or
// dialing anything. Inprocess tool names come from the config manifest
// (see mcp-host's transport/inprocess); process-mode tools aren't known
// statically, since a process-mode server's own tools/list is only ever
// called live, once actually served.
func runList(args []string) int {
	fs := flag.NewFlagSet("station list", flag.ContinueOnError)
	configPath := fs.String("config", "station.yaml", "path to the logical-server config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station: %s: invalid: %v\n", *configPath, err)
		return 1
	}

	for _, s := range cfg.Summarize() {
		tools := "(discovered when served)"
		if s.ToolNames != nil {
			tools = strings.Join(s.ToolNames, ", ")
		}
		fmt.Printf("%-10s %-20s %-10s tools: %s\n", s.ID, s.Name, s.Transport, tools)
	}
	return 0
}
