package llmgateway

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/pricesource"
	"github.com/hollis-labs/libs/llm/core/usageledger"
	"gopkg.in/yaml.v3"
)

func TestSpendAdmissionRequiresBaseRates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    Operation
		price *usageledger.PriceSnapshot
		allow bool
	}{
		{"missing", OpChat, nil, false},
		{"input unknown", OpChat, &usageledger.PriceSnapshot{UnknownRates: usageledger.UnknownRates{UncachedInput: true}}, false},
		{"output unknown", OpResponses, &usageledger.PriceSnapshot{UnknownRates: usageledger.UnknownRates{Output: true}}, false},
		{"decision output unknown", OpDecide, &usageledger.PriceSnapshot{UnknownRates: usageledger.UnknownRates{Output: true}}, false},
		{"embedding input unknown", OpEmbed, &usageledger.PriceSnapshot{UnknownRates: usageledger.UnknownRates{UncachedInput: true}}, false},
		{"embedding needs only input", OpEmbed, &usageledger.PriceSnapshot{InputPerMillion: 2, UnknownRates: usageledger.UnknownRates{Output: true, CacheRead: true, CacheWrite: true, Reasoning: true}}, true},
		{"known free", OpChat, &usageledger.PriceSnapshot{}, true},
		{"known base rates", OpChat, &usageledger.PriceSnapshot{InputPerMillion: 2, OutputPerMillion: 4, UnknownRates: usageledger.UnknownRates{CacheRead: true, CacheWrite: true, Reasoning: true}}, true},
		{"negative", OpChat, &usageledger.PriceSnapshot{InputPerMillion: -1}, false},
		{"infinite", OpChat, &usageledger.PriceSnapshot{OutputPerMillion: math.Inf(1)}, false},
		{"NaN cache", OpChat, &usageledger.PriceSnapshot{CacheReadPerMillion: math.NaN()}, false},
		{"future operation", Operation("future"), &usageledger.PriceSnapshot{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore(10)
			b, err := NewBudgets([]BudgetConfig{{ID: "dollars", MaxUSD: 1}, {ID: "calls", MaxRequests: 1}}, store, nil)
			if err != nil {
				t.Fatal(err)
			}
			target := &target{provider: &provider{id: "p"}, model: "m", price: tc.price}
			e, s, err := b.Reserve(t.Context(), Caller{ID: "alice", Known: true}, "r", "fast", tc.op, "openai", target, false)
			if err != nil || (e != nil) != tc.allow {
				t.Fatalf("admission: entry=%+v status=%+v err=%v", e, s, err)
			}
			rows, err := store.Recent(t.Context(), Query{})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.allow {
				if s.State != "price_unknown" || len(rows) != 0 || len(s.Unknown) != 1 || s.Unknown[0] != "dollars" {
					t.Fatalf("refusal consumed quota or lost cause: %+v %+v", s, rows)
				}
				// The rejected attempt must not poison a later correctly priced one.
				target.price = &usageledger.PriceSnapshot{}
				if e, s, err = b.Reserve(t.Context(), Caller{ID: "alice", Known: true}, "r2", "fast", OpChat, "openai", target, false); err != nil || e == nil {
					t.Fatalf("recovery: %+v %v", s, err)
				}
			} else if s.State != "ok" || len(rows) != 1 || rows[0].Outcome != OutcomeReserved {
				t.Fatalf("missing admitted reservation: %+v %+v", s, rows)
			}
		})
	}
}

func TestPriceAdmissionUsesMatchingBudgetsAndVerifiedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    []BudgetConfig
		caller Caller
		deny   bool
	}{
		{"request only", []BudgetConfig{{ID: "calls", MaxRequests: 1}}, Caller{ID: "alice", Known: true}, false},
		{"other model", []BudgetConfig{{ID: "usd", MaxUSD: 1, Model: "other*"}}, Caller{ID: "alice", Known: true}, false},
		{"other principal", []BudgetConfig{{ID: "usd", MaxUSD: 1, Principal: "bob"}}, Caller{ID: "alice", Known: true}, false},
		{"matching glob", []BudgetConfig{{ID: "usd", MaxUSD: 1, Principal: "ali*", Model: "fa*"}}, Caller{ID: "alice", Known: true}, true},
		{"unverified identity", []BudgetConfig{{ID: "usd", MaxUSD: 1, Principal: "anonymous"}}, Caller{ID: "alice", Known: false}, true},
		{"anonymous cannot impersonate", []BudgetConfig{{ID: "usd", MaxUSD: 1, Principal: "alice"}}, Caller{ID: "alice", Known: false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBudgets(tc.cfg, newMemStore(10), nil)
			if err != nil {
				t.Fatal(err)
			}
			e, s, err := b.Reserve(t.Context(), tc.caller, "r", "fast", OpChat, "openai", &target{provider: &provider{id: "p"}, model: "m"}, false)
			if err != nil || (e == nil) != tc.deny {
				t.Fatalf("scope mismatch: %+v %+v %v", e, s, err)
			}
		})
	}
}

