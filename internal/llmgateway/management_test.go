package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/libs/security/auth"
)

type managementTestEnv struct {
	t     *testing.T
	store *ManagementStore
	path  string
	app   *testApp
	logs  *bytes.Buffer
	keys  []testKey
}

func newManagementTestEnv(t *testing.T) *managementTestEnv {
	t.Helper()
	dir := managementTempDir(t)
	path := filepath.Join(dir, "gateway.db")
	s, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(openaiOK))
	t.Cleanup(up.Close)
	doc := managementFixture()
	doc.Gateway.Providers[0].APIKeyRef = ""
	doc.Gateway.Providers[0].BaseURL = up.URL
	doc.Gateway.Policy = PolicyConfig{Mode: "enforce", DefaultEffect: "allow"}
	doc.Gateway.Catalog = CatalogConfig{Refresh: new(bool), CacheDir: dir}
	r, err := s.Save(t.Context(), doc, 0, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(t.Context(), r.ID, 0, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	e := &managementTestEnv{t: t, store: s.ManagementStore, path: path, logs: &bytes.Buffer{}, keys: []testKey{
		{ID: "operator", SHA256: hashOf("operator-key"), Scopes: []string{"llm-gateway.*"}},
		{ID: "reader", SHA256: hashOf("reader-key"), Scopes: []string{ScopeManagementRead}},
		{ID: "editor", SHA256: hashOf("editor-key"), Scopes: []string{ScopeManagementRead, ScopeManagementWrite, ScopeManagementActivate}},
		{ID: "app", SHA256: hashOf("app-key"), Scopes: []string{ScopeChat, ScopeModelRead}},
	}}
	e.app = e.assemble(r.ID, "enforce")
	return e
}

func (e *managementTestEnv) assemble(id int64, mode string) *testApp {
	e.t.Helper()
	r, err := e.store.Revision(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	app, err := assembleTest(e.t.Context(), r.Document.Gateway, testOptions{
		keys: e.keys, mode: auth.Mode(mode), logger: logger,
		Options: Options{Management: e.store, RunningRevision: id},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = app.Close(context.Background()) })
	return app
}

func (e *managementTestEnv) request(method, path, token, etag string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "/api/llm-gateway/v1/management"+path, bytes.NewReader(raw))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.app.Handler().ServeHTTP(w, r)
	return w
}

func requireManagementStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("management response may be cached")
	}
}

func TestManagementAuthorization(t *testing.T) {
	e := newManagementTestEnv(t)
	for _, mode := range []string{"enforce", "permissive", "off"} {
		t.Run(mode, func(t *testing.T) {
			e.app = e.assemble(1, mode)
			for token, want := range map[string]int{"": 401, "bad-key": 401, "app-key": 403, "reader-key": 200, "operator-key": 200} {
				requireManagementStatus(t, e.request("GET", "/status", token, "", nil), want)
			}
			requireManagementStatus(t, e.request("PUT", "/budgets", "reader-key", `"1"`, map[string]any{"budgets": []BudgetConfig{}}), 403)
		})
	}
	e.app = e.assemble(1, "enforce")
	for _, header := range []string{"X-Api-Key", "Cookie"} {
		r := httptest.NewRequest("GET", "/api/llm-gateway/v1/management/status", nil)
		r.Header.Set(header, "operator-key")
		w := httptest.NewRecorder()
		e.app.Handler().ServeHTTP(w, r)
		requireManagementStatus(t, w, 401)
	}
	r := httptest.NewRequest("GET", "/api/llm-gateway/v1/management/status", nil)
	r.Header.Add("Authorization", "Bearer operator-key")
	r.Header.Add("Authorization", "Bearer operator-key")
	w := httptest.NewRecorder()
	e.app.Handler().ServeHTTP(w, r)
	requireManagementStatus(t, w, 401)
}

