package llmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// Scopes the plugin's routes require. A caller key holding "llm-gateway.*"
// holds all three.
const (
	ScopeChat      = "llm-gateway.chat"
	ScopeEmbed     = "llm-gateway.embed"
	ScopeDecide    = "llm-gateway.decide"
	ScopeModelRead = "llm-gateway.models.read"
	ScopeUsageRead = "llm-gateway.usage.read"
	// ScopeUsageDetail reads every caller's ledger entries with their Detail (with capture on, the
	// request and response bodies). No collector holds it; usage.read alone sees only its own rows.
	ScopeUsageDetail  = "llm-gateway.usage.detail"
	ScopeCatalogWrite = "llm-gateway.catalog.write"
)

// Options are what Start needs besides the configuration.
type Options struct {
	// Transport overrides the upstream HTTP transport; tests inject one.
	Transport http.RoundTripper
	// Store is the database module's store. It holds the SQLite usage ledger (ledger.store: sqlite)
	// and the management state.
	Store *store.Store
	// Management enables the management routes; RunningRevision is the revision this process loaded.
	Management      *ManagementStore
	RunningRevision int64
}

// Service is the running gateway: the routes to serve and the stops to run after they drain.
type Service struct {
	routes []Route
	stops  []func(context.Context) error
}

// Routes are the HTTP routes to mount under RoutePrefix.
func (s *Service) Routes() []Route { return s.routes }

// Close runs the cleanup callbacks in reverse order, after the HTTP server has drained.
func (s *Service) Close(ctx context.Context) error {
	var errs []error
	for i := len(s.stops) - 1; i >= 0; i-- {
		errs = append(errs, s.stops[i](ctx))
	}
	return errors.Join(errs...)
}

func (s *Service) onStop(fn func(context.Context) error) { s.stops = append(s.stops, fn) }

// Start builds the gateway from a validated configuration: it resolves provider credentials,
// opens the ledger, warms and refreshes the catalog, and returns the routes. On error nothing is
// left running.
func Start(ctx context.Context, host Host, cfg Config, o Options) (svc *Service, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	svc = &Service{}
	built := svc // svc is nil in the caller's view on error, but the stops must still run
	defer func() {
		if err != nil {
			_ = built.Close(context.Background())
		}
	}()
	providers := map[string]*provider{}
	for _, pc := range cfg.Providers {
		if !enabled(pc.Enabled) {
			continue
		}
		key := ""
		if pc.APIKeyRef != "" {
			k, err := host.ResolveSecret(ctx, pc.APIKeyRef)
			if err != nil {
				return nil, fmt.Errorf("llm-gateway: provider %q: %w", pc.ID, err)
			}
			key = k
		} else if pc.APIKeyEnv != "" {
			key = os.Getenv(pc.APIKeyEnv)
			if key == "" {
				return nil, fmt.Errorf("llm-gateway: provider %q: environment variable %s is empty", pc.ID, pc.APIKeyEnv)
			}
		}
		prov := &provider{
			id: pc.ID, catalogProvider: pc.CatalogProvider, dialect: dialects[pc.Dialect],
			baseURL: normalizeBaseURL(pc.BaseURL), headers: pc.Headers, opencodeSession: pc.OpenCodeSession,
			apiKey: key, client: upstreamClient(o.Transport, pc.timeout()),
		}
		providers[pc.ID] = prov
		if pc.DiscoverModels {
			cfg.Models = applyModelDiscovery(ctx, cfg.Models, prov, host.Logger())
		}
	}
	if len(cfg.Models) == 0 {
		for _, pc := range cfg.Providers {
			if enabled(pc.Enabled) && pc.DiscoverModels {
				return nil, fmt.Errorf("llm-gateway: discovery returned no models")
			}
		}
	}
	ledgerStore, err := openStore(ctx, cfg.Ledger, host, o.Store)
	if err != nil {
		return nil, err
	}
	ledger := NewLedger(ledgerStore, host.Logger())
	svc.onStop(func(context.Context) error { ledger.Close(); return nil })
	gw, err := NewGateway(cfg, providers, ledger, host.Logger(), host.RedactScope())
	if err != nil {
		return nil, err
	}
	if o.Transport != nil {
		cat, err := NewCatalog(cfg.Catalog, o.Transport, host.Logger())
		if err != nil {
			return nil, err
		}
		gw.SetCatalog(cat)
	}
	if cfg.Catalog.Warmup {
		// The catalog client applies the configured refresh timeout.
		warmCtx, cancel := context.WithCancel(ctx)
		err := gw.Catalog().Refresh(warmCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("llm-gateway: catalog warmup: %w", err)
		}
	}
	if cfg.Catalog.shouldRefresh() && gw.Catalog() != nil {
		rctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			gw.Catalog().Run(rctx)
		}()
		svc.onStop(func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
			case <-stopCtx.Done():
			}
			return nil
		})
	}

	max := cfg.maxBody()
	svc.routes = []Route{
		{Method: "POST", Path: "/v1/chat/completions", Scope: ScopeChat, MaxBodyBytes: max, Handler: gw.Handler(openaiDialect{}, OpChat)},
		{Method: "POST", Path: "/v1/responses", Scope: ScopeChat, MaxBodyBytes: max, Handler: gw.Handler(openaiDialect{}, OpResponses)},
		{Method: "POST", Path: "/v1/messages", Scope: ScopeChat, MaxBodyBytes: max, Handler: gw.Handler(anthropicDialect{}, OpChat)},
		{Method: "POST", Path: "/v1/embeddings", Scope: ScopeEmbed, MaxBodyBytes: max, Handler: gw.Handler(openaiDialect{}, OpEmbed)},
		{Method: "POST", Path: "/v1/systemone", Scope: ScopeDecide, MaxBodyBytes: max, Handler: gw.Handler(jevDialect{}, OpDecide)},
		{Method: "GET", Path: "/v1/models", Scope: ScopeModelRead, Handler: gw.ModelsHandler()},
		{Method: "GET", Path: "/v1/catalog", Scope: ScopeModelRead, Handler: gw.CatalogHandler()},
		{Method: "POST", Path: "/v1/catalog/refresh", Scope: ScopeCatalogWrite, Handler: gw.CatalogRefreshHandler()},
		{Method: "POST", Path: "/v1/catalog/prices/reload", Scope: ScopeCatalogWrite, Handler: gw.CatalogPricesReloadHandler()},
		{Method: "GET", Path: "/v1/usage", Scope: ScopeUsageRead, Handler: gw.UsageHandler()},
		{Method: "GET", Path: "/v1/usage/export", Scope: ScopeUsageRead, Handler: gw.UsageExportHandler()},
	}
	for i := range svc.routes {
		svc.routes[i].Raw = true
	}
	if o.Management != nil {
		if o.RunningRevision <= 0 {
			return nil, fmt.Errorf("llm-gateway: management requires the loaded running revision")
		}
		svc.routes = append(svc.routes, ManagementRoutes(host.Logger(), o.Management, o.RunningRevision)...)
	}
	return svc, nil
}

