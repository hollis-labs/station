// Command station is the Station MCP host, built on Substrate. The shell (config, logging,
// health, HTTP, auth, identity, secrets, database, CLI skeleton) comes from the manifest; the
// engine that spawns, supervises and serves logical MCP servers is libs mcp-host, kept as it is.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/boot"

	"github.com/hollis-labs/substrate/modules/cli"

	"github.com/hollis-labs/station/internal/appcmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "station:", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, args []string) error {
	modules, prepare := plan(args)
	started, err := boot.StartWith(ctx, substrateApp, modules, substrateConfigJSON, boot.Options{AfterConfig: afterConfig(getenv, prepare)})
	if err != nil {
		return err
	}
	defer func() { _ = started.Stop(context.Background()) }()

	s := &station{app: started.App}
	rootCmd := s.root(ctx, args)
	return rootCmd.ExecuteContext(ctx)
}

// root is the command tree: the framework's skeleton plus Station's commands. `station` with no
// command serves, like `station serve`.
func (s *station) root(ctx context.Context, args []string) *cobra.Command {
	rootCmd := cli.Root(s.app)
	rootCmd.SetArgs(args) // the same words plan() looked at
	serve := s.serveCmd(ctx)
	rootCmd.RunE = func(c *cobra.Command, args []string) error { return serve.RunE(c, args) }
	rootCmd.Args = cobra.NoArgs
	rootCmd.AddCommand(serve, s.listCmd(), s.validateCmd(), appcmd.Secret(s.app))
	return rootCmd
}
