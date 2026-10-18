package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"
)

func TestManagedGatewayCLIAndRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "gateway.db")
	api := filepath.Join(dir, "api.yaml")
	keys := filepath.Join(dir, "clients.yaml")
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	if err := os.WriteFile(keys, []byte(fmt.Sprintf("- {id: app, sha256: %x, scopes: [llm-gateway.chat, llm-gateway.models.read]}\n", sha256.Sum256([]byte("test-key")))), 0600); err != nil {
		t.Fatal(err)
	}
	bootstrap := fmt.Sprintf(`listen: 127.0.0.1:0
auth: {mode: enforce, keys_file: %q}
plugins:
  llm-gateway:
    catalog: {refresh: false, cache_dir: %q}
    ledger: {store: sqlite, sqlite: {path: %q}}
    providers:
      - {id: disabled, dialect: openai, base_url: %q, enabled: false, api_key_env: STATION_TEST_MISSING_KEY, discover_models: true}
      - {id: local, dialect: openai, base_url: %q}
    models:
      - name: hidden
        enabled: false
        targets: [{provider: local, model: upstream}]
      - name: unavailable
        targets: [{provider: disabled, model: upstream}]
      - name: chat
        targets: [{provider: disabled, model: upstream}, {provider: local, model: upstream}]
`, keys, dir, db, up.URL, up.URL)
	if err := os.WriteFile(api, []byte(bootstrap), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runGatewayStore([]string{"import", "-db", db, "-api-config", api, "-expected", "0", "-actor", "operator"}); code != 0 {
		t.Fatalf("import: %d", code)
	}
	if _, err := loadAPIApp(t.Context(), api, validateLogging(), db); err == nil {
		t.Fatal("unactivated store served")
	}
	if code := runGatewayStore([]string{"activate", "-db", db, "-revision", "1", "-expected", "0", "-actor", "operator"}); code != 0 {
		t.Fatal("activate failed")
	}
	// Imported credentials must be independent of the old YAML registry.
	if err := os.Remove(keys); err != nil {
		t.Fatal(err)
	}
	app, err := loadAPIApp(t.Context(), api, validateLogging(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	send := func(h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/llm-gateway/v1/"+path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := send(app.Handler(), "", "GET", "models", "").Code; got != 401 {
		t.Fatalf("missing credential: %d", got)
	}
	if got := send(app.Handler(), "test-key", "GET", "usage", "").Code; got != 403 {
		t.Fatalf("scope boundary: %d", got)
	}
	w := send(app.Handler(), "test-key", "GET", "models", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"chat"`) || strings.Contains(w.Body.String(), `"hidden"`) || strings.Contains(w.Body.String(), `"unavailable"`) {
		t.Fatalf("enabled models: %d %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"hidden", "unavailable"} {
		w := send(app.Handler(), "test-key", "POST", "chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, name))
		if w.Code < 400 {
			t.Fatalf("disabled model invoked: %s", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("disabled provider or alias reached network")
	}
	if got := send(app.Handler(), "test-key", "POST", "chat/completions", `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`).Code; got != 200 {
		t.Fatalf("fallback from disabled provider: %d", got)
	}
	out := filepath.Join(dir, "document.json")
	if code := runGatewayStore([]string{"show", "-db", db, "-output", out}); code != 0 {
		t.Fatal("export failed")
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "test-key") {
		t.Fatal("raw credential persisted")
	}
	doc, err := llmgateway.DecodeManagementDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.Clients = nil
	doc.Gateway.Models[2].Enabled = new(bool) // disable the remaining alias
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(out, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code := runGatewayStore([]string{"stage", "-db", db, "-input", out, "-expected", "1", "-actor", "operator"}); code != 0 {
		t.Fatal("stage failed")
	}
	if code := runGatewayStore([]string{"activate", "-db", db, "-revision", "2", "-expected", "1", "-actor", "operator"}); code != 0 {
		t.Fatal("second activate failed")
	}
	if got := send(app.Handler(), "test-key", "GET", "models", "").Code; got != 200 {
		t.Fatal("activation changed the running snapshot")
	}
	restarted, err := loadAPIApp(t.Context(), api, validateLogging(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(context.Background())
	if got := send(restarted.Handler(), "test-key", "GET", "models", "").Code; got != 401 {
		t.Fatalf("revoked credential accepted after restart: %d", got)
	}
}

func TestManagedGatewayDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	api := filepath.Join(dir, "api.yaml")
	if err := os.WriteFile(api, []byte("plugins: {llm-gateway: {}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.db")
	if _, err := loadAPIApp(t.Context(), api, validateLogging(), missing); err == nil {
		t.Fatal("missing DB did not fail closed")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("startup created a missing DB")
	}
	if code := runValidate([]string{"-gateway-db", missing}); code != 2 {
		t.Fatal("gateway DB flag ignored without API config")
	}
}

func TestStoredClientLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "gateway.db")
	s, err := llmgateway.OpenManagementStore(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	doc := llmgateway.ManagementDocument{SchemaVersion: 1, Gateway: llmgateway.Config{
		Providers: []llmgateway.ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: "http://127.0.0.1:1"}},
		Models:    []llmgateway.ModelConfig{{Name: "chat", Targets: []llmgateway.TargetConfig{{Provider: "p", Model: "upstream"}}}},
	}}
	r, err := s.Save(t.Context(), doc, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(t.Context(), r.ID, 0, "operator"); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "client.token")
	if err := manageStoredClient("issue", db, "app", output, "llm-gateway.chat", r.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(output)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("credential output not private")
	}
	status, err := s.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.Revision(t.Context(), status.LatestRevision)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(string(raw)))))
	if len(latest.Document.Clients) != 1 || latest.Document.Clients[0].SHA256 != want {
		t.Fatal("credential hash mismatch")
	}
	active, err := s.Active(t.Context())
	if err != nil || len(active.Document.Clients) != 0 {
		t.Fatal("credential issue unexpectedly activated a revision")
	}
	staleOutput := filepath.Join(dir, "stale.token")
	if err := manageStoredClient("issue", db, "stale", staleOutput, "llm-gateway.chat", r.ID, "operator"); err == nil {
		t.Fatal("stale issue succeeded")
	}
	if _, err := os.Stat(staleOutput); !os.IsNotExist(err) {
		t.Fatal("stale issue left a credential file")
	}
	if err := manageStoredClient("revoke", db, "app", "", "", latest.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	status, _ = s.Status(t.Context())
	latest, err = s.Revision(t.Context(), status.LatestRevision)
	if err != nil || len(latest.Document.Clients) != 0 {
		t.Fatal("revocation not staged")
	}
}
