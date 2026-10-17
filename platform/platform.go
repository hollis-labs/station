// Package platform is Station's plugin platform: the small set of hooks a
// plugin or a consumer uses to extend Station without touching its core.
//
// Station's core is deliberately thin: it hosts MCP logical servers (mcp-host)
// and, when enabled, one HTTP API surface assembled with app-host. Everything
// product-shaped is a Plugin. A Plugin registers through a *Registry, which
// offers exactly these hooks:
//
//   - Mount: an HTTP route under /api/<plugin>/, each with a required auth scope.
//   - UseMiddleware: an HTTP middleware, ordered by Phase, not by registration.
//   - AddTransport: an additional way to serve the assembled HTTP handler
//     (a unix socket, a TLS listener, a tunnel). The default is one TCP listener.
//   - OnStop: a cleanup callback, run after HTTP requests drain.
//
// Hooks are additive and validated at assembly time, before anything listens.
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/hollis-labs/libs/security/redact"
	"github.com/hollis-labs/libs/security/secrets"
)

// Plugin is a unit of product functionality hosted by Station.
type Plugin interface {
	// Name is the plugin's stable identifier: lower-case letters, digits and
	// hyphens. It is the plugin's route prefix (/api/<name>/) and the key of
	// its configuration block.
	Name() string
	// Register wires the plugin's hooks. cfg decodes the plugin's own
	// configuration block into v (a pointer); it is a no-op returning nil when
	// the block is absent. Register must not start goroutines that outlive the
	// context passed to the hooks it registers (use OnStop).
	Register(ctx context.Context, reg *PluginRegistry, cfg ConfigDecoder) error
}

// ConfigDecoder decodes a plugin's configuration block into v.
type ConfigDecoder func(v any) error

// FailureMode states what a middleware does when it cannot make a decision.
// It has no default: every middleware declares one, so the choice is visible.
type FailureMode int

const (
	// FailureUnset is the invalid zero value.
	FailureUnset FailureMode = iota
	// FailOpen lets the request through when the middleware itself fails.
	// For middleware that observes or decorates: logging, tracing, request ID.
	FailOpen
	// FailClosed refuses the request when the middleware itself fails.
	// For middleware that protects: authentication, authorization, limits.
	FailClosed
)

func (f FailureMode) String() string {
	switch f {
	case FailOpen:
		return "open"
	case FailClosed:
		return "closed"
	default:
		return "unset"
	}
}

// Phase orders middleware, outermost first. Phases are spaced so a consumer can
// place middleware between two standard ones.
type Phase int

// Standard phases, outermost first. The order follows Nanite's documented
// chain: a wrong order is a defect class, so it is data, not convention.
const (
	PhaseRecover   Phase = 0
	PhaseTrace     Phase = 100 // trace and request ID
	PhaseAccessLog Phase = 200
	PhaseLimits    Phase = 300 // timeout, body size
	PhaseCORS      Phase = 400
	PhaseAuthn     Phase = 500
	PhaseAuthz     Phase = 600 // authorization and policy
	PhaseRateLimit Phase = 700
	PhaseIdentity  Phase = 800 // identity enrichment
	PhaseCache     Phase = 900
)

// Middleware is one HTTP middleware plus the descriptor assembly checks.
type Middleware struct {
	// Name is unique within a chain.
	Name string
	// Phase positions the middleware. Equal phases keep registration order.
	Phase Phase
	// Failure is required: FailOpen or FailClosed.
	Failure FailureMode
	// Reason is required when an observing phase (trace, access log, cache)
	// declares FailClosed, or a protecting phase (authn, authz, limits, rate
	// limit) declares FailOpen. It states why the usual choice does not apply.
	Reason string
	// Wrap returns the wrapped handler.
	Wrap func(http.Handler) http.Handler
}

func (m Middleware) validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("platform: middleware has no name")
	}
	if m.Wrap == nil {
		return fmt.Errorf("platform: middleware %q has no Wrap", m.Name)
	}
	if m.Failure != FailOpen && m.Failure != FailClosed {
		return fmt.Errorf("platform: middleware %q must declare a failure mode", m.Name)
	}
	protecting := m.Phase >= PhaseLimits && m.Phase != PhaseCORS && m.Phase < PhaseIdentity
	observing := m.Phase == PhaseTrace || m.Phase == PhaseAccessLog || m.Phase >= PhaseCache
	if (observing && m.Failure == FailClosed || protecting && m.Failure == FailOpen) && strings.TrimSpace(m.Reason) == "" {
		return fmt.Errorf("platform: middleware %q declares failure=%s at phase %d against its usual mode and states no reason", m.Name, m.Failure, m.Phase)
	}
	return nil
}

// Route is one HTTP route a plugin serves.
type Route struct {
	// Method is an HTTP method; required, so a route never silently accepts
	// every verb.
	Method string
	// Path is relative to the plugin's prefix and starts with "/". It may use
	// net/http pattern syntax ("/v1/models/{id}").
	Path string
	// Scope is the auth scope a caller must hold. Required: a route with no
	// scope would be a route with no authorization decision.
	Scope string
	// MaxBodyBytes caps this route's request body. Zero uses the API-wide
	// default (max_body_bytes). It can raise or lower the default, so a route
	// that takes large payloads (images, long contexts) does not force every
	// other route to accept them.
	MaxBodyBytes int64
	// Handler serves the route.
	Handler http.Handler
}

