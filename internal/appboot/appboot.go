// Package appboot is what the Station binaries share around the Substrate boot: the hook that
// places run-time paths in the unit's data directory, and the plan that decides, from the command
// line, which modules to start and whether the data directory may be created.
package appboot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hollis-labs/substrate/boot"
	"github.com/hollis-labs/substrate/module"

	"github.com/hollis-labs/station/internal/appdirs"
)

// AfterConfig is the boot hook that resolves run-time paths. It runs once, right after the config
// module applied the SUBSTRATE_CONFIG override, so an operator's absolute path wins and a relative
// default from the manifest is placed in the unit's data directory (<APP>_DATA_DIR, else the XDG
// data home). With prepare it also creates that directory (0700); the read-only commands pass
// false and create nothing (see Plan).
func AfterConfig(getenv func(string) string, prepare bool) boot.Hook {
	return func(_ context.Context, app *module.App) error {
		dirs, err := appdirs.Resolve(app.Name, getenv)
		if err != nil {
			return err
		}
		if prepare {
			if err = dirs.Prepare(); err != nil {
				return fmt.Errorf("prepare the data directory %s: %w", dirs.Data, err)
			}
		}
		inData := func(p, fallback string) string {
			if p == "" {
				p = fallback
			}
			if filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(dirs.Data, p)
		}
		set := func(module, key, fallback string, nested ...string) {
			m, _ := app.Config[module].(map[string]any)
			if m == nil {
				m = map[string]any{}
				app.Config[module] = m
			}
			for _, n := range nested {
				child, _ := m[n].(map[string]any)
				if child == nil {
					child = map[string]any{}
					m[n] = child
				}
				m = child
			}
			cur, _ := m[key].(string)
			m[key] = inData(cur, fallback)
		}
		set("database", "path", appdirs.DatabaseFile(app.Name), "sqlite")
		set("identity", "key_file", appdirs.SigningKeyFile)
		set("secrets", "state_dir", appdirs.SecretsDir)
		return nil
	}
}

// Options is the boot configuration for one command: the path hook and, when the database module
// starts, the guards that run just before it opens the file, in order. A command that opens the
// database without being allowed to create it (prepare false) also gets a guard that refuses a
// missing file (the database module would otherwise create an empty one).
func Options(getenv func(string) string, modules []string, prepare bool, guards ...func(path string) error) boot.Options {
	o := boot.Options{AfterConfig: AfterConfig(getenv, prepare)}
	if !slices.Contains(modules, "database") {
		return o
	}
	o.Before = map[string]boot.Hook{"database": func(_ context.Context, app *module.App) error {
		path := DatabasePath(app)
		if !prepare {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("there is no database at %s: this command reads existing data and does not create it", path)
			}
		}
		for _, g := range guards {
			if err := g(path); err != nil {
				return err
			}
		}
		return nil
	}}
	return o
}

// DatabasePath is the SQLite file the database module will open, from the effective config.
func DatabasePath(app *module.App) string {
	db, _ := app.Config["database"].(map[string]any)
	sq, _ := db["sqlite"].(map[string]any)
	p, _ := sq["path"].(string)
	return p
}

// Module sets. ReadOnly starts nothing that opens a file; WithSecrets adds the secrets module;
// WithIdentity is what `identity setup` and `identity key create` need: the database, the identity
// module, the auth module it requires and secrets (the boot hook places its directory). No listener.
// WithDatabase and WithDatabaseSecrets are for commands that work on the service database without
// identity (which would create a signing key at start).
var (
	ReadOnly            = []string{"config", "logging", "health", "cli"}
	WithSecrets         = []string{"config", "logging", "secrets", "cli"}
	WithIdentity        = []string{"config", "logging", "secrets", "auth", "database", "identity", "cli"}
	WithDatabase        = []string{"config", "logging", "database", "cli"}
	WithDatabaseSecrets = []string{"config", "logging", "secrets", "database", "cli"}
)

// Pick keeps the start order of all.
func Pick(all, want []string) []string {
	var out []string
	for _, m := range all {
		if slices.Contains(want, m) {
			out = append(out, m)
		}
	}
	return out
}

// Cmd is a command that starts only some modules: which, and whether it may create the data
// directory and the database.
type Cmd struct {
	Modules []string
	Create  bool
}

// Plan decides, from the command line, which framework modules to start and whether the data
// directory may be created. all is the binary's start order. Only serving (the commands in full,
// or no command), `secret set`, `identity setup`, `identity key create` and the commands in cmds
// that say so write anything. Every other command, including validate, list, secret check and
// help, reads configuration and starts no module that opens a file, so running one without
// <APP>_DATA_DIR cannot touch the live directory of the app name.
//
// cmds is keyed by the command words ("gateway-store show").
//
// Every argument that does not start with "-" counts as a command word. That is right only while
// the root command has no global flag that takes a value (today its one flag is -h/--help):
// `--level debug serve` would read "debug" as the command. Each binary has a test that fails when
// such a flag is added, and then Plan must learn it.
func Plan(args, all, full []string, cmds map[string]Cmd) (modules []string, prepare bool) {
	var words []string
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "--version" {
			return Pick(all, ReadOnly), false
		}
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	at := func(n int) string {
		if n < len(words) {
			return words[n]
		}
		return ""
	}
	switch {
	case len(words) == 0 || slices.Contains(full, words[0]):
		return all, true
	case words[0] == "secret" && at(1) == "set":
		return Pick(all, WithSecrets), true
	case words[0] == "identity" && (at(1) == "setup" || (at(1) == "key" && at(2) == "create")):
		// Offline bootstrap: creates the database, the signing key and the owner or a key.
		return Pick(all, WithIdentity), true
	case words[0] == "secret" && at(1) == "check":
		return Pick(all, WithSecrets), false
	}
	for name, c := range cmds {
		fields := strings.Fields(name)
		if len(words) >= len(fields) && slices.Equal(words[:len(fields)], fields) {
			return Pick(all, c.Modules), c.Create
		}
	}
	return Pick(all, ReadOnly), false
}
