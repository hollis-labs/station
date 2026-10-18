package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/pricesource"
)

const syncCatalog = `{"openai":{"id":"openai","models":{"m":{"id":"m","cost":{"input":1}}}}}`

func awaitCatalog(t *testing.T, c *Catalog, accept func(CatalogStatus) bool) CatalogStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := c.Status()
		if accept(s) {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("catalog did not reach expected state: %+v", c.Status().Sync)
	return CatalogStatus{}
}

func TestCatalogRecurringFailureRecoveryAndShutdown(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(401)
			io.WriteString(w, "secret-upstream-body")
			return
		}
		io.WriteString(w, syncCatalog)
	}))
	defer srv.Close()
	c, err := NewCatalog(CatalogConfig{URL: srv.URL + "?key=private", CacheDir: t.TempDir(), TTL: "20ms", RetryMin: "30ms", RetryMax: "60ms"}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := c.LastFetchedAt()
	fail.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	s := awaitCatalog(t, c, func(s CatalogStatus) bool { return s.Sync.ConsecutiveFailures >= 2 && !s.Sync.Refreshing })
	if !s.Sync.Running || s.State != "stale" || s.Sync.HTTPStatus != 401 || s.Sync.NextAttempt.Sub(s.Sync.LastFinished) != 60*time.Millisecond || !s.FetchedAt.Equal(first) {
		t.Fatalf("failure/backoff health: %+v", s.Sync)
	}
	encoded, _ := json.Marshal(s)
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret-upstream-body") || strings.Contains(string(encoded), srv.URL) {
		t.Fatalf("unsafe health: %s", encoded)
	}
	if p := c.ResolvePrice(&target{provider: &provider{id: "openai"}, model: "m"}); p == nil || p.InputPerMillion != 1 || !p.AsOf.Equal(first) {
		t.Fatalf("last good price lost: %+v", p)
	}
	fail.Store(false)
	awaitCatalog(t, c, func(s CatalogStatus) bool { return s.Sync.ConsecutiveFailures == 0 && s.Sync.LastSuccess.After(first) })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop")
	}
	if s := c.Status(); s.Sync.Running || s.Sync.Refreshing {
		t.Fatalf("worker still active: %+v", s.Sync)
	}
}

func TestCatalogFetchControlsKeepLastGood(t *testing.T) {
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			io.WriteString(w, strings.Repeat("x", 1025))
		case 2:
			<-r.Context().Done()
		default:
			io.WriteString(w, syncCatalog)
		}
	}))
	defer srv.Close()
	c, err := NewCatalog(CatalogConfig{URL: srv.URL, CacheDir: t.TempDir(), RefreshTimeout: "30ms", MaxResponseBytes: 1024}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := c.LastFetchedAt()
	for _, m := range []int32{1, 2} {
		mode.Store(m)
		if err := c.Refresh(t.Context()); err == nil {
			t.Fatal("invalid fetch succeeded")
		}
		if s := c.Status(); s.Sync.LastError == "" || !s.FetchedAt.Equal(first) || len(c.ListModels("", "", "")) != 1 {
			t.Fatalf("last good snapshot lost: %+v", s)
		}
	}
}

func TestCatalogStopsInFlightFetchAndSeparatesSources(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	dir := t.TempDir()
	c, err := NewCatalog(CatalogConfig{URL: srv.URL, CacheDir: dir}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fetch did not cancel")
	}
	if s := c.Status(); s.Sync.Running || s.Sync.Refreshing || s.Sync.LastError != "canceled" {
		t.Fatalf("shutdown health: %+v", s.Sync)
	}

	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, syncCatalog) }))
	defer valid.Close()
	cfg := CatalogConfig{URL: valid.URL + "?source=one", CacheDir: dir}
	first, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg.URL = valid.URL + "?source=two"
	other, err := NewCatalog(cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s := other.Status(); s.State != "empty" || s.Sync.SourceID == first.Status().Sync.SourceID || len(other.ListModels("", "", "")) != 0 {
		t.Fatalf("source identity mixed: %+v", s)
	}
}

