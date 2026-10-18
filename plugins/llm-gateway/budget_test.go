package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/usageledger"
	"github.com/hollis-labs/libs/security/auth"
)

func authedReq(method, url, body, principalID string) *http.Request {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		ctx := auth.WithPrincipal(req.Context(), auth.Principal{
			ID:       principalID,
			Kind:     "service",
			Verified: true,
		})
		req = req.WithContext(ctx)
	}
	return req
}

func TestBudgetsPerPrincipalIndependentOfPolicyMode(t *testing.T) {
	u := newUpstream(t, openaiOK)
	store := newMemStore(100)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	baseCfg := Config{
		Providers: []ProviderConfig{{ID: "o1", Dialect: "openai", BaseURL: u.URL}},
		Models: []ModelConfig{{
			Name: "fast",
			Targets: []TargetConfig{{
				Provider: "o1",
				Model:    "gpt-x",
				Price:    &PriceConfig{InputPerMillion: 1000, OutputPerMillion: 2000},
			}},
		}},
		Budgets: []BudgetConfig{{
			ID:          "per-caller-2reqs",
			Scope:       "principal",
			Window:      "day",
			MaxRequests: 2,
		}},
		Policy: PolicyConfig{Mode: "shadow"},
	}
	providers := map[string]*provider{
		"o1": {id: "o1", dialect: openaiDialect{}, baseURL: u.URL, client: u.Client()},
	}

	// Policy shadow mode must not relax configured budget limits.
	ledger := NewLedger(store, logger)
	gw, err := NewGateway(baseCfg, providers, ledger, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	h := gw.Handler(openaiDialect{}, OpChat)
	reqBody := `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`

	for i := range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedReq("POST", "/v1/chat/completions", reqBody, "alice"))
		if rec.Code != http.StatusOK {
			t.Fatalf("shadow call %d: status = %d, want 200 (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	recThird := httptest.NewRecorder()
	h.ServeHTTP(recThird, authedReq("POST", "/v1/chat/completions", reqBody, "alice"))
	if recThird.Code != http.StatusTooManyRequests {
		t.Fatalf("shadow budget: got %d, want 429", recThird.Code)
	}

	// 2. Build a new Gateway over the SAME store (simulating a process restart)
	// with policy.mode = enforce. Because Budgets seeds from store.Recent on
	// first check, alice is already over budget, while bob has a fresh quota.
	enforceCfg := baseCfg
	enforceCfg.Policy = PolicyConfig{Mode: "enforce"}
	gwEnforce, err := NewGateway(enforceCfg, providers, ledger, logger)
	if err != nil {
		t.Fatalf("NewGateway enforce: %v", err)
	}
	he := gwEnforce.Handler(openaiDialect{}, OpChat)

	recAlice := httptest.NewRecorder()
	he.ServeHTTP(recAlice, authedReq("POST", "/v1/chat/completions", reqBody, "alice"))
	if recAlice.Code != http.StatusTooManyRequests {
		t.Fatalf("enforce alice status = %d, want 429 (%s)", recAlice.Code, recAlice.Body.String())
	}
	if recAlice.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 429 budget_exceeded")
	}
	if !strings.Contains(recAlice.Body.String(), `"budget_exceeded"`) {
		t.Fatalf("expected budget_exceeded code, got %s", recAlice.Body.String())
	}

	// Bob's per-principal budget is independent and still has 2 calls available.
	recBob := httptest.NewRecorder()
	he.ServeHTTP(recBob, authedReq("POST", "/v1/chat/completions", reqBody, "bob"))
	if recBob.Code != http.StatusOK {
		t.Fatalf("enforce bob status = %d, want 200 (%s)", recBob.Code, recBob.Body.String())
	}
}

func TestBudgetExhaustionCannotBeBypassedByTargetAllowRule(t *testing.T) {
	localUp := newUpstream(t, openaiOK)
	paidUp := newUpstream(t, openaiOK)
	store := newMemStore(100)

	// Pre-seed $1.50 of spend so the $1.00 daily budget is already exhausted.
	cost := 1.50
	_ = store.Add(context.Background(), Entry{
		ID:      "seed-1",
		Alias:   "coder",
		Outcome: OutcomeOK,
		CostUSD: &cost,
		Row: usageledger.Row{
			SessionID:  "dev-1",
			Provider:   "paid-cloud",
			Model:      "gpt-4o",
			RecordedAt: time.Now().UTC(),
		},
	})

	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "local-llama", Dialect: "openai", BaseURL: localUp.URL},
			{ID: "paid-cloud", Dialect: "openai", BaseURL: paidUp.URL},
		},
		Models: []ModelConfig{
			{
				Name: "coder",
				Targets: []TargetConfig{
					{Provider: "paid-cloud", Model: "gpt-4o", Tags: []string{"paid", "cloud"}},
					{Provider: "local-llama", Model: "qwen-coder", Tags: []string{"local", "free"}, BestEffort: true},
				},
			},
			{
				Name: "paid-only",
				Targets: []TargetConfig{
					{Provider: "paid-cloud", Model: "gpt-4o", Tags: []string{"paid", "cloud"}},
				},
			},
		},
		Budgets: []BudgetConfig{{
			ID:     "daily-usd",
			Scope:  "total",
			Window: "day",
			MaxUSD: 1.00,
		}},
		Policy: PolicyConfig{
			Mode: "enforce",
			Rules: []RuleConfig{
				{
					ID:          "allow-free-over-budget",
					BudgetState: "exhausted",
					Tag:         "free",
					Effect:      "allow",
					Reason:      "local free routes remain available when paid budget is exhausted",
				},
			},
		},
	}
	providers := map[string]*provider{
		"local-llama": {id: "local-llama", dialect: openaiDialect{}, baseURL: localUp.URL, client: localUp.Client()},
		"paid-cloud":  {id: "paid-cloud", dialect: openaiDialect{}, baseURL: paidUp.URL, client: paidUp.Client()},
	}
	gw, err := NewGateway(cfg, providers, NewLedger(store, quiet), quiet)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	h := gw.Handler(openaiDialect{}, OpChat)

	// A free tag is operator metadata, not proof that an attempt cannot spend.
	// Neither target-specific nor broad allow rules can override a budget.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedReq("POST", "/v1/chat/completions", `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`, "dev-1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("coder status = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	paidCalls, _, _ := paidUp.snapshot()
	localCalls, _, _ := localUp.snapshot()
	if paidCalls != 0 || localCalls != 0 {
		t.Fatalf("budget bypass: paid=%d local=%d", paidCalls, localCalls)
	}

	// "paid-only" has no free target, so it is refused with 429 budget_exceeded.
	recPaid := httptest.NewRecorder()
	h.ServeHTTP(recPaid, authedReq("POST", "/v1/chat/completions", `{"model":"paid-only","messages":[{"role":"user","content":"hi"}]}`, "dev-1"))
	if recPaid.Code != http.StatusTooManyRequests || !strings.Contains(recPaid.Body.String(), `"budget_exceeded"`) {
		t.Fatalf("paid-only status = %d, want 429 budget_exceeded (%s)", recPaid.Code, recPaid.Body.String())
	}
}

