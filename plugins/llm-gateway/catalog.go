package llmgateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/libs/llm/core/costcalc"
	"github.com/hollis-labs/libs/llm/core/llmtypes"
	"github.com/hollis-labs/libs/llm/core/modelsdev"
	"github.com/hollis-labs/libs/llm/core/pricesource"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

const defaultCatalogTTL = 90 * time.Second

// Catalog centralizes model metadata, capabilities, limits, modalities, and
// pricing for the gateway using libs/llm/core/modelsdev and pricesource.
type Catalog struct {
	client     *modelsdev.Client
	ttl        time.Duration
	filePrices *pricesource.Table
	livePrices pricesource.Live
	mdPrices   costcalc.PriceSource
	chain      costcalc.PriceSource
	logger     *slog.Logger
}

// TargetCapabilities reports the effective capabilities of a target or model,
// combining models.dev metadata with any operator overrides in api.yaml.
type TargetCapabilities struct {
	Tools       bool `json:"tools"`
	Vision      bool `json:"vision"`
	Streaming   bool `json:"streaming"`
	Reasoning   bool `json:"reasoning,omitempty"`
	Attachment  bool `json:"attachment,omitempty"`
	Temperature bool `json:"temperature,omitempty"`
}

// ModelMetadata carries descriptive model fields from models.dev.
type ModelMetadata struct {
	Name            string `json:"name,omitempty"`
	Family          string `json:"family,omitempty"`
	OpenWeights     bool   `json:"open_weights,omitempty"`
	ReleaseDate     string `json:"release_date,omitempty"`
	KnowledgeCutoff string `json:"knowledge_cutoff,omitempty"`
	LastUpdated     string `json:"last_updated,omitempty"`
}

// CatalogModelEntry is one model entry returned by GET /v1/catalog.
type CatalogModelEntry struct {
	ProviderID      string                     `json:"provider_id"`
	ID              string                     `json:"id"`
	Name            string                     `json:"name"`
	Family          string                     `json:"family,omitempty"`
	OpenWeights     bool                       `json:"open_weights,omitempty"`
	ReleaseDate     string                     `json:"release_date,omitempty"`
	KnowledgeCutoff string                     `json:"knowledge_cutoff,omitempty"`
	LastUpdated     string                     `json:"last_updated,omitempty"`
	Cost            modelsdev.Pricing          `json:"cost"`
	PriceSnapshot   *usageledger.PriceSnapshot `json:"price_snapshot,omitempty"`
	Limit           modelsdev.Limits           `json:"limit"`
	Modalities      modelsdev.Modality         `json:"modalities"`
	Capabilities    modelsdev.Capabilities     `json:"capabilities"`
}

// CatalogProviderSummary describes a provider in GET /v1/catalog.
type CatalogProviderSummary struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	API        string   `json:"api,omitempty"`
	Doc        string   `json:"doc,omitempty"`
	Env        []string `json:"env,omitempty"`
	ModelCount int      `json:"model_count"`
}

// NewCatalog constructs a Catalog from config. It loads any configured price
// override file and initializes the models.dev client with the configured TTL
// (default 90s).
func NewCatalog(cfg CatalogConfig, rt http.RoundTripper, logger *slog.Logger) (*Catalog, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ttl, err := cfg.ttlDuration()
	if err != nil {
		return nil, err
	}
	var filePrices *pricesource.Table
	if cfg.PriceFile != "" {
		t, err := pricesource.LoadFile(cfg.PriceFile)
		if err != nil {
			return nil, fmt.Errorf("llm-gateway: catalog.price_file: %w", err)
		}
		filePrices = t
	}

	c := &Catalog{
		ttl:        ttl,
		filePrices: filePrices,
		logger:     logger,
	}

	opts := []modelsdev.Option{
		modelsdev.WithCacheTTL(ttl),
		modelsdev.WithOnRefresh(func(mc *modelsdev.Client) {
			if tbl, err := pricesource.FromCatalog(pricesource.SourceModelsDev, mc.LastFetchedAt(), mc.List()); err == nil {
				c.livePrices.Store(tbl)
			}
		}),
	}
	if cfg.CacheDir != "" {
		opts = append(opts, modelsdev.WithCacheDir(cfg.CacheDir))
	}
	if cfg.URL != "" {
		opts = append(opts, modelsdev.WithURL(cfg.URL))
	}
	if rt != nil {
		opts = append(opts, modelsdev.WithHTTPClient(&http.Client{
			Transport: rt,
			Timeout:   30 * time.Second,
		}))
	}

	c.client = modelsdev.New(opts...)
	c.mdPrices = pricesource.FromModelsDev(c.client)
	c.chain = pricesource.Chain(c.filePrices, &c.livePrices, c.mdPrices)
	return c, nil
}

