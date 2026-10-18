package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hollis-labs/station/platform"
)

// Scopes the plugin's routes require. A caller key holding "llm-gateway.*"
// holds all three.
const (
	ScopeChat         = "llm-gateway.chat"
	ScopeEmbed        = "llm-gateway.embed"
	ScopeDecide       = "llm-gateway.decide"
	ScopeModelRead    = "llm-gateway.models.read"
	ScopeUsageRead    = "llm-gateway.usage.read"
	ScopeCatalogWrite = "llm-gateway.catalog.write"
)

// Plugin is the llm-gateway platform plugin.
type Plugin struct {
	// transport overrides the upstream HTTP transport; tests inject one.
	transport       http.RoundTripper
	managementPath  string
	runningRevision int64
}

// New returns the plugin.
func New() *Plugin { return &Plugin{} }

// WithTransport returns a plugin whose upstream calls use rt.
func WithTransport(rt http.RoundTripper) *Plugin { return &Plugin{transport: rt} }

// Name implements platform.Plugin.
func (*Plugin) Name() string { return "llm-gateway" }

// Register implements platform.Plugin.
func (p *Plugin) Register(ctx context.Context, reg *platform.PluginRegistry, decode platform.ConfigDecoder) error {
	var cfg Config
	if err := decode(&cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	providers := map[string]*provider{}
	for _, pc := range cfg.Providers {
		if !enabled(pc.Enabled) {
			continue
		}
		key := ""
		if pc.APIKeyRef != "" {
			k, err := reg.ResolveSecret(ctx, pc.APIKeyRef)
			if err != nil {
				return fmt.Errorf("llm-gateway: provider %q: %w", pc.ID, err)
			}
			key = k
		} else if pc.APIKeyEnv != "" {
			key = os.Getenv(pc.APIKeyEnv)
			if key == "" {
				return fmt.Errorf("llm-gateway: provider %q: environment variable %s is empty", pc.ID, pc.APIKeyEnv)
			}
		}
		prov := &provider{
			id: pc.ID, catalogProvider: pc.CatalogProvider, dialect: dialects[pc.Dialect],
			baseURL: normalizeBaseURL(pc.BaseURL), headers: pc.Headers, opencodeSession: pc.OpenCodeSession,
			apiKey: key, client: upstreamClient(p.transport, pc.timeout()),
		}
		providers[pc.ID] = prov
		if pc.DiscoverModels {
			cfg.Models = applyModelDiscovery(ctx, cfg.Models, prov, reg.Logger())
		}
	}
	if len(cfg.Models) == 0 {
		for _, pc := range cfg.Providers {
			if enabled(pc.Enabled) && pc.DiscoverModels {
				return fmt.Errorf("llm-gateway: discovery returned no models")
			}
		}
	}
	store, err := openStore(ctx, cfg.Ledger, reg)
	if err != nil {
		return err
	}
	ledger := NewLedger(store, reg.Logger())
	reg.OnStop(func(context.Context) error { ledger.Close(); return nil })
	gw, err := NewGateway(cfg, providers, ledger, reg.Logger(), reg.RedactScope())
	if err != nil {
		ledger.Close()
		return err
	}
	if p.transport != nil {
		cat, err := NewCatalog(cfg.Catalog, p.transport, reg.Logger())
		if err != nil {
			ledger.Close()
			return err
		}
		gw.SetCatalog(cat)
	}
	if cfg.Catalog.Warmup {
		// The catalog client applies the configured refresh timeout.
		warmCtx, cancel := context.WithCancel(ctx)
		err := gw.Catalog().Refresh(warmCtx)
		cancel()
		if err != nil {
			ledger.Close()
			return fmt.Errorf("llm-gateway: catalog warmup: %w", err)
		}
	}
	if cfg.Catalog.shouldRefresh() && gw.Catalog() != nil {
		rctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			gw.Catalog().Run(rctx)
		}()
		reg.OnStop(func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
			case <-stopCtx.Done():
			}
			return nil
		})
	}

	// Anthropic clients authenticate with x-api-key; Station's auth guard reads
	// Authorization. This maps one to the other for this plugin's routes only.
	// It only moves a header, and the closed-failing auth guard runs next, so a
	// request that is not mapped is still refused there.
	prefix := reg.Prefix() + "/"
	if err := reg.UseMiddleware(platform.Middleware{
		Name: "x-api-key-bridge", Phase: platform.PhaseAuthn - 10, Failure: platform.FailOpen,
		Reason: "only translates a header; the fail-closed authn middleware runs immediately after",
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isManagement := strings.HasPrefix(r.URL.Path, prefix+"v1/management/")
				if isManagement {
					w.Header().Set("Cache-Control", "no-store")
					w.Header().Set("X-Content-Type-Options", "nosniff")
				}
				if !isManagement && strings.HasPrefix(r.URL.Path, prefix) && r.Header.Get("Authorization") == "" {
					if k := r.Header.Get("X-Api-Key"); k != "" {
						r.Header.Set("Authorization", "Bearer "+k)
					}
				}
				r.Header.Del("X-Api-Key")
				next.ServeHTTP(w, r)
			})
		},
	}); err != nil {
		return err
	}

	max := cfg.maxBody()
	for _, rt := range []platform.Route{
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
	} {
		if err := reg.Mount(rt); err != nil {
			return err
		}
	}
	if p.managementPath != "" {
		if p.runningRevision <= 0 {
			return fmt.Errorf("llm-gateway: management requires the loaded running revision")
		}
		if _, err := os.Stat(p.managementPath); err != nil {
			return err
		}
		management, err := OpenManagementStore(ctx, p.managementPath)
		if err != nil {
			return err
		}
		reg.OnStop(func(context.Context) error { return management.Close() })
		if err := p.registerManagement(reg, management); err != nil {
			return err
		}
	}
	return nil
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
// so the database is never on the request path.
func openStore(ctx context.Context, cfg LedgerConfig, reg *platform.PluginRegistry) (Store, error) {
	if cfg.Store == "sqlite" {
		s, err := openSQLiteStore(ctx, cfg.SQLite.Path)
		if err != nil {
			return nil, err
		}
		return newAsyncStore(s, reg.Logger()), nil
	}
	if cfg.Store != "postgres" {
		return newMemStore(cfg.History), nil
	}
	dsn := ""
	if cfg.Postgres.DSNRef != "" {
		d, err := reg.ResolveSecret(ctx, cfg.Postgres.DSNRef)
		if err != nil {
			return nil, fmt.Errorf("llm-gateway: ledger: %w", err)
		}
		dsn = d
	} else {
		dsn = os.Getenv(cfg.Postgres.DSNEnv)
		if dsn == "" {
			return nil, fmt.Errorf("llm-gateway: ledger: environment variable %s is empty", cfg.Postgres.DSNEnv)
		}
	}
	pg, err := openPGStore(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return newAsyncStore(pg, reg.Logger()), nil
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
	defer resp.Body.Close()
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
