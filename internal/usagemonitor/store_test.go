package usagemonitor

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

// openTestDB opens (creating) the usage database at path with the schema applied, as the database
// module does at start. Handles are closed by testStore.Close or when the test binary exits.
func openTestDB(ctx context.Context, path string) (*store.Store, error) {
	cfg := store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path, CreateParentDir: true}}
	st, err := store.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := migrate.Ensure(ctx, cfg, st, migrate.Source{FS: Migrations, Root: "migrations"}); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// testStore is a Store with the Close the tests use to stand in for a restart.
type testStore struct {
	*Store
	db *store.Store
}

func (s testStore) Close() error { return s.db.Close() }

func OpenStore(ctx context.Context, path string) (testStore, error) {
	st, err := openTestDB(ctx, path)
	if err != nil {
		return testStore{}, err
	}
	return testStore{NewStore(st), st}, nil
}

func testStoreAt(t *testing.T) (testStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.PutAccount(t.Context(), Account{ID: "claude", Provider: "anthropic", Kind: "subscription", Enabled: true}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}
func testObservation(id string, at time.Time) Observation {
	return Observation{ID: id, ObservedAt: at, Status: "ok", Windows: []Window{percentWindow("five_hour", number(25), nil)}}
}
func testEvent(id string) Event {
	return Event{ID: id, Outcome: "ok", Row: usageledger.Row{Provider: "anthropic", Model: "example", Usage: usageledger.NewUsage(), RecordedAt: time.Now().UTC(), CostKind: usageledger.CostKindSubscriptionEquivalent}}
}

func TestIngestAtomicIdempotentAndDurable(t *testing.T) {
	s, path := testStoreAt(t)
	ctx := t.Context()
	event := testEvent("event1")
	event.CostUSD = number(1.25)
	b := Batch{Observations: []Observation{testObservation("sample", time.Now().UTC())}, Events: []Event{event}}
	for range 2 {
		if err := s.Ingest(ctx, "claude", "machine", "agent", b); err != nil {
			t.Fatal(err)
		}
	}
	conflict := event
	conflict.CostUSD = number(3)
	if err := s.Ingest(ctx, "claude", "machine", "agent", Batch{Events: []Event{testEvent("should-rollback"), conflict}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	_ = s.Close()
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	page, err := s.Events(ctx, Query{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Data.ID != "event1" {
		t.Fatalf("durability or rollback: %+v %v", page, err)
	}
	totals, err := s.Totals(ctx, Query{})
	if err != nil || len(totals) != 1 || *totals[0].KnownCostUSD != 1.25 || totals[0].CostKind != "subscription_equivalent" {
		t.Fatalf("totals: %+v %v", totals, err)
	}
}

func TestLastSuccessSurvivesFailureAndOldImports(t *testing.T) {
	s, _ := testStoreAt(t)
	ctx := t.Context()
	now := time.Now().UTC()
	for _, o := range []Observation{testObservation("good", now.Add(-time.Minute)), {ID: "failed", ObservedAt: now, Status: "error", ErrorCode: "rate_limited"}, testObservation("old", now.Add(-time.Hour))} {
		if err := s.Ingest(ctx, "claude", "native", "collector", Batch{Observations: []Observation{o}}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.Accounts(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v[0].Latest.Data.ID != "failed" || v[0].LastSuccess.Data.ID != "good" || !v[0].Stale {
		t.Fatalf("lost history/freshness: %+v", v[0])
	}
	page, err := s.Observations(ctx, Query{Limit: 2})
	if err != nil || !page.HasMore {
		t.Fatalf("page: %+v %v", page, err)
	}
	next, err := s.Observations(ctx, Query{Limit: 2, After: page.NextAfter})
	if err != nil || len(next.Items) != 1 || next.Items[0].Data.ID != "old" || next.HasMore {
		t.Fatalf("cursor loses out-of-order import: %+v %v", next, err)
	}
}

func TestConcurrentAccountCASAndDisabledIngestion(t *testing.T) {
	s, path := testStoreAt(t)
	other, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	a, err := s.Account(t.Context(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	a.Enabled = false
	var won atomic.Int32
	var wg sync.WaitGroup
	for _, db := range []*Store{s.Store, other.Store} {
		wg.Go(func() {
			_, e := db.PutAccount(context.Background(), a, a.Revision, "editor")
			if e == nil {
				won.Add(1)
			} else if !errors.Is(e, ErrConflict) {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatal("lost update was not rejected")
	}
	if err = s.Ingest(t.Context(), "claude", "source", "actor", Batch{Events: []Event{testEvent("new")}}); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	a.Revision++
	a.Provider = "another"
	if _, err = s.PutAccount(t.Context(), a, a.Revision, "editor"); !errors.Is(err, ErrConflict) {
		t.Fatalf("history could be relabeled: %v", err)
	}
}

func TestUnknownCostsAndSourceIsolation(t *testing.T) {
	s, _ := testStoreAt(t)
	e := testEvent("same")
	e.Row.Usage.UncachedInputTokens = usageledger.Component{Tokens: 12, Provenance: usageledger.ProvenanceMeasured}
	e.Row.Usage.SetDim("audio", usageledger.Component{Tokens: 3, Provenance: usageledger.ProvenanceEstimated})
	for _, source := range []string{"device1", "device2"} {
		if err := s.Ingest(t.Context(), "claude", source, "actor", Batch{Events: []Event{e}}); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := s.Totals(t.Context(), Query{})
	if err != nil || len(totals) != 2 {
		t.Fatal(totals, err)
	}
	for _, v := range totals {
		if v.KnownTokens != 15 || v.UncertainTokenComponents != 5 {
			t.Fatalf("disjoint token total: %+v", v)
		}
		if v.KnownCostUSD != nil || v.UnpricedEvents != 1 {
			t.Fatalf("unknown was coerced to zero: %+v", v)
		}
	}
	e.Row.CostKind = usageledger.CostKindAPIBilled
	e.CostUSD = number(3)
	if err = s.Ingest(t.Context(), "claude", "another", "actor", Batch{Events: []Event{e}}); err == nil {
		t.Fatal("subscription equivalent accepted as a bill")
	}
}