// Transport serves the assembled HTTP handler over some listener. Serve blocks
// until ctx is cancelled, then drains and returns nil.
type Transport interface {
	Name() string
	Serve(ctx context.Context, h http.Handler) error
}

type mounted struct {
	plugin string
	route  Route
}

// Registry collects hooks. Create one with NewRegistry; plugins receive a view
// scoped to their own name.
type Registry struct {
	logger     *slog.Logger
	routes     []mounted
	middleware []Middleware
	transports []Transport
	stops      []func(context.Context) error
	plugins    []string
	secrets    *secrets.Resolver
	scope      *redact.Scope
}

// NewRegistry returns an empty Registry. A nil logger selects slog.Default().
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{logger: logger, scope: redact.NewScope()}
}

var pluginName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Scoped returns the view of r a plugin named name registers through.
func (r *Registry) Scoped(name string) (*PluginRegistry, error) {
	if !pluginName.MatchString(name) {
		return nil, fmt.Errorf("platform: invalid plugin name %q", name)
	}
	for _, p := range r.plugins {
		if p == name {
			return nil, fmt.Errorf("platform: duplicate plugin %q", name)
		}
	}
	r.plugins = append(r.plugins, name)
	return &PluginRegistry{r: r, name: name}, nil
}

// UseMiddleware registers consumer-level middleware (not owned by a plugin).
func (r *Registry) UseMiddleware(m Middleware) error {
	if err := m.validate(); err != nil {
		return err
	}
	for _, e := range r.middleware {
		if e.Name == m.Name {
			return fmt.Errorf("platform: duplicate middleware %q", m.Name)
		}
	}
	r.middleware = append(r.middleware, m)
	return nil
}

// AddTransport registers an additional transport for the assembled handler.
func (r *Registry) AddTransport(t Transport) error {
	if t == nil || t.Name() == "" {
		return fmt.Errorf("platform: transport needs a name")
	}
	for _, e := range r.transports {
		if e.Name() == t.Name() {
			return fmt.Errorf("platform: duplicate transport %q", t.Name())
		}
	}
	r.transports = append(r.transports, t)
	return nil
}

// OnStop registers a cleanup callback. Callbacks run in reverse registration
// order, after the HTTP server has drained.
func (r *Registry) OnStop(fn func(context.Context) error) { r.stops = append(r.stops, fn) }

// PluginRegistry is the Registry view given to one plugin.
type PluginRegistry struct {
	r    *Registry
	name string
}

// Logger returns a logger tagged with the plugin name.
func (p *PluginRegistry) Logger() *slog.Logger { return p.r.logger.With("plugin", p.name) }

// Prefix returns the plugin's route prefix, "/api/<name>".
func (p *PluginRegistry) Prefix() string { return "/api/" + p.name }

// Mount registers a route under the plugin's prefix.
func (p *PluginRegistry) Mount(rt Route) error {
	if rt.Method == "" || rt.Scope == "" || rt.Handler == nil || !strings.HasPrefix(rt.Path, "/") {
		return fmt.Errorf("platform: plugin %q route needs method, absolute path, scope and handler", p.name)
	}
	rt.Method = strings.ToUpper(rt.Method)
	for _, e := range p.r.routes {
		if e.plugin == p.name && e.route.Method == rt.Method && e.route.Path == rt.Path {
			return fmt.Errorf("platform: plugin %q mounts %s %s twice", p.name, rt.Method, rt.Path)
		}
	}
	p.r.routes = append(p.r.routes, mounted{plugin: p.name, route: rt})
	return nil
}

// UseMiddleware registers middleware owned by the plugin; its name is
// prefixed with the plugin name so two plugins cannot collide.
func (p *PluginRegistry) UseMiddleware(m Middleware) error {
	m.Name = p.name + "/" + m.Name
	return p.r.UseMiddleware(m)
}

// AddTransport registers a transport owned by the plugin.
func (p *PluginRegistry) AddTransport(t Transport) error { return p.r.AddTransport(t) }

// OnStop registers a cleanup callback.
func (p *PluginRegistry) OnStop(fn func(context.Context) error) { p.r.OnStop(fn) }

// ResolveSecret resolves a secret reference (keychain://, helper://, file://,
// op://) through Station's resolver. Every value it returns is also added to
// the log redaction scope before the caller sees it, so a resolved key cannot
// later appear in a log line or captured payload.
func (p *PluginRegistry) ResolveSecret(ctx context.Context, ref string) (string, error) {
	res, err := p.r.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	val := res.Secret.Reveal()
	if p.r.scope != nil {
		p.r.scope.Add("resolved-secret", val)
	}
	return val, nil
}

// RedactScope returns Station's shared redaction scope, which learns every
// secret resolved through ResolveSecret. Plugins use it to build payload
// sanitizers that recognize known credentials as well as pattern/PII rules.
func (p *PluginRegistry) RedactScope() *redact.Scope {
	if p.r.scope == nil {
		p.r.scope = redact.NewScope()
	}
	return p.r.scope
}

// orderedMiddleware returns the middleware sorted outermost first.
func (r *Registry) orderedMiddleware() []Middleware {
	out := append([]Middleware(nil), r.middleware...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Phase < out[j].Phase })
	return out
}

// Explain describes the assembled chain, outermost first, one line each.
func (r *Registry) Explain() []string {
	var lines []string
	for _, m := range r.orderedMiddleware() {
		lines = append(lines, fmt.Sprintf("%4d %-28s failure=%s", m.Phase, m.Name, m.Failure))
	}
	return lines
}
