package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/modules/database"

	"github.com/hollis-labs/station/internal/usagemonitor"
)

// migrateCmd copies the old unit's usage.db into this (empty) service database. Usage is written
// continuously, so the procedure is a write pause, not a delta pass: stop the old unit, take an
// online-backup copy of its usage.db, run this against a new data directory, check the report, start
// this unit. What the old unit would have collected during the pause is not recoverable.
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
