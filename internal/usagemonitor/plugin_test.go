package usagemonitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
	"github.com/hollis-labs/libs/security/auth"
)

// testApp drives the monitor the way the service does: its routes behind the libs auth guard with a
// scope table. The keys are a test fixture; the service verifies identity-issued keys instead.
type testApp struct {
	h   http.Handler
	svc *Service
}

type testHost struct{ logger *slog.Logger }

func (h testHost) Logger() *slog.Logger { return h.logger }
func (testHost) ResolveSecret(context.Context, string) (string, error) {
	return "", errors.New("no secrets in this test")
}

func newTestApp(t *testing.T, mode auth.Mode, cfg Config, st *store.Store) *testApp {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := Start(t.Context(), testHost{logger}, cfg, Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	table := auth.NewScopeTable()
	mux := http.NewServeMux()
	for _, rt := range svc.Routes() {
		table.Route(rt.Method, RoutePrefix+rt.Path, rt.Scope)
		mux.Handle(rt.Method+" "+RoutePrefix+rt.Path, rt.Limited())
	}
	var keys []auth.APIKey
	for _, k := range []struct{ id, scope string }{{"operator", "usage-monitor.*"}, {"reader", ScopeRead}, {"reporter", ScopeIngest}, {"intruder", ScopeIngest}} {
		h := sha256.Sum256([]byte(k.id + "-key"))
		keys = append(keys, auth.APIKey{ID: k.id, Hash: hex.EncodeToString(h[:]), Scopes: []string{k.scope}})
	}
	guard, err := auth.New(auth.Config{Mode: mode, Verifier: auth.NewAPIKeyVerifier(keys...), Scopes: table, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return &testApp{h: guard.Middleware(mux), svc: svc}
}

func (a *testApp) Handler() http.Handler { return a.h }

func startTestApp(t *testing.T, mode string) *testApp {
	t.Helper()
	st, err := openTestDB(t.Context(), filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := Config{Accounts: []Account{{ID: "claude", Provider: "anthropic", Kind: "subscription", Enabled: true}}, Sources: []Source{{ID: "laptop", AccountID: "claude", Collector: "push", Principal: "reporter"}}}
	return newTestApp(t, auth.Mode(mode), cfg, st)
}

func request(t *testing.T, app *testApp, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "/api/usage-monitor/v1"+path, bytes.NewReader(raw))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	return w
}

func TestHTTPReadManageIngestPermissions(t *testing.T) {
	app := startTestApp(t, "enforce")
	batch := Batch{Observations: []Observation{testObservation("one", time.Now().UTC())}}
	for _, tc := range []struct {
		method, path, token string
		body                any
		want                int
	}{
		{"GET", "/accounts", "", nil, 401},
		{"GET", "/accounts", "reader-key", nil, 200},
		{"GET", "/accounts", "reporter-key", nil, 403},
		{"POST", "/sources/laptop/ingest", "reader-key", batch, 403},
		{"POST", "/sources/laptop/ingest", "intruder-key", batch, 403},
		{"POST", "/sources/laptop/ingest", "reporter-key", batch, 200},
		{"POST", "/sources/laptop/ingest", "reporter-key", batch, 200},
		{"POST", "/sources/laptop/collect", "reader-key", nil, 403},
		{"GET", "/observations?limit=0", "reader-key", nil, 400},
		{"GET", "/events?after=-1", "reader-key", nil, 400},
		{"GET", "/events?limit=1&limit=2", "reader-key", nil, 400},
		{"GET", "/summary?after=1", "reader-key", nil, 400},
	} {
		t.Run(fmt.Sprintf("%s %s %s", tc.method, tc.path, tc.token), func(t *testing.T) {
			w := request(t, app, tc.method, tc.path, tc.token, tc.body)
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
		})
	}
	w := request(t, app, "GET", "/observations", "reader-key", nil)
	var page Page[Record[Observation]]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Actor != "reporter" || page.Items[0].Source != "laptop" {
		t.Fatal(w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private data is cacheable")
	}
	account := Account{ID: "claude", Provider: "anthropic", Kind: "subscription", Enabled: false, Label: "paused"}
	update := map[string]any{"account": account, "expected_revision": 1}
	if w = request(t, app, "PUT", "/accounts/claude", "reader-key", update); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w = request(t, app, "PUT", "/accounts/claude", "operator-key", update); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(t, app, "PUT", "/accounts/claude", "operator-key", update); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(t, app, "POST", "/sources/laptop/ingest", "reporter-key", batch); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPluginRejectsUnverifiedHostsAndUnknownFields(t *testing.T) {
	for _, mode := range []string{"off", "permissive"} {
		t.Run(mode, func(t *testing.T) {
			app := startTestApp(t, mode)
			w := request(t, app, "GET", "/accounts", "", nil)
			if w.Code != 401 {
				t.Fatalf("%s bypassed authorization: %d", mode, w.Code)
			}
		})
	}
	app := startTestApp(t, "enforce")
	for _, raw := range []string{`{"observations":[],"arbitrary_secret":"secret"}`, `{"events":[]} {"events":[]}`} {
		r := httptest.NewRequest("POST", "/api/usage-monitor/v1/sources/laptop/ingest", strings.NewReader(raw))
		r.Header.Set("Authorization", "Bearer reporter-key")
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestPushOnlyServiceWaitsForShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	svc := &Service{}
	go func() { svc.Run(ctx); close(done) }()
	select {
	case <-done:
		t.Fatal("push-only service stopped its host")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
}

// The design case of the service store: collectors write while the API reads, with one write
// connection and a single read connection. Nothing may wedge or fail, and every write lands.
func TestCollectorsWriteWhileAPIReadsOnOneReadConnection(t *testing.T) {
	cfg := store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: filepath.Join(t.TempDir(), "monitor.db"), ReadConns: 1}}
	st, err := store.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := migrate.Ensure(t.Context(), cfg, st, migrate.Source{FS: Migrations, Root: "migrations"}); err != nil {
		t.Fatal(err)
	}
	app := newTestApp(t, auth.ModeEnforce, Config{
		Accounts: []Account{{ID: "claude", Provider: "anthropic", Kind: "subscription", Enabled: true}, {ID: "codex", Provider: "openai", Kind: "subscription", Enabled: true}},
		Sources:  []Source{{ID: "laptop", AccountID: "claude", Collector: "push", Principal: "reporter"}},
	}, st)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	collector := NewStore(st)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	const writers, perWriter = 4, 25
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				b := Batch{Observations: []Observation{testObservation(fmt.Sprintf("w%d-%d", w, i), time.Now().UTC())}, Events: []Event{testEvent(fmt.Sprintf("e%d-%d", w, i))}}
				if err := collector.Ingest(ctx, "claude", fmt.Sprintf("collector-%d", w), "collector", b); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				for _, path := range []string{"/accounts", "/sources", "/observations?limit=50", "/events?limit=50", "/summary"} {
					w := request(t, app, "GET", path, "reader-key", nil)
					if w.Code != 200 {
						errs <- fmt.Errorf("GET %s = %d %s", path, w.Code, w.Body.String())
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := st.Reader().QueryRowContext(ctx, "SELECT count(*) FROM observations").Scan(&n); err != nil || n != writers*perWriter {
		t.Fatalf("observations = %d (%v), want %d", n, err, writers*perWriter)
	}
}
