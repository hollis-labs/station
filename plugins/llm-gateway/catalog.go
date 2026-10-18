package llmgateway

import (
	"context"
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
	client         *modelsdev.Client
	ttl            time.Duration
	refreshEnabled bool
	filePrices     *filePriceSource
	mdPrices       costcalc.PriceSource
	chain          costcalc.PriceSource
	logger         *slog.Logger
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
	Description      string `json:"description,omitempty"`
	CanonicalModelID string `json:"canonical_model_id,omitempty"`
	Type             string `json:"type,omitempty"`
	Status           string `json:"status,omitempty"`
	Name             string `json:"name,omitempty"`
	Family           string `json:"family,omitempty"`
	OpenWeights      bool   `json:"open_weights,omitempty"`
	ReleaseDate      string `json:"release_date,omitempty"`
	KnowledgeCutoff  string `json:"knowledge_cutoff,omitempty"`
	LastUpdated      string `json:"last_updated,omitempty"`
}

// CatalogModelEntry is one model entry returned by GET /v1/catalog.
type CatalogModelEntry struct {
	Description      string `json:"description,omitempty"`
	CanonicalModelID string `json:"canonical_model_id,omitempty"`
	Type             string `json:"type,omitempty"`
	Status           string `json:"status,omitempty"`
	// ProviderMetadata is descriptive upstream data, never applied to requests.
	ProviderMetadata *modelsdev.ModelProvider    `json:"provider_metadata,omitempty"`
	ReasoningOptions []modelsdev.ReasoningOption `json:"reasoning_options,omitempty"`
	Interleaved      *modelsdev.Interleaved      `json:"interleaved,omitempty"`
	Experimental     *modelsdev.Experimental     `json:"experimental,omitempty"`
	CostCurrency     string                      `json:"cost_currency"`
	CostUnit         string                      `json:"cost_unit"`
	ProviderID       string                      `json:"provider_id"`
	ID               string                      `json:"id"`
	Name             string                      `json:"name"`
	Family           string                      `json:"family,omitempty"`
	OpenWeights      bool                        `json:"open_weights,omitempty"`
	ReleaseDate      string                      `json:"release_date,omitempty"`
	KnowledgeCutoff  string                      `json:"knowledge_cutoff,omitempty"`
	LastUpdated      string                      `json:"last_updated,omitempty"`
	Cost             modelsdev.Pricing           `json:"cost"`
	PriceSnapshot    *usageledger.PriceSnapshot  `json:"price_snapshot,omitempty"`
	Limit            modelsdev.Limits            `json:"limit"`
	Modalities       modelsdev.Modality          `json:"modalities"`
	Capabilities     modelsdev.Capabilities      `json:"capabilities"`
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
	settings, err := cfg.refreshSettings()
	if err != nil {
		return nil, err
	}
	filePrices := newFilePriceSource(cfg.PriceFile)
	if cfg.PriceFile != "" {
		if err := filePrices.Reload(context.Background()); err != nil {
			return nil, err
		}
	}

	c := &Catalog{
		ttl:            ttl,
		refreshEnabled: cfg.shouldRefresh(),
		filePrices:     filePrices,
		logger:         logger,
	}

	opts := []modelsdev.Option{
		modelsdev.WithCacheTTL(ttl),
		modelsdev.WithRefreshTimeout(settings.timeout),
		modelsdev.WithMaxResponseBytes(settings.maxBytes),
		modelsdev.WithRetryBackoff(settings.retryMin, settings.retryMax),
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
			Timeout:   settings.timeout,
		}))
	}

	c.client = modelsdev.New(opts...)
	c.mdPrices = pricesource.FromModelsDev(c.client)
	c.chain = pricesource.Chain(c.filePrices, c.mdPrices)
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
// updates the source-bound cache used by metadata and price lookups.
func (c *Catalog) Refresh(ctx context.Context) error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Refresh(ctx)
}

