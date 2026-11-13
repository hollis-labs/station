package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/modules/database"

	"github.com/hollis-labs/station/internal/usagemonitor"
)

// migrateCmd copies the old unit's usage.db into this (empty) service database. Usage is written
// continuously, so the procedure is a write pause, not a delta pass: stop the old unit, take an
// online-backup copy of its usage.db, run this against a new data directory, check the report, start
// this unit. What the old unit would have collected during the pause is not recoverable.
// initCmd marks an empty database (no rows in the history tables) as a unit that starts with no history: the alternative to migrate-from for
// a fresh install. `serve` refuses a database that has neither.
func (m *monitor) initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Mark this database as starting with no history (the alternative to migrate-from); serve needs one or the other",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st := database.From(m.app)
			if st == nil {
				return errNoModules
			}
			how, err := usagemonitor.Init(c.Context(), st)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "usage-monitor: database began as %q\n", how)
			return err
		},
	}
}

func (m *monitor) migrateCmd() *cobra.Command {
	var oldDB string
	c := &cobra.Command{
		Use:   "migrate-from",
		Short: "Copy an old usage.db (a copy taken with the old unit stopped, never the live file) into this empty database",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st := database.From(m.app)
			if st == nil {
				return errNoModules
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Minute)
			defer cancel()
			rep, err := usagemonitor.MigrateLegacy(ctx, st, oldDB)
			if err != nil {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(rep)
		},
	}
	c.Flags().StringVar(&oldDB, "old-db", "", "path to the copy of the old usage.db")
	_ = c.MarkFlagRequired("old-db")
	return c
}