func TestBestEffortFastFailoverAndModelsReachability(t *testing.T) {
	// Sleeping local llama server: hangs for 500ms unless request context is cancelled.
	sleepingLocal := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
			openaiOK(w, r)
		}
	})
	cloudBackup := newUpstream(t, openaiOK)

	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "agent-os-llama", Dialect: "openai", BaseURL: sleepingLocal.URL},
			{ID: "openai-hub", Dialect: "openai", BaseURL: cloudBackup.URL},
		},
		Models: []ModelConfig{{
			Name: "general",
			Targets: []TargetConfig{
				{
					Provider:   "agent-os-llama",
					Model:      "llama-3.3-70b",
					Tags:       []string{"local", "free"},
					BestEffort: true,
					TimeoutMS:  40,
				},
				{
					Provider: "openai-hub",
					Model:    "gpt-4o-mini",
					Tags:     []string{"paid", "cloud"},
				},
			},
		}},
	}
	providers := map[string]*provider{
		"agent-os-llama": {id: "agent-os-llama", dialect: openaiDialect{}, baseURL: sleepingLocal.URL, client: sleepingLocal.Client()},
		"openai-hub":     {id: "openai-hub", dialect: openaiDialect{}, baseURL: cloudBackup.URL, client: cloudBackup.Client()},
	}
	gw, err := NewGateway(cfg, providers, NewLedger(newMemStore(50), quiet), quiet)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	chatH := gw.Handler(openaiDialect{}, OpChat)
	modelsH := gw.ModelsHandler()

	// Before any call, both targets report reachable=true, state=closed.
	recM1 := httptest.NewRecorder()
	modelsH.ServeHTTP(recM1, httptest.NewRequest("GET", "/v1/models", nil))
	var list1 struct {
		Data []struct {
			ID        string       `json:"id"`
			Kind      string       `json:"kind"`
			Reachable bool         `json:"reachable"`
			Tags      []string     `json:"tags"`
			Targets   []TargetInfo `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recM1.Body.Bytes(), &list1); err != nil || len(list1.Data) != 1 {
		t.Fatalf("models before call: %v (%s)", err, recM1.Body.String())
	}
	if !list1.Data[0].Reachable || len(list1.Data[0].Targets) != 2 || !list1.Data[0].Targets[0].Reachable {
		t.Fatalf("expected both targets reachable initially, got %+v", list1.Data[0])
	}

	// Call 1: sleeping local server times out after ~40ms, trips its threshold-1
	// best-effort breaker, and fails over cleanly to openai-hub.
	start1 := time.Now()
	rec1 := httptest.NewRecorder()
	chatH.ServeHTTP(rec1, authedReq("POST", "/v1/chat/completions", `{"model":"general","messages":[{"role":"user","content":"hi"}]}`, "caller-1"))
	elapsed1 := time.Since(start1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 status = %d, want 200 (%s)", rec1.Code, rec1.Body.String())
	}
	if elapsed1 >= 300*time.Millisecond {
		t.Fatalf("call 1 took %s; expected fast failover well under 300ms", elapsed1)
	}
	localCalls1, _, _ := sleepingLocal.snapshot()
	cloudCalls1, _, _ := cloudBackup.snapshot()
	if localCalls1 != 1 || cloudCalls1 != 1 {
		t.Fatalf("after call 1: localCalls=%d cloudCalls=%d, want 1 and 1", localCalls1, cloudCalls1)
	}

	// Call 2: best-effort breaker is now open, so sleepingLocal is not even dialed.
	rec2 := httptest.NewRecorder()
	chatH.ServeHTTP(rec2, authedReq("POST", "/v1/chat/completions", `{"model":"general","messages":[{"role":"user","content":"hi"}]}`, "caller-1"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("call 2 status = %d, want 200", rec2.Code)
	}
	localCalls2, _, _ := sleepingLocal.snapshot()
	cloudCalls2, _, _ := cloudBackup.snapshot()
	if localCalls2 != 1 || cloudCalls2 != 2 {
		t.Fatalf("after call 2: localCalls=%d (want still 1, skipped by open breaker), cloudCalls=%d (want 2)", localCalls2, cloudCalls2)
	}

	// GET /v1/models now shows the local target as reachable=false, state="open",
	// while the overall "general" model remains reachable=true via the backup.
	recM2 := httptest.NewRecorder()
	modelsH.ServeHTTP(recM2, httptest.NewRequest("GET", "/v1/models", nil))
	var list2 struct {
		Data []struct {
			ID        string       `json:"id"`
			Reachable bool         `json:"reachable"`
			Targets   []TargetInfo `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recM2.Body.Bytes(), &list2); err != nil || len(list2.Data) != 1 {
		t.Fatalf("models after call: %v", err)
	}
	m := list2.Data[0]
	if !m.Reachable {
		t.Fatalf("model general should still be reachable via backup target: %+v", m)
	}
	if m.Targets[0].Reachable || m.Targets[0].State != "open" || !m.Targets[0].BestEffort {
		t.Fatalf("target[0] (agent-os-llama) = %+v, want Reachable=false State=open BestEffort=true", m.Targets[0])
	}
	if !m.Targets[1].Reachable || m.Targets[1].State != "closed" {
		t.Fatalf("target[1] (openai-hub) = %+v, want Reachable=true State=closed", m.Targets[1])
	}
}
