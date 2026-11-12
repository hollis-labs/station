// Package appdirs decides where a Station service keeps its files and makes them private.
// Durable data (the identity database, the signing key and the secrets store) lives under
// the XDG data home, resolved by the shared apppaths layer, or under <APP>_DATA_DIR (STATION_DATA_DIR,
// LLM_GATEWAY_DATA_DIR, USAGE_MONITOR_DATA_DIR) when the operator sets it. Two units of one
// binary (avatar and design) use different values.
package appdirs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	paths "github.com/hollis-labs/libs/core/util/apppaths"
)

const (
	// SigningKeyFile and SecretsDir are the names inside the data directory.
	SigningKeyFile = "identity-signing.key"
	SecretsDir     = "secrets"
)

// DataDirEnv names the variable that overrides the whole data directory of app: the app name
// upper-cased with "-" as "_", plus _DATA_DIR. It is the one setting read from the environment:
// the directory is needed before the config module runs.
func DataDirEnv(app string) string {
	return strings.ToUpper(strings.ReplaceAll(app, "-", "_")) + "_DATA_DIR"
}

// DatabaseFile is the SQLite file name inside the data directory.
func DatabaseFile(app string) string { return app + ".db" }

// Dirs are the resolved directories.
type Dirs struct{ App, Data string }

// Resolve returns the data directory for app. getenv is os.Getenv outside tests. Nothing is created.
func Resolve(app string, getenv func(string) string) (Dirs, error) {
	if v := getenv(DataDirEnv(app)); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return Dirs{}, fmt.Errorf("resolve %s: %w", DataDirEnv(app), err)
		}
		return Dirs{App: app, Data: abs}, nil
	}
	layout, err := paths.Resolve(app, paths.WithoutMaterialize())
	if err != nil {
		return Dirs{}, fmt.Errorf("resolve application directories: %w", err)
	}
	return Dirs{App: app, Data: layout.DataDir()}, nil
}

// Database is the SQLite file the identity module uses.
func (d Dirs) Database() string { return filepath.Join(d.Data, DatabaseFile(d.App)) }

// SigningKey is the identity module's Ed25519 key file.
func (d Dirs) SigningKey() string { return filepath.Join(d.Data, SigningKeyFile) }

// Secrets is the managed file store for credentials.
func (d Dirs) Secrets() string { return filepath.Join(d.Data, SecretsDir) }

// Prepare creates the data directory (0700) and the database file (0600) when missing and
// tightens both when they exist. The signing key and the secrets store create themselves private.
func (d Dirs) Prepare() error {
	if err := os.MkdirAll(d.Data, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(d.Data, 0o700); err != nil { //nolint:gosec // 0700 on a directory is the point: owner-only
		return err
	}
	f, err := os.OpenFile(d.Database(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
