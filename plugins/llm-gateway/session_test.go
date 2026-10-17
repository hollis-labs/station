package llmgateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/station/platform"
)

func TestConversationIsolation(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Station-Session", "conversation-1")
	a := Caller{ID: "app-a", Known: true}
	b := Caller{ID: "app-b", Known: true}
	first, err := conversationOf(r, a)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := conversationOf(r, a)
	other, _ := conversationOf(r, b)
	if first == "" || first != same || first == other {
		t.Fatal("conversation not stable and principal scoped")
	}
	r.Header.Set("X-Station-Session", "conversation-2")
	next, _ := conversationOf(r, a)
	if next == first {
		t.Fatal("conversation collision")
	}
	if _, err := conversationOf(r, Caller{}); err == nil {
		t.Fatal("unauthenticated correlation accepted")
	}
	r.Header.Add("X-Station-Session", "second")
	if _, err := conversationOf(r, a); err == nil {
		t.Fatal("duplicate session accepted")
	}
	r.Header.Set("X-Station-Session", "bad session")
	if _, err := conversationOf(r, a); err == nil {
		t.Fatal("invalid session accepted")
	}
}

func TestOpenCodeConversationRelay(t *testing.T) {
	var sessions []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		if r.Header.Get("X-Station-Session") != "" {
			t.Error("raw session forwarded")
		}
		openaiOK(w, r)
	}))
	defer up.Close()
	cfg, err := platform.ParseConfig([]byte(fmt.Sprintf(`
auth:
  keys:
    - {id: app, sha256: %s, scopes: [llm-gateway.chat, llm-gateway.models.read]}
plugins:
  llm-gateway:
    catalog: {refresh: false, cache_dir: %q}
    providers:
      - {id: go, dialect: openai, base_url: %q, opencode_session: true}
    models:
      - name: model
        targets: [{provider: go, model: upstream}]
`, hashOf("good"), t.TempDir(), up.URL)))
	if err != nil {
		t.Fatal(err)
	}
	app, err := platform.Assemble(t.Context(), platform.Options{Config: cfg, Logger: quiet, Plugins: []platform.Plugin{New()}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	send := func(session string) int {
		r := httptest.NewRequest("POST", "/api/llm-gateway/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer good")
		r.Header.Set("X-Opencode-Session", "spoofed")
		if session != "" {
			r.Header.Set("X-Station-Session", session)
		}
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, r)
		_, _ = io.Copy(io.Discard, w.Result().Body)
		return w.Code
	}
	if got := send(""); got != 400 {
		t.Fatalf("missing session: %d", got)
	}
	if len(sessions) != 0 {
		t.Fatal("missing session reached upstream")
	}
	for i := 0; i < 2; i++ {
		if got := send("turns"); got != 200 {
			t.Fatalf("valid session: %d", got)
		}
	}
	if sessions[0] != sessions[1] || sessions[0] == "spoofed" || !strings.HasPrefix(sessions[0], "station-") {
		t.Fatal("bad upstream session")
	}
	r := httptest.NewRequest("POST", "/api/llm-gateway/v1/catalog/refresh", nil)
	r.Header.Set("Authorization", "Bearer good")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("read scope allowed refresh: %d", w.Code)
	}
}

func TestTargetOperationRestrictions(t *testing.T) {
	cfg := Config{Providers: []ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: "http://localhost/v1"}}, Models: []ModelConfig{{Name: "gpt", Targets: []TargetConfig{{Provider: "p", Model: "upstream", Operations: []Operation{OpResponses}}}}}}
	g, err := NewGateway(cfg, map[string]*provider{"p": {id: "p", dialect: openaiDialect{}}}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.eligible(openaiDialect{}, OpChat, Meta{Model: "gpt"})) != 0 {
		t.Fatal("Responses-only model accepted chat")
	}
	if len(g.eligible(openaiDialect{}, OpResponses, Meta{Model: "gpt"})) != 1 {
		t.Fatal("Responses-only model rejected Responses")
	}
	cfg.Models[0].Targets[0].Operations = []Operation{OpEmbed}
	if cfg.Validate() == nil {
		t.Fatal("chat model accepted embedding operation")
	}
	cfg.Models[0].Targets[0].Operations = []Operation{OpResponses, OpResponses}
	if cfg.Validate() == nil {
		t.Fatal("duplicate operation accepted")
	}
}
