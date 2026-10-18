// Package llmgateway is Station's LLM gateway plugin ("llm-gateway").
//
// It exposes an OpenAI-compatible HTTP API (chat completions and a model
// list) in front of one or more upstream providers, and adds what a direct
// provider call lacks: model aliases with ordered fallback, a circuit breaker
// and cooldown per upstream (llm/core guard), per-call usage and cost records
// (llm/core usageledger and costcalc), and Station's shared authentication.
//
// The plugin imports only libs and the platform hook package, never Station
// internals, so it can move to its own repository unchanged. Tether remains
// the production gateway while this is built; the routes here are additive.
package llmgateway

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hollis-labs/libs/llm/core/llmtypes"
)

// Config is the plugin's configuration block (plugins.llm-gateway in api.yaml).
type Config struct {
	Providers []ProviderConfig `yaml:"providers" json:"providers"`
	Models    []ModelConfig    `yaml:"models" json:"models"`
	// Catalog configures the centralized model metadata and pricing catalog
	// backed by models.dev (default 90s TTL) and pricesource.
	Catalog CatalogConfig `yaml:"catalog" json:"catalog"`
	// Budgets configures spend and request limits measured over the ledger.
	Budgets []BudgetConfig `yaml:"budgets" json:"budgets"`
	// Policy configures the llm.invoke admission decision.
	Policy PolicyConfig `yaml:"policy" json:"policy"`
	// MaxBodyBytes caps request bodies on the gateway's routes (images and long
	// contexts are large). Default 32 MiB.
	MaxBodyBytes int64 `yaml:"max_body_bytes" json:"max_body_bytes"`
	// Ledger configures where usage records live.
	Ledger LedgerConfig `yaml:"ledger" json:"ledger"`
}

// CatalogConfig configures the centralized model metadata and pricing catalog.
type CatalogConfig struct {
	// TTL is how long cached model data is considered fresh before refreshing
	// (a positive Go duration such as "90s" or "5m"). Default "90s".
	TTL string `yaml:"ttl" json:"ttl"`
	// CacheDir overrides the directory where catalog.json is stored on disk
	// (default ~/.cache/go-modelsdev).
	CacheDir string `yaml:"cache_dir" json:"cache_dir"`
	// URL overrides the models.dev API endpoint (default https://models.dev/api.json).
	URL string `yaml:"url" json:"url"`
	// PriceFile is an optional path to a pricesource JSON price table
	// (pricesource.LoadFile) layered ahead of models.dev.
	PriceFile string `yaml:"price_file" json:"price_file"`
	// Refresh controls whether the background refresher runs on TTL. Default
	// true; set false to serve strictly from disk cache without background
	// network fetches.
	Refresh *bool `yaml:"refresh" json:"refresh"`
	// Warmup fetches metadata before serving and fails startup on fetch failure.
	// Useful for an installed service that must not start with an empty catalog.
	Warmup bool `yaml:"warmup" json:"warmup"`
}

func (c CatalogConfig) ttlDuration() (time.Duration, error) {
	if c.TTL == "" {
		return defaultCatalogTTL, nil
	}
	d, err := time.ParseDuration(c.TTL)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("llm-gateway: catalog.ttl must be a positive duration (got %q)", c.TTL)
	}
	return d, nil
}

func (c CatalogConfig) shouldRefresh() bool {
	if c.Refresh != nil {
		return *c.Refresh
	}
	return true
}

func (c CatalogConfig) validate() error {
	_, err := c.ttlDuration()
	return err
}

// BudgetConfig defines one spend or request limit measured over the ledger.
type BudgetConfig struct {
	ID string `yaml:"id" json:"id"`
	// Scope is "total" (default; all callers share the limit) or "principal"
	// (each verified caller gets its own counter).
	Scope string `yaml:"scope" json:"scope"`
	// Principal is an optional glob filtering which callers this budget applies
	// to ("ci-*"). Empty matches all callers.
	Principal string `yaml:"principal" json:"principal"`
	// Model is an optional glob filtering which model aliases this budget
	// counts and gates. Empty matches all models.
	Model string `yaml:"model" json:"model"`
	// Window is "day", "week", "month" (UTC calendar windows) or a positive Go
	// duration such as "1h" or "24h" (rolling window). Default "month".
	Window string `yaml:"window" json:"window"`
	// MaxUSD caps estimated spend in US dollars over Window.
	MaxUSD float64 `yaml:"max_usd" json:"max_usd"`
	// MaxRequests caps call count over Window.
	MaxRequests int64 `yaml:"max_requests" json:"max_requests"`
}

