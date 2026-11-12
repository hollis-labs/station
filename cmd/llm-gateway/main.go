// Command llm-gateway is the Station LLM gateway, built on Substrate. The shell (config, logging,
// health, HTTP, auth, identity, secrets, database, CLI skeleton) comes from the manifest; the relay
// and its ledger are internal/llmgateway.
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
	"github.com/hollis-labs/substrate/modules/database"

	"github.com/hollis-labs/station/internal/appboot"
	"github.com/hollis-labs/station/internal/appcmd"
	"github.com/hollis-labs/station/internal/llmgateway"
)

func init() { database.UseMigrationFS(llmgateway.Migrations, "migrations") }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "llm-gateway:", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, args []string) error {
	modules, prepare := plan(args)
	opts := appboot.Options(getenv, modules, prepare, llmgateway.RefuseLegacyDatabase)
	started, err := boot.StartWith(ctx, substrateApp, modules, substrateConfigJSON, opts)
	if err != nil {
		return err
	}
	defer func() { _ = started.Stop(context.Background()) }()

	g := &gateway{app: started.App}
	return g.root(ctx, args).ExecuteContext(ctx)
}

// root is the command tree: the framework's skeleton plus the gateway's commands. `llm-gateway`
// with no command serves, like `llm-gateway serve`.
func (g *gateway) root(ctx context.Context, args []string) *cobra.Command {
	rootCmd := cli.Root(g.app)
	rootCmd.SetArgs(args) // the same words plan() looked at
	serve := g.serveCmd(ctx)
	rootCmd.RunE = func(c *cobra.Command, args []string) error { return serve.RunE(c, args) }
	rootCmd.Args = cobra.NoArgs
	rootCmd.AddCommand(serve, g.validateCmd(), g.storeCmd(), g.ledgerMigrateCmd(), appcmd.Secret(g.app))
	return rootCmd
}
