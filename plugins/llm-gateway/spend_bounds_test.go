package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/usageledger"
)

func TestTokenizedEmbeddingBoundShapes(t *testing.T) {
	target := &target{provider: &provider{id: "p", dialect: openaiDialect{}}, model: "m", costBoundMethod: tokenizedEmbeddingBound, price: &usageledger.PriceSnapshot{InputPerMillion: 0.3}}
	for _, tc := range []struct {
		body   string
		tokens int64
	}{
		{`{"model":"m","input":[1,2,3]}`, 3},
		{`{"model":"m","input":[[1,2],[3]],"encoding_format":"base64","dimensions":32,"user":"caller"}`, 3},
		{`{"model":"m","input":"hello"}`, 0},
		{`{"model":"m","input":["hello"]}`, 0},
		{`{"model":"m","input":[]}`, 0},
		{`{"model":"m","input":[[]]}`, 0},
		{`{"model":"m","input":[[1],2]}`, 0},
		{`{"model":"m","input":[1,[2]]}`, 0},
		{`{"model":"m","input":[null]}`, 0},
		{`{"model":"m","input":[-1]}`, 0},
		{`{"model":"m","input":[1.5]}`, 0},
		{`{"model":"m","input":[2147483648]}`, 0},
		{`{"model":"m","input":[1],"input":[1,2]}`, 0},
		{`{"model":"m","input":[1],"service_tier":"priority"}`, 0},
		{`{"model":"m","input":[1],"estimated_cost":0}`, 0},
		{`{"model":"m","input":[1],"stream":false}`, 0},
		{`{"model":"m","input":[1],"dimensions":null}`, 0},
		{`{"model":"m","input":[1],"user":null}`, 0},
		{`{"model":"m","input":[1],"encoding_format":"unknown"}`, 0},
		{`{"model":"other","input":[1]}`, 0},
		{`{"model":"m","input":[1]} {}`, 0},
	} {
		bound := boundAttempt(OpEmbed, []byte(tc.body), target)
		if tc.tokens == 0 {
			if bound != nil {
				t.Errorf("bounded unsupported request %s: %+v", tc.body, bound)
			}
			continue
		}
		if bound == nil || bound.InputTokens != tc.tokens || bound.MaxCostMicros != 1 {
			t.Errorf("bound %s: %+v", tc.body, bound)
		}
	}
	if boundAttempt(OpChat, []byte(`{"model":"m","input":[1]}`), target) != nil {
		t.Fatal("bounded unsupported operation")
	}
	target.costBoundMethod = ""
	if boundAttempt(OpEmbed, []byte(`{"model":"m","input":[1]}`), target) != nil {
		t.Fatal("inferred billing contract without declaration")
	}
}

func TestBoundMoneyRounding(t *testing.T) {
	for _, tc := range []struct {
		value float64
		scale int64
		up    bool
		want  int64
		ok    bool
	}{
		{0.1, 10, true, 1, true}, {0.3, 3, true, 1, true}, {0.0000029, 1000000, false, 2, true},
		{0.0000006, 1000000, true, 1, true}, {0.0000006, 1000000, false, 0, true},
		{math.SmallestNonzeroFloat64, 1, true, 1, true}, {0, 1, true, 0, true},
		{math.MaxFloat64, 1000000, true, 0, false}, {math.NaN(), 1, true, 0, false},
		{-1, 1, true, 0, false},
	} {
		got, ok := scaledMicros(tc.value, tc.scale, tc.up)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("round %+v: %d %v", tc, got, ok)
		}
	}
}

func boundedGateway(t *testing.T, store Store, url string, client *http.Client, maxUSD, rate float64) *Gateway {
	t.Helper()
	cfg := Config{
		Providers: []ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: url}},
		Models:    []ModelConfig{{Name: "embed", Kind: "embedding", Targets: []TargetConfig{{Provider: "p", Model: "m", Price: &PriceConfig{InputPerMillion: rate}, CostBoundMethod: tokenizedEmbeddingBound}}}},
		Budgets:   []BudgetConfig{{ID: "strict", MaxUSD: maxUSD, RequireCostBound: true}},
	}
	g, err := NewGateway(cfg, map[string]*provider{"p": {id: "p", dialect: openaiDialect{}, baseURL: url, client: client}}, NewLedger(store, quiet), quiet)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func boundedRequest(g *Gateway, input string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	g.Handler(openaiDialect{}, OpEmbed).ServeHTTP(w, authedReq("POST", "/v1/embeddings", `{"model":"embed","input":`+input+`}`, "alice"))
	return w
}