// upstreamClient builds the HTTP client for one provider. There is no overall
// client timeout, because a stream legitimately runs long; the request context
// bounds the exchange and the header timeout bounds a silent upstream.
// Redirects are refused so an upstream cannot bounce the credential to
// another host.
func upstreamClient(rt http.RoundTripper, headerTimeout time.Duration) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = headerTimeout
		rt = t
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// openStore builds the configured ledger store. The durable store is wrapped
// for asynchronous unbudgeted usage; budget reservations and settlement bypass
// that queue and require synchronous database access.
func openStore(ctx context.Context, cfg LedgerConfig, host Host, st *store.Store) (Store, error) {
	if cfg.Store == "sqlite" {
		if st == nil {
			return nil, errors.New("llm-gateway: ledger.store sqlite needs the database module's store")
		}
		return newAsyncStore(&sqliteStore{st: st}, host.Logger()), nil
	}
	if cfg.Store != "postgres" {
		return newMemStore(cfg.History), nil
	}
	dsn, err := cfg.Postgres.resolveDSN(ctx, host.ResolveSecret)
	if err != nil {
		return nil, err
	}
	pg, err := openPGStoreMode(ctx, dsn, cfg.Postgres.schemaMode())
	if err != nil {
		return nil, err
	}
	return newAsyncStore(pg, host.Logger()), nil
}

// normalizeBaseURL appends /v1 to known vendor roots when an operator omits the
// version segment (for example copying base_url: https://opencode.ai/zen or
// https://opencode.ai/zen/go from Tether).
func normalizeBaseURL(raw string) string {
	u := strings.TrimRight(raw, "/")
	switch u {
	case "https://opencode.ai/zen",
		"https://opencode.ai/zen/go",
		"https://api.openai.com",
		"https://api.anthropic.com",
		"https://api.typesafe.ai":
		return u + "/v1"
	}
	return u
}

const (
	defaultDiscoveryTimeout = 5 * time.Second
	maxDiscoveryBody        = 8 << 20
)

func applyModelDiscovery(ctx context.Context, models []ModelConfig, p *provider, logger *slog.Logger) []ModelConfig {
	if p == nil {
		return models
	}
	dctx, cancel := context.WithTimeout(ctx, defaultDiscoveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return models
	}
	req.Header.Set("Accept", "application/json")
	if p.dialect != nil {
		p.dialect.Authorize(req, req, p.apiKey)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	client := p.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if logger != nil {
			logger.Warn("llm-gateway: discover_models failed", "provider", p.id, "err", err)
		}
		return models
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if logger != nil {
			logger.Warn("llm-gateway: discover_models unexpected status", "provider", p.id, "status", resp.StatusCode)
		}
		return models
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBody)).Decode(&body); err != nil {
		if logger != nil {
			logger.Warn("llm-gateway: discover_models decode failed", "provider", p.id, "err", err)
		}
		return models
	}

	kind := "chat"
	if p.dialect != nil && p.dialect.Name() == "jev" {
		kind = "decision"
	}
	out := append([]ModelConfig(nil), models...)
	byName := make(map[string]int, len(out))
	for i, m := range out {
		byName[m.Name] = i
	}
	added := 0
	for _, entry := range body.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || !idPattern.MatchString(id) {
			continue
		}
		// Skip JEV decision models on non-JEV chat providers (such as
		// opencode-zen's shared /v1/models listing).
		if kind != "decision" && strings.HasPrefix(id, "jev-") {
			continue
		}
		if idx, exists := byName[id]; exists {
			existingKind := out[idx].Kind
			if existingKind == "" {
				existingKind = "chat"
			}
			if existingKind != kind {
				continue
			}
			hasTarget := false
			for _, t := range out[idx].Targets {
				if t.Provider == p.id && t.Model == id {
					hasTarget = true
					break
				}
			}
			if !hasTarget {
				out[idx].Targets = append(out[idx].Targets, TargetConfig{Provider: p.id, Model: id})
				added++
			}
			continue
		}
		byName[id] = len(out)
		out = append(out, ModelConfig{
			Name:    id,
			Kind:    kind,
			Targets: []TargetConfig{{Provider: p.id, Model: id}},
		})
		added++
	}
	if added > 0 && logger != nil {
		logger.Info("llm-gateway: discover_models registered models", "provider", p.id, "added", added)
	}
	return out
}