func TestCatalogRefreshConfigValidationAndPersistence(t *testing.T) {
	for _, cfg := range []CatalogConfig{
		{RefreshTimeout: "0s"}, {RefreshTimeout: "bad"}, {RetryMin: "-1s"}, {RetryMax: "0s"},
		{RetryMin: "2m"}, {RetryMin: "2s", RetryMax: "1s"}, {MaxResponseBytes: -1}, {MaxResponseBytes: 1<<30 + 1},
	} {
		if err := cfg.validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", cfg)
		}
		if _, err := NewCatalog(cfg, nil, quiet); err == nil {
			t.Fatal("constructor skipped validation")
		}
	}
	cfg := CatalogConfig{RefreshTimeout: "2s", MaxResponseBytes: 2048, RetryMin: "20ms", RetryMax: "1s"}
	path := filepath.Join(managementTempDir(t), "gateway.db")
	s, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	doc := managementFixture()
	doc.Gateway.Catalog = cfg
	r, err := s.Save(t.Context(), doc, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(t.Context(), r.ID, 0, "operator"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Active(t.Context())
	if err != nil || got.Document.Gateway.Catalog != cfg {
		t.Fatalf("refresh config lost: %+v %v", got, err)
	}
}

func saveTestPrices(t *testing.T, path string, rate float64) {
	t.Helper()
	table, err := pricesource.NewTable("operator", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), pricesource.Entry{Provider: "openai", Model: "m", Input: pricesource.Rate(rate)})
	if err != nil {
		t.Fatal(err)
	}
	if err := pricesource.SaveFile(path, table); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPriceReloadLastGoodAndConcurrentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-price-file.json")
	saveTestPrices(t, path, 2)
	c, err := NewCatalog(CatalogConfig{PriceFile: path, CacheDir: t.TempDir()}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	target := &target{provider: &provider{id: "openai"}, model: "m"}
	first := c.Status().PriceFile
	for _, raw := range []string{`null`, `{}`, `{"entries":null}`, `{secret`, `{"entries":[]} {}`, `{"entries":[{"provider":"openai","model":"m","input_per_million":-1}]}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.ReloadPrices(t.Context()); err == nil || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid reload: %v", err)
		}
		s := c.Status().PriceFile
		if s.State != "loaded" || s.LastError != "invalid_price_file" || !s.LastSuccess.Equal(first.LastSuccess) || c.ResolvePrice(target).InputPerMillion != 2 {
			t.Fatalf("last good override lost: %+v", s)
		}
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				p := c.ResolvePrice(target)
				if p == nil || (p.InputPerMillion != 2 && p.InputPerMillion != 3) || p.Source != "operator" {
					t.Errorf("invalid concurrent price: %+v", p)
					return
				}
				_ = c.Status()
			}
		})
	}
	for i := 0; i < 10; i++ {
		saveTestPrices(t, path, float64(2+i%2))
		if err := c.ReloadPrices(t.Context()); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	wg.Wait()
	if s := c.Status().PriceFile; s.LastError != "" || s.ConsecutiveFailures != 0 || s.Entries != 1 || !s.LastSuccess.After(first.LastSuccess) {
		t.Fatalf("recovery health: %+v", s)
	}
	// A queued canceled reload must not publish or count as an acquired attempt.
	c.filePrices.gate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.ReloadPrices(ctx); err == nil {
		t.Fatal("canceled reload succeeded")
	}
	<-c.filePrices.gate
	// A deliberately empty valid table removes overrides.
	if err := os.WriteFile(path, []byte(`{"name":"operator","entries":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.ReloadPrices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.filePrices.Snapshot("openai", "m"); ok {
		t.Fatal("empty table did not clear overrides")
	}
}

func TestCatalogPriceReloadHTTPScopesAndSourceIndependence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	saveTestPrices(t, path, 2)
	extra := fmt.Sprintf("    catalog:\n      price_file: %q\n      cache_dir: %q\n      refresh: false", path, t.TempDir())
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{extra: extra})
	endpoint := "/v1/catalog/prices/reload"
	for _, tt := range []struct {
		auth   map[string]string
		status int
	}{
		{map[string]string{}, 401}, {map[string]string{"Authorization": "Bearer narrow"}, 403},
	} {
		if r, _ := e.do(t, "POST", endpoint, tt.auth, ""); r.StatusCode != tt.status {
			t.Fatalf("scope status: %d", r.StatusCode)
		}
	}
	saveTestPrices(t, path, 3)
	r, body := e.do(t, "POST", endpoint, oaAuth, "")
	var response struct {
		Status CatalogStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil || r.StatusCode != 200 || response.Status.PriceFile.Attempts != 2 || response.Status.Sync.Attempts != 0 {
		t.Fatalf("reload fetched upstream or unauthorized request reloaded: %s %v", body, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r, body = e.do(t, "POST", endpoint, oaAuth, "")
	if r.StatusCode != 422 || strings.Contains(body, path) || !strings.Contains(body, `"last_error":"file_read"`) || !strings.Contains(body, `"state":"loaded"`) {
		t.Fatalf("failure status: %s", body)
	}
}

func TestCatalogPriceFileInputBoundsAndDisabledReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxPriceFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, bad := range []string{dir, path} {
		if _, err := NewCatalog(CatalogConfig{PriceFile: bad, CacheDir: t.TempDir()}, nil, quiet); err == nil {
			t.Fatal("invalid startup file accepted")
		}
	}
	c, err := NewCatalog(CatalogConfig{CacheDir: t.TempDir()}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	(&Gateway{catalog: c}).CatalogPricesReloadHandler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/catalog/prices/reload", nil))
	if w.Code != 409 || c.Status().PriceFile.State != "disabled" {
		t.Fatalf("unconfigured reload: %d %s", w.Code, w.Body.String())
	}
}
