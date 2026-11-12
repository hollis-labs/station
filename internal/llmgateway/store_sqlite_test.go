package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
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
	defer func() { _ = m.Close() }()
	doc := managementFixture()
	doc.Gateway.Ledger = LedgerConfig{Store: "sqlite"}
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

// The service store has one write connection; with a single reader as well, a read that is still
// open while a write is issued (or the reverse) must not wedge. Readers and writers run together.
func TestSQLiteLedgerOneReadConnection(t *testing.T) {
	cfg := store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{
		Path: filepath.Join(t.TempDir(), "gateway.db"), ReadConns: 1,
	}}
	st, err := store.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := migrate.Ensure(t.Context(), cfg, st, migrate.Source{FS: Migrations, Root: "migrations"}); err != nil {
		t.Fatal(err)
	}
	s := &sqliteStore{st: st}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				e := sampleEntry(fmt.Sprintf("w%d-%d", w, i), "app", "provider", OutcomeOK, time.Now().UTC(), nil)
				if err := s.Add(ctx, e); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := s.Recent(ctx, Query{Limit: 10}); err != nil {
					errs <- err
					return
				}
				if _, err := s.Totals(ctx, Query{}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	totals, err := s.Totals(ctx, Query{})
	if err != nil || len(totals) == 0 {
		t.Fatalf("totals after concurrent run: %+v %v", totals, err)
	}
	var calls int
	for _, tot := range totals {
		calls += tot.Calls
	}
	if calls != 100 {
		t.Fatalf("calls = %d, want 100", calls)
	}
}
