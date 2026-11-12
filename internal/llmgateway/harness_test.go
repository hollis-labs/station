package llmgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
	"github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/libs/security/redact"
	"github.com/hollis-labs/libs/security/secrets"
	"gopkg.in/yaml.v3"
)

// The tests drive the gateway the way the service does: its routes behind the libs auth guard with
// a scope table, with the API-key bridge and request ids in front. The keys here are a test
// fixture; the service verifies identity-issued keys instead.

type testKey struct {
	ID     string   `yaml:"id"`
	SHA256 string   `yaml:"sha256"`
	Scopes []string `yaml:"scopes"`
}

type testOptions struct {
	keys    []testKey
	mode    auth.Mode
	logger  *slog.Logger
	secrets *secrets.Resolver
	Options
}

type testApp struct {
	h   http.Handler
	svc *Service
}

func (a *testApp) Handler() http.Handler           { return a.h }
func (a *testApp) Close(ctx context.Context) error { return a.svc.Close(ctx) }

type testHost struct {
	logger  *slog.Logger
	secrets *secrets.Resolver
	scope   *redact.Scope
}

func (h *testHost) Logger() *slog.Logger       { return h.logger }
func (h *testHost) RedactScope() *redact.Scope { return h.scope }
func (h *testHost) ResolveSecret(ctx context.Context, ref string) (string, error) {
	res, err := h.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	v := res.Secret.Reveal()
	h.scope.Add("resolved-secret", v)
	return v, nil
}

func assembleTest(ctx context.Context, cfg Config, to testOptions) (*testApp, error) {
	logger := to.logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	res := to.secrets
	if res == nil {
		res = secrets.New()
	}
	mode := to.mode
	if mode == "" {
		mode = auth.ModeEnforce
	}
	if cfg.Ledger.Store == "sqlite" && to.Store == nil {
		db, err := openTestDB(ctx, filepath.Join(os.TempDir(), "llmgw-test-"+randomSuffix()+".db"))
		if err != nil {
			return nil, err
		}
		to.Store = db
	}
	svc, err := Start(ctx, &testHost{logger: logger, secrets: res, scope: redact.NewScope()}, cfg, to.Options)
	if err != nil {
		return nil, err
	}
	table := auth.NewScopeTable()
	mux := http.NewServeMux()
	for _, rt := range svc.Routes() {
		table.Route(rt.Method, RoutePrefix+rt.Path, rt.Scope)
		mux.Handle(rt.Method+" "+RoutePrefix+rt.Path, rt.Limited())
	}
	keys := make([]auth.APIKey, 0, len(to.keys))
	for _, k := range to.keys {
		keys = append(keys, auth.APIKey{ID: k.ID, Hash: k.SHA256, Scopes: k.Scopes})
	}
	guard, err := auth.New(auth.Config{Mode: mode, Verifier: auth.NewAPIKeyVerifier(keys...), Scopes: table, Logger: logger})
	if err != nil {
		_ = svc.Close(ctx)
		return nil, err
	}
	return &testApp{h: WithRequestID(APIKeyBridge(guard.Middleware(mux))), svc: svc}, nil
}

// assembleYAML parses a test document and assembles the gateway from it. auth.mode "off" in the
// document selects the guard's off mode.
func assembleYAML(ctx context.Context, y string, to testOptions) (*testApp, error) {
	cfg, keys, err := parseTestYAML(y)
	if err != nil {
		return nil, err
	}
	to.keys = keys
	var doc struct {
		Auth struct {
			Mode string `yaml:"mode"`
		} `yaml:"auth"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		return nil, err
	}
	if doc.Auth.Mode == "off" {
		to.mode = auth.ModeOff
	}
	return assembleTest(ctx, cfg, to)
}

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// parseTestYAML reads the shape the older tests were written in: top-level auth.keys and the
// gateway block under plugins.llm-gateway.
func parseTestYAML(y string) (Config, []testKey, error) {
	var doc struct {
		Auth struct {
			Keys []testKey `yaml:"keys"`
		} `yaml:"auth"`
		Plugins map[string]yaml.Node `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		return Config{}, nil, err
	}
	var cfg Config
	if node, ok := doc.Plugins["llm-gateway"]; ok {
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return Config{}, nil, err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, nil, err
		}
	}
	return cfg, doc.Auth.Keys, nil
}

var (
	openMu     sync.Mutex
	openStore_ []*store.Store
)

// openTestDB opens (creating) the gateway database at path with the schema applied, as the
// database module does at start. Handles are closed when the test binary exits, so a test can
// open the same file twice to stand in for two processes.
func openTestDB(ctx context.Context, path string) (*store.Store, error) {
	cfg := store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path, CreateParentDir: true}}
	st, err := store.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := migrate.Ensure(ctx, cfg, st, migrate.Source{FS: Migrations, Root: "migrations"}); err != nil {
		_ = st.Close()
		return nil, err
	}
	openMu.Lock()
	openStore_ = append(openStore_, st)
	openMu.Unlock()
	return st, nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	openMu.Lock()
	for _, st := range openStore_ {
		_ = st.Close()
	}
	openMu.Unlock()
	os.Exit(code)
}

func openSQLiteStore(ctx context.Context, path string) (*sqliteStore, error) {
	st, err := openTestDB(ctx, path)
	if err != nil {
		return nil, err
	}
	return &sqliteStore{st: st}, nil
}

// testManagement is a ManagementStore with the Close the tests used to call.
type testManagement struct{ *ManagementStore }

func (testManagement) Close() error { return nil }

func OpenManagementStore(ctx context.Context, path string) (testManagement, error) {
	st, err := openTestDB(ctx, path)
	if err != nil {
		return testManagement{}, err
	}
	return testManagement{NewManagementStore(st)}, nil
}
