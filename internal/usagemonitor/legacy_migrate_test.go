package usagemonitor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// legacyDDL is the old usage.db schema exactly as the old code created it (user_version 1,
// application id "STUM").
const legacyDDL = `CREATE TABLE accounts(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, document TEXT NOT NULL);
CREATE TABLE account_audit(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, actor TEXT NOT NULL, changed_at TEXT NOT NULL, document TEXT NOT NULL);
CREATE TABLE observations(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, status TEXT NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX observations_latest ON observations(account_id,observed_at DESC,sequence DESC);
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, model TEXT NOT NULL, cost_kind TEXT NOT NULL, cost_usd REAL, cost_partial INTEGER NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX events_account_time ON events(account_id,observed_at,sequence);
PRAGMA application_id=1398035789;
PRAGMA user_version=1;`

// newLegacy writes an old-format database with two accounts, several observations and events
// (including an unpriced event and a null cost) written with the statements the old code used.
func newLegacy(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, a := range []Account{{ID: "claude", Provider: "anthropic", Kind: "subscription", Enabled: true, Revision: 2}, {ID: "codex", Provider: "openai", Kind: "subscription", Enabled: true, Revision: 1}} {
		raw, _ := json.Marshal(a)
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts(id,revision,document) VALUES(?,?,?)", a.ID, a.Revision, string(raw)); err != nil {
			t.Fatal(i, err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO account_audit(account_id,actor,changed_at,document) VALUES(?,?,?,?)", a.ID, "bootstrap", now.Format(time.RFC3339Nano), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		o := testObservation(string(rune('a'+i)), now.Add(time.Duration(i)*time.Minute))
		raw, _ := json.Marshal(o)
		if _, err := db.ExecContext(ctx, "INSERT INTO observations(account_id,source,record_id,actor,collected_at,observed_at,status,document) VALUES(?,?,?,?,?,?,?,?)",
			"claude", "claude-native", o.ID, "collector:claude-native", now.Format(time.RFC3339Nano), o.ObservedAt.UnixMilli(), o.Status, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		e := testEvent(string(rune('x' + i)))
		if i != 1 {
			e.CostUSD = number(float64(i) + 0.5)
		}
		raw, _ := json.Marshal(e)
		if _, err := db.ExecContext(ctx, "INSERT INTO events(account_id,source,record_id,actor,collected_at,observed_at,model,cost_kind,cost_usd,cost_partial,document) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
			"claude", "gateway", e.ID, "collector:gateway", now.Format(time.RFC3339Nano), e.Row.RecordedAt.UnixMilli(), e.Row.Model, e.Row.CostKind, e.CostUSD, e.CostPartial, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func emptyDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := openTestDB(context.Background(), filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestMigrateLegacyCopiesEverythingWithSequences(t *testing.T) {
	ctx := t.Context()
	old := newLegacy(t)
	dst := emptyDB(t)
	rep, err := MigrateLegacy(ctx, dst, old)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"accounts": 2, "account_audit": 2, "observations": 5, "events": 3}
	for table, n := range want {
		if rep.Rows[table] != n || rep.Digests[table] == "" {
			t.Errorf("%s: %d rows, digest %q; want %d rows", table, rep.Rows[table], rep.Digests[table], n)
		}
	}
	if rep.Integrity != "ok" {
		t.Fatalf("integrity: %q", rep.Integrity)
	}
	s := NewStore(dst)
	page, err := s.Observations(ctx, Query{Limit: 10})
	if err != nil || len(page.Items) != 5 || page.Items[0].Sequence != 1 || page.Items[4].Sequence != 5 {
		t.Fatalf("observations: %+v %v", page, err)
	}
	totals, err := s.Totals(ctx, Query{})
	if err != nil || len(totals) != 1 || totals[0].Events != 3 || totals[0].UnpricedEvents != 1 || totals[0].KnownCostUSD == nil || *totals[0].KnownCostUSD != 3 {
		t.Fatalf("totals: %+v %v", totals, err)
	}
	if a, err := s.Account(ctx, "claude"); err != nil || a.Revision != 2 {
		t.Fatalf("account: %+v %v", a, err)
	}
	// New writes continue the sequences, and a bootstrap check on the migrated account still holds.
	if err := s.Ingest(ctx, "claude", "claude-native", "collector", Batch{Observations: []Observation{testObservation("next", time.Now().UTC())}}); err != nil {
		t.Fatal(err)
	}
	if page, _ = s.Observations(ctx, Query{Limit: 10, After: 5}); len(page.Items) != 1 || page.Items[0].Sequence != 6 {
		t.Fatalf("sequence after migrating: %+v", page.Items)
	}
}

func TestMigrateLegacyRefusals(t *testing.T) {
	ctx := t.Context()
	t.Run("missing file", func(t *testing.T) {
		if _, err := MigrateLegacy(ctx, emptyDB(t), filepath.Join(t.TempDir(), "nope.db")); err == nil {
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
		if _, err := MigrateLegacy(ctx, emptyDB(t), path); err == nil || !strings.Contains(err.Error(), "not an old usage database") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("destination not empty", func(t *testing.T) {
		old := newLegacy(t)
		dst := emptyDB(t)
		if _, err := MigrateLegacy(ctx, dst, old); err != nil {
			t.Fatal(err)
		}
		if _, err := MigrateLegacy(ctx, dst, old); err == nil || !strings.Contains(err.Error(), "already has rows") {
			t.Fatalf("second migration: %v", err)
		}
	})
}

func TestRefuseLegacyDatabase(t *testing.T) {
	if err := RefuseLegacyDatabase(filepath.Join(t.TempDir(), "missing.db")); err != nil {
		t.Fatalf("a missing file: %v", err)
	}
	if err := RefuseLegacyDatabase(newLegacy(t)); err == nil || !strings.Contains(err.Error(), "old Station usage database") {
		t.Fatalf("an old usage database: %v", err)
	}
	st := emptyDB(t)
	var path string
	if err := st.Reader().QueryRowContext(t.Context(), "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := RefuseLegacyDatabase(path); err != nil {
		t.Fatalf("a service database: %v", err)
	}
}
