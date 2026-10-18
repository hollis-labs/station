package llmgateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/libs/llm/core/modelsdev"
)

func TestCatalogMetadataHTTPAndOfflineRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"openai":{"id":"openai","models":{"m":{
		  "id":"m","canonical_model_id":"lab/m","description":"Fixture","type":"language","status":"beta",
		  "structured_output":false,"reasoning":true,"limit":{"input":100},
		  "provider":{"shape":"responses","headers":{"x-fixture":"descriptive"}},
		  "reasoning_options":[{"type":"effort","values":[null,"high"]}],
		  "interleaved":{"field":"reasoning_content"},"experimental":{"modes":{"fast":{"cost":{"input":0}}}},
		  "cost":{"input":0,"output":null,"input_audio":3,"context_over_200k":{"input":4}}
		}}}}`)
	}))
	t.Cleanup(srv.Close)
	cfg := CatalogConfig{URL: srv.URL, CacheDir: t.TempDir()}
	c, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	// The first read after restart must expose the same metadata and preserve
	// unknown rates without requiring the upstream to be reachable.
	restarted, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	for name, catalog := range map[string]*Catalog{"live": c, "offline": restarted} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Gateway{catalog: catalog}).CatalogHandler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/catalog?provider=openai&model=m", nil))
			var body struct {
				Models []CatalogModelEntry `json:"models"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || len(body.Models) != 1 {
				t.Fatalf("catalog: %v %s", err, w.Body.String())
			}
			m := body.Models[0]
			if m.CanonicalModelID != "lab/m" || m.Description != "Fixture" || m.Type != "language" || m.Status != "beta" {
				t.Fatalf("metadata lost: %+v", m)
			}
			if m.ProviderMetadata == nil || m.ProviderMetadata.Shape != "responses" || m.ProviderMetadata.Headers["x-fixture"] != "descriptive" ||
				len(m.ReasoningOptions) != 1 || len(m.ReasoningOptions[0].Values) != 2 || m.ReasoningOptions[0].Values[0] != nil ||
				m.Interleaved == nil || m.Interleaved.Field != "reasoning_content" || m.Experimental == nil || m.Experimental.Modes["fast"].Cost == nil {
				t.Fatalf("controls lost: %+v", m)
			}
			if m.Capabilities.StructuredOutput || m.Capabilities.Unknown.StructuredOutput || !m.Capabilities.Unknown.ToolCall || !m.Capabilities.Reasoning {
				t.Fatalf("capability availability lost: %+v", m.Capabilities)
			}
			if m.CostCurrency != modelsdev.PricingCurrency || m.CostUnit != modelsdev.PricingUnit || m.Cost.UnknownRates.Input || !m.Cost.UnknownRates.Output ||
				m.Cost.InputAudio == nil || *m.Cost.InputAudio != 3 || m.Cost.ContextOver200K == nil || m.Cost.ContextOver200K.Input != 4 {
				t.Fatalf("price metadata lost: %+v", m.Cost)
			}
			if m.PriceSnapshot == nil || !m.PriceSnapshot.UnknownRates.UncachedInput || !m.PriceSnapshot.UnknownRates.Output {
				t.Fatalf("context-dependent price projected as known: %+v", m.PriceSnapshot)
			}
			_, _, limits, _, meta := catalog.ResolveTargetDetails(&target{provider: &provider{id: "openai"}, model: "m"})
			if limits == nil || limits.MaxInputTokens == nil || *limits.MaxInputTokens != 100 || meta == nil || meta.CanonicalModelID != "lab/m" {
				t.Fatalf("target metadata/input-only limit lost: %+v %+v", limits, meta)
			}
		})
	}
}

func TestCatalogUnknownPricesReachUsageLedger(t *testing.T) {
	for _, tt := range []struct {
		name, cost            string
		want                  float64
		partial, unknownInput bool
	}{
		{"missing cache rate", `"cost":{"input":0,"output":2,"cache_read":null}`, 0.00004, true, false},
		{"explicit free", `"cost":{"input":0,"output":0,"cache_read":0}`, 0, false, false},
		{"missing price object", `"description":"unpriced"`, 0, true, true},
		{"unselected context tier", `"cost":{"input":1,"output":2,"cache_read":0,"context_over_200k":{"input":3}}`, 0, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"openai":{"id":"openai","models":{"gpt-backup":{"id":"gpt-backup",%s}}}}`, tt.cost)
			}))
			defer srv.Close()
			extra := fmt.Sprintf("    catalog:\n      cache_dir: %q\n      url: %q\n      refresh: false\n      warmup: true", t.TempDir(), srv.URL)
			e := newGW(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, openaiOK, anthropicOK, cfgOpts{extra: extra})
			if response, body := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq); response.StatusCode != 200 {
				t.Fatalf("chat: %d %s", response.StatusCode, body)
			}
			entries, totals := usageOf(t, e)
			var found bool
			for _, entry := range entries {
				if entry.Row.Model != "gpt-backup" {
					continue
				}
				found = true
				p := entry.Row.Price
				if p == nil || p.Source != "models.dev" || p.AsOf.IsZero() || p.UnknownRates.UncachedInput != tt.unknownInput ||
					entry.CostPartial != tt.partial || entry.CostUSD == nil || *entry.CostUSD < tt.want-1e-12 || *entry.CostUSD > tt.want+1e-12 {
					t.Fatalf("usage price/cost: %+v", entry)
				}
			}
			if !found || len(totals) != 1 || totals[0].Calls != 1 || (totals[0].UnpricedCalls == 1) != tt.partial {
				t.Fatalf("aggregate completeness: found=%t totals=%+v", found, totals)
			}
		})
	}
}