// ReloadPrices explicitly reloads the configured operator price file.
// Failed reloads preserve the last good table; no network fetch is performed.
func (c *Catalog) ReloadPrices(ctx context.Context) error {
	if c == nil || c.filePrices == nil {
		return errPriceFileNotConfigured
	}
	return c.filePrices.Reload(ctx)
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

// CatalogStatus describes metadata freshness, not provider reachability,
// or workspace entitlement. Sync reports worker activity independently.
type CatalogStatus struct {
	Sync           *modelsdev.RefreshHealth `json:"sync,omitempty"`
	PriceFile      PriceFileStatus          `json:"price_file"`
	Source         string                   `json:"source"`
	State          string                   `json:"state"` // empty, fresh, stale
	RefreshEnabled bool                     `json:"refresh_enabled"`
	FetchedAt      *time.Time               `json:"fetched_at,omitempty"`
	AgeSeconds     *float64                 `json:"age_seconds,omitempty"`
	StaleAfter     *time.Time               `json:"stale_after,omitempty"`
}

// Status reports the last usable snapshot. Stale metadata remains readable;
// callers decide whether it is acceptable for their estimate or admission.
func (c *Catalog) Status() CatalogStatus { return c.statusAt(time.Now()) }

func (c *Catalog) statusAt(now time.Time) CatalogStatus {
	s := CatalogStatus{Source: pricesource.SourceModelsDev, State: "empty", PriceFile: PriceFileStatus{State: "disabled"}}
	if c == nil || c.client == nil {
		return s
	}
	s.RefreshEnabled = c.refreshEnabled
	s.PriceFile = c.filePrices.Status()
	h := c.client.Health()
	s.State = h.State
	fetched := h.LastSuccess.UTC()
	if !fetched.IsZero() {
		expires := fetched.Add(c.TTL())
		age := max(0, now.Sub(fetched).Seconds())
		s.FetchedAt, s.AgeSeconds, s.StaleAfter = &fetched, &age, &expires
		if !h.LegacyCache && h.State != "empty" {
			s.State = "fresh"
			if !now.Before(expires) {
				s.State = "stale"
			}
		}
	}
	h.State = s.State
	s.Sync = &h
	return s
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

func catalogProviderCandidates(catalogProvider, providerID string) []string {
	if catalogProvider != "" {
		return []string{catalogProvider}
	}
	ids := []string{providerID}
	if inferred := inferCatalogProvider(providerID); inferred != "" && inferred != providerID {
		ids = append(ids, inferred)
	}
	return ids
}

// LookupModel looks up a model in the models.dev catalog. It checks any
// explicit catalog_provider exclusively, or the configured provider ID and
// its conventional vendor alias (such as "opencode" for "opencode-zen").
// A wire dialect never establishes a model's provider or commercial terms.
func (c *Catalog) LookupModel(catalogProvider, providerID, modelID string) (modelsdev.Model, string, bool) {
	if c == nil || c.client == nil {
		return modelsdev.Model{}, "", false
	}
	for _, pid := range catalogProviderCandidates(catalogProvider, providerID) {
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
	var catProv, provID string
	if t.provider != nil {
		catProv = t.provider.catalogProvider
		provID = t.provider.id
	}
	ids := catalogProviderCandidates(catProv, provID)
	// Local account-specific overrides outrank every catalog source, including
	// overrides keyed by the mapped catalog identity rather than the local ID.
	if c.filePrices != nil {
		for _, pid := range append([]string{provID}, ids...) {
			if snap, ok := c.filePrices.Snapshot(pid, t.model); ok {
				return &snap
			}
		}
	}
	for _, pid := range ids {
		if pid == "" {
			continue
		}
		if snap, ok := c.mdPrices.Snapshot(pid, t.model); ok {
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
	var catProv, provID string
	if t.provider != nil {
		catProv = t.provider.catalogProvider
		provID = t.provider.id
	}

	// Start with permissive defaults for uncataloged models.
	tools := true
	vision := true
	streaming := true
	reasoning := false
	attachment := false
	temperature := false

	if md, _, ok := c.LookupModel(catProv, provID, t.model); ok {
		tools = md.Capabilities.ToolCall
		vision = hasVisionModality(md)
		reasoning = md.Capabilities.Reasoning
		attachment = md.Capabilities.Attachment
		temperature = md.Capabilities.Temperature
		if md.Limit.ContextWindow > 0 || md.Limit.MaxOutputTokens > 0 || md.Limit.MaxInputTokens != nil {
			l := md.Limit
			limits = &l
		}
		if len(md.Modality.Input) > 0 || len(md.Modality.Output) > 0 {
			m := md.Modality
			modalities = &m
		}
		if md.Name != "" || md.Family != "" || md.ReleaseDate != "" || md.KnowledgeCutoff != "" || md.OpenWeights || md.LastUpdated != "" || md.Description != "" || md.CanonicalModelID != "" || md.Type != "" || md.Status != "" {
			meta = &ModelMetadata{
				Description:      md.Description,
				CanonicalModelID: md.CanonicalModelID,
				Type:             md.Type,
				Status:           md.Status,
				Name:             md.Name,
				Family:           md.Family,
				OpenWeights:      md.OpenWeights,
				ReleaseDate:      md.ReleaseDate,
				KnowledgeCutoff:  md.KnowledgeCutoff,
				LastUpdated:      md.LastUpdated,
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
			Description:      r.Description,
			CanonicalModelID: r.CanonicalModelID,
			Type:             r.Type,
			Status:           r.Status,
			ProviderMetadata: r.Provider,
			ReasoningOptions: r.ReasoningOptions,
			Interleaved:      r.Interleaved,
			Experimental:     r.Experimental,
			CostCurrency:     modelsdev.PricingCurrency,
			CostUnit:         modelsdev.PricingUnit,
			ProviderID:       r.ProviderID,
			ID:               r.ID,
			Name:             r.Name,
			Family:           r.Family,
			OpenWeights:      r.OpenWeights,
			ReleaseDate:      r.ReleaseDate,
			KnowledgeCutoff:  r.KnowledgeCutoff,
			LastUpdated:      r.LastUpdated,
			Cost:             r.Cost,
			PriceSnapshot:    snapPtr,
			Limit:            r.Limit,
			Modalities:       r.Modality,
			Capabilities:     r.Capabilities,
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