// Run runs the blocking refresher loop until ctx is cancelled.
func (c *Catalog) Run(ctx context.Context) {
	if c == nil || c.client == nil {
		return
	}
	c.client.Run(ctx)
}

// Refresh forces an immediate fetch from the upstream models.dev endpoint and
// updates both the on-disk cache and the in-memory price source.
func (c *Catalog) Refresh(ctx context.Context) error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Refresh(ctx)
}

// TTL returns the configured cache TTL (default 90s).
func (c *Catalog) TTL() time.Duration {
	if c == nil || c.ttl <= 0 {
		return defaultCatalogTTL
	}
	return c.ttl
}

// LastFetchedAt returns when the models.dev catalog was last fetched.
func (c *Catalog) LastFetchedAt() time.Time {
	if c == nil || c.client == nil {
		return time.Time{}
	}
	return c.client.LastFetchedAt()
}

func inferCatalogProvider(providerID string) string {
	id := strings.ToLower(providerID)
	switch {
	case strings.HasPrefix(id, "opencode-go"):
		return "opencode-go"
	case strings.HasPrefix(id, "opencode"):
		return "opencode"
	case strings.HasPrefix(id, "openrouter"):
		return "openrouter"
	case strings.HasPrefix(id, "huggingface"):
		return "huggingface"
	case strings.HasPrefix(id, "gemini"):
		return "google"
	case strings.HasPrefix(id, "openai"):
		return "openai"
	case strings.HasPrefix(id, "anthropic"):
		return "anthropic"
	}
	return ""
}

func catalogProviderCandidates(catalogProvider, providerID, dialectName string) []string {
	return []string{catalogProvider, providerID, inferCatalogProvider(providerID), dialectName}
}

// LookupModel looks up a model in the models.dev catalog. It checks any
// explicit catalog_provider first, then the configured provider ID (and any
// inferred vendor family such as "opencode" for "opencode-zen"), and falls back
// to the provider's dialect name.
func (c *Catalog) LookupModel(catalogProvider, providerID, dialectName, modelID string) (modelsdev.Model, string, bool) {
	if c == nil || c.client == nil {
		return modelsdev.Model{}, "", false
	}
	for _, pid := range catalogProviderCandidates(catalogProvider, providerID, dialectName) {
		if pid == "" {
			continue
		}
		if m, ok := c.client.Get(pid, modelID); ok {
			return m, pid, true
		}
	}
	return modelsdev.Model{}, "", false
}

// ResolvePrice returns the PriceSnapshot to use for a target:
//  1. Explicit price configured on the target in api.yaml (highest priority).
//  2. Configured catalog.price_file override table.
//  3. Live/cached models.dev catalog via pricesource.
func (c *Catalog) ResolvePrice(t *target) *usageledger.PriceSnapshot {
	if t == nil {
		return nil
	}
	if t.price != nil {
		return t.price
	}
	if c == nil || c.chain == nil {
		return nil
	}
	var catProv, provID, dialectName string
	if t.provider != nil {
		catProv = t.provider.catalogProvider
		provID = t.provider.id
		if t.provider.dialect != nil {
			dialectName = t.provider.dialect.Name()
		}
	}
	for _, pid := range catalogProviderCandidates(catProv, provID, dialectName) {
		if pid == "" {
			continue
		}
		if snap, ok := c.chain.Snapshot(pid, t.model); ok {
			return &snap
		}
	}
	return nil
}

