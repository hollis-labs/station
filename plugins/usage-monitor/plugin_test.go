package usagemonitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/station/platform"
	"gopkg.in/yaml.v3"
)

func testApp(t *testing.T, mode string) *platform.App {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pc := Config{Database: filepath.Join(dir, "monitor.db"), Accounts: []Account{{ID: "claude", Provider: "anthropic", Kind: "subscription", Enabled: true}}, Sources: []Source{{ID: "laptop", AccountID: "claude", Collector: "push", Principal: "reporter"}}}
	var node yaml.Node
	if err := node.Encode(pc); err != nil {
		t.Fatal(err)
	}
	keys := []platform.KeyConfig{}
	for _, k := range []struct{ id, scope string }{{"operator", "usage-monitor.*"}, {"reader", ScopeRead}, {"reporter", ScopeIngest}, {"intruder", ScopeIngest}} {
		h := sha256.Sum256([]byte(k.id + "-key"))
		keys = append(keys, platform.KeyConfig{ID: k.id, SHA256: hex.EncodeToString(h[:]), Scopes: []string{k.scope}})
	}
	cfg := &platform.Config{Auth: platform.AuthConfig{Mode: mode, Keys: keys}, Plugins: map[string]yaml.Node{"usage-monitor": node}}
	app, err := platform.Assemble(t.Context(), platform.Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Plugins: []platform.Plugin{New()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(context.Background()) })
	return app
}

func request(t *testing.T, app *platform.App, method, path, token string, body any) *httptest.ResponseRecorder {
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
	app := testApp(t, "enforce")
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
			app := testApp(t, mode)
			w := request(t, app, "GET", "/accounts", "", nil)
			if w.Code != 401 {
				t.Fatalf("%s bypassed authorization: %d", mode, w.Code)
			}
		})
	}
	app := testApp(t, "enforce")
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

func TestPushOnlyPollerWaitsForShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	p := &poller{}
	go func() { done <- p.Serve(ctx, nil) }()
	select {
	case <-done:
		t.Fatal("push-only plugin stopped its host")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("poller did not stop")
	}
}
