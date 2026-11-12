package llmgateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// legacyDDL is the old gateway.db schema exactly as the old code created it (user_version 1,
// application id "STGW").
const legacyDDL = `
CREATE TABLE gateway_revisions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 created_at TEXT NOT NULL, actor TEXT NOT NULL CHECK(length(actor)>0),
 document_version INTEGER NOT NULL CHECK(document_version=1),
 document TEXT NOT NULL CHECK(json_valid(document)), sha256 TEXT NOT NULL
);
CREATE TABLE gateway_state (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 latest_revision INTEGER REFERENCES gateway_revisions(id),
 active_revision INTEGER REFERENCES gateway_revisions(id)
);
INSERT INTO gateway_state(singleton) VALUES(1);
CREATE TABLE gateway_activations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, activated_at TEXT NOT NULL, actor TEXT NOT NULL,
 previous_revision INTEGER REFERENCES gateway_revisions(id),
 revision INTEGER NOT NULL REFERENCES gateway_revisions(id)
);
CREATE TABLE gateway_usage (
 id TEXT PRIMARY KEY NOT NULL, recorded_at INTEGER NOT NULL,
 principal TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
 outcome TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
 cost_usd REAL, entry TEXT NOT NULL CHECK(json_valid(entry))
);
CREATE INDEX gateway_usage_time ON gateway_usage(recorded_at,id);
CREATE INDEX gateway_usage_principal ON gateway_usage(principal,recorded_at);
CREATE INDEX gateway_usage_target ON gateway_usage(provider,model,recorded_at);
PRAGMA application_id=1398032215;
PRAGMA user_version=1;`

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type legacyFixture struct {
	path       string
	docs       [2]string
	usageEntry Entry
}

// newLegacy writes an old-format database: two revisions (the second names legacy clients), one
// activation and one usage row.
func newLegacy(t *testing.T) legacyFixture {
	t.Helper()
	ctx := t.Context()
	f := legacyFixture{path: filepath.Join(t.TempDir(), "gateway.db")}
	db, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: f.path}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatal(err)
	}
	doc := managementFixture()
	raw1, _ := json.Marshal(doc)
	doc2 := managementFixture()
	doc2.Gateway.Budgets = nil
	doc2.Clients = json.RawMessage(`[{"id":"app","sha256":"` + strings.Repeat("a", 64) + `","scopes":["llm-gateway.chat"]}]`)
	raw2, _ := json.Marshal(doc2)
	f.docs = [2]string{string(raw1), string(raw2)}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, raw := range f.docs {
		if _, err := db.ExecContext(ctx, "INSERT INTO gateway_revisions(created_at,actor,document_version,document,sha256) VALUES(?,?,1,?,?)", now, "operator", raw, sumOf(raw)); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := db.ExecContext(ctx, "UPDATE gateway_state SET latest_revision=2, active_revision=2 WHERE singleton=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO gateway_activations(activated_at,actor,previous_revision,revision) VALUES(?,?,NULL,1),(?,?,1,2)", now, "operator", now, "operator"); err != nil {
		t.Fatal(err)
	}
	f.usageEntry = sampleEntry("legacy-1", "app", "local", OutcomeOK, time.Now().UTC().Truncate(time.Millisecond), f64(0.25))
	if err := addSQLite(ctx, db, f.usageEntry); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMigrateLegacyCopiesEverything(t *testing.T) {
	ctx := t.Context()
	f := newLegacy(t)
	dst := newGatewayDB(t)
	rep, err := MigrateLegacy(ctx, dst, f.path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Revisions != 2 || rep.Activations != 2 || rep.Usage != 1 || rep.Active == nil || *rep.Active != 2 || rep.Latest == nil || *rep.Latest != 2 {
		t.Fatalf("report: %+v", rep)
	}
	ms := NewManagementStore(dst)
	active, err := ms.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != 2 || active.SHA256 != sumOf(f.docs[1]) || active.Actor != "operator" {
		t.Fatalf("active revision: %+v", active)
	}
	// The legacy clients are kept in the stored document and never reach the gateway config.
	if len(active.Document.Clients) == 0 {
		t.Fatal("legacy clients were not preserved in the stored revision")
	}
	first, err := ms.Revision(ctx, 1)
	if err != nil || first.SHA256 != sumOf(f.docs[0]) {
		t.Fatalf("first revision: %+v %v", first, err)
	}
	acts, err := ms.Activations(ctx, 0, 10)
	if err != nil || len(acts) != 2 {
		t.Fatalf("activations: %+v %v", acts, err)
	}
	got, err := (&sqliteStore{st: dst}).Recent(ctx, Query{Principal: "app"})
	if err != nil || len(got) != 1 || got[0].ID != "legacy-1" {
		t.Fatalf("usage: %+v %v", got, err)
	}
	if got[0].CostUSD == nil || *got[0].CostUSD != 0.25 {
		t.Fatalf("usage cost: %+v", got[0].CostUSD)
	}
	// New staging continues the id sequence.
	r3, err := ms.Save(ctx, managementFixture(), 2, "operator")
	if err != nil || r3.ID != 3 {
		t.Fatalf("next revision: %+v %v", r3, err)
	}
}

func TestMigrateLegacyRefusals(t *testing.T) {
	ctx := t.Context()
	f := newLegacy(t)
	t.Run("missing file", func(t *testing.T) {
		if _, err := MigrateLegacy(ctx, newGatewayDB(t), filepath.Join(t.TempDir(), "nope.db")); err == nil {
			t.Fatal("missing file accepted")
		}
	})
	t.Run("not a legacy database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "plain.db") // application_id 0
		db, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path}})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if _, err := MigrateLegacy(ctx, newGatewayDB(t), path); err == nil || !strings.Contains(err.Error(), "not an old gateway database") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("destination not empty", func(t *testing.T) {
		dst := newGatewayDB(t)
		if _, err := MigrateLegacy(ctx, dst, f.path); err != nil {
			t.Fatal(err)
		}
		if _, err := MigrateLegacy(ctx, dst, f.path); err == nil || !strings.Contains(err.Error(), "already has rows") {
			t.Fatalf("second migration: %v", err)
		}
	})
	t.Run("corrupt checksum leaves the destination empty", func(t *testing.T) {
		bad := newLegacy(t)
		db, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: bad.path}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE gateway_revisions SET sha256=? WHERE id=2", strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		dst := newGatewayDB(t)
		if _, err := MigrateLegacy(ctx, dst, bad.path); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatalf("err = %v", err)
		}
		var n int
		if err := dst.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM gateway_revisions").Scan(&n); err != nil || n != 0 {
			t.Fatalf("destination has %d revisions after a failed migration (%v)", n, err)
		}
	})
}