func embeddingReply(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m, _ := boundObject(body)
	n, _ := embeddingTokenCount(m["input"])
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"data":[],"usage":{"prompt_tokens":%d,"total_tokens":%d}}`, n, n)
}

func runBoundedLedger(t *testing.T, open func() Store) {
	t.Helper()
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); embeddingReply(w, r) }))
	defer u.Close()
	store := open()
	g := boundedGateway(t, store, u.URL, u.Client(), 0.0000029, 0.3)
	// Three tokens cost 0.9 microdollars, rounded up to one. Seven need three,
	// exceeding the two microdollars available before any provider call.
	if w := boundedRequest(g, `[1,2,3,4,5,6,7]`); w.Code != 429 || calls.Load() != 0 {
		t.Fatalf("oversized first call: %d %s", w.Code, w.Body.String())
	}
	for range 2 {
		if w := boundedRequest(g, `[1,2,3]`); w.Code != 200 {
			t.Fatalf("bounded call: %d %s", w.Code, w.Body.String())
		}
	}
	if w := boundedRequest(g, `[1]`); w.Code != 429 || calls.Load() != 2 {
		t.Fatalf("rounded budget overspend: %d calls=%d", w.Code, calls.Load())
	}
	rows, err := store.Recent(t.Context(), Query{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	for _, e := range rows {
		if e.CostBound == nil || e.CostBound.Method != tokenizedEmbeddingBound || e.CostBound.InputTokens != 3 || e.CostBound.MaxCostMicros != 1 || e.Outcome != OutcomeOK || e.CostPartial {
			t.Fatalf("bound or settlement lost: %+v", e)
		}
		changed := e
		copy := *e.CostBound
		copy.MaxCostMicros = 0
		changed.CostBound = &copy
		if err := store.(reservationStore).Settle(t.Context(), changed); err == nil {
			t.Fatal("rewrote immutable admission bound")
		}
	}
	store.Close()
	store = open()
	defer store.Close()
	g = boundedGateway(t, store, u.URL, u.Client(), 0.0000029, 0.3)
	if w := boundedRequest(g, `[1]`); w.Code != 429 {
		t.Fatalf("restart lost bound charges: %d %s", w.Code, w.Body.String())
	}
}

func TestSQLiteBoundedSpend(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	runBoundedLedger(t, func() Store {
		s, e := openSQLiteStore(t.Context(), path)
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
func TestPGBoundedSpend(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	runBoundedLedger(t, func() Store {
		s, e := openPGStore(t.Context(), dsn)
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}

func TestStrictSpendRefusesUnsupportedRequestsAndHeaders(t *testing.T) {
	calls := 0
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; embeddingReply(w, r) }))
	defer u.Close()
	g := boundedGateway(t, newMemStore(20), u.URL, u.Client(), 1, 1)
	for _, input := range []string{`"text"`, `["text"]`, `[null]`, `[]`} {
		if w := boundedRequest(g, input); w.Code != 422 || !strings.Contains(w.Body.String(), "budget_cost_unbounded") {
			t.Fatalf("unsupported shape: %d %s", w.Code, w.Body.String())
		}
	}
	for _, values := range [][]string{{"future-billing-feature"}, {"", "future-billing-feature"}} {
		req := authedReq("POST", "/v1/embeddings", `{"model":"embed","input":[1]}`, "alice")
		req.Header["Openai-Beta"] = values
		w := httptest.NewRecorder()
		g.Handler(openaiDialect{}, OpEmbed).ServeHTTP(w, req)
		if w.Code != 422 || calls != 0 {
			t.Fatalf("beta request dispatched: %d calls=%d", w.Code, calls)
		}
	}
	g.models["embed"][0].costBoundMethod = ""
	if w := boundedRequest(g, `[1]`); w.Code != 422 || calls != 0 {
		t.Fatalf("undeclared contract dispatched: %d", w.Code)
	}
}

func TestBoundedUsageMismatchClosesSpend(t *testing.T) {
	for _, usage := range []string{`{"prompt_tokens":2,"total_tokens":2}`, `{"prompt_tokens":1}`, `{"prompt_tokens":1,"total_tokens":2}`, `{"prompt_tokens":1,"total_tokens":1,"extra_tokens":9}`, `{"prompt_tokens":1,"prompt_tokens":2,"total_tokens":1}`} {
		u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"data":[],"usage":%s}`, usage) }))
		store := newMemStore(10)
		g := boundedGateway(t, store, u.URL, u.Client(), 1, 1)
		if w := boundedRequest(g, `[1]`); w.Code != 200 {
			t.Fatalf("response should remain transparent: %d", w.Code)
		}
		rows, _ := store.Recent(t.Context(), Query{})
		if len(rows) != 1 || !rows[0].CostPartial || rows[0].CostBound == nil {
			t.Fatalf("mismatch lost: %+v", rows)
		}
		if w := boundedRequest(g, `[1]`); w.Code != 503 || !strings.Contains(w.Body.String(), "budget_cost_unknown") {
			t.Fatalf("mismatch reopened budget: %d %s", w.Code, w.Body.String())
		}
		u.Close()
	}
}

