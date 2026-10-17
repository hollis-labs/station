package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/pricesource"
	"github.com/hollis-labs/station/platform"
)

const sampleModelsDevJSON = `{
  "openai": {
    "id": "openai",
    "name": "OpenAI",
    "env": ["OPENAI_API_KEY"],
    "api": "https://api.openai.com/v1",
    "doc": "https://platform.openai.com/docs",
    "models": {
      "gpt-backup": {
        "id": "gpt-backup",
        "name": "GPT Backup 4o Mini",
        "family": "gpt-4o",
        "open_weights": false,
        "release_date": "2024-07-18",
        "knowledge": "2023-10",
        "last_updated": "2025-01-01",
        "cost": {
          "input": 0.15,
          "output": 0.60,
          "cache_read": 0.075
        },
        "limit": {
          "context": 128000,
          "output": 16384
        },
        "modalities": {
          "input": ["text", "image"],
          "output": ["text"]
        },
        "tool_call": true,
        "reasoning": false,
        "attachment": true,
        "temperature": true
      },
      "t1": {
        "id": "t1",
        "name": "Text Only From Catalog",
        "family": "gpt-3.5",
        "open_weights": false,
        "cost": {
          "input": 0.50,
          "output": 1.50
        },
        "limit": {
          "context": 16385,
          "output": 4096
        },
        "modalities": {
          "input": ["text"],
          "output": ["text"]
        },
        "tool_call": false,
        "reasoning": false,
        "attachment": false,
        "temperature": true
      }
    }
  },
  "anthropic": {
    "id": "anthropic",
    "name": "Anthropic",
    "env": ["ANTHROPIC_API_KEY"],
    "models": {
      "claude-real": {
        "id": "claude-real",
        "name": "Claude Sonnet 4.5",
        "family": "claude-sonnet",
        "open_weights": false,
        "release_date": "2025-09-29",
        "knowledge": "2025-03",
        "cost": {
          "input": 3.0,
          "output": 15.0,
          "cache_read": 0.3,
          "cache_write": 3.75
        },
        "limit": {
          "context": 200000,
          "output": 64000
        },
        "modalities": {
          "input": ["text", "image", "pdf"],
          "output": ["text"]
        },
        "tool_call": true,
        "reasoning": true,
        "attachment": true,
        "temperature": true
      }
    }
  }
}`

func TestCatalogDefaultTTLAndConfigValidation(t *testing.T) {
	var cfg CatalogConfig
	d, err := cfg.ttlDuration()
	if err != nil || d != 90*time.Second {
		t.Fatalf("expected default 90s TTL, got %v (err=%v)", d, err)
	}

	cfg.TTL = "45s"
	if d, err := cfg.ttlDuration(); err != nil || d != 45*time.Second {
		t.Fatalf("expected 45s TTL, got %v (err=%v)", d, err)
	}

	for _, bad := range []string{"0s", "-5s", "not-a-duration"} {
		c := CatalogConfig{TTL: bad}
		if err := c.validate(); err == nil {
			t.Fatalf("expected error for invalid catalog.ttl %q", bad)
		}
	}
}

