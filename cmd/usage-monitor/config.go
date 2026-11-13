package main

import (
	"github.com/hollis-labs/station/internal/appboot"
)

// plan decides which modules a command starts and how much it may touch
// (internal/appboot.Plan). Only a line that is exactly a known command with the flags it needs
// starts anything; every other line, including a bare command and help, reads configuration and
// starts no module that opens a file. Serving and `migrate-from` are the writers; `validate`
// needs an existing database and verifies its schema without migrating.
func plan(args []string) (modules []string, mode appboot.Mode) {
	return appboot.Plan(args, substrateModules, map[string]appboot.Cmd{
		"migrate-from": {Modules: appboot.WithDatabase, Mode: appboot.ModeCreate,
			Flags: []string{"old-db"}, Required: []string{"old-db"}},
		"init":     {Modules: appboot.WithDatabase, Mode: appboot.ModeCreate},
		"validate": {Modules: appboot.WithDatabaseSecrets},
	})
}
