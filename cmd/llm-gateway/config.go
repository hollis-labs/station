package main

import (
	"github.com/hollis-labs/station/internal/appboot"
)

// plan decides which modules a command starts and whether it may create the data directory
// (internal/appboot.Plan). Serving, the offline bootstrap commands and `gateway-store stage`,
// `activate` and `migrate-from` write; `validate` and the `gateway-store` reads open the existing
// database and create nothing; `ledger-migrate` needs only secrets. Everything else, including
// help, reads configuration and starts no module that opens a file.
func plan(args []string) (modules []string, prepare bool) {
	return appboot.Plan(args, substrateModules, []string{"serve"}, map[string]appboot.Cmd{
		"gateway-store stage":        {Modules: appboot.WithDatabase, Create: true},
		"gateway-store activate":     {Modules: appboot.WithDatabase, Create: true},
		"gateway-store migrate-from": {Modules: appboot.WithDatabase, Create: true},
		"gateway-store show":         {Modules: appboot.WithDatabase},
		"gateway-store status":       {Modules: appboot.WithDatabase},
		"gateway-store history":      {Modules: appboot.WithDatabase},
		"validate":                   {Modules: appboot.WithDatabaseSecrets},
		"ledger-migrate":             {Modules: appboot.WithSecrets},
	})
}
