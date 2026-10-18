package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/quota"
)

func reserveTestAttempt(b *Budgets, ctx context.Context, principal string) (*Entry, BudgetStatus, error) {
	return b.Reserve(ctx, Caller{ID: principal, Known: true}, "request", "fast", OpChat, "openai", &target{provider: &provider{id: "openai"}, model: "m"}, false)
}

func runReservationConcurrency(t *testing.T, open func() Store) {
	t.Helper()
	// Independent connections and evaluators emulate separate gateway workers.
	first, second := open(), open()
	defer first.Close()
	defer second.Close()
	cfg := []BudgetConfig{{ID: "total", MaxRequests: 3}, {ID: "principal", Scope: "principal", MaxRequests: 1}}
	a, err := NewBudgets(cfg, first, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudgets(cfg, second, nil)
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			evaluator := a
			if i%2 != 0 {
				evaluator = b
			}
			e, status, err := reserveTestAttempt(evaluator, t.Context(), "alice")
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if e != nil {
				admitted.Add(1)
			} else if status.State != "exhausted" {
				t.Errorf("unexpected refusal: %+v", status)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent principal cap admitted %d attempts", admitted.Load())
	}
	for _, principal := range []string{"bob", "carol"} {
		if e, _, err := reserveTestAttempt(a, t.Context(), principal); err != nil || e == nil {
			t.Fatalf("independent principal %s: %v", principal, err)
		}
	}
	if e, s, err := reserveTestAttempt(b, t.Context(), "dave"); err != nil || e != nil || s.State != "exhausted" {
		t.Fatalf("total cap bypass: %+v %+v %v", e, s, err)
	}
	entries, err := first.Recent(t.Context(), Query{Limit: -1})
	if err != nil || len(entries) != 3 {
		t.Fatalf("refused admission wrote a row: %d %v", len(entries), err)
	}
}

func runReservationRestart(t *testing.T, open func() Store) {
	t.Helper()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := quota.ClockFunc(func() time.Time { return now })
	cfg := []BudgetConfig{{ID: "spend", MaxUSD: 1, Window: "1h"}, {ID: "requests", MaxRequests: 2, Window: "1h"}}
	store := open()
	b, err := NewBudgets(cfg, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	e, s, err := reserveTestAttempt(b, t.Context(), "alice")
	if err != nil || e == nil {
		t.Fatalf("reserve: %+v %v", s, err)
	}
	store.Close() // Simulate a crash with no completion or cancellation.
	store = open()
	defer store.Close()
	b, err = NewBudgets(cfg, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	if next, s, err := reserveTestAttempt(b, t.Context(), "alice"); err != nil || next != nil || s.State != "unknown" {
		t.Fatalf("restart released outstanding spend: %+v %v", s, err)
	}
	pending, err := store.Recent(t.Context(), Query{ID: e.ID})
	if err != nil || len(pending) != 1 || pending[0].Outcome != OutcomeReserved {
		t.Fatalf("missing persisted reservation: %+v %v", pending, err)
	}
	complete := *e
	complete.Outcome = OutcomeCanceled
	complete.Detail = []byte(`{"z":1e3,"a":9007199254740993}`)
	cost := 0.25
	complete.CostUSD = &cost
	journal := store.(reservationStore)
	changed := complete
	changed.Row.RecordedAt = now.Add(time.Second)
	if err := journal.Settle(t.Context(), changed); !errors.Is(err, errSettlementConflict) {
		t.Fatalf("rewrote admission window: %v", err)
	}
	changed = complete
	changed.Row.SessionID = "other"
	if err := journal.Settle(t.Context(), changed); !errors.Is(err, errSettlementConflict) {
		t.Fatalf("rewrote principal: %v", err)
	}
	for range 2 {
		if err := journal.Settle(t.Context(), complete); err != nil {
			t.Fatalf("idempotent settlement: %v", err)
		}
	}
	changed = complete
	changed.Outcome = OutcomeOK
	if err := journal.Settle(t.Context(), changed); !errors.Is(err, errSettlementConflict) {
		t.Fatalf("conflicting retry: %v", err)
	}
	second, s, err := reserveTestAttempt(b, t.Context(), "alice")
	if err != nil || second == nil {
		t.Fatalf("known cost did not release uncertainty: %+v %v", s, err)
	}
	if next, s, err := reserveTestAttempt(b, t.Context(), "alice"); err != nil || next != nil || s.State != "exhausted" {
		t.Fatalf("settlement lost request count: %+v %v", s, err)
	}
	now = now.Add(2 * time.Hour)
	if next, s, err := reserveTestAttempt(b, t.Context(), "alice"); err != nil || next == nil {
		t.Fatalf("admission window failed to expire: %+v %v", s, err)
	}
}

func TestSQLiteReservationConcurrencyAndRestart(t *testing.T) {
	for name, run := range map[string]func(*testing.T, func() Store){"concurrency": runReservationConcurrency, "restart": runReservationRestart} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(managementTempDir(t), "gateway.db")
			run(t, func() Store {
				s, err := openSQLiteStore(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				return s
			})
		})
	}
}

func TestPGReservationConcurrencyAndRestart(t *testing.T) {
	dsn := pgDSN(t)
	for name, run := range map[string]func(*testing.T, func() Store){"concurrency": runReservationConcurrency, "restart": runReservationRestart} {
		t.Run(name, func(t *testing.T) {
			resetPG(t, dsn)
			run(t, func() Store {
				s, err := openPGStore(t.Context(), dsn)
				if err != nil {
					t.Fatal(err)
				}
				return s
			})
		})
	}
}

func TestMemoryReservationsAreAtomic(t *testing.T) {
	store := newMemStore(20)
	runReservationConcurrency(t, func() Store { return store })
}

type settlementFailure struct{ *memStore }

func (s settlementFailure) Settle(context.Context, Entry) error {
	return errors.New("settlement unavailable")
}

func TestReservationSurvivesFailedSettlement(t *testing.T) {
	store := settlementFailure{newMemStore(20)}
	g := budgetTestGateway(t, store, Config{Budgets: []BudgetConfig{{ID: "calls", MaxRequests: 1}}}, openaiOK)
	if w := budgetRequest(g); w.Code != 200 {
		t.Fatalf("first response: %d", w.Code)
	}
	if w := budgetRequest(g); w.Code != 429 {
		t.Fatalf("failed settlement released slot: %d", w.Code)
	}
	entries, _ := store.Recent(t.Context(), Query{})
	if len(entries) != 1 || entries[0].Outcome != OutcomeReserved {
		t.Fatalf("lost unresolved reservation: %+v", entries)
	}
}

func TestReservationBypassesAsyncQueue(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	store, err := openSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	async := newAsyncStore(store, quiet)
	defer async.Close()
	g := budgetTestGateway(t, async, Config{Budgets: []BudgetConfig{{ID: "calls", MaxRequests: 1}}}, func(w http.ResponseWriter, r *http.Request) {
		entries, err := store.Recent(r.Context(), Query{})
		if err != nil || len(entries) != 1 || entries[0].Outcome != OutcomeReserved {
			t.Errorf("provider reached without durable reservation: %+v %v", entries, err)
		}
		openaiOK(w, r)
	})
	if w := budgetRequest(g); w.Code != 200 {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	entries, err := store.Recent(t.Context(), Query{})
	if err != nil || len(entries) != 1 || entries[0].Outcome != OutcomeOK {
		t.Fatalf("completion queued instead of settled: %+v %v", entries, err)
	}
	if entries[0].Row.RecordedAt.IsZero() {
		t.Fatal("missing admission timestamp")
	}
	if w := budgetRequest(g); w.Code != 429 {
		t.Fatalf("completed attempt lost request count: %d", w.Code)
	}
}

func TestReservationFailureDoesNotDispatch(t *testing.T) {
	// An older/custom Store without the atomic contract cannot silently fall back
	// to the old check-then-dispatch path.
	var store Store = newMemStore(20)
	wrapper := struct{ Store }{store}
	calls := 0
	g := budgetTestGateway(t, wrapper, Config{Budgets: []BudgetConfig{{ID: "calls", MaxRequests: 1}}}, func(w http.ResponseWriter, r *http.Request) { calls++; openaiOK(w, r) })
	if w := budgetRequest(g); w.Code != 503 || calls != 0 {
		t.Fatalf("unreserved dispatch: %d calls=%d", w.Code, calls)
	}
}

type slowReservationStore struct{ *memStore }

func (s slowReservationStore) Reserve(ctx context.Context, e *Entry, since time.Time, check func([]Entry) error) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.memStore.Reserve(ctx, e, since, check)
}

func TestReservationWaitDoesNotConsumeProviderHeaderTimeout(t *testing.T) {
	s := slowReservationStore{newMemStore(10)}
	g := budgetTestGateway(t, s, Config{Budgets: []BudgetConfig{{ID: "calls", MaxRequests: 1}}}, openaiOK)
	g.models["fast"][0].headerTimeout = 100 * time.Millisecond
	if w := budgetRequest(g); w.Code != 200 {
		t.Fatalf("database wait consumed provider timeout: %d %s", w.Code, w.Body.String())
	}
}

func TestPGReservationSettlesActualRelayUsage(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	s, err := openPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	buffer := newAsyncStore(s, quiet)
	defer buffer.Close()
	g := budgetTestGateway(t, buffer, Config{Budgets: []BudgetConfig{{ID: "calls", MaxRequests: 1}}}, openaiOK)
	if w := budgetRequest(g); w.Code != 200 {
		t.Fatalf("relay: %d %s", w.Code, w.Body.String())
	}
	entries, err := s.Recent(t.Context(), Query{})
	if err != nil || len(entries) != 1 || entries[0].Outcome != OutcomeOK || entries[0].Row.Usage.OutputTokens.Tokens == 0 {
		t.Fatalf("actual settlement: %+v %v", entries, err)
	}
}

func TestReservationCallbackFailureRollsBack(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	store, err := openSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := sampleEntry("refused", "alice", "openai", OutcomeReserved, time.Now().UTC(), nil)
	want := fmt.Errorf("check failed")
	if err := store.Reserve(t.Context(), &e, time.Time{}, func([]Entry) error { return want }); !errors.Is(err, want) {
		t.Fatalf("failure lost: %v", err)
	}
	entries, err := store.Recent(t.Context(), Query{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed transaction persisted: %+v %v", entries, err)
	}
}

// The helper exits without closing its store or settling, like a process dying
// immediately after durable admission and before a provider response arrives.
func TestReservationCrashHelper(t *testing.T) {
	backend := os.Getenv("STATION_RESERVATION_CRASH_HELPER")
	if backend == "" {
		return
	}
	var store Store
	var err error
	if backend == "sqlite" {
		store, err = openSQLiteStore(t.Context(), os.Getenv("STATION_RESERVATION_TEST_PATH"))
	} else if backend == "postgres" {
		store, err = openPGStore(t.Context(), pgDSN(t))
	} else {
		t.Fatal("invalid helper backend")
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudgets([]BudgetConfig{{ID: "crash", MaxRequests: 1}}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, _, err := reserveTestAttempt(b, t.Context(), "alice"); err != nil || e == nil {
		t.Fatalf("reserve: %v", err)
	}
	os.Exit(0)
}

func TestSQLiteReservationSurvivesProcessExit(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReservationCrashHelper$")
	cmd.Env = append(os.Environ(), "STATION_RESERVATION_CRASH_HELPER=sqlite", "STATION_RESERVATION_TEST_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v %s", err, out)
	}
	s, err := openSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkCrashedReservation(t, s)
}

func TestPGReservationSurvivesProcessExit(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReservationCrashHelper$")
	cmd.Env = append(os.Environ(), "STATION_RESERVATION_CRASH_HELPER=postgres")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v %s", err, out)
	}
	s, err := openPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkCrashedReservation(t, s)
}

func checkCrashedReservation(t *testing.T, s Store) {
	t.Helper()
	b, err := NewBudgets([]BudgetConfig{{ID: "crash", MaxRequests: 1}}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, status, err := reserveTestAttempt(b, t.Context(), "alice"); err != nil || e != nil || status.State != "exhausted" {
		t.Fatalf("process exit released quota: %+v %v", status, err)
	}
}

func TestSettlementJSONPrecision(t *testing.T) {
	if !sameReservationJSON([]byte(`{"z":1e3,"a":9007199254740993}`), []byte(`{"a":9007199254740993,"z":1000}`)) {
		t.Fatal("jsonb canonicalization changed settlement identity")
	}
	if sameReservationJSON([]byte(`{"a":9007199254740993}`), []byte(`{"a":9007199254740992}`)) {
		t.Fatal("lost integer precision in settlement comparison")
	}
}
