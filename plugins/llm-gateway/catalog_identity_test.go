package llmgateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/pricesource"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

const identityCatalog = `{
  "openai": {"id":"openai", "models":{
    "shared":{"id":"shared","name":"Lab offering","cost":{"input":1,"output":2},"tool_call":true},
    "lab-only":{"id":"lab-only","name":"Lab only","cost":{"input":1,"output":2}}
  }},
  "openai-work": {"id":"openai-work", "models":{
    "shared":{"id":"shared","name":"Account offering","cost":{"input":20,"output":40}}
  }},
  "openrouter": {"id":"openrouter", "models":{
    "shared":{"id":"shared","name":"Router offering","cost":{"input":10,"output":20},"tool_call":false}
  }},
  "opencode": {"id":"opencode", "models":{
    "shared":{"id":"shared","name":"Zen offering","cost":{"input":30,"output":60}}
  }}
}`

func TestCatalogProviderIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, identityCatalog)
	}))
	defer srv.Close()
	c, err := NewCatalog(CatalogConfig{URL: srv.URL, CacheDir: t.TempDir()}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, provider, mapping, model, matched, display string
		input                                            float64
	}{
		{"local wire compatibility is not commercial identity", "llama-local", "", "shared", "", "", 0},
		{"explicit router", "custom", "openrouter", "shared", "openrouter", "Router offering", 10},
		{"missing explicit mapping", "openai", "missing", "shared", "", "", 0},
		{"missing model under explicit mapping", "openai", "openrouter", "lab-only", "", "", 0},
		{"Go does not inherit Zen", "opencode-go-claude", "", "shared", "", "", 0},
		{"exact provider wins over inferred family", "openai-work", "", "shared", "openai-work", "Account offering", 20},
		{"conventional provider alias", "openai-other", "", "shared", "openai", "Lab offering", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := &target{provider: &provider{id: tt.provider, catalogProvider: tt.mapping, dialect: openaiDialect{}}, model: tt.model}
			_, id, found := c.LookupModel(tt.mapping, tt.provider, tt.model)
			price := c.ResolvePrice(target)
			_, _, _, _, metadata := c.ResolveTargetDetails(target)
			if tt.matched == "" {
				if found || id != "" || price != nil || metadata != nil {
					t.Fatalf("borrowed another provider's data: %q %+v %+v", id, price, metadata)
				}
				return
			}
			if !found || id != tt.matched || price == nil || price.InputPerMillion != tt.input || metadata == nil || metadata.Name != tt.display {
				t.Fatalf("incorrect provider data: %q %+v %+v", id, price, metadata)
			}
		})
	}
	// An operator-supplied price remains usable for uncataloged local models.
	explicit := &usageledger.PriceSnapshot{InputPerMillion: 7, Source: "operator"}
	if got := c.ResolvePrice(&target{model: "shared", price: explicit}); got != explicit {
		t.Fatalf("explicit price lost: %+v", got)
	}
}

func TestCatalogOverridePrecedenceAcrossIdentities(t *testing.T) {
	dir := t.TempDir()
	table, err := pricesource.NewTable("operator", time.Now(),
		pricesource.Entry{Provider: "openai", Model: "shared", Input: pricesource.Rate(2)},
		pricesource.Entry{Provider: "custom", Model: "shared", Input: pricesource.Rate(4)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "prices.json")
	if err := pricesource.SaveFile(path, table); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, identityCatalog) }))
	defer srv.Close()
	c, err := NewCatalog(CatalogConfig{URL: srv.URL, CacheDir: dir, PriceFile: path}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		id, mapping string
		input       float64
	}{
		{"openai-work", "", 2},      // mapped file override beats exact-ID catalog rate of 20
		{"custom", "openrouter", 4}, // local account override beats mapped catalog rate of 10
	} {
		p := c.ResolvePrice(&target{provider: &provider{id: tt.id, catalogProvider: tt.mapping}, model: "shared"})
		if p == nil || p.Source != "operator" || p.InputPerMillion != tt.input {
			t.Fatalf("override precedence for %s: %+v", tt.id, p)
		}
	}
}

func TestCatalogFreshnessAndCachedRestart(t *testing.T) {
	var bad atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bad.Load() {
			io.WriteString(w, `invalid JSON`)
			return
		}
		io.WriteString(w, sampleModelsDevJSON)
	}))
	defer srv.Close()
	cfg := CatalogConfig{URL: srv.URL, CacheDir: t.TempDir(), TTL: "1h", Refresh: new(bool)}
	c, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Status(); s.State != "empty" || s.FetchedAt != nil || s.RefreshEnabled {
		t.Fatalf("cold status: %+v", s)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := c.Status()
	if s.State != "fresh" || s.FetchedAt == nil || s.AgeSeconds == nil || s.StaleAfter == nil || s.RefreshEnabled {
		t.Fatalf("fresh status: %+v", s)
	}
	if stale := c.statusAt(*s.StaleAfter); stale.State != "stale" {
		t.Fatalf("expired status: %+v", stale)
	}
	// Failed fetches preserve the last good snapshot and its age, not a fresh
	// timestamp claiming the failure was a successful update.
	bad.Store(true)
	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("invalid refresh succeeded")
	}
	if after := c.Status(); !after.FetchedAt.Equal(*s.FetchedAt) || len(c.ListModels("", "", "")) == 0 {
		t.Fatalf("failed fetch replaced cache: %+v", after)
	}
	restarted, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	// The very first HTTP read must include the persisted timestamp, even
	// though the library lazily hydrates the cache on its first data lookup.
	w := httptest.NewRecorder()
	(&Gateway{catalog: restarted}).CatalogHandler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/catalog", nil))
	var response struct {
		Version   int           `json:"schema_version"`
		FetchedAt string        `json:"fetched_at"`
		Status    CatalogStatus `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Version != 1 || response.FetchedAt == "" || response.Status.State != "fresh" {
		t.Fatalf("first cached HTTP read: %v %s", err, w.Body.String())
	}
	if response.Status.Source != "models.dev" || response.FetchedAt != response.Status.FetchedAt.Format(time.RFC3339) {
		t.Fatalf("missing cache provenance: %+v", response)
	}
	// A subsequent successful fetch is visible to both metadata and pricing
	// readers; they must not stay bound to the earlier on-disk snapshot.
	updated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.ReplaceAll(sampleModelsDevJSON, `"input": 0.15`, `"input": 9.0`))
	}))
	defer updated.Close()
	cfg.URL = updated.URL
	next, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	price := next.ResolvePrice(&target{provider: &provider{id: "openai"}, model: "gpt-backup"})
	if price == nil || price.InputPerMillion != 9 || !next.LastFetchedAt().After(*s.FetchedAt) {
		t.Fatalf("refreshed price/timestamp not published: %+v", price)
	}
}
