package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func hashOf(k string) string { s := sha256.Sum256([]byte(k)); return hex.EncodeToString(s[:]) }

// testPlugin mounts one route and optionally mutates the registry.
type testPlugin struct {
	name     string
	scope    string
	handler  http.Handler
	register func(*PluginRegistry) error
	decoded  any
}

func (p *testPlugin) Name() string { return p.name }
func (p *testPlugin) Register(_ context.Context, reg *PluginRegistry, decode ConfigDecoder) error {
	if p.decoded != nil {
		if err := decode(p.decoded); err != nil {
			return err
		}
	}
	if p.register != nil {
		if err := p.register(reg); err != nil {
			return err
		}
	}
	h := p.handler
	if h == nil {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	}
	return reg.Mount(Route{Method: "GET", Path: "/ping", Scope: p.scope, Handler: h})
}

func mustAssemble(t *testing.T, yaml string, plugins ...Plugin) *App {
	t.Helper()
	cfg, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	app, err := Assemble(context.Background(), Options{Config: cfg, Logger: quiet, Plugins: plugins})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func do(app *App, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func authYAML(scopes string) string {
	return "auth:\n  keys:\n    - id: k1\n      sha256: " + hashOf("secret") + "\n      scopes: [" + scopes + "]\nplugins:\n  demo: {}\n"
}

func TestAuthEnforcedByDefault(t *testing.T) {
	app := mustAssemble(t, authYAML("demo.read"), &testPlugin{name: "demo", scope: "demo.read"})
	if rec := do(app, "GET", "/api/demo/ping", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", rec.Code)
	}
	if rec := do(app, "GET", "/api/demo/ping", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d, want 401", rec.Code)
	}
	if rec := do(app, "GET", "/api/demo/ping", "secret"); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("good token: %d %q", rec.Code, rec.Body.String())
	}
}

func TestScopeIsRequiredPerRoute(t *testing.T) {
	app := mustAssemble(t, authYAML("other.read"), &testPlugin{name: "demo", scope: "demo.read"})
	if rec := do(app, "GET", "/api/demo/ping", "secret"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong scope: %d, want 403", rec.Code)
	}
}

func TestWildcardScope(t *testing.T) {
	app := mustAssemble(t, authYAML(`"demo.*"`), &testPlugin{name: "demo", scope: "demo.read"})
	if rec := do(app, "GET", "/api/demo/ping", "secret"); rec.Code != 200 {
		t.Fatalf("wildcard scope: %d, want 200", rec.Code)
	}
}

func TestUnmappedRouteDenied(t *testing.T) {
	app := mustAssemble(t, authYAML(`"*"`), &testPlugin{name: "demo", scope: "demo.read"})
	if rec := do(app, "GET", "/api/demo/other", "secret"); rec.Code != http.StatusForbidden {
		t.Fatalf("unmapped route: %d, want 403 (fail closed)", rec.Code)
	}
}

func TestHealthzIsOpen(t *testing.T) {
	app := mustAssemble(t, authYAML("demo.read"), &testPlugin{name: "demo", scope: "demo.read"})
	if rec := do(app, "GET", "/healthz", ""); rec.Code != 200 {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestPluginOnlyRegistersWithConfigBlock(t *testing.T) {
	app := mustAssemble(t, "auth: {mode: off}\n", &testPlugin{name: "demo", scope: "demo.read"})
	if rec := do(app, "GET", "/api/demo/ping", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unconfigured plugin served: %d", rec.Code)
	}
}

func TestConfigForUnknownPluginRejected(t *testing.T) {
	cfg, _ := ParseConfig([]byte("plugins:\n  ghost: {}\n"))
	if _, err := Assemble(context.Background(), Options{Config: cfg, Logger: quiet}); err == nil {
		t.Fatal("config for a plugin that does not exist was accepted")
	}
}

func TestParseConfigIsStrict(t *testing.T) {
	if _, err := ParseConfig([]byte("auth:\n  mdoe: off\n")); err == nil {
		t.Fatal("misspelled auth key accepted")
	}
}

func TestPluginBlockIsStrict(t *testing.T) {
	type blk struct {
		Known string `yaml:"known"`
	}
	cfg, _ := ParseConfig([]byte("auth: {mode: off}\nplugins:\n  demo:\n    knwon: x\n"))
	_, err := Assemble(context.Background(), Options{Config: cfg, Logger: quiet,
		Plugins: []Plugin{&testPlugin{name: "demo", scope: "s", decoded: &blk{}}}})
	if err == nil {
		t.Fatal("unknown key in plugin block accepted")
	}
}

func TestAuthKeyMustBeHashed(t *testing.T) {
	cfg, _ := ParseConfig([]byte("auth:\n  keys:\n    - id: k\n      sha256: not-a-hash\n"))
	if _, err := Assemble(context.Background(), Options{Config: cfg, Logger: quiet}); err == nil {
		t.Fatal("raw-looking key accepted")
	}
}

func TestMiddlewareOrderedByPhaseNotRegistration(t *testing.T) {
	var order []string
	mk := func(name string, phase Phase) Middleware {
		return Middleware{Name: name, Phase: phase, Failure: FailOpen, Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}}
	}
	cfg, _ := ParseConfig([]byte("auth: {mode: off}\nplugins:\n  demo: {}\n"))
	app, err := Assemble(context.Background(), Options{
		Config: cfg, Logger: quiet,
		// Registered inner-first on purpose.
		Middleware: []Middleware{mk("inner", PhaseCache), mk("outer", PhaseTrace+1)},
		Plugins:    []Plugin{&testPlugin{name: "demo", scope: "demo.read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	do(app, "GET", "/api/demo/ping", "")
	if got := strings.Join(order, ","); got != "outer,inner" {
		t.Fatalf("order = %s, want outer,inner", got)
	}
}

func TestMiddlewareDescriptorValidation(t *testing.T) {
	noop := func(h http.Handler) http.Handler { return h }
	reg := NewRegistry(quiet)
	cases := map[string]Middleware{
		"no name":         {Phase: PhaseTrace, Failure: FailOpen, Wrap: noop},
		"no wrap":         {Name: "a", Phase: PhaseTrace, Failure: FailOpen},
		"no failure mode": {Name: "b", Phase: PhaseTrace, Wrap: noop},
		"authn fail-open": {Name: "c", Phase: PhaseAuthn, Failure: FailOpen, Wrap: noop},
		"log fail-closed": {Name: "d", Phase: PhaseAccessLog, Failure: FailClosed, Wrap: noop},
	}
	for name, m := range cases {
		if err := reg.UseMiddleware(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Middleware{Name: "e", Phase: PhaseAuthn, Failure: FailOpen, Reason: "health probe path only", Wrap: noop}
	if err := reg.UseMiddleware(ok); err != nil {
		t.Errorf("stated reason rejected: %v", err)
	}
	if err := reg.UseMiddleware(ok); err == nil {
		t.Error("duplicate middleware name accepted")
	}
}

func TestRouteNeedsScopeAndMethod(t *testing.T) {
	reg := NewRegistry(quiet)
	p, _ := reg.Scoped("demo")
	h := http.NotFoundHandler()
	for name, rt := range map[string]Route{
		"no scope":   {Method: "GET", Path: "/a", Handler: h},
		"no method":  {Path: "/a", Scope: "s", Handler: h},
		"no slash":   {Method: "GET", Path: "a", Scope: "s", Handler: h},
		"no handler": {Method: "GET", Path: "/a", Scope: "s"},
	} {
		if err := p.Mount(rt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInvalidAndDuplicatePluginNames(t *testing.T) {
	reg := NewRegistry(quiet)
	for _, bad := range []string{"", "Bad", "a/b", "../x", "-x"} {
		if _, err := reg.Scoped(bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := reg.Scoped("ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Scoped("ok"); err == nil {
		t.Error("duplicate plugin accepted")
	}
}

func TestRecoverContainsPanic(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret-internal-detail") })
	app := mustAssemble(t, "auth: {mode: off}\nplugins:\n  demo: {}\n", &testPlugin{name: "demo", scope: "s", handler: boom})
	rec := do(app, "GET", "/api/demo/ping", "")
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-internal-detail") {
		t.Fatal("panic value leaked to the caller")
	}
}

func TestRequestIDStamped(t *testing.T) {
	app := mustAssemble(t, "auth: {mode: off}\nplugins:\n  demo: {}\n", &testPlugin{name: "demo", scope: "s"})
	rec := do(app, "GET", "/api/demo/ping", "")
	if id := rec.Header().Get("X-Request-Id"); len(id) != 16 {
		t.Fatalf("request id %q", id)
	}
}

type countingTransport struct{ served atomic.Int32 }

func (*countingTransport) Name() string { return "counting" }
func (c *countingTransport) Serve(ctx context.Context, h http.Handler) error {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code == 200 {
		c.served.Add(1)
	}
	<-ctx.Done()
	return nil
}

func TestExtraTransportServesAssembledHandlerAndStopHookRuns(t *testing.T) {
	var stopped atomic.Bool
	tr := &countingTransport{}
	cfg, _ := ParseConfig([]byte("auth: {mode: off}\nplugins:\n  demo: {}\n"))
	app, err := Assemble(context.Background(), Options{
		Config: cfg, Logger: quiet, Transports: []Transport{tr},
		Plugins: []Plugin{&testPlugin{name: "demo", scope: "s", register: func(r *PluginRegistry) error {
			r.OnStop(func(context.Context) error { stopped.Store(true); return nil })
			return nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ln := newLocalListener(t)
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, ln) }()
	waitFor(t, func() bool { return tr.served.Load() == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("OnStop hook did not run")
	}
}
