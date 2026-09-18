// Command station is a prototype built on github.com/hollis-labs/mcp-host:
// two starter virtual MCPs (an echo server and a clock plugin, one per
// transport mode) proving the host library is feature-complete enough to
// build a real product on. See ../../station.yaml for the config and
// ../../README.md for how to run it.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	mcphost "github.com/hollis-labs/mcp-host"
	"github.com/hollis-labs/mcp-host/config"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("station", flag.ContinueOnError)
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
