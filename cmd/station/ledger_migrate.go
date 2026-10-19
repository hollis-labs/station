package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hollis-labs/libs/security/secrets"
	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"
)

// Schema administration is an explicit, finite command. Serve never needs to
// receive the migration credential when configured with schema_mode=verify.
func runLedgerMigrate(args []string) int {
	fs := flag.NewFlagSet("station ledger-migrate", flag.ContinueOnError)
	var cfg llmgateway.PostgresConfig
	fs.StringVar(&cfg.DSNRef, "dsn-ref", "", "legacy migration DSN secret reference")
	fs.StringVar(&cfg.Host, "host", "", "PostgreSQL hostname (TLS verify-full)")
	fs.IntVar(&cfg.Port, "port", 0, "PostgreSQL port")
	fs.StringVar(&cfg.Database, "database", "", "ledger database")
	fs.StringVar(&cfg.User, "user", "", "migration login")
	fs.StringVar(&cfg.PasswordRef, "password-ref", "", "migration password secret reference")
	fs.StringVar(&cfg.Schema, "schema", "", "ledger schema (default public for structured connections)")
	owner := fs.String("owner-role", "", "NOLOGIN role that owns ledger objects")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *owner == "" {
		fmt.Fprintln(os.Stderr, "station: ledger-migrate requires -owner-role and a migration connection")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resolver := secrets.New()
	err := llmgateway.MigratePostgresLedger(ctx, cfg, *owner, func(ctx context.Context, ref string) (string, error) {
		r, e := resolver.Resolve(ctx, ref)
		if e != nil {
			return "", e
		}
		return r.Secret.Reveal(), nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "station: ledger migration failed:", err)
		return 1
	}
	fmt.Println("station: ledger schema is current")
	return 0
}
