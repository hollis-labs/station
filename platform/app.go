package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/hollis-labs/libs/app/app-host/host"
	"github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/libs/security/secrets"
	"gopkg.in/yaml.v3"
)

// Config is Station's HTTP API configuration (api.yaml). The whole surface is
// opt-in: with no Listen address Station only hosts MCP logical servers.
type Config struct {
	// Listen is the TCP address of the API listener, for example
	// "127.0.0.1:8081". Empty disables the API.
	Listen string `yaml:"listen"`
	// MaxBodyBytes is the default request body cap; default 4 MiB. A route can
	// set its own (Route.MaxBodyBytes).
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// Auth configures the shared authentication guard.
	Auth AuthConfig `yaml:"auth"`
	// Plugins holds one configuration block per plugin, keyed by plugin name.
	Plugins map[string]yaml.Node `yaml:"plugins"`
}

// AuthConfig configures authentication for every plugin route.
type AuthConfig struct {
	// Mode is off, permissive or enforce. Default enforce.
	Mode string `yaml:"mode"`
	// Keys are static API keys, stored as SHA-256 hex only.
	Keys []KeyConfig `yaml:"keys"`
	// KeysFile is an absolute path to a separate YAML list of hashed client keys.
	// Changes take effect after a service reload; the raw credentials never live here.
	KeysFile string `yaml:"keys_file"`
	// JWT, when set, accepts EdDSA JWTs from an issuer (Tether issues them),
	// verified offline against a cached JWKS. Tried before static keys.
	JWT *JWTConfig `yaml:"jwt"`
}

// JWTConfig configures offline JWT verification.
type JWTConfig struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
	// JWKSURL is the issuer's /.well-known/jwks.json. Keys are fetched in the
	// background and cached, so a request never waits on the issuer.
	JWKSURL string `yaml:"jwks_url"`
	// BootstrapKeyFile is a JWKS file or PEM Ed25519 public key used until the
	// first fetch succeeds (the issuer may be down at start).
	BootstrapKeyFile string `yaml:"bootstrap_key_file"`
}

