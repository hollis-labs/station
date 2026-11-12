package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hollis-labs/substrate/boot"
	"github.com/hollis-labs/substrate/module"

	"github.com/hollis-labs/station/internal/appdirs"
)

// afterConfig is the boot hook that resolves run-time paths. It runs once, right after the config
// module applied the SUBSTRATE_CONFIG override, so an operator's absolute path wins and a relative
// default from the manifest is placed in the unit's data directory (STATION_DATA_DIR, else the XDG
// data home). With prepare it also creates that directory (0700); the read-only commands pass
// false and create nothing (see plan).
func afterConfig(getenv func(string) string, prepare bool) boot.Hook {
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
		set("database", "path", appdirs.DatabaseFile, "sqlite")
		set("identity", "key_file", appdirs.SigningKeyFile)
		set("secrets", "state_dir", appdirs.SecretsDir)
		return nil
	}
}

// plan decides, from the command line, which framework modules to start and whether the data
// directory may be created. Only serving (the default), `secret set`, `identity setup` and
// `identity key create` write anything. Every
// other command, including validate, list, secret check and help, reads configuration and starts
// no module that opens a file, so running one without STATION_DATA_DIR cannot touch the live
// directory of the app name.
//
// Every argument that does not start with "-" counts as a command word. That is right only while
// the root command has no global flag that takes a value (today its one flag is -h/--help):
// `--level debug serve` would read "debug" as the command. TestRootHasNoValueFlags fails when
// such a flag is added, and then plan must learn it.
func plan(args []string) (modules []string, prepare bool) {
	var words []string
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "--version" {
			return pick(readOnly), false
		}
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	switch {
	case len(words) == 0 || words[0] == "serve":
		return substrateModules, true
	case words[0] == "secret" && len(words) > 1 && words[1] == "set":
		return pick(withSecrets), true
	case words[0] == "identity" && len(words) > 1 && (words[1] == "setup" || (words[1] == "key" && len(words) > 2 && words[2] == "create")):
		// Offline bootstrap: creates the database, the signing key and the owner or a key.
		return pick(withIdentity), true
	case words[0] == "secret" && len(words) > 1 && words[1] == "check":
		return pick(withSecrets), false
	}
	return pick(readOnly), false
}

var (
	readOnly    = []string{"config", "logging", "health", "cli"}
	withSecrets = []string{"config", "logging", "secrets", "cli"}
	// withIdentity is what `identity setup` and `identity key create` need: the database and the
	// identity module and the auth module it requires (secrets because the boot hook places its directory). No listener.
	withIdentity = []string{"config", "logging", "secrets", "auth", "database", "identity", "cli"}
)

// pick keeps the start order of substrateModules.
func pick(want []string) []string {
	var out []string
	for _, m := range substrateModules {
		if slices.Contains(want, m) {
			out = append(out, m)
		}
	}
	return out
}
