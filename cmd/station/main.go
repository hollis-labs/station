// Command station is a prototype built on github.com/hollis-labs/mcp-host:
// two starter virtual MCPs (an echo server and a clock plugin, one per
// transport mode) proving the host library is feature-complete enough to
// build a real product on. See ../../station.yaml for the config and
// ../../README.md for how to run it.
//
// MCP is the only agent-facing surface — matching Tangent's own
// convention (see AGENTS.md's "Service layer" section). This CLI is an
// operator/ops surface: bare invocation serves (unchanged from before
// this file had subcommands at all), and validate/list/version are
// inspection tools an agent can run before touching anything live, not
// a second agent-facing API.
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

	mcphost "github.com/hollis-labs/mcp-host"
	"github.com/hollis-labs/mcp-host/config"
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
	case "version":
		fmt.Println("station " + version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "station: unknown subcommand %q\n\nUsage: station [serve|validate|list|version] [flags]\n", cmd)
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
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("station: failed to load config", "path", *configPath, "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := mcphost.Run(ctx, cfg, mcphost.Options{Logger: logger, HTTPAddr: *httpAddr}); err != nil {
		logger.Error("station: run failed", "err", err)
		return 1
	}
	return 0
}

// runValidate parses and validates a config file without spawning or
// dialing anything — safe for an agent to run before touching a live
// station process.
func runValidate(args []string) int {
	fs := flag.NewFlagSet("station validate", flag.ContinueOnError)
	configPath := fs.String("config", "station.yaml", "path to the logical-server config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station: %s: invalid: %v\n", *configPath, err)
		return 1
	}
	fmt.Printf("station: %s: OK, %d logical server(s) configured\n", *configPath, len(cfg.LogicalServers))
	return 0
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
