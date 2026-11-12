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

// Mode is how much a command may touch.
type Mode int

const (
	// ModeRead starts no module that writes: the data directory and the database must already
	// exist, and the database is opened to verify its schema, never to migrate it.
	ModeRead Mode = iota
	// ModeServe is ModeRead plus schema migrations: serving needs a database that the offline
	// bootstrap or the migration already created, so a mistyped data directory fails instead of
	// becoming a new empty one.
	ModeServe
	// ModeCreate may create the data directory and the database: the offline bootstrap and the
	// commands that copy or stage data into a new database.
	ModeCreate
)

// AfterConfig is the boot hook that resolves run-time paths. It runs once, right after the config
// module applied the SUBSTRATE_CONFIG override, so an operator's absolute path wins and a relative
// default from the manifest is placed in the unit's data directory (<APP>_DATA_DIR, else the XDG
// data home). Only ModeCreate creates that directory (0700). With a database module, every other
// mode requires the database file to exist, and this check runs before any module starts, so a
// wrong data directory is refused before the secrets or identity module can create anything in it.
func AfterConfig(getenv func(string) string, mode Mode, withDatabase bool) boot.Hook {
	return func(_ context.Context, app *module.App) error {
		dirs, err := appdirs.Resolve(app.Name, getenv)
		if err != nil {
			return err
		}
		if mode == ModeCreate {
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
		if withDatabase && mode != ModeCreate {
			path := DatabasePath(app)
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("there is no database at %s: run the offline setup (identity setup) or the migration (migrate-from) first; this command does not create one", path)
			}
			if mode == ModeRead {
				db, _ := app.Config["database"].(map[string]any)
				db["schema_mode"] = "verify"
			}
		}
		return nil
	}
}

// Options is the boot configuration for one command: the path hook and, when the database module
// starts, the guards that run just before it opens the file, in order.
func Options(getenv func(string) string, modules []string, mode Mode, guards ...func(path string) error) boot.Options {
	withDatabase := slices.Contains(modules, "database")
	o := boot.Options{AfterConfig: AfterConfig(getenv, mode, withDatabase)}
	if !withDatabase || len(guards) == 0 {
		return o
	}
	o.Before = map[string]boot.Hook{"database": func(_ context.Context, app *module.App) error {
		path := DatabasePath(app)
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

// Cmd describes one command for Plan: the modules it starts, how much it may touch, and the
// command line it accepts. A line that does not fit the description cannot run (cobra would
// reject it), so Plan starts only the read-only set for it, before anything is created.
type Cmd struct {
	Modules []string
	Mode    Mode
	// Flags take a value; Bools do not. Together they are every flag the command accepts.
	Flags, Bools []string
	// Required flags must be present with a value. ExactlyOne names flags of which exactly one
	// must be present.
	Required, ExactlyOne []string
	// Args is the exact number of positional arguments.
	Args int
}

// fits reports whether rest (the words after the command) is a line cobra would accept.
func (c Cmd) fits(rest []string) bool {
	seen := map[string]string{}
	positional := 0
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			positional += len(rest) - i - 1
			i = len(rest)
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			switch {
			case slices.Contains(c.Bools, name):
				if !hasVal {
					val = "true"
				}
			case slices.Contains(c.Flags, name):
				if !hasVal {
					if i+1 >= len(rest) {
						return false
					}
					i++
					val = rest[i]
				}
			default:
				return false
			}
			seen[name] = val
		case strings.HasPrefix(a, "-") && a != "-":
			return false
		default:
			positional++
		}
	}
	if positional != c.Args {
		return false
	}
	for _, r := range c.Required {
		if seen[r] == "" {
			return false
		}
	}
	if len(c.ExactlyOne) > 0 {
		n := 0
		for _, f := range c.ExactlyOne {
			if v, ok := seen[f]; ok && v != "" && v != "false" {
				n++
			}
		}
		if n != 1 {
			return false
		}
	}
	return true
}

// builtin are the commands every Station binary has. Serving is exactly `serve`.
func builtin(all []string) map[string]Cmd {
	return map[string]Cmd{
		"serve":        {Modules: all, Mode: ModeServe},
		"secret set":   {Modules: Pick(all, WithSecrets), Mode: ModeCreate, Args: 1},
		"secret check": {Modules: Pick(all, WithSecrets), Mode: ModeRead, Args: 1},
		"identity setup": {Modules: Pick(all, WithIdentity), Mode: ModeCreate,
			Flags: []string{"username", "display-name"}, Required: []string{"username"}},
		"identity key create": {Modules: Pick(all, WithIdentity), Mode: ModeCreate,
			Flags: []string{"name", "scope", "audience", "ttl", "out"}, Bools: []string{"stdout"},
			Required: []string{"name", "scope"}, ExactlyOne: []string{"out", "stdout"}},
	}
}

// Plan decides, from the command line, which framework modules to start and how much the command
// may touch. all is the binary's start order and cmds its own commands, keyed by the command words
// ("gateway-store show").
//
// Only a line that is exactly a known command, with every flag it needs and none it does not
// know, gets that command's modules. Everything else (no command, a bare flag, an unknown word,
// an extra argument, a missing required flag, help) starts the read-only set and creates
// nothing, so running it without <APP>_DATA_DIR cannot touch the live directory of the app name.
// Full start is exactly `serve`, with no further argument.
//
// The command is read from the first words, so the root command must have no flag before it that
// takes a value (today its one flag is -h/--help); each binary has a test that fails when such a
// flag is added, and then Plan must learn it.
func Plan(args, all []string, cmds map[string]Cmd) (modules []string, mode Mode) {
	readOnly := Pick(all, ReadOnly)
	var words int
	for words < len(args) && !strings.HasPrefix(args[words], "-") {
		words++
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "--version" {
			return readOnly, ModeRead
		}
	}
	table := builtin(all)
	for k, v := range cmds {
		table[k] = v
	}
	// The longest command whose words are the line's leading words.
	best, bestN := "", 0
	for name := range table {
		fields := strings.Fields(name)
		if len(fields) <= words && len(fields) > bestN && slices.Equal(args[:len(fields)], fields) {
			best, bestN = name, len(fields)
		}
	}
	if bestN == 0 {
		return readOnly, ModeRead
	}
	c := table[best]
	if !c.fits(args[bestN:]) {
		return readOnly, ModeRead
	}
	return Pick(all, c.Modules), c.Mode
}