// ResolveTargetDetails resolves a target's effective ProviderCapabilities (for
// routing), TargetCapabilities, Limits, Modalities, and ModelMetadata by
// merging models.dev catalog data with any explicit operator overrides.
func (c *Catalog) ResolveTargetDetails(t *target) (
	provCaps llmtypes.ProviderCapabilities,
	viewCaps TargetCapabilities,
	limits *modelsdev.Limits,
	modalities *modelsdev.Modality,
	meta *ModelMetadata,
) {
	if t == nil {
		return llmtypes.ProviderCapabilities{}, TargetCapabilities{}, nil, nil, nil
	}
	var catProv, provID, dialectName string
	if t.provider != nil {
		catProv = t.provider.catalogProvider
		provID = t.provider.id
		if t.provider.dialect != nil {
			dialectName = t.provider.dialect.Name()
		}
	}

	// Start with permissive defaults for uncataloged models.
	tools := true
	vision := true
	streaming := true
	reasoning := false
	attachment := false
	temperature := false

	if md, _, ok := c.LookupModel(catProv, provID, dialectName, t.model); ok {
		tools = md.Capabilities.ToolCall
		vision = hasVisionModality(md)
		reasoning = md.Capabilities.Reasoning
		attachment = md.Capabilities.Attachment
		temperature = md.Capabilities.Temperature
		if md.Limit.ContextWindow > 0 || md.Limit.MaxOutputTokens > 0 {
			l := md.Limit
			limits = &l
		}
		if len(md.Modality.Input) > 0 || len(md.Modality.Output) > 0 {
			m := md.Modality
			modalities = &m
		}
		if md.Name != "" || md.Family != "" || md.ReleaseDate != "" || md.KnowledgeCutoff != "" || md.OpenWeights {
			meta = &ModelMetadata{
				Name:            md.Name,
				Family:          md.Family,
				OpenWeights:     md.OpenWeights,
				ReleaseDate:     md.ReleaseDate,
				KnowledgeCutoff: md.KnowledgeCutoff,
				LastUpdated:     md.LastUpdated,
			}
		}
	}

	// Explicit operator overrides in api.yaml always win.
	if t.capsCfg.Tools != nil {
		tools = *t.capsCfg.Tools
	}
	if t.capsCfg.Vision != nil {
		vision = *t.capsCfg.Vision
	}
	if t.capsCfg.Streaming != nil {
		streaming = *t.capsCfg.Streaming
	}
	if t.capsCfg.Reasoning != nil {
		reasoning = *t.capsCfg.Reasoning
	}

	provCaps = llmtypes.ProviderCapabilities{
		SupportsToolCalling: tools,
		SupportsImageInput:  vision,
		SupportsStreamJSON:  streaming,
	}
	if limits != nil {
		provCaps.ContextWindowSize = limits.ContextWindow
		provCaps.MaxTokens = limits.MaxOutputTokens
	}
	viewCaps = TargetCapabilities{
		Tools:       tools,
		Vision:      vision,
		Streaming:   streaming,
		Reasoning:   reasoning,
		Attachment:  attachment,
		Temperature: temperature,
	}
	return provCaps, viewCaps, limits, modalities, meta
}

func hasVisionModality(m modelsdev.Model) bool {
	for _, in := range m.Modality.Input {
		switch strings.ToLower(in) {
		case "image", "video", "pdf":
			return true
		}
	}
	return m.Capabilities.Attachment
}

// ListModels returns catalog entries filtered by optional provider, model, and
// family query parameters.
func (c *Catalog) ListModels(providerFilter, modelFilter, familyFilter string) []CatalogModelEntry {
	if c == nil || c.client == nil {
		return nil
	}
	refs := c.client.List()
	out := make([]CatalogModelEntry, 0, len(refs))
	for _, r := range refs {
		if providerFilter != "" && !strings.EqualFold(r.ProviderID, providerFilter) {
			continue
		}
		if modelFilter != "" && !strings.EqualFold(r.ID, modelFilter) && !strings.EqualFold(r.Name, modelFilter) {
			continue
		}
		if familyFilter != "" && !strings.EqualFold(r.Family, familyFilter) {
			continue
		}
		var snapPtr *usageledger.PriceSnapshot
		if c.chain != nil {
			if snap, ok := c.chain.Snapshot(r.ProviderID, r.ID); ok {
				snapPtr = &snap
			}
		}
		out = append(out, CatalogModelEntry{
			ProviderID:      r.ProviderID,
			ID:              r.ID,
			Name:            r.Name,
			Family:          r.Family,
			OpenWeights:     r.OpenWeights,
			ReleaseDate:     r.ReleaseDate,
			KnowledgeCutoff: r.KnowledgeCutoff,
			LastUpdated:     r.LastUpdated,
			Cost:            r.Cost,
			PriceSnapshot:   snapPtr,
			Limit:           r.Limit,
			Modalities:      r.Modality,
			Capabilities:    r.Capabilities,
		})
	}
	return out
}

// ListProviders returns summaries of all providers in the catalog, optionally
// filtered by provider ID.
func (c *Catalog) ListProviders(providerFilter string) []CatalogProviderSummary {
	if c == nil || c.client == nil {
		return nil
	}
	ps := c.client.ListProviders()
	out := make([]CatalogProviderSummary, 0, len(ps))
	for _, p := range ps {
		if providerFilter != "" && !strings.EqualFold(p.ID, providerFilter) {
			continue
		}
		out = append(out, CatalogProviderSummary{
			ID:         p.ID,
			Name:       p.Name,
			API:        p.API,
			Doc:        p.Doc,
			Env:        slices.Clone(p.Env),
			ModelCount: len(p.Models),
		})
	}
	return out
}