func TestCatalogCentralizesModelsDevMetadataCapabilitiesAndPricing(t *testing.T) {
	var fetches atomic.Int32
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sampleModelsDevJSON)
	}))
	t.Cleanup(mdSrv.Close)

	cacheDir := t.TempDir()
	extra := fmt.Sprintf(`    catalog:
      ttl: 90s
      cache_dir: %q
      url: %q
      refresh: true
      warmup: true`, cacheDir, mdSrv.URL)

	// Make o1 fail with 503 so gpt fails over to o2 (gpt-backup), which has NO
	// explicit price in api.yaml and relies on models.dev for pricing!
	e := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, openaiOK, anthropicOK, cfgOpts{extra: extra})

	// Wait briefly for the initial background refresh to populate the cache.
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fetches.Load() == 0 {
		t.Fatal("expected initial catalog fetch on startup")
	}

	// 1. GET /v1/catalog returns providers, models, pricing, limits, modalities, capabilities, and ttl_seconds=90.
	resp, catBody := e.do(t, "GET", "/v1/catalog?provider=anthropic&model=claude-real", oaAuth, "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/catalog status=%d body=%s", resp.StatusCode, catBody)
	}
	for _, want := range []string{
		`"ttl_seconds":90`,
		`"fetched_at":`,
		`"provider_id":"anthropic"`,
		`"id":"claude-real"`,
		`"name":"Claude Sonnet 4.5"`,
		`"family":"claude-sonnet"`,
		`"context":200000`,
		`"output":64000`,
		`"tool_call":true`,
		`"reasoning":true`,
		`"source":"models.dev"`,
	} {
		if !strings.Contains(catBody, want) {
			t.Fatalf("missing %s in /v1/catalog response: %s", want, catBody)
		}
	}

	// 2. GET /v1/models (both OpenAI and Anthropic shapes) surfaces enriched capabilities, price, limits, modalities, metadata.
	_, oaModels := e.do(t, "GET", "/v1/models", oaAuth, "")
	for _, want := range []string{
		`"GPT Backup 4o Mini"`,
		`"context":128000`,
		`"output":16384`,
		`"family":"gpt-4o"`,
		`"source":"models.dev"`,
	} {
		if !strings.Contains(oaModels, want) {
			t.Fatalf("missing %s in OpenAI /v1/models: %s", want, oaModels)
		}
	}

	_, anModels := e.do(t, "GET", "/v1/models", anAuth, "")
	for _, want := range []string{
		`"display_name":"Claude Sonnet 4.5"`,
		`"context":200000`,
		`"reasoning":true`,
		`"family":"claude-sonnet"`,
	} {
		if !strings.Contains(anModels, want) {
			t.Fatalf("missing %s in Anthropic /v1/models: %s", want, anModels)
		}
	}

	// 3. Call "gpt" -> fails over from o1 to o2 ("gpt-backup", which has no price in config).
	// It should be priced automatically from models.dev (input: $0.15/M, output: $0.60/M, cache_read: $0.075/M)!
	cResp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq)
	if cResp.StatusCode != 200 {
		t.Fatalf("chat status %d", cResp.StatusCode)
	}
	entries, _ := usageOf(t, e)
	var backupEntry *Entry
	for i := range entries {
		if entries[i].Row.Model == "gpt-backup" {
			backupEntry = &entries[i]
		}
	}
	if backupEntry == nil {
		t.Fatalf("expected ledger entry for gpt-backup, got %+v", entries)
	}
	if backupEntry.Row.Price == nil || backupEntry.Row.Price.Source != pricesource.SourceModelsDev {
		t.Fatalf("expected models.dev price snapshot on gpt-backup, got %+v", backupEntry.Row.Price)
	}
	// openaiBody has 60 uncached input, 40 cache_read, 20 output:
	// 60*0.15/1e6 + 40*0.075/1e6 + 20*0.60/1e6 = (9 + 3 + 12)/1e6 = 0.000024 USD.
	wantCost := 0.000024
	if backupEntry.CostUSD == nil || *backupEntry.CostUSD < wantCost-1e-12 || *backupEntry.CostUSD > wantCost+1e-12 {
		t.Fatalf("expected models.dev computed cost %v, got %v", wantCost, backupEntry.CostUSD)
	}

	// 4. Capability routing from models.dev: model "textonly" has target t1 (tool_call: false in models.dev)
	// and target t2 (uncataloged, defaults to tool_call: true). Sending a tool request to "textonly"
	// must skip t1 (o1) and route to t2 (o2) automatically!
	toolReq := `{"model":"textonly","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`
	o1Before, _, _ := e.o1.snapshot()
	o2Before, _, _ := e.o2.snapshot()
	if tr, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, toolReq); tr.StatusCode != 200 {
		t.Fatalf("tool request status %d", tr.StatusCode)
	}
	o1After, _, _ := e.o1.snapshot()
	o2After, _, _ := e.o2.snapshot()
	if o1After != o1Before {
		t.Fatal("target t1 with tool_call:false in models.dev should have been skipped for a tool request")
	}
	if o2After != o2Before+1 {
		t.Fatal("capable target t2 was not called")
	}

	// 5. Explicit POST /v1/catalog/refresh endpoint triggers an immediate refresh.
	beforeFetches := fetches.Load()
	rResp, rBody := e.do(t, "POST", "/v1/catalog/refresh", oaAuth, "")
	if rResp.StatusCode != 200 || fetches.Load() <= beforeFetches {
		t.Fatalf("POST /v1/catalog/refresh status=%d body=%s fetches=%d", rResp.StatusCode, rBody, fetches.Load())
	}
	if !strings.Contains(rBody, `"provider_count":2`) || !strings.Contains(rBody, `"model_count":3`) {
		t.Fatalf("unexpected refresh body: %s", rBody)
	}
}

