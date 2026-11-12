// Command usage-monitor is the Station usage monitor, built on Substrate. The shell (config,
// logging, health, HTTP, auth, identity, secrets, database, CLI skeleton) comes from the manifest;
// accounts, collectors and history are internal/usagemonitor.
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
	"github.com/hollis-labs/station/internal/usagemonitor"
)

func init() { database.UseMigrationFS(usagemonitor.Migrations, "migrations") }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "usage-monitor:", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, args []string) error {
	modules, mode := plan(args)
	opts := appboot.Options(getenv, modules, mode, usagemonitor.RefuseLegacyDatabase)
	started, err := boot.StartWith(ctx, substrateApp, modules, substrateConfigJSON, opts)
	if err != nil {
		return err
	}
	defer func() { _ = started.Stop(context.Background()) }()

	m := &monitor{app: started.App}
	return m.root(ctx, args).ExecuteContext(ctx)
}

// root is the command tree: the framework's skeleton plus the monitor's commands. Only `serve` serves; a bare command prints help.
func (m *monitor) root(ctx context.Context, args []string) *cobra.Command {
	rootCmd := cli.Root(m.app)
	rootCmd.SetArgs(args) // the same words plan() looked at
	rootCmd.AddCommand(m.serveCmd(ctx), m.validateCmd(), m.migrateCmd(), appcmd.Secret(m.app))
	return rootCmd
}
