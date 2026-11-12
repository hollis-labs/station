package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/quota"
)

type budgetReadFailure struct {
	*memStore
	fail bool
}

func (s *budgetReadFailure) Recent(ctx context.Context, q Query) ([]Entry, error) {
	if s.fail {
		return nil, errors.New("private database connection details")
	}
	return s.memStore.Recent(ctx, q)
}

func budgetTestGateway(t *testing.T, store Store, cfg Config, handler http.HandlerFunc) *Gateway {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg.Providers = []ProviderConfig{{ID: "openai", Dialect: "openai", BaseURL: upstream.URL}}
	cfg.Models = []ModelConfig{{Name: "fast", Targets: []TargetConfig{{Provider: "openai", Model: "m"}}}}
	providers := map[string]*provider{"openai": {id: "openai", dialect: openaiDialect{}, baseURL: upstream.URL, client: upstream.Client()}}
	g, err := NewGateway(cfg, providers, NewLedger(store, quiet), quiet)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func budgetRequest(g *Gateway) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	g.Handler(openaiDialect{}, OpChat).ServeHTTP(w, authedReq("POST", "/v1/chat/completions", `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, "alice"))
	return w
}

func TestBudgetSeedFailureClosesAdmissionAndCanRecover(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			store := &budgetReadFailure{memStore: newMemStore(20), fail: true}
			calls := 0
			g := budgetTestGateway(t, store, Config{
				Budgets: []BudgetConfig{{ID: "requests", MaxRequests: 1}},
				Policy:  PolicyConfig{Mode: mode, Rules: []RuleConfig{{ID: "allow-all", Effect: "allow"}}},
			}, func(w http.ResponseWriter, r *http.Request) { calls++; openaiOK(w, r) })
			w := budgetRequest(g)
			if w.Code != 503 || !strings.Contains(w.Body.String(), "budget_unavailable") || strings.Contains(w.Body.String(), "private database") || calls != 0 {
				t.Fatalf("seed failure admitted or leaked details: %d %s calls=%d", w.Code, w.Body.String(), calls)
			}
			store.fail = false
			if w = budgetRequest(g); w.Code != 200 {
				t.Fatalf("read recovery: %d %s", w.Code, w.Body.String())
			}
			if w = budgetRequest(g); w.Code != 429 || calls != 1 {
				t.Fatalf("allow rule bypass: %d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestBudgetRecordFailureStaysClosed(t *testing.T) {
	store := &budgetReadFailure{memStore: newMemStore(20), fail: true}
	b, err := NewBudgets([]BudgetConfig{{ID: "requests", MaxRequests: 2}}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := sampleEntry("failed-record", "alice", "openai", OutcomeCanceled, time.Now().UTC(), nil)
	e.Alias = "fast"
	if err := b.Record(e); err == nil {
		t.Fatal("lost record failure")
	}
	store.fail = false
	if _, err := b.Check(t.Context(), Caller{ID: "alice", Known: true}, "fast"); err == nil {
		t.Fatal("admitted after incomplete counter update")
	}
}

func TestBudgetMissingHistoryFailsClosed(t *testing.T) {
	b, err := NewBudgets([]BudgetConfig{{ID: "requests", MaxRequests: 1}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Check(t.Context(), Caller{ID: "alice", Known: true}, "fast"); err == nil {
		t.Fatal("missing history admitted")
	}
}

func TestBudgetRechecksBeforeFallback(t *testing.T) {
	first := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	second := newUpstream(t, openaiOK)
	cfg := Config{
		Providers: []ProviderConfig{{ID: "first", Dialect: "openai", BaseURL: first.URL}, {ID: "second", Dialect: "openai", BaseURL: second.URL}},
		Models:    []ModelConfig{{Name: "fast", Targets: []TargetConfig{{Provider: "first", Model: "m"}, {Provider: "second", Model: "m"}}}},
		Budgets:   []BudgetConfig{{ID: "attempts", MaxRequests: 1}},
	}
	providers := map[string]*provider{
		"first":  {id: "first", dialect: openaiDialect{}, baseURL: first.URL, client: first.Client()},
		"second": {id: "second", dialect: openaiDialect{}, baseURL: second.URL, client: second.Client()},
	}
	g, err := NewGateway(cfg, providers, NewLedger(newMemStore(10), quiet), quiet)
	if err != nil {
		t.Fatal(err)
	}
	w := budgetRequest(g)
	a, _, _ := first.snapshot()
	b, _, _ := second.snapshot()
	if w.Code != 429 || a != 1 || b != 0 {
		t.Fatalf("fallback bypassed attempt budget: status=%d first=%d second=%d", w.Code, a, b)
	}
}

func TestCanceledDispatchCountsAgainstBudget(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	store := newMemStore(20)
	g := budgetTestGateway(t, store, Config{Budgets: []BudgetConfig{{ID: "attempts", MaxRequests: 1}}}, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	req := authedReq("POST", "/v1/chat/completions", `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, "alice")
	// Preserve the authenticated principal while adding cancellation.
	ctx, cancelRequest := context.WithCancel(req.Context())
	defer cancelRequest()
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { g.Handler(openaiDialect{}, OpChat).ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	<-started
	cancelRequest()
	<-done
	entries, err := store.Recent(t.Context(), Query{})
	if err != nil || len(entries) != 1 || entries[0].Outcome != OutcomeCanceled {
		t.Fatalf("canceled dispatch lost: %+v %v", entries, err)
	}
	if w := budgetRequest(g); w.Code != 429 {
		t.Fatalf("cancellation reset request budget: %d", w.Code)
	}
}

func TestBudgetUnknownSpendHTTPDenial(t *testing.T) {
	store := newMemStore(20)
	e := sampleEntry("unpriced", "alice", "openai", OutcomeInterrupted, time.Now().UTC(), nil)
	e.Alias = "fast"
	if err := store.Add(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	g := budgetTestGateway(t, store, Config{Budgets: []BudgetConfig{{ID: "spend", MaxUSD: 1}}}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unpriced budget reached provider")
		w.WriteHeader(500)
	})
	if w := budgetRequest(g); w.Code != 503 || !strings.Contains(w.Body.String(), "budget_cost_unknown") {
		t.Fatalf("unknown spend: %d %s", w.Code, w.Body.String())
	}
}

func TestBudgetIncompleteSpendExpiresAndRespectsScope(t *testing.T) {
	zero, partial := 0.0, 0.25
	for _, tc := range []struct {
		name    string
		cost    *float64
		partial bool
		want    string
	}{
		{"missing", nil, false, "unknown"}, {"partial", &partial, true, "unknown"}, {"free", &zero, false, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
			store := newMemStore(20)
			e := sampleEntry("history", "alice", "openai", OutcomeCanceled, now, tc.cost)
			e.Alias = "fast"
			e.CostPartial = tc.partial
			if err := store.Add(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			b, err := NewBudgets([]BudgetConfig{{ID: "spend", Scope: "principal", Window: "1h", MaxUSD: 10}}, store, quota.ClockFunc(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			check := func(id, want string) {
				t.Helper()
				s, err := b.Check(t.Context(), Caller{ID: id, Known: true}, "fast")
				if err != nil || s.State != want {
					t.Fatalf("%s: %+v %v", id, s, err)
				}
			}
			check("alice", tc.want)
			check("bob", "ok")
			now = now.Add(2 * time.Hour)
			check("alice", "ok")
		})
	}
}

func TestBudgetCountsEveryDispatchedOutcomeAndDeduplicates(t *testing.T) {
	for _, outcome := range []string{OutcomeOK, OutcomeCanceled, OutcomeInterrupted, OutcomeFailedOver, OutcomeUpstreamErr} {
		t.Run(outcome, func(t *testing.T) {
			b, err := NewBudgets([]BudgetConfig{{ID: "requests", MaxRequests: 2}}, newMemStore(20), nil)
			if err != nil {
				t.Fatal(err)
			}
			e := sampleEntry("attempt", "alice", "openai", outcome, time.Now().UTC(), nil)
			e.Alias = "fast"
			for range 2 {
				if err := b.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			s, err := b.Check(t.Context(), Caller{ID: "alice", Known: true}, "fast")
			if err != nil || s.State != "ok" {
				t.Fatalf("duplicate charged twice: %+v %v", s, err)
			}
			e.ID = "next"
			if err := b.Record(e); err != nil {
				t.Fatal(err)
			}
			s, err = b.Check(t.Context(), Caller{ID: "alice", Known: true}, "fast")
			if err != nil || s.State != "exhausted" {
				t.Fatalf("attempt not counted: %+v %v", s, err)
			}
		})
	}
}

func TestBudgetRejectsNonFiniteAndOverflowingLimits(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e20} {
		if _, err := NewBudgets([]BudgetConfig{{ID: "spend", MaxUSD: v}}, newMemStore(1), nil); err == nil {
			t.Fatalf("accepted %v", v)
		}
	}
}

func TestAttemptPricePinnedAcrossReloadIncludingUnknown(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prices.json")
			if known {
				saveTestPrices(t, path, 2)
			} else if err := os.WriteFile(path, []byte(`{"entries":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			store := newMemStore(20)
			g := budgetTestGateway(t, store, Config{Catalog: CatalogConfig{PriceFile: path, CacheDir: t.TempDir()}}, func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				openaiOK(w, r)
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- budgetRequest(g) }()
			<-started
			saveTestPrices(t, path, 9)
			err := g.catalog.ReloadPrices(t.Context())
			close(release)
			w := <-done
			if err != nil || w.Code != 200 {
				t.Fatalf("request/reload: %d %v", w.Code, err)
			}
			entries, err := store.Recent(t.Context(), Query{})
			if err != nil || len(entries) != 1 {
				t.Fatalf("ledger: %+v %v", entries, err)
			}
			p := entries[0].Row.Price
			if known && (p == nil || p.InputPerMillion != 2) {
				t.Fatalf("retroactive price: %+v", p)
			}
			if !known && p != nil {
				t.Fatalf("retroactively priced unknown attempt: %+v", p)
			}
		})
	}
}

func checkBudgetRestart(t *testing.T, open func() Store) {
	t.Helper()
	store := open()
	cost := 0.75
	e := sampleEntry("canceled-spend", "alice", "openai", OutcomeCanceled, time.Now().UTC().Add(-time.Minute), &cost)
	e.Alias = "fast"
	if err := store.Add(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	// The expensive entry must survive beyond the default recent-read page.
	zero := 0.0
	var later []Entry
	for i := 0; i < 1000; i++ {
		e := sampleEntry(fmt.Sprintf("later-%d", i), "alice", "openai", OutcomeCanceled, time.Now().UTC(), &zero)
		e.Alias = "fast"
		later = append(later, e)
	}
	if err := store.Add(t.Context(), later...); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store = open()
	defer store.Close()
	for _, cfg := range []BudgetConfig{{ID: "spend", MaxUSD: 0.5}, {ID: "requests", MaxRequests: 1001}} {
		b, err := NewBudgets([]BudgetConfig{cfg}, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := b.Check(t.Context(), Caller{ID: "alice", Known: true}, "fast")
		if err != nil || s.State != "exhausted" {
			t.Fatalf("restart lost %s: %+v %v", cfg.ID, s, err)
		}
	}
}

func TestSQLiteBudgetRestartIncludesCanceledSpend(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	checkBudgetRestart(t, func() Store {
		s, err := openSQLiteStore(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestPGBudgetRestartIncludesCanceledSpend(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	checkBudgetRestart(t, func() Store {
		s, err := openPGStore(t.Context(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