// LedgerConfig selects the ledger's store and payload capture policy.
type LedgerConfig struct {
	// Store is "memory" (default), "postgres" or "sqlite" (durable).
	Store string `yaml:"store" json:"store"`
	// History is how many entries the memory store keeps. Default 1000.
	History  int            `yaml:"history" json:"history"`
	Postgres PostgresConfig `yaml:"postgres" json:"postgres"`
	SQLite   SQLiteConfig   `yaml:"sqlite" json:"sqlite"`
	// Capture configures sanitized leader, tool, request and response payload
	// capture in Entry.Detail.
	Capture CaptureConfig `yaml:"capture" json:"capture"`
}

// CaptureConfig controls what payload data is sanitized and stored in Detail.
type CaptureConfig struct {
	// Mode is "off" (default: only decision answers are stored) or "full"
	// (sanitized leader/system prompt, tools, request and response/SSE events).
	Mode string `yaml:"mode" json:"mode"`
	// MaxBytes caps each payload passed to security/redact's Sanitizer
	// (default 256 KiB). Oversized payloads fail closed to a tombstone.
	MaxBytes int `yaml:"max_bytes" json:"max_bytes"`
}

// PostgresConfig locates the durable ledger. The DSN carries a password, so it
// is a secret reference (or an environment variable), never a literal. Point it
// at a database and role dedicated to the ledger.
type PostgresConfig struct {
	DSNRef string `yaml:"dsn_ref" json:"dsn_ref"`
	DSNEnv string `yaml:"dsn_env" json:"dsn_env"`
}

type SQLiteConfig struct {
	// Path is an absolute path in an owner-only directory; may share the management database.
	Path string `yaml:"path" json:"path"`
}

func (l LedgerConfig) validate() error {
	if l.Store != "sqlite" && l.SQLite.Path != "" {
		return fmt.Errorf("llm-gateway: ledger.sqlite requires store sqlite")
	}
	if l.Store != "postgres" && l.Postgres != (PostgresConfig{}) {
		return fmt.Errorf("llm-gateway: ledger.postgres requires store postgres")
	}
	switch l.Store {
	case "", "memory":
	case "sqlite":
		if !filepath.IsAbs(l.SQLite.Path) || filepath.Clean(l.SQLite.Path) != l.SQLite.Path {
			return fmt.Errorf("llm-gateway: ledger.sqlite.path must be absolute and clean")
		}
	case "postgres":
		if (l.Postgres.DSNRef == "") == (l.Postgres.DSNEnv == "") {
			return fmt.Errorf("llm-gateway: ledger.postgres needs exactly one of dsn_ref or dsn_env")
		}
	default:
		return fmt.Errorf("llm-gateway: ledger.store must be memory, postgres or sqlite")
	}
	if l.History < 0 {
		return fmt.Errorf("llm-gateway: ledger.history must not be negative")
	}
	switch l.Capture.Mode {
	case "", "off", "full":
	default:
		return fmt.Errorf("llm-gateway: ledger.capture.mode must be off or full")
	}
	if l.Capture.MaxBytes < 0 {
		return fmt.Errorf("llm-gateway: ledger.capture.max_bytes must not be negative")
	}
	return nil
}

// PolicyConfig configures the policy decision point for llm.invoke.
type PolicyConfig struct {
	// Mode is "shadow" (evaluate and audit, admit everything; the default) or
	// "enforce". Shadow first: turn enforce on once the audit log looks right.
	Mode string `yaml:"mode" json:"mode"`
	// DefaultEffect applies when no rule matches: allow (default), ask or deny.
	DefaultEffect string `yaml:"default_effect" json:"default_effect"`
	// Rules are evaluated in order; the first match decides.
	Rules []RuleConfig `yaml:"rules" json:"rules"`
}

// RuleConfig is one declarative policy rule. Empty match fields match anything.
type RuleConfig struct {
	ID string `yaml:"id" json:"id"`
	// Principal is a glob over the verified principal ID ("ci-*").
	Principal string `yaml:"principal" json:"principal"`
	// Model is a glob over the requested model name.
	Model string `yaml:"model" json:"model"`
	// Provider is a glob over the candidate target's provider ID.
	Provider string `yaml:"provider" json:"provider"`
	// Tag matches a candidate target carrying this tag ("local", "free", "paid").
	Tag string `yaml:"tag" json:"tag"`
	// Operation matches chat, embedding or decision requests.
	Operation string `yaml:"operation" json:"operation"`
	// Needs matches requests that need one of: tools, vision, streaming.
	Needs string `yaml:"needs" json:"needs"`
	// BudgetState matches "ok" or "exhausted" from the quota evaluator.
	BudgetState string `yaml:"budget_state" json:"budget_state"`
	Effect      string `yaml:"effect" json:"effect"`
	Reason      string `yaml:"reason" json:"reason"`
}