func TestManagementConfigurationSelectionAndRestart(t *testing.T) {
	e := newManagementTestEnv(t)
	w := e.request("GET", "/config", "reader-key", "", nil)
	requireManagementStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "sha256") || strings.Contains(w.Body.String(), "clients") || strings.Contains(w.Body.String(), "operator-key") {
		t.Fatal("configuration leaked credentials")
	}
	etag := w.Header().Get("ETag")
	r1, _ := e.store.Revision(t.Context(), 1)
	models := r1.Document.Gateway.Models
	models[0].Enabled = new(bool)
	w = e.request("PUT", "/models", "editor-key", etag, map[string]any{"models": models})
	requireManagementStatus(t, w, 201)
	r2, _ := e.store.Revision(t.Context(), 2)
	if r2.Actor != "editor" || enabled(r2.Document.Gateway.Models[0].Enabled) {
		t.Fatal("mutation or verified actor not persisted")
	}
	requireManagementStatus(t, e.request("PUT", "/budgets", "editor-key", etag, map[string]any{"budgets": []BudgetConfig{}}), 412)
	visible := func(app *testApp) bool {
		r := httptest.NewRequest("GET", "/api/llm-gateway/v1/models", nil)
		r.Header.Set("Authorization", "Bearer app-key")
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return strings.Contains(w.Body.String(), `"chat"`)
	}
	if !visible(e.app) {
		t.Fatal("stage changed running models")
	}
	w = e.request("POST", "/activate", "editor-key", etag, map[string]any{"revision": 2})
	requireManagementStatus(t, w, 202)
	if !visible(e.app) {
		t.Fatal("selection unexpectedly hot applied")
	}
	w = e.request("GET", "/status", "reader-key", "", nil)
	var status struct {
		Running  int64 `json:"running_revision"`
		Selected int64 `json:"selected_revision"`
		Restart  bool  `json:"restart_required"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Running != 1 || status.Selected != 2 || !status.Restart {
		t.Fatal("pending apply state is wrong", w.Body.String())
	}
	e.app = e.assemble(2, "enforce")
	if visible(e.app) {
		t.Fatal("restart did not apply enablement")
	}
	w = e.request("GET", "/status", "reader-key", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Restart || status.Running != 2 {
		t.Fatal("restart status wrong")
	}
	w = e.request("GET", "/activations?limit=1", "reader-key", "", nil)
	requireManagementStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"actor":"editor"`) {
		t.Fatal("activation actor was not audited")
	}
	w = e.request("POST", "/activate", "editor-key", `"2"`, map[string]any{"revision": 1})
	requireManagementStatus(t, w, 202)
	e.app = e.assemble(1, "enforce")
	if !visible(e.app) {
		t.Fatal("rollback did not restore model")
	}
	requireManagementStatus(t, e.request("GET", "/models?revision=2", "reader-key", "", nil), 200)
	requireManagementStatus(t, e.request("GET", "/revisions?before=2&limit=1", "reader-key", "", nil), 200)
}