func runBoundedConcurrency(t *testing.T, open func() Store) {
	t.Helper()
	first, second := open(), open()
	defer first.Close()
	defer second.Close()
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); embeddingReply(w, r) }))
	defer u.Close()
	gateways := []*Gateway{boundedGateway(t, first, u.URL, u.Client(), 0.0000019, 0.6), boundedGateway(t, second, u.URL, u.Client(), 0.0000019, 0.6)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w := boundedRequest(gateways[i%2], `[1]`)
			if w.Code != 200 && w.Code != 429 && w.Code != 503 {
				t.Errorf("unexpected status %d", w.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent spend admitted %d calls", calls.Load())
	}
}
func TestSQLiteBoundedConcurrency(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	runBoundedConcurrency(t, func() Store {
		s, e := openSQLiteStore(t.Context(), path)
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
func TestPGBoundedConcurrency(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	runBoundedConcurrency(t, func() Store {
		s, e := openPGStore(t.Context(), dsn)
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}

func TestStrictSpendRoundsHistoricalCostsUpAndRejectsOverflow(t *testing.T) {
	for _, costs := range [][]float64{{0.0000006, 0.0000006}, {4e12, 4e12, 4e12}} {
		store := newMemStore(10)
		for i, cost := range costs {
			e := sampleEntry(fmt.Sprint(i), "alice", "p", OutcomeOK, time.Now().UTC(), f64(cost))
			e.Alias = "embed"
			if err := store.Add(t.Context(), e); err != nil {
				t.Fatal(err)
			}
		}
		b, err := NewBudgets([]BudgetConfig{{ID: "strict", MaxUSD: 0.000002, RequireCostBound: true}}, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			s, err := b.Check(context.Background(), Caller{ID: "alice", Known: true}, "embed")
			if costs[0] > 1 {
				if err == nil {
					t.Fatal("overflow admitted or failed seed retried without history")
				}
			} else if err != nil || s.State != "exhausted" {
				t.Fatalf("historical rounding: %+v %v", s, err)
			}
		}
	}
}

func TestCostBoundConfigValidationAndRoundTrip(t *testing.T) {
	base := Config{Providers: []ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: "http://x"}}, Models: []ModelConfig{{Name: "embed", Kind: "embedding", Targets: []TargetConfig{{Provider: "p", Model: "m", CostBoundMethod: tokenizedEmbeddingBound}}}}, Budgets: []BudgetConfig{{ID: "strict", MaxUSD: 1, RequireCostBound: true}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(managementTempDir(t), "gateway.db")
	state, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := state.Save(t.Context(), ManagementDocument{SchemaVersion: 1, Gateway: base}, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Activate(t.Context(), rev.ID, 0, "operator"); err != nil {
		t.Fatal(err)
	}
	state.Close()
	state, err = OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	active, err := state.Active(t.Context())
	if err != nil || !active.Document.Gateway.Budgets[0].RequireCostBound || active.Document.Gateway.Models[0].Targets[0].CostBoundMethod != tokenizedEmbeddingBound {
		t.Fatalf("active configuration lost bound requirements after restart: %+v %v", active, err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Budgets[0].MaxUSD = 0 }, func(c *Config) { c.Budgets[0].MaxUSD = 0.0000009 }, func(c *Config) { c.Models[0].Targets[0].CostBoundMethod = "guess" }, func(c *Config) { c.Models[0].Kind = "chat" }} {
		var c Config
		json.Unmarshal(raw, &c)
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("accepted invalid bound configuration")
		}
	}
}

func runPendingBoundRestart(t *testing.T, open func() Store) {
	t.Helper()
	store := open()
	b, err := NewBudgets([]BudgetConfig{{ID: "strict", MaxUSD: 0.000002, RequireCostBound: true}}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := &target{provider: &provider{id: "p", dialect: openaiDialect{}}, model: "m", price: &usageledger.PriceSnapshot{InputPerMillion: 0.6}, costBound: &CostBound{Method: tokenizedEmbeddingBound, InputTokens: 1, MaxCostMicros: 1}}
	e, s, err := b.Reserve(t.Context(), Caller{ID: "alice", Known: true}, "pending", "embed", OpEmbed, "openai", target, false)
	if err != nil || e == nil {
		t.Fatalf("reserve: %+v %v", s, err)
	}
	store.Close()
	store = open()
	defer store.Close()
	rows, err := store.Recent(t.Context(), Query{ID: e.ID})
	if err != nil || len(rows) != 1 || rows[0].CostBound == nil || *rows[0].CostBound != *e.CostBound {
		t.Fatalf("pending bound lost: %+v %v", rows, err)
	}
	b, err = NewBudgets([]BudgetConfig{{ID: "strict", MaxUSD: 0.000002, RequireCostBound: true}}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, s, err := b.Reserve(t.Context(), Caller{ID: "alice", Known: true}, "overlap", "embed", OpEmbed, "openai", target, false); err != nil || s.State != "unknown" {
		t.Fatalf("restart released unresolved cost: %+v %v", s, err)
	}
	changed := rows[0]
	changed.Outcome = OutcomeOK
	copy := *changed.CostBound
	copy.MaxCostMicros = 0
	changed.CostBound = &copy
	if err := store.(reservationStore).Settle(t.Context(), changed); err == nil {
		t.Fatal("settlement changed pending bound")
	}
	target.reservation = e
	NewLedger(store, quiet).Add(Record{Caller: Caller{ID: "alice", Known: true}, RequestID: "pending", Alias: "embed", Operation: OpEmbed, Dialect: "openai", Target: target, Usage: &Usage{Input: 1}, Status: 200, Outcome: OutcomeOK})
	e2, s, err := b.Reserve(t.Context(), Caller{ID: "alice", Known: true}, "after-settlement", "embed", OpEmbed, "openai", target, false)
	if err != nil || e2 == nil {
		t.Fatalf("known settled spend did not reopen available capacity: %+v %v", s, err)
	}
}

func TestSQLitePendingBoundRestart(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	runPendingBoundRestart(t, func() Store {
		s, e := openSQLiteStore(t.Context(), path)
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
func TestPGPendingBoundRestart(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	runPendingBoundRestart(t, func() Store {
		s, e := openPGStore(t.Context(), dsn)
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}

func TestPGBoundMigrationPreservesExistingUsage(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	s, err := openPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	old := sampleEntry("pre-bound", "alice", "p", OutcomeOK, time.Now().UTC().Truncate(time.Microsecond), f64(0.25))
	if err := s.Add(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	// Recreate the immediately preceding schema in this isolated test database,
	// retaining a real row so the upgrade proves data preservation.
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `ALTER TABLE llm_calls DROP COLUMN cost_bound; DELETE FROM llm_ledger_schema WHERE version=3`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Recent(t.Context(), Query{ID: old.ID})
	if err != nil || len(rows) != 1 || rows[0].CostBound != nil || rows[0].CostUSD == nil || *rows[0].CostUSD != 0.25 || !rows[0].Row.RecordedAt.Equal(old.Row.RecordedAt) {
		t.Fatalf("migration changed historical usage: %+v %v", rows, err)
	}
}