// newGatewayDB is an empty service database with the gateway schema.
func newGatewayDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := openTestDB(context.Background(), filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRefuseLegacyDatabase(t *testing.T) {
	if err := RefuseLegacyDatabase(filepath.Join(t.TempDir(), "missing.db")); err != nil {
		t.Fatalf("a missing file: %v", err)
	}
	if err := RefuseLegacyDatabase(newLegacy(t).path); err == nil || !strings.Contains(err.Error(), "old Station gateway database") {
		t.Fatalf("an old gateway database: %v", err)
	}
	st := newGatewayDB(t)
	var path string
	if err := st.Reader().QueryRowContext(t.Context(), "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := RefuseLegacyDatabase(path); err != nil {
		t.Fatalf("a service database: %v", err)
	}
}

// tree lists dir with each file's size, mode and content digest.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		sum := ""
		if !d.IsDir() {
			raw, _ := os.ReadFile(p)
			h := sha256.Sum256(raw)
			sum = hex.EncodeToString(h[:])
		}
		fmt.Fprintf(&b, "%s %d %v %s\n", p, info.Size(), info.Mode(), sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The old file is never written: after the start guard and a migration, its bytes, mode and
// directory listing (no sidecar appeared) are exactly what they were, and the report proves it.
func TestOldFileIsLeftExactlyAsItWas(t *testing.T) {
	path := newLegacy(t).path
	rollbackJournal(t, path)
	dir := filepath.Dir(path)
	before := tree(t, dir)
	if err := RefuseLegacyDatabase(path); err == nil {
		t.Fatal("the guard did not refuse an old database")
	}
	if after := tree(t, dir); after != before {
		t.Fatalf("the guard changed the old file's directory:\n%s\nvs\n%s", before, after)
	}
	rep, err := MigrateLegacy(t.Context(), newGatewayDB(t), path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	sum := sha256.Sum256(raw)
	if !rep.SourceUnchanged || rep.SourceSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("report %+v does not match the file's sha256 %x", rep, sum)
	}
	if after := tree(t, dir); after != before {
		t.Fatalf("the migration changed the old file's directory:\n%s\nvs\n%s", before, after)
	}
}

// rollbackJournal puts the file in a plain rollback-journal database, as a copy that was never
// opened by the service would be: opening it read-write with the service's settings switches it to WAL,
// which rewrites its header. It fails the test if the file is not in that state afterwards.
func rollbackJournal(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err = db.QueryRowContext(t.Context(), "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	hdr, _ := os.ReadFile(path)
	if hdr[18] != 1 || hdr[19] != 1 {
		t.Fatalf("the fixture is not a rollback-journal file (header bytes %d %d)", hdr[18], hdr[19])
	}
}