func TestManagementValidationAndResourceEdits(t *testing.T) {
	e := newManagementTestEnv(t)
	r1, _ := e.store.Revision(t.Context(), 1)
	doc := gatewayEdit{SchemaVersion: 1, Gateway: &r1.Document.Gateway}
	requireManagementStatus(t, e.request("POST", "/validate", "editor-key", "", doc), 200)
	st, _ := e.store.Status(t.Context())
	if st.LatestRevision != 1 {
		t.Fatal("validation wrote state")
	}
	for _, tc := range []struct {
		path string
		body any
		want int
	}{
		{"/config", map[string]any{"schema_version": 2, "gateway": r1.Document.Gateway}, 422},
		{"/config", map[string]any{"schema_version": 1, "gateway": r1.Document.Gateway, "clients": []any{}}, 400},
		{"/providers", map[string]any{"providers": []ProviderConfig{}}, 422},
		{"/models", map[string]any{"models": []ModelConfig{{Name: "bad", Targets: []TargetConfig{{Provider: "missing", Model: "x"}}}}}, 422},
		{"/budgets", map[string]any{"budgets": []BudgetConfig{{ID: "invalid", MaxUSD: -1}}}, 422},
		{"/policy", map[string]any{"policy": PolicyConfig{Mode: "enforce", DefaultEffect: "misspelled"}}, 422},
		{"/budgets", map[string]any{}, 400},
		{"/budgets", map[string]any{"budgets": nil}, 400},
		{"/budgets", map[string]any{"budgets": []BudgetConfig{}, "actor": "spoofed"}, 400},
	} {
		requireManagementStatus(t, e.request("PUT", tc.path, "editor-key", `"1"`, tc.body), tc.want)
	}
	st, _ = e.store.Status(t.Context())
	if st.LatestRevision != 1 {
		t.Fatal("invalid edits wrote state")
	}
	providers := r1.Document.Gateway.Providers
	providers[0].Enabled = new(bool)
	w := e.request("PUT", "/providers", "editor-key", `"1"`, map[string]any{"providers": providers})
	requireManagementStatus(t, w, 201)
	w = e.request("PUT", "/policy", "editor-key", w.Header().Get("ETag"), map[string]any{"policy": PolicyConfig{Mode: "enforce", DefaultEffect: "deny"}})
	requireManagementStatus(t, w, 201)
	w = e.request("PUT", "/budgets", "editor-key", w.Header().Get("ETag"), map[string]any{"budgets": []BudgetConfig{{ID: "daily", MaxRequests: 20}}})
	requireManagementStatus(t, w, 201)
	for _, resource := range []string{"providers", "models", "policy", "budgets"} {
		requireManagementStatus(t, e.request("GET", "/"+resource, "reader-key", "", nil), 200)
	}
	w = e.request("PUT", "/config", "editor-key", w.Header().Get("ETag"), doc)
	requireManagementStatus(t, w, 201)
}

func TestManagementRequestBoundaries(t *testing.T) {
	e := newManagementTestEnv(t)
	for _, etag := range []string{"", "*", `W/"1"`, `"1", "2"`, "1", `"-1"`} {
		want := 400
		if etag == "" {
			want = 428
		}
		requireManagementStatus(t, e.request("PUT", "/budgets", "editor-key", etag, map[string]any{"budgets": []BudgetConfig{}}), want)
	}
	for _, tc := range []struct {
		raw, media string
		want       int
	}{
		{`{"budgets":[]} {}`, "application/json", 400},
		{`null`, "application/json", 400},
		{`{"budgets":[]}`, "text/plain", 415},
		{strings.Repeat(" ", maxManagementDocument+1), "application/json", 413},
	} {
		r := httptest.NewRequest("PUT", "/api/llm-gateway/v1/management/budgets", strings.NewReader(tc.raw))
		r.Header.Set("Authorization", "Bearer editor-key")
		r.Header.Set("If-Match", `"1"`)
		r.Header.Set("Content-Type", tc.media)
		w := httptest.NewRecorder()
		e.app.Handler().ServeHTTP(w, r)
		requireManagementStatus(t, w, tc.want)
	}
	requireManagementStatus(t, e.request("GET", "/config?revision=99999", "reader-key", "", nil), 404)
	requireManagementStatus(t, e.request("GET", "/config?revision=oops", "reader-key", "", nil), 400)
	requireManagementStatus(t, e.request("GET", "/revisions?limit=1000", "reader-key", "", nil), 400)
}

// Client keys are issued by the identity module now: the old /clients routes are not served, even
// to a caller holding every gateway scope, and a configuration read never carries a client list.
func TestManagementClientRoutesAreGone(t *testing.T) {
	e := newManagementTestEnv(t)
	for _, rt := range [][2]string{{"GET", "/clients"}, {"POST", "/clients"}, {"PUT", "/clients"}, {"DELETE", "/clients?id=app"}, {"POST", "/clients/rotate"}} {
		w := e.request(rt[0], rt[1], "operator-key", `"1"`, map[string]any{"id": "app"})
		if w.Code < 400 {
			t.Fatalf("%s %s is still served: %d %s", rt[0], rt[1], w.Code, w.Body.String())
		}
	}
	w := e.request("GET", "/config", "reader-key", "", nil)
	requireManagementStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "clients") {
		t.Fatal("the configuration read carries a client list")
	}
	st, _ := e.store.Status(t.Context())
	if st.LatestRevision != 1 {
		t.Fatal("a removed route changed state")
	}
}