// ProviderConfig describes one upstream endpoint.
type ProviderConfig struct {
	ID string `yaml:"id" json:"id"`
	// Enabled defaults to true. Disabled providers retain configuration but serve no traffic.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	// Dialect is the provider's native wire protocol: "openai" (Chat
	// Completions and Embeddings, and servers that copy them), "anthropic"
	// (Messages) or "jev" (System-1 decisions).
	Dialect string `yaml:"dialect" json:"dialect"`
	// CatalogProvider overrides the models.dev provider ID used to look up
	// model pricing, limits, and capabilities (e.g. "openrouter", "opencode",
	// "huggingface"). Empty defaults to ID then Dialect.
	CatalogProvider string `yaml:"catalog_provider" json:"catalog_provider"`
	// BaseURL is the API root as the vendor documents it, including the
	// version segment ("https://api.openai.com/v1").
	BaseURL string `yaml:"base_url" json:"base_url"`
	// Headers are optional static HTTP headers sent on every upstream request
	// to this provider (e.g. x-opencode-session).
	Headers map[string]string `yaml:"headers" json:"headers"`
	// OpenCodeSession requires X-Station-Session and sends a principal-scoped
	// conversation identifier as x-opencode-session on inference requests.
	OpenCodeSession bool `yaml:"opencode_session" json:"opencode_session"`
	// DiscoverModels queries GET <base_url>/models at startup and registers any
	// discovered model IDs not already declared in models.
	DiscoverModels bool `yaml:"discover_models" json:"discover_models"`
	// APIKeyEnv names the environment variable holding the upstream key. The
	// key itself never appears in configuration. Empty means no credential.
	APIKeyEnv string `yaml:"api_key_env" json:"api_key_env"`
	// APIKeyRef is a secret reference (keychain://openai/work, op://..., file://...)
	// resolved at startup. Provision one with `station secret set <ref>`.
	// Use this or api_key_env, not both.
	APIKeyRef      string `yaml:"api_key_ref" json:"api_key_ref"`
	TimeoutSeconds int    `yaml:"timeout_seconds" json:"timeout_seconds"`
}

// ModelConfig is a public model name and its ordered upstream targets.
type ModelConfig struct {
	// Name is what callers put in the request's "model" field.
	Name string `yaml:"name" json:"name"`
	// Enabled defaults to true. Disabled aliases are hidden and cannot be invoked.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	// Kind is what the model does: "chat" (default), "embedding" or "decision"
	// (a System-1 judgment model such as JEV). A model has one kind and a
	// request for another is refused.
	Kind string `yaml:"kind" json:"kind"`
	// Targets are tried in order until one is admitted and accepts the call.
	Targets []TargetConfig `yaml:"targets" json:"targets"`
}

// TargetConfig is one upstream model behind an alias.
type TargetConfig struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	// Operations narrows the native endpoint families this model supports.
	// Empty retains dialect defaults; explicit lists are recommended for live routes.
	Operations []Operation `yaml:"operations" json:"operations"`
	// Translate opts this target in to cross-protocol translation when called
	// from another dialect's endpoint (for example an OpenAI /v1/chat/completions
	// request routed to an Anthropic provider, or vice versa). Default false:
	// targets only serve their native dialect unless explicitly opted in.
	Translate bool `yaml:"translate" json:"translate"`
	// Tags label the route for policy and discovery ("local", "free", "paid").
	Tags []string `yaml:"tags" json:"tags"`
	// BestEffort marks an opportunistic route (such as a tailnet llama server
	// that may sleep or reboot): it uses a short connect/header timeout and
	// trips its breaker on the first failure so a down local host never blocks
	// hub requests.
	BestEffort bool `yaml:"best_effort" json:"best_effort"`
	// TimeoutMS overrides the response-header timeout for this target in
	// milliseconds (default 1500ms when best_effort is true).
	TimeoutMS int `yaml:"timeout_ms" json:"timeout_ms"`
	// Capabilities lets an operator mark what a target cannot do, so a
	// request needing it skips the target. Unset means supported.
	Capabilities CapabilitiesConfig `yaml:"capabilities" json:"capabilities"`
	// Price is optional. With it, calls record an estimated cost; without it
	// they record usage and an explicitly unpriced cost.
	Price *PriceConfig `yaml:"price" json:"price"`
}