// KeyConfig is one static API key. The raw key is never stored; produce the
// hash with `printf %s "$KEY" | sha256sum`.
type KeyConfig struct {
	ID     string   `yaml:"id" json:"id"`
	SHA256 string   `yaml:"sha256" json:"sha256"`
	Scopes []string `yaml:"scopes" json:"scopes"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseConfig decodes api.yaml strictly: an unknown key is an error, so a
// misspelled security setting cannot be silently ignored.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("platform: parse config: %w", err)
	}
	return &cfg, nil
}

// Options assembles an App.
type Options struct {
	Config  *Config
	Logger  *slog.Logger
	Plugins []Plugin
	// Middleware is consumer-level middleware, merged with the stock set and
	// each plugin's, then ordered by Phase.
	Middleware []Middleware
	// Transports are additional transports serving the assembled handler.
	Transports []Transport
	// Secrets resolves secret references for plugins. Nil builds a resolver
	// with the standard backends.
	Secrets *secrets.Resolver
	// Sink, when set, learns every resolved secret value (the log redaction
	// scope), if Secrets is nil. Ignored when Secrets is supplied.
	Sink ValueSink
}

// App is an assembled API: routes, middleware chain and transports.
type App struct {
	cfg     *Config
	logger  *slog.Logger
	reg     *Registry
	host    *host.Host
	handler http.Handler
	jwt     *auth.JWTVerifier
}

// Assemble registers every plugin, validates all hooks and builds the handler.
// Nothing listens until Run.
func Assemble(ctx context.Context, opts Options) (*App, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = &Config{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	reg := NewRegistry(logger)
	if ss, ok := opts.Sink.(scopeSink); ok && ss.s != nil {
		reg.scope = ss.s
	}
	if opts.Secrets != nil {
		reg.secrets = opts.Secrets
	} else {
		sopts := []secrets.Option{secrets.WithLegacy("tether", legacyTetherAccount)}
		if opts.Sink != nil {
			sopts = append(sopts, secrets.WithRedactor(opts.Sink))
		}
		reg.secrets = secrets.New(sopts...)
	}

	for _, m := range opts.Middleware {
		if err := reg.UseMiddleware(m); err != nil {
			return nil, err
		}
	}
	for _, t := range opts.Transports {
		if err := reg.AddTransport(t); err != nil {
			return nil, err
		}
	}
	// A plugin is opt-in: it registers only when its configuration block is
	// present, and a block for a plugin that does not exist is an error.
	catalog := map[string]bool{}
	for _, p := range opts.Plugins {
		catalog[p.Name()] = true
		node, has := cfg.Plugins[p.Name()]
		if !has {
			continue
		}
		scoped, err := reg.Scoped(p.Name())
		if err != nil {
			return nil, err
		}
		if err := p.Register(ctx, scoped, strictDecoder(node)); err != nil {
			return nil, fmt.Errorf("platform: register plugin %q: %w", p.Name(), err)
		}
	}
	for name := range cfg.Plugins {
		if !catalog[name] {
			return nil, fmt.Errorf("platform: configuration for unknown plugin %q", name)
		}
	}

	// Routes are known now, so the route-aware stock middleware can be built.
	mux := http.NewServeMux()
	limits := map[string]int64{}
	for _, m := range reg.routes {
		pattern := m.route.Method + " /api/" + m.plugin + m.route.Path
		mux.Handle(pattern, m.route.Handler)
		if m.route.MaxBodyBytes > 0 {
			limits[pattern] = m.route.MaxBodyBytes
		}
	}
	routeOf := func(r *http.Request) string { _, p := mux.Handler(r); return p }
	limitFor := func(r *http.Request) int64 {
		if l, ok := limits[routeOf(r)]; ok {
			return l
		}
		return cfg.MaxBodyBytes
	}
	for _, m := range []Middleware{StockRecover(logger), StockRequestID(), StockAccessLog(logger, routeOf), StockBodyLimit(limitFor)} {
		if err := reg.UseMiddleware(m); err != nil {
			return nil, err
		}
	}

	guard, jwt, err := buildGuard(cfg, reg, logger)
	if err != nil {
		return nil, err
	}
	if err := reg.UseMiddleware(Middleware{
		Name: "auth", Phase: PhaseAuthn, Failure: FailClosed,
		Wrap: guard.Middleware,
	}); err != nil {
		return nil, err
	}

	var api http.Handler = mux
	chain := reg.orderedMiddleware()
	for i := len(chain) - 1; i >= 0; i-- {
		api = chain[i].Wrap(api)
	}

	a := &App{cfg: cfg, logger: logger, reg: reg, handler: api, jwt: jwt}
	h, err := host.New(host.Config{Name: "station", Routes: api, Stop: a.stop})
	if err != nil {
		return nil, err
	}
	a.host = h
	return a, nil
}

// strictDecoder decodes a plugin's block rejecting unknown keys, so a
// misspelled setting fails at startup instead of being ignored.
func strictDecoder(node yaml.Node) ConfigDecoder {
	return func(v any) error {
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		return dec.Decode(v)
	}
}

// buildGuard builds the auth guard and its scope table from the mounted routes.
func buildGuard(cfg *Config, reg *Registry, logger *slog.Logger) (*auth.Guard, *auth.JWTVerifier, error) {
	modeStr := cfg.Auth.Mode
	if modeStr == "" {
		modeStr = string(auth.ModeEnforce)
	}
	mode, err := auth.ParseMode(modeStr)
	if err != nil {
		return nil, nil, fmt.Errorf("platform: auth.mode: %w", err)
	}
	configuredKeys := append([]KeyConfig(nil), cfg.Auth.Keys...)
	if cfg.Auth.KeysFile != "" {
		loaded, err := LoadClientKeys(cfg.Auth.KeysFile)
		if err != nil {
			return nil, nil, err
		}
		configuredKeys = append(configuredKeys, loaded...)
	}
	if err := ValidateClientKeys(configuredKeys); err != nil {
		return nil, nil, err
	}
	keys := make([]auth.APIKey, 0, len(configuredKeys))
	for _, k := range configuredKeys {
		if k.ID == "" || !sha256Hex.MatchString(k.SHA256) {
			return nil, nil, fmt.Errorf("platform: auth key %q needs an id and a lower-case sha256 hex digest", k.ID)
		}
		keys = append(keys, auth.APIKey{ID: k.ID, Hash: k.SHA256, Scopes: k.Scopes})
	}
	table := auth.NewScopeTable()
	for _, m := range reg.routes {
		table.Route(m.route.Method, "/api/"+m.plugin+m.route.Path, m.route.Scope)
	}
	var (
		verifier auth.Verifier = auth.NewAPIKeyVerifier(keys...)
		jwt      *auth.JWTVerifier
	)
	if jc := cfg.Auth.JWT; jc != nil {
		jwt, err = auth.NewJWTVerifier(auth.JWTConfig{
			Issuer: jc.Issuer, Audience: jc.Audience, JWKSURL: jc.JWKSURL, BootstrapKeyFile: jc.BootstrapKeyFile,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("platform: auth.jwt: %w", err)
		}
		verifier = auth.Chain(jwt, verifier)
	}
	g, err := auth.New(auth.Config{
		Mode:     mode,
		Verifier: verifier,
		OnRequest: func(r *http.Request, p auth.Principal, ev auth.Event) {
			if ri := infoOf(r.Context()); ri != nil {
				ri.SetPrincipal(p.ID, p.Verified)
			}
			logger.Info("auth", "request_id", RequestID(r), "mode", string(ev.Mode), "authn", ev.Authentication, "route", ev.Route,
				"principal", ev.PrincipalID, "scope", ev.RequiredScope, "denied", ev.Denied, "would_deny", ev.WouldDeny, "reason", ev.Reason)
		},
		Scopes: table,
		Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}
	return g, jwt, nil
}

// Handler returns the full HTTP handler (health, API routes, placeholder UI).
func (a *App) Handler() http.Handler { return a.host.Handler() }

// Explain describes the middleware chain, outermost first.
func (a *App) Explain() []string { return a.reg.Explain() }

// Close runs the plugins' stop hooks without serving. Serve does this itself on
// shutdown; Close is for an App that was assembled but never served (a
// validation run, a test), so its connections and buffers are released.
func (a *App) Close(ctx context.Context) error { return a.stop(ctx) }

func (a *App) stop(ctx context.Context) error {
	var err error
	for i := len(a.reg.stops) - 1; i >= 0; i-- {
		err = errors.Join(err, a.reg.stops[i](ctx))
	}
	return err
}

// Enabled reports whether the configuration asks for an API listener.
func (a *App) Enabled() bool { return a.cfg.Listen != "" }

// Run serves the API on the configured TCP listener plus any extra transports
// and blocks until ctx is cancelled or a transport fails.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("platform: listen: %w", err)
	}
	return a.Serve(ctx, ln)
}

// Serve is Run on a caller-supplied listener.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	record := func(name string, err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			mu.Unlock()
		}
		cancel()
	}
	for _, t := range a.reg.transports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record("transport "+t.Name(), t.Serve(ctx, a.host.Handler()))
		}()
	}
	if a.jwt != nil {
		// Keys refresh in the background; requests never wait on the issuer.
		wg.Add(1)
		go func() { defer wg.Done(); a.jwt.Run(ctx) }()
	}
	a.logger.Info("station api listening", "addr", ln.Addr().String())
	wg.Add(1)
	go func() {
		defer wg.Done()
		record("http", a.host.Serve(ctx, ln))
	}()
	wg.Wait()
	return errors.Join(errs...)
}

// legacyTetherAccount maps a keychain:// ref to the account name used by
// tether-apikey-helper before the shared "hollis-labs" keychain service.
func legacyTetherAccount(ref secrets.Ref) string {
	if ref.Authority == "" || len(ref.Path) == 0 {
		return ""
	}
	return "provider-api-key:" + ref.Key()
}
