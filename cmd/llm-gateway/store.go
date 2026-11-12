package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/modules/database"
	"github.com/hollis-labs/substrate/modules/secrets"

	"github.com/hollis-labs/station/internal/llmgateway"
)

// storeCmd administers the gateway's configuration revisions in the service database, offline
// (the same operations the management API has, for the first revision and for recovery). Every
// action prints one JSON document on stdout. Mutations need the expected current revision and an
// actor name, recorded in the activation audit.
func (g *gateway) storeCmd() *cobra.Command {
	root := &cobra.Command{Use: "gateway-store", Short: "Stage, inspect and activate gateway configuration revisions offline"}
	var (
		input, output, oldDB, actor string
		expected, revision, before  int64
	)
	needMutation := func(c *cobra.Command) error {
		if !c.Flags().Changed("expected") || actor == "" {
			return errors.New("a mutation requires --expected and --actor")
		}
		return nil
	}
	emit := func(c *cobra.Command, v any) error { return json.NewEncoder(c.OutOrStdout()).Encode(v) }
	ms := func() (*llmgateway.ManagementStore, error) {
		st := database.From(g.app)
		if st == nil {
			return nil, errNoModules
		}
		return llmgateway.NewManagementStore(st), nil
	}

	stage := &cobra.Command{
		Use:   "stage",
		Short: "Stage a versioned JSON document as a new revision (does not activate it)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := needMutation(c); err != nil {
				return err
			}
			f, err := os.Open(input)
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
			_ = f.Close()
			if err != nil {
				return err
			}
			doc, err := llmgateway.DecodeManagementDocument(raw)
			if err != nil {
				return err
			}
			s, err := ms()
			if err != nil {
				return err
			}
			r, err := s.Save(c.Context(), doc, expected, actor)
			if err != nil {
				return err
			}
			return emit(c, r)
		},
	}
	stage.Flags().StringVar(&input, "input", "", "versioned JSON document to stage")
	stage.Flags().Int64Var(&expected, "expected", 0, "the current latest revision; 0 means none")
	stage.Flags().StringVar(&actor, "actor", "", "operator identity recorded in the audit")
	_ = stage.MarkFlagRequired("input")

	activate := &cobra.Command{
		Use:   "activate",
		Short: "Select a stored revision for the next start",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := needMutation(c); err != nil {
				return err
			}
			if revision <= 0 {
				return errors.New("activate requires a positive --revision")
			}
			s, err := ms()
			if err != nil {
				return err
			}
			if err := s.Activate(c.Context(), revision, expected, actor); err != nil {
				return err
			}
			return emit(c, map[string]any{"active_revision": revision, "apply": "validate, then restart the service to use this revision"})
		},
	}
	activate.Flags().Int64Var(&revision, "revision", 0, "revision to activate")
	activate.Flags().Int64Var(&expected, "expected", 0, "the current active revision; 0 means none")
	activate.Flags().StringVar(&actor, "actor", "", "operator identity recorded in the audit")

	show := &cobra.Command{
		Use:   "show",
		Short: "Write a revision's document to a new private file (default: the active revision)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if output == "" {
				return errors.New("show requires --output for a new private document file")
			}
			s, err := ms()
			if err != nil {
				return err
			}
			var r llmgateway.ManagementRevision
			if revision == 0 {
				r, err = s.Active(c.Context())
			} else {
				r, err = s.Revision(c.Context(), revision)
			}
			if err != nil {
				return err
			}
			// Literal provider header values are credentials and are not written; staging the
			// edited file restores each placeholder from the revision named by --expected.
			shown := r.Document.Redacted()
			if err := writeDocument(output, &shown); err != nil {
				return err
			}
			return emit(c, map[string]any{"revision": r.ID, "output": output})
		},
	}
	show.Flags().Int64Var(&revision, "revision", 0, "revision to show; default the active one")
	show.Flags().StringVar(&output, "output", "", "new private JSON output file")

	status := &cobra.Command{
		Use:   "status",
		Short: "Latest and active revision",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := ms()
			if err != nil {
				return err
			}
			r, err := s.Status(c.Context())
			if err != nil {
				return err
			}
			return emit(c, r)
		},
	}

	history := &cobra.Command{
		Use:   "history",
		Short: "Revision metadata, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := ms()
			if err != nil {
				return err
			}
			r, err := s.History(c.Context(), before, 100)
			if err != nil {
				return err
			}
			return emit(c, r)
		},
	}
	history.Flags().Int64Var(&before, "before", 0, "cursor: revision ids below this value")

	migrate := &cobra.Command{
		Use:   "migrate-from",
		Short: "Copy an old gateway.db (an online-backup copy, never the live file) into this empty database",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st := database.From(g.app)
			if st == nil {
				return errNoModules
			}
			ctx, cancel := context.WithTimeout(c.Context(), 10*time.Minute)
			defer cancel()
			rep, err := llmgateway.MigrateLegacy(ctx, st, oldDB)
			if err != nil {
				return err
			}
			return emit(c, rep)
		},
	}
	migrate.Flags().StringVar(&oldDB, "old-db", "", "path to the copy of the old gateway.db")
	_ = migrate.MarkFlagRequired("old-db")

	root.AddCommand(stage, activate, show, status, history, migrate)
	return root
}

func writeDocument(path string, d *llmgateway.ManagementDocument) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	err = enc.Encode(d)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// ledgerMigrateCmd is the explicit, finite schema administration of the Postgres ledger. Serve
// never needs the migration credential when the ledger is configured with schema_mode verify.
func (g *gateway) ledgerMigrateCmd() *cobra.Command {
	var (
		cfg   llmgateway.PostgresConfig
		owner string
	)
	c := &cobra.Command{
		Use:   "ledger-migrate",
		Short: "Apply the Postgres ledger schema as the migration login (SET ROLE to the owner)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			res := secrets.Resolver(g.app)
			if res == nil {
				return errors.New("the secrets module must be started")
			}
			if owner == "" {
				return errors.New("ledger-migrate requires --owner-role and a migration connection")
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()
			err := llmgateway.MigratePostgresLedger(ctx, cfg, owner, func(ctx context.Context, ref string) (string, error) {
				r, e := res.Resolve(ctx, ref)
				if e != nil {
					return "", e
				}
				return r.Secret.Reveal(), nil
			})
			if err != nil {
				return fmt.Errorf("ledger migration failed: %w", err)
			}
			_, err = fmt.Fprintln(c.OutOrStdout(), "llm-gateway: ledger schema is current")
			return err
		},
	}
	f := c.Flags()
	f.StringVar(&cfg.DSNRef, "dsn-ref", "", "legacy migration DSN secret reference")
	f.StringVar(&cfg.Host, "host", "", "PostgreSQL hostname (TLS verify-full)")
	f.IntVar(&cfg.Port, "port", 0, "PostgreSQL port")
	f.StringVar(&cfg.Database, "database", "", "ledger database")
	f.StringVar(&cfg.User, "user", "", "migration login")
	f.StringVar(&cfg.PasswordRef, "password-ref", "", "migration password secret reference")
	f.StringVar(&cfg.Schema, "schema", "", "ledger schema (default public for structured connections)")
	f.StringVar(&owner, "owner-role", "", "NOLOGIN role that owns ledger objects")
	return c
}