func TestManagementConcurrentHTTPWriters(t *testing.T) {
	e := newManagementTestEnv(t)
	// slog serializes writes through this handler; inspect results after both requests finish.
	server := httptest.NewServer(e.app.Handler())
	defer server.Close()
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for n := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"budgets":[{"id":"b","max_requests":%d}]}`, n+10)
			r, _ := http.NewRequest("PUT", server.URL+"/api/llm-gateway/v1/management/budgets", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer editor-key")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("If-Match", `"1"`)
			resp, err := server.Client().Do(r)
			if err != nil {
				codes <- 0
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	one, two := <-codes, <-codes
	if (one != 201 || two != 412) && (one != 412 || two != 201) {
		t.Fatalf("concurrent writes: %d %d", one, two)
	}
}

func TestManagementSmokeProbe(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("management probe requires python3")
	}
	e := newManagementTestEnv(t)
	server := httptest.NewServer(e.app.Handler())
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(tokenPath, []byte("operator-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), python, "../../scripts/gateway-management-smoke.py",
		"--base-url", server.URL+"/api/llm-gateway/v1/management", "--token-file", tokenPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte("operator-key")) {
		t.Fatal("probe printed a credential")
	}
	state, err := e.store.Status(t.Context())
	if err != nil || state.LatestRevision != 1 || state.ActiveRevision != 1 {
		t.Fatalf("probe mutated management state: %+v %v", state, err)
	}
}

func TestManagementEmptyCollectionsRoundTrip(t *testing.T) {
	e := newManagementTestEnv(t)
	// A YAML import may leave these slices nil. API callers must still be
	// able to read and write the resources without manufacturing an array.
	rev, err := e.store.Revision(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	rev.Document.Gateway.Providers[0].DiscoverModels = true
	rev.Document.Gateway.Models = nil
	rev.Document.Gateway.Budgets = nil
	if _, err := e.store.Save(t.Context(), *rev.Document, 1, "import"); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"models", "budgets"} {
		w := e.request("GET", "/"+resource, "editor-key", "", nil)
		requireManagementStatus(t, w, 200)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		update := map[string]json.RawMessage{resource: body[resource]}
		requireManagementStatus(t, e.request("PUT", "/"+resource, "editor-key", w.Header().Get("ETag"), update), 201)
	}
}

// Literal provider headers are credentials by another name. The management GET shows their names
// and never their values, and reading then writing the resource back keeps the stored value.
func TestManagementNeverReturnsProviderHeaderValues(t *testing.T) {
	const headerValue = "header-value-not-for-readers"
	e := newManagementTestEnv(t)
	rev, err := e.store.Revision(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	rev.Document.Gateway.Providers[0].Headers = map[string]string{"X-Tenant": headerValue}
	saved, err := e.store.Save(t.Context(), *rev.Document, 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/config", "/providers", "/config?revision=2"} {
		w := e.request("GET", path, "reader-key", "", nil)
		requireManagementStatus(t, w, 200)
		if strings.Contains(w.Body.String(), headerValue) {
			t.Fatalf("GET %s returned a header value: %s", path, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "X-Tenant") || !strings.Contains(w.Body.String(), RedactedHeader) {
			t.Fatalf("GET %s lost the header name or the placeholder: %s", path, w.Body.String())
		}
	}
	// Read the providers, write them back unchanged: the stored value is restored.
	w := e.request("GET", "/providers", "editor-key", "", nil)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	requireManagementStatus(t, e.request("PUT", "/providers", "editor-key", w.Header().Get("ETag"), map[string]json.RawMessage{"providers": body["providers"]}), 201)
	after, err := e.store.Revision(t.Context(), saved.ID+1)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Document.Gateway.Providers[0].Headers["X-Tenant"]; got != headerValue {
		t.Fatalf("the round trip stored %q, want the original value", got)
	}
	// A placeholder with nothing to restore is refused instead of stored.
	doc := managementFixture()
	doc.Gateway.Providers[0].Headers = map[string]string{"X-New": RedactedHeader}
	if _, err := e.store.Save(t.Context(), doc, 0, "operator"); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("a placeholder with no base was saved: %v", err)
	}
}