// CapabilitiesConfig overrides a target's capabilities; nil means inherit from
// the models.dev catalog (or supported when absent from the catalog).
type CapabilitiesConfig struct {
	Tools     *bool `yaml:"tools" json:"tools"`
	Vision    *bool `yaml:"vision" json:"vision"`
	Streaming *bool `yaml:"streaming" json:"streaming"`
	Reasoning *bool `yaml:"reasoning" json:"reasoning"`
}

func (c CapabilitiesConfig) resolve() llmtypes.ProviderCapabilities {
	on := func(b *bool) bool { return b == nil || *b }
	return llmtypes.ProviderCapabilities{
		SupportsToolCalling: on(c.Tools), SupportsImageInput: on(c.Vision), SupportsStreamJSON: on(c.Streaming),
	}
}

// PriceConfig holds per-million-token rates in US dollars.
type PriceConfig struct {
	InputPerMillion      float64 `yaml:"input_per_million" json:"input_per_million"`
	OutputPerMillion     float64 `yaml:"output_per_million" json:"output_per_million"`
	CacheReadPerMillion  float64 `yaml:"cache_read_per_million" json:"cache_read_per_million"`
	CacheWritePerMillion float64 `yaml:"cache_write_per_million" json:"cache_write_per_million"`
	ReasoningPerMillion  float64 `yaml:"reasoning_per_million" json:"reasoning_per_million"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

// Validate checks the whole configuration so a mistake fails at startup, not
// on the first request.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("llm-gateway: at least one provider is required")
	}
	providers := map[string]bool{}
	hasDiscover := false
	for _, p := range c.Providers {
		if p.DiscoverModels {
			hasDiscover = true
		}
		if !idPattern.MatchString(p.ID) {
			return fmt.Errorf("llm-gateway: invalid provider id %q", p.ID)
		}
		if providers[p.ID] {
			return fmt.Errorf("llm-gateway: duplicate provider %q", p.ID)
		}
		providers[p.ID] = true
		if _, ok := dialects[p.Dialect]; !ok {
			return fmt.Errorf("llm-gateway: provider %q: unsupported dialect %q", p.ID, p.Dialect)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("llm-gateway: provider %q: base_url is required", p.ID)
		}
		if p.APIKeyEnv != "" && p.APIKeyRef != "" {
			return fmt.Errorf("llm-gateway: provider %q: set api_key_env or api_key_ref, not both", p.ID)
		}
		if p.TimeoutSeconds < 0 {
			return fmt.Errorf("llm-gateway: provider %q: negative timeout", p.ID)
		}
	}
	if len(c.Models) == 0 && !hasDiscover {
		return fmt.Errorf("llm-gateway: at least one model is required")
	}
	names := map[string]bool{}
	for _, m := range c.Models {
		if !idPattern.MatchString(m.Name) {
			return fmt.Errorf("llm-gateway: invalid model name %q", m.Name)
		}
		if names[m.Name] {
			return fmt.Errorf("llm-gateway: duplicate model %q", m.Name)
		}
		names[m.Name] = true
		if _, err := m.operation(); err != nil {
			return fmt.Errorf("llm-gateway: model %q: %w", m.Name, err)
		}
		if len(m.Targets) == 0 {
			return fmt.Errorf("llm-gateway: model %q has no targets", m.Name)
		}
		for _, t := range m.Targets {
			if !providers[t.Provider] {
				return fmt.Errorf("llm-gateway: model %q targets unknown provider %q", m.Name, t.Provider)
			}
			op, _ := m.operation()
			upDialect := dialectOf(c, t.Provider)
			seenOps := map[Operation]bool{}
			for _, allowed := range t.Operations {
				if seenOps[allowed] || upDialect.UpstreamPath(allowed) == "" || (allowed != op && !(op == OpChat && allowed == OpResponses)) {
					return fmt.Errorf("llm-gateway: model %q: incompatible or duplicate operation %q", m.Name, allowed)
				}
				seenOps[allowed] = true
			}
			if upDialect.UpstreamPath(op) == "" {
				return fmt.Errorf("llm-gateway: model %q targets provider %q, whose dialect does not offer %s", m.Name, t.Provider, op)
			}
			if t.Translate && !hasTranslatorTo(upDialect.Name(), op) {
				return fmt.Errorf("llm-gateway: model %q target %q: translate is not supported for dialect %q (%s)", m.Name, t.Provider, upDialect.Name(), op)
			}
			if t.Model == "" {
				return fmt.Errorf("llm-gateway: model %q: target has no model", m.Name)
			}
			if t.TimeoutMS < 0 {
				return fmt.Errorf("llm-gateway: model %q: negative target timeout_ms", m.Name)
			}
			if t.Price != nil && (t.Price.InputPerMillion < 0 || t.Price.OutputPerMillion < 0 || t.Price.CacheReadPerMillion < 0 || t.Price.CacheWritePerMillion < 0 || t.Price.ReasoningPerMillion < 0) {
				return fmt.Errorf("llm-gateway: model %q: negative price", m.Name)
			}
		}
	}
	if err := c.Catalog.validate(); err != nil {
		return err
	}
	if err := validateBudgets(c.Budgets); err != nil {
		return err
	}
	if err := c.Ledger.validate(); err != nil {
		return err
	}
	return c.Policy.validate()
}

func validateBudgets(bs []BudgetConfig) error {
	seen := map[string]bool{}
	for _, b := range bs {
		if !idPattern.MatchString(b.ID) || seen[b.ID] {
			return fmt.Errorf("llm-gateway: budget needs a unique id (%q)", b.ID)
		}
		seen[b.ID] = true
		switch b.Scope {
		case "", "total", "principal":
		default:
			return fmt.Errorf("llm-gateway: budget %q: scope must be total or principal", b.ID)
		}
		if _, _, err := parseBudgetWindow(b.Window); err != nil {
			return fmt.Errorf("llm-gateway: budget %q: %w", b.ID, err)
		}
		if b.MaxUSD < 0 || b.MaxRequests < 0 || (b.MaxUSD == 0 && b.MaxRequests == 0) {
			return fmt.Errorf("llm-gateway: budget %q: set a positive max_usd and/or max_requests", b.ID)
		}
		for _, g := range []string{b.Principal, b.Model} {
			if _, err := path.Match(g, ""); err != nil {
				return fmt.Errorf("llm-gateway: budget %q: bad glob", b.ID)
			}
		}
	}
	return nil
}

func (m ModelConfig) operation() (Operation, error) {
	switch m.Kind {
	case "", "chat":
		return OpChat, nil
	case "embedding":
		return OpEmbed, nil
	case "decision":
		return OpDecide, nil
	}
	return "", fmt.Errorf("kind must be chat, embedding or decision")
}

func dialectOf(c *Config, providerID string) Dialect {
	for _, p := range c.Providers {
		if p.ID == providerID {
			return dialects[p.Dialect]
		}
	}
	return nil
}

func (c *Config) maxBody() int64 {
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return 32 << 20
}

func (p PolicyConfig) validate() error {
	switch p.Mode {
	case "", "shadow", "enforce":
	default:
		return fmt.Errorf("llm-gateway: policy.mode must be shadow or enforce")
	}
	if p.DefaultEffect != "" && !validEffect(p.DefaultEffect) {
		return fmt.Errorf("llm-gateway: policy.default_effect must be allow, ask or deny")
	}
	seen := map[string]bool{}
	for _, r := range p.Rules {
		if r.ID == "" || seen[r.ID] {
			return fmt.Errorf("llm-gateway: policy rule needs a unique id (%q)", r.ID)
		}
		seen[r.ID] = true
		if !validEffect(r.Effect) {
			return fmt.Errorf("llm-gateway: policy rule %q: effect must be allow, ask or deny", r.ID)
		}
		if r.Operation != "" && r.Operation != "chat" && r.Operation != "responses" && r.Operation != "embedding" && r.Operation != "decision" {
			return fmt.Errorf("llm-gateway: policy rule %q: operation must be chat, responses, embedding or decision", r.ID)
		}
		switch r.Needs {
		case "", "tools", "vision", "streaming":
		default:
			return fmt.Errorf("llm-gateway: policy rule %q: needs must be tools, vision or streaming", r.ID)
		}
		switch r.BudgetState {
		case "", "ok", "exhausted":
		default:
			return fmt.Errorf("llm-gateway: policy rule %q: budget_state must be ok or exhausted", r.ID)
		}
		for _, g := range []string{r.Principal, r.Model, r.Provider} {
			if _, err := path.Match(g, ""); err != nil {
				return fmt.Errorf("llm-gateway: policy rule %q: bad glob", r.ID)
			}
		}
	}
	return nil
}

func validEffect(s string) bool { return s == "allow" || s == "ask" || s == "deny" }

func (p ProviderConfig) timeout() time.Duration {
	if p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	return 120 * time.Second
}

func (t TargetConfig) headerTimeout() time.Duration {
	if t.TimeoutMS > 0 {
		return time.Duration(t.TimeoutMS) * time.Millisecond
	}
	if t.BestEffort {
		return 1500 * time.Millisecond
	}
	return 0
}

func enabled(value *bool) bool { return value == nil || *value }