func TestPriceAdmissionCannotBeBypassedByPolicyOrFreeTag(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			store := newMemStore(10)
			g := budgetTestGateway(t, store, Config{
				Budgets: []BudgetConfig{{ID: "usd", MaxUSD: 1}},
				Policy:  PolicyConfig{Mode: mode, Rules: []RuleConfig{{ID: "allow", Effect: "allow", Tag: "free"}}},
			}, func(w http.ResponseWriter, r *http.Request) { calls++; openaiOK(w, r) })
			g.models["fast"][0].tags = []string{"free"}
			// Repeat to prove a refusal neither trips the provider breaker nor consumes a slot.
			for range 4 {
				w := budgetRequest(g)
				if w.Code != 503 || !strings.Contains(w.Body.String(), "budget_price_unknown") || w.Header().Get("Retry-After") != "" || calls != 0 {
					t.Fatalf("unpriced dispatch: %d %s calls=%d", w.Code, w.Body.String(), calls)
				}
			}
			g.models["fast"][0].price = &usageledger.PriceSnapshot{}
			if w := budgetRequest(g); w.Code != 200 || calls != 1 {
				t.Fatalf("known free price failed recovery: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func runPriceReloadAdmission(t *testing.T, store Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices.json")
	save := func(output *float64) {
		t.Helper()
		table, err := pricesource.NewTable("operator", time.Now().UTC(), pricesource.Entry{Provider: "openai", Model: "m", Input: pricesource.Rate(0), Output: output})
		if err != nil {
			t.Fatal(err)
		}
		if err := pricesource.SaveFile(path, table); err != nil {
			t.Fatal(err)
		}
	}
	save(nil) // An absent output rate is different from an explicit zero.
	var g *Gateway
	calls := 0
	g = budgetTestGateway(t, store, Config{Catalog: CatalogConfig{PriceFile: path, CacheDir: t.TempDir()}, Budgets: []BudgetConfig{{ID: "usd", MaxUSD: 1}}}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Concurrent catalog changes cannot change the snapshot admitted for this attempt.
		save(pricesource.Rate(1000))
		if err := g.catalog.ReloadPrices(t.Context()); err != nil {
			t.Error(err)
		}
		openaiOK(w, r)
	})
	w := budgetRequest(g)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "budget_price_unknown") || calls != 0 {
		t.Fatalf("partial table admitted: %d %s", w.Code, w.Body.String())
	}
	rows, err := store.Recent(t.Context(), Query{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("price refusal persisted a reservation: %+v %v", rows, err)
	}
	save(pricesource.Rate(0))
	if err := g.catalog.ReloadPrices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := budgetRequest(g); w.Code != 200 || calls != 1 {
		t.Fatalf("reload did not recover: %d %s", w.Code, w.Body.String())
	}
	rows, err = store.Recent(t.Context(), Query{})
	if err != nil || len(rows) != 1 || rows[0].Outcome != OutcomeOK || rows[0].Row.Price.OutputPerMillion != 0 || rows[0].CostUSD == nil || *rows[0].CostUSD != 0 {
		t.Fatalf("admission/settlement price disagreement: %+v %v", rows, err)
	}
	// Base pricing is not complete pricing: this fixture reports cached tokens
	// for which the override has no rate. Preserve the existing partial-cost guard.
	if !rows[0].CostPartial {
		t.Fatal("unknown cache price treated as free")
	}
	if w := budgetRequest(g); w.Code != 503 || !strings.Contains(w.Body.String(), "budget_cost_unknown") || calls != 1 {
		t.Fatalf("partial-cost settlement reopened spend: %d %s calls=%d", w.Code, w.Body.String(), calls)
	}
}

func TestModelsDevPriceAvailabilityControlsAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, cost string
		allow      bool
	}{
		{"missing", ``, false},
		{"partial", `,"cost":{"input":0}`, false},
		{"tiered", `,"cost":{"input":1,"output":2,"tiers":[{"tier":{"type":"context","size":200000},"input":2,"output":4}]}`, false},
		{"free", `,"cost":{"input":0,"output":0,"cache_read":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"openai":{"id":"openai","models":{"m":{"id":"m"%s}}}}`, tc.cost)
			}))
			defer catalogServer.Close()
			calls := 0
			g := budgetTestGateway(t, newMemStore(10), Config{Catalog: CatalogConfig{URL: catalogServer.URL, CacheDir: t.TempDir()}, Budgets: []BudgetConfig{{ID: "usd", MaxUSD: 1}}}, func(w http.ResponseWriter, r *http.Request) { calls++; openaiOK(w, r) })
			if err := g.catalog.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			w := budgetRequest(g)
			if tc.allow {
				if w.Code != 200 || calls != 1 {
					t.Fatalf("known catalog rates refused: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != 503 || !strings.Contains(w.Body.String(), "budget_price_unknown") || calls != 0 {
				t.Fatalf("catalog uncertainty admitted: %d %s calls=%d", w.Code, w.Body.String(), calls)
			}
		})
	}
}

func TestSQLitePriceAdmissionReloadAndSettlement(t *testing.T) {
	store, err := openSQLiteStore(t.Context(), filepath.Join(managementTempDir(t), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runPriceReloadAdmission(t, store)
}

func TestPGPriceAdmissionReloadAndSettlement(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	store, err := openPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runPriceReloadAdmission(t, store)
}

func TestExplicitPriceRejectsNonFiniteRates(t *testing.T) {
	for _, field := range []string{"input_per_million", "output_per_million", "cache_read_per_million", "cache_write_per_million", "reasoning_per_million"} {
		for _, value := range []string{".nan", ".inf", "-.inf", "-1"} {
			var price PriceConfig
			if err := yaml.Unmarshal([]byte(field+": "+value), &price); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Providers: []ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: "http://x"}}, Models: []ModelConfig{{Name: "m", Targets: []TargetConfig{{Provider: "p", Model: "m", Price: &price}}}}}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("accepted %s=%s", field, value)
			}
		}
	}
}
