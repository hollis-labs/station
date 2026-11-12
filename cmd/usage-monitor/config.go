package main

import (
	"github.com/hollis-labs/station/internal/appboot"
)

// plan decides which modules a command starts and whether it may create the data directory
// (internal/appboot.Plan). Serving, the offline bootstrap commands and `migrate-from` write;
// `validate` opens the existing database and creates nothing. Everything else, including help,
// reads configuration and starts no module that opens a file.
func plan(args []string) (modules []string, prepare bool) {
	return appboot.Plan(args, substrateModules, []string{"serve"}, map[string]appboot.Cmd{
		"migrate-from": {Modules: appboot.WithDatabase, Create: true},
		"validate":     {Modules: appboot.WithDatabaseSecrets},
	})
}
