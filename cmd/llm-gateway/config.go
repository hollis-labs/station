package main

import (
	"github.com/hollis-labs/station/internal/appboot"
)

// plan decides which modules a command starts and how much it may touch
// (internal/appboot.Plan). Only a line that is exactly a known command with the flags it needs
// starts anything; every other line, including a bare command and help, reads configuration and
// starts no module that opens a file. Serving and `gateway-store stage|activate|migrate-from`
// are the writers; `validate` and the `gateway-store` reads need an existing database and verify
// its schema without migrating; `ledger-migrate` needs only secrets.
func plan(args []string) (modules []string, mode appboot.Mode) {
	mutation := []string{"expected", "actor"}
	return appboot.Plan(args, substrateModules, map[string]appboot.Cmd{
		"gateway-store stage": {Modules: appboot.WithDatabase, Mode: appboot.ModeCreate,
			Flags: []string{"input", "expected", "actor"}, Required: append([]string{"input"}, mutation...)},
		"gateway-store activate": {Modules: appboot.WithDatabase, Mode: appboot.ModeCreate,
			Flags: []string{"revision", "expected", "actor"}, Required: append([]string{"revision"}, mutation...)},
		"gateway-store migrate-from": {Modules: appboot.WithDatabase, Mode: appboot.ModeCreate,
			Flags: []string{"old-db"}, Required: []string{"old-db"}},
		"gateway-store show":    {Modules: appboot.WithDatabase, Flags: []string{"revision", "output"}},
		"gateway-store status":  {Modules: appboot.WithDatabase},
		"gateway-store history": {Modules: appboot.WithDatabase, Flags: []string{"before"}},
		"validate":              {Modules: appboot.WithDatabaseSecrets},
		"ledger-migrate": {Modules: appboot.WithSecrets,
			Flags:    []string{"dsn-ref", "host", "port", "database", "user", "password-ref", "schema", "owner-role"},
			Required: []string{"owner-role"}},
	})
}
