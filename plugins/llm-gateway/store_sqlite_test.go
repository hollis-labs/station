package llmgateway

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStoreContract(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	s, err := openSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	runStoreContract(t, s)
	s.Close()
	s, err = openSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := s.Recent(t.Context(), Query{Principal: "bob"})
	if err != nil || len(entries) != 1 || entries[0].ID != "e2" {
		t.Fatalf("usage lost after reopen: %+v %v", entries, err)
	}
	// Management and usage connections coexist; selecting config never clears usage.
	m, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	doc := managementFixture()
	doc.Gateway.Ledger = LedgerConfig{Store: "sqlite", SQLite: SQLiteConfig{Path: path}}
	r, err := m.Save(t.Context(), doc, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(t.Context(), r.ID, 0, "operator"); err != nil {
		t.Fatal(err)
	}
	totals, err := s.Totals(t.Context(), Query{Principal: "bob"})
	if err != nil || len(totals) != 1 || totals[0].UnpricedCalls != 1 {
		t.Fatalf("management changed usage: %+v %v", totals, err)
	}
}

func TestSQLiteLedgerAtomicBatch(t *testing.T) {
	s, err := openSQLiteStore(t.Context(), filepath.Join(managementTempDir(t), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := sampleEntry("good", "app", "provider", OutcomeOK, time.Now().UTC(), nil)
	a.ConversationID = "conversation"
	b := a
	b.ID = "bad"
	b.Detail = json.RawMessage("not JSON")
	if err := s.Add(t.Context(), a, b); err == nil {
		t.Fatal("invalid batch accepted")
	}
	entries, err := s.Recent(t.Context(), Query{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial batch leaked: %+v %v", entries, err)
	}
	if err := s.Add(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Recent(t.Context(), Query{})
	if err != nil || len(entries) != 1 || entries[0].ConversationID != a.ConversationID {
		t.Fatal("conversation attribution lost", err)
	}
}
