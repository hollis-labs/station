package appboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/module"
)

func afterConfig(t *testing.T, dir string, mode Mode, withDB bool) (*module.App, error) {
	t.Helper()
	app := &module.App{Name: "demo", Config: map[string]any{}}
	getenv := func(k string) string {
		if k == "DEMO_DATA_DIR" {
			return dir
		}
		return ""
	}
	return app, AfterConfig(getenv, mode, withDB)(t.Context(), app)
}

// Reads verify the schema and never migrate it; serving migrates; nothing but ModeCreate and
// ModeDir may start without a database, and the refusal creates nothing.
func TestModesSetSchemaModeAndRequireADatabase(t *testing.T) {
	for _, tc := range []struct {
		mode       Mode
		wantVerify bool
		needsDB    bool
	}{{ModeRead, true, true}, {ModeServe, false, true}, {ModeDir, false, false}, {ModeCreate, false, false}} {
		dir := filepath.Join(t.TempDir(), "data")
		app, err := afterConfig(t, dir, tc.mode, true)
		if tc.needsDB {
			if err == nil || !strings.Contains(err.Error(), "there is no database") {
				t.Errorf("mode %d with no database: %v", tc.mode, err)
			}
			if _, e := os.Stat(dir); e == nil {
				t.Errorf("mode %d created the data directory", tc.mode)
			}
			if err = os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "demo.db"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if app, err = afterConfig(t, dir, tc.mode, true); err != nil {
				t.Fatalf("mode %d with a database: %v", tc.mode, err)
			}
		} else if err != nil {
			t.Fatalf("mode %d: %v", tc.mode, err)
		}
		db, _ := app.Config["database"].(map[string]any)
		if got := db["schema_mode"] == "verify"; got != tc.wantVerify {
			t.Errorf("mode %d: schema_mode verify = %v, want %v", tc.mode, got, tc.wantVerify)
		}
	}
}

// ModeDir creates the directory and no database; ModeCreate creates both.
func TestOnlyCreateCreatesTheDatabase(t *testing.T) {
	for mode, wantDB := range map[Mode]bool{ModeDir: false, ModeCreate: true} {
		app, err := afterConfig(t, filepath.Join(t.TempDir(), "data"), mode, false)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(DatabasePath(app))
		if st, e := os.Stat(dir); e != nil || !st.IsDir() {
			t.Errorf("mode %d: no data directory: %v", mode, e)
		}
		_, e := os.Stat(DatabasePath(app))
		if (e == nil) != wantDB {
			t.Errorf("mode %d: database exists = %v, want %v", mode, e == nil, wantDB)
		}
	}
}
