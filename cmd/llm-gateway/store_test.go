package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/core/store"
)

// legacyDDL is the old gateway.db schema exactly as the old code created it.
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

// writeLegacy creates an old-format gateway.db at path holding one active revision.
func writeLegacy(t *testing.T, path, document string) {
	t.Helper()
	db, err := store.Open(context.Background(), store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path, CreateParentDir: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatal(err)
	}
	sum := sha256Hex(document)
	if _, err := db.ExecContext(ctx, "INSERT INTO gateway_revisions(created_at,actor,document_version,document,sha256) VALUES('2026-01-01T00:00:00Z','operator',1,?,?)", document, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE gateway_state SET latest_revision=1, active_revision=1 WHERE singleton=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO gateway_activations(activated_at,actor,previous_revision,revision) VALUES('2026-01-01T00:00:00Z','operator',NULL,1)"); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayStoreCommands(t *testing.T) {
	u := newUnit(t, "")
	doc := writeFile(t, "doc.json", gatewayDoc(t))
	// Mutations need the precondition and an actor.
	if out, err := u.run("", "gateway-store", "stage", "--input", doc); err == nil || !strings.Contains(out, "--expected and --actor") {
		t.Fatalf("stage without --expected/--actor: %v\n%s", err, out)
	}
	u.mustRun("", "gateway-store", "stage", "--input", doc, "--expected", "0", "--actor", "test")
	// A stale precondition is refused, and the failed command changes nothing.
	if out, err := u.run("", "gateway-store", "stage", "--input", doc, "--expected", "0", "--actor", "test"); err == nil {
		t.Fatalf("a stale --expected was accepted\n%s", out)
	}
	var status struct {
		Latest int64 `json:"latest_revision"`
		Active int64 `json:"active_revision"`
	}
	if err := json.Unmarshal([]byte(u.mustRunStdout("", "gateway-store", "status")), &status); err != nil || status.Latest != 1 || status.Active != 0 {
		t.Fatalf("status after staging: %+v %v", status, err)
	}
	u.mustRun("", "gateway-store", "activate", "--revision", "1", "--expected", "0", "--actor", "test")
	out := filepath.Join(t.TempDir(), "shown.json")
	u.mustRun("", "gateway-store", "show", "--output", out)
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), `"models"`) {
		t.Fatalf("show wrote %q (%v)", b, err)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Fatalf("shown document mode %v, want 0600", info.Mode().Perm())
	}
	if _, err := u.run("", "gateway-store", "show", "--output", out); err == nil {
		t.Fatal("show overwrote an existing file")
	}
	if h := u.mustRunStdout("", "gateway-store", "history"); !strings.Contains(h, `"id":1`) {
		t.Fatalf("history: %s", h)
	}
	// A document naming clients is refused: identity issues keys now.
	withClients := writeFile(t, "clients.json", strings.Replace(gatewayDoc(t), `{"schema_version":1,`, `{"schema_version":1,"clients":[{"id":"a","sha256":"`+strings.Repeat("a", 64)+`","scopes":["llm-gateway.chat"]}],`, 1))
	if out, err := u.run("", "gateway-store", "stage", "--input", withClients, "--expected", "1", "--actor", "test"); err == nil || !strings.Contains(out, "identity module") {
		t.Fatalf("a document with clients: %v\n%s", err, out)
	}
}

func TestMigrateFromOldGatewayDatabase(t *testing.T) {
	u := newUnit(t, "")
	old := filepath.Join(t.TempDir(), "old", "gateway.db")
	writeLegacy(t, old, gatewayDoc(t))
	u.mustRun("", "gateway-store", "migrate-from", "--old-db", old)
	var status struct {
		Latest int64 `json:"latest_revision"`
		Active int64 `json:"active_revision"`
	}
	if err := json.Unmarshal([]byte(u.mustRunStdout("", "gateway-store", "status")), &status); err != nil || status.Latest != 1 || status.Active != 1 {
		t.Fatalf("status after migrating: %+v %v", status, err)
	}
	if out := u.mustRunStdout("", "validate"); !strings.Contains(out, "OK, revision 1") {
		t.Fatalf("validate after migrating: %s", out)
	}
	if out, err := u.run("", "gateway-store", "migrate-from", "--old-db", old); err == nil || !strings.Contains(out, "already has rows") {
		t.Fatalf("a second migration: %v\n%s", err, out)
	}
}

// A service never opens an old gateway.db as its own database, whatever its path.
func TestServiceRefusesOldGatewayDatabase(t *testing.T) {
	u := newUnit(t, "")
	writeLegacy(t, filepath.Join(u.dir, "llm-gateway.db"), gatewayDoc(t))
	for _, args := range [][]string{{"serve"}, {"validate"}, {"gateway-store", "status"}} {
		if out, err := u.run("", args...); err == nil || !strings.Contains(out, "old Station gateway database") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
}

// `gateway-store show` writes a file meant to be edited and staged again. Literal provider header
// values are not written to it, and staging the edited file keeps the stored values.
func TestGatewayStoreShowRedactsHeaderValuesAndStageRestoresThem(t *testing.T) {
	const headerValue = "tenant-value-that-must-not-leave"
	u := newUnit(t, "")
	withHeader := strings.Replace(gatewayDoc(t), `"api_key_env":"LLMGW_TEST_KEY"`, `"api_key_env":"LLMGW_TEST_KEY","headers":{"X-Tenant":"`+headerValue+`"}`, 1)
	u.mustRun("", "gateway-store", "stage", "--input", writeFile(t, "doc.json", withHeader), "--expected", "0", "--actor", "test")
	u.mustRun("", "gateway-store", "activate", "--revision", "1", "--expected", "0", "--actor", "test")
	out := filepath.Join(t.TempDir(), "shown.json")
	u.mustRun("", "gateway-store", "show", "--output", out)
	b, err := os.ReadFile(out)
	if err != nil || strings.Contains(string(b), headerValue) || !strings.Contains(string(b), "X-Tenant") || !strings.Contains(string(b), "[redacted]") {
		t.Fatalf("show wrote %q (%v)", b, err)
	}
	u.mustRun("", "gateway-store", "stage", "--input", out, "--expected", "1", "--actor", "test")
	// Revision 2 is the edited file staged back unchanged; its checksum equals revision 1's only if
	// every placeholder was restored to the stored value.
	var hist []struct {
		ID     int64  `json:"id"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(u.mustRunStdout("", "gateway-store", "history")), &hist); err != nil || len(hist) != 2 {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if hist[0].SHA256 == "" || hist[0].SHA256 != hist[1].SHA256 {
		t.Fatalf("the staged copy differs from the stored revision, so a placeholder was stored: %+v", hist)
	}
}