func TestCatalogPriceFileOverridesModelsDev(t *testing.T) {
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sampleModelsDevJSON)
	}))
	t.Cleanup(mdSrv.Close)

	dir := t.TempDir()
	priceFile := filepath.Join(dir, "prices.json")
	asOf := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	tbl, err := pricesource.NewTable("custom-price-table", asOf, pricesource.Entry{
		Provider:  "openai",
		Model:     "gpt-backup",
		Input:     pricesource.Rate(1.0),
		Output:    pricesource.Rate(2.0),
		CacheRead: pricesource.Rate(0.5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pricesource.SaveFile(priceFile, tbl); err != nil {
		t.Fatal(err)
	}

	extra := fmt.Sprintf(`    catalog:
      ttl: 90s
      cache_dir: %q
      url: %q
      price_file: %q
      refresh: false`, dir, mdSrv.URL, priceFile)

	e := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, openaiOK, anthropicOK, cfgOpts{extra: extra})

	// Populate models.dev via refresh endpoint; price_file should still take precedence over models.dev!
	if resp, _ := e.do(t, "POST", "/v1/catalog/refresh", oaAuth, ""); resp.StatusCode != 200 {
		t.Fatalf("refresh status %d", resp.StatusCode)
	}

	_, catBody := e.do(t, "GET", "/v1/catalog?provider=openai&model=gpt-backup", oaAuth, "")
	var parsed struct {
		Models []CatalogModelEntry `json:"models"`
	}
	if err := json.Unmarshal([]byte(catBody), &parsed); err != nil || len(parsed.Models) != 1 {
		t.Fatalf("unexpected catalog response: %v %s", err, catBody)
	}
	snap := parsed.Models[0].PriceSnapshot
	if snap == nil || snap.Source != "custom-price-table" || snap.InputPerMillion != 1.0 {
		t.Fatalf("expected custom-price-table to override models.dev, got %+v", snap)
	}
}

func TestDiscoverModelsAndCatalogInference(t *testing.T) {
	var gotAuth, gotSession string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			gotAuth = r.Header.Get("Authorization")
			gotSession = r.Header.Get("X-Opencode-Session")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"deepseek-v4.1-flash"},{"id":"kimi-k3"},{"id":"jev-1.13"}]}`)
		case "/v1/chat/completions":
			openaiOK(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upSrv.Close)

	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"opencode": {
				"id": "opencode",
				"name": "OpenCode Zen",
				"models": {
					"deepseek-v4.1-flash": {
						"id": "deepseek-v4.1-flash",
						"name": "DeepSeek V4.1 Flash",
						"family": "deepseek",
						"tool_call": true,
						"reasoning": true,
						"cost": {"input": 0.3, "output": 1.2}
					}
				}
			}
		}`)
	}))
	t.Cleanup(mdSrv.Close)

	t.Setenv("TEST_ZEN_KEY", "zen-secret-key")
	y := fmt.Sprintf(`
auth:
  keys:
    - {id: caller-1, sha256: %s, scopes: ["llm-gateway.*"]}
plugins:
  llm-gateway:
    catalog:
      cache_dir: %q
      url: %q
      refresh: true
      warmup: true
    providers:
      - id: opencode-zen
        dialect: openai
        base_url: "%s/v1"
        api_key_env: TEST_ZEN_KEY
        discover_models: true
        headers:
          x-opencode-session: station
`, hashOf("good"), t.TempDir(), mdSrv.URL, upSrv.URL)

	cfg, err := platform.ParseConfig([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	app, err := platform.Assemble(t.Context(), platform.Options{
		Config:  cfg,
		Logger:  quiet,
		Plugins: []platform.Plugin{New()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	if gotAuth != "Bearer zen-secret-key" || gotSession != "station" {
		t.Fatalf("discover_models did not send auth/headers: auth=%q session=%q", gotAuth, gotSession)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/llm-gateway/v1/models", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(raw)

	if !strings.Contains(body, `"id":"deepseek-v4.1-flash"`) || !strings.Contains(body, `"id":"kimi-k3"`) {
		t.Fatalf("expected discovered models in /v1/models, got: %s", body)
	}
	if strings.Contains(body, `"id":"jev-1.13"`) {
		t.Fatalf("expected JEV decision model to be skipped on chat provider discovery, got: %s", body)
	}
	if !strings.Contains(body, `"display_name":"DeepSeek V4.1 Flash"`) {
		t.Fatalf("expected opencode-zen provider ID to infer catalog_provider=opencode and enrich metadata: %s", body)
	}

	if got := normalizeBaseURL("https://opencode.ai/zen"); got != "https://opencode.ai/zen/v1" {
		t.Fatalf("normalizeBaseURL(opencode.ai/zen) = %q", got)
	}
	if got := normalizeBaseURL("https://opencode.ai/zen/go/"); got != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("normalizeBaseURL(opencode.ai/zen/go/) = %q", got)
	}
}
