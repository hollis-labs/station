package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/core/store"
)

// legacyDDL is the old usage.db schema exactly as the old code created it.
const legacyDDL = `CREATE TABLE accounts(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, document TEXT NOT NULL);
CREATE TABLE account_audit(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, actor TEXT NOT NULL, changed_at TEXT NOT NULL, document TEXT NOT NULL);
CREATE TABLE observations(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, status TEXT NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX observations_latest ON observations(account_id,observed_at DESC,sequence DESC);
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, model TEXT NOT NULL, cost_kind TEXT NOT NULL, cost_usd REAL, cost_partial INTEGER NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX events_account_time ON events(account_id,observed_at,sequence);
PRAGMA application_id=1398035789;
PRAGMA user_version=1;`

// writeLegacy creates an old-format usage.db with one account and three observations.
func writeLegacy(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path, CreateParentDir: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts(id,revision,document) VALUES('claude',3,'{"id":"claude","provider":"anthropic","kind":"subscription","label":"Claude","plan":"","enabled":true,"revision":3}')`); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a", "b", "c"} {
		doc := `{"id":"` + id + `","observed_at":"2026-11-01T00:0` + string(rune('0'+i)) + `:00Z","status":"ok","windows":[{"id":"five_hour","unit":"percent","used":25,"limit":100,"remaining":75,"resets_at":null}]}`
		if _, err := db.ExecContext(ctx, "INSERT INTO observations(account_id,source,record_id,actor,collected_at,observed_at,status,document) VALUES('claude','claude-native',?,'collector:claude-native','2026-11-01T00:00:00Z',?,'ok',?)", id, 1793491200000+int64(i)*60000, doc); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateFromOldUsageDatabase(t *testing.T) {
	u := newUnit(t, "")
	writeConfig(t, u, bootstrapAccounts)
	old := filepath.Join(t.TempDir(), "old", "usage.db")
	writeLegacy(t, old)

	var rep struct {
		Rows      map[string]int64 `json:"rows"`
		Integrity string           `json:"integrity_check"`
	}
	if err := json.Unmarshal([]byte(u.mustRunStdout("", "migrate-from", "--old-db", old)), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Rows["accounts"] != 1 || rep.Rows["observations"] != 3 || rep.Integrity != "ok" {
		t.Fatalf("report: %+v", rep)
	}
	if out, err := u.run("", "migrate-from", "--old-db", old); err == nil || !strings.Contains(out, "already has rows") {
		t.Fatalf("a second migration: %v\n%s", err, out)
	}
	if out, err := u.run("", "migrate-from", "--old-db", filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatalf("a missing old database was accepted:\n%s", out)
	}

	// The migrated unit serves its history: the bootstrap account matches the migrated one, the
	// sequences are kept, and a new write continues them.
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	reader := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "reader", "--scope", "usage-monitor.read", "--stdout"))
	s := u.serve()
	defer s.stop(t)
	code, body := s.call(t, "GET", "/api/usage-monitor/v1/observations", reader, "", nil)
	var page struct {
		Items []struct {
			Sequence int64
			Actor    string
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); code != 200 || err != nil || len(page.Items) != 3 || page.Items[0].Sequence != 1 || page.Items[2].Sequence != 3 || page.Items[0].Actor != "collector:claude-native" {
		t.Fatalf("observations after migrating = %d %s", code, body)
	}
	if code, body = s.call(t, "GET", "/api/usage-monitor/v1/accounts", reader, "", nil); code != 200 || !strings.Contains(body, `"revision":3`) {
		t.Fatalf("accounts after migrating = %d %s", code, body)
	}
}

// A service never opens an old usage.db as its own database, whatever its path.
func TestServiceRefusesOldUsageDatabase(t *testing.T) {
	u := newUnit(t, "")
	writeLegacy(t, filepath.Join(u.dir, "usage-monitor.db"))
	for _, args := range [][]string{{"serve"}, {"validate"}} {
		if out, err := u.run("", args...); err == nil || !strings.Contains(out, "old Station usage database") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
}

// scripts/first-run sets the unit up offline: an owner, then one 0600 key file per client, in an
// explicit throwaway data directory with the real binary.
func TestFirstRunScript(t *testing.T) {
	script, err := filepath.Abs("../../scripts/first-run")
	if err != nil {
		t.Fatal(err)
	}
	u := newUnit(t, "")
	pw := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pw, []byte("a long test passphrase\nignored second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func() (string, error) {
		c := exec.Command("bash", script, "usage-monitor")
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + guardRoot, "FIRST_RUN_BIN=" + monitorBin, "SUBSTRATE_CONFIG=" + u.cfg,
			"FIRST_RUN_DATA_DIR=" + u.dir, "FIRST_RUN_PASSWORD_FILE=" + pw, "FIRST_RUN_KEYS_DIR=" + keys}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}
	if strings.Contains(out, "a long test passphrase") {
		t.Fatal("the password appears in the script output")
	}
	for _, c := range []string{"usage-operator", "usage-reader"} {
		f := filepath.Join(keys, c+".key")
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("key file %s: %v %v", f, err, st)
		}
		if body, _ := os.ReadFile(f); !strings.HasPrefix(string(body), "key_") {
			t.Fatalf("key file %s does not hold a key", f)
		}
	}
	if _, err := os.Stat(filepath.Join(u.dir, "usage-monitor.db")); err != nil {
		t.Fatalf("the database is not in the explicit data directory: %v", err)
	}
	before, _ := os.ReadDir(keys)
	if out, err := run(); err == nil {
		t.Fatalf("second run succeeded:\n%s", out)
	}
	if after, _ := os.ReadDir(keys); len(before) != len(after) {
		t.Fatal("the second run changed the key files")
	}
}

// `validate` checks the configuration against an existing database and writes nothing: after it
// ran on a database that holds only the owner, with a bootstrap account configured, the migration
// still works (it refuses a database whose accounts table has rows). On a directory with no
// database it fails and leaves the directory as it was.
func TestValidateWritesNothingAndLeavesRoomForTheMigration(t *testing.T) {
	u := newUnit(t, "")
	writeConfig(t, u, bootstrapAccounts)
	if out, err := u.run("", "validate"); err == nil || !strings.Contains(out, "no database") {
		t.Fatalf("validate with no database: %v\n%s", err, out)
	}
	if _, err := os.Stat(u.dir); !os.IsNotExist(err) {
		t.Fatalf("validate created the data directory: %v", err)
	}
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	if out, err := u.run("", "validate"); err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	old := filepath.Join(t.TempDir(), "old", "usage.db")
	writeLegacy(t, old)
	if out, err := u.run("", "migrate-from", "--old-db", old); err != nil {
		t.Fatalf("migrate-from after validate: %v\n%s", err, out)
	}
}
