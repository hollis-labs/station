package llmgateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/security/secrets"

	"github.com/hollis-labs/station/platform"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func hashOf(k string) string { s := sha256.Sum256([]byte(k)); return hex.EncodeToString(s[:]) }

// upstream is a fake provider that records what it was sent.
type upstream struct {
	*httptest.Server
	mu      sync.Mutex
	calls   int
	headers http.Header
	body    string
	handler http.HandlerFunc
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{handler: h}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls++
		u.headers, u.body = r.Header.Clone(), string(b)
		u.mu.Unlock()
		u.handler(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) snapshot() (int, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.headers, u.body
}

const openaiBody = `{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`

func openaiOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Ratelimit-Remaining-Requests", "99")
	_, _ = io.WriteString(w, openaiBody)
}

const anthropicBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":7,"cache_creation_input_tokens":5,"cache_read_input_tokens":60}}`

func anthropicOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, anthropicBody)
}

type cfgOpts struct {
	policy string
	extra  string
	logger *slog.Logger
}

// fakeBackend resolves test:// references to fixed values.
type fakeBackend struct{}

func (fakeBackend) Scheme() string { return "test" }
func (fakeBackend) Resolve(_ context.Context, ref secrets.Ref) (secrets.Resolved, error) {
	if ref.Key() != "emb/key" {
		return secrets.Resolved{}, &secrets.Error{Kind: secrets.KindNotFound}
	}
	return secrets.Resolved{Secret: secrets.FromString("upstream-emb-secret"), Ref: ref.Raw, Source: "test"}, nil
}

// gwEnv wires two OpenAI-dialect upstreams (o1, o2) and one Anthropic (an).
type gwEnv struct {
	app        *platform.App
	srv        *httptest.Server
	o1, o2, an *upstream
	jv         *upstream
	jvh        http.HandlerFunc // the JEV upstream's behavior; defaults to jevOK
}

func newGW(t *testing.T, o1h, o2h, anh http.HandlerFunc, opts cfgOpts) *gwEnv {
	t.Helper()
	e := &gwEnv{o1: newUpstream(t, o1h), o2: newUpstream(t, o2h), an: newUpstream(t, anh), jvh: jevOK}
	e.jv = newUpstream(t, func(w http.ResponseWriter, r *http.Request) { e.jvh(w, r) })
	t.Setenv("TEST_JV_KEY", "upstream-jev-secret")
	t.Setenv("TEST_O1_KEY", "upstream-o1-secret")
	t.Setenv("TEST_AN_KEY", "upstream-an-secret")
	catalogLine := ""
	if !strings.Contains(opts.extra, "catalog:") {
		catalogLine = fmt.Sprintf("    catalog: {refresh: false, cache_dir: %q}", t.TempDir())
	}
	y := fmt.Sprintf(`
auth:
  keys:
    - {id: caller-1, sha256: %s, scopes: ["llm-gateway.*"]}
    - {id: narrow, sha256: %s, scopes: ["llm-gateway.models.read"]}
plugins:
  llm-gateway:
%s
    providers:
      - {id: o1, dialect: openai, catalog_provider: openai, base_url: "%s/v1", api_key_env: TEST_O1_KEY}
      - {id: o2, dialect: openai, catalog_provider: openai, base_url: "%s/v1"}
      - {id: an, dialect: anthropic, catalog_provider: anthropic, base_url: "%s/v1", api_key_env: TEST_AN_KEY}
      - {id: emb, dialect: openai, base_url: "%s/v1", api_key_ref: "test://emb/key"}
      - {id: jv, dialect: jev, base_url: "%s/v1", api_key_env: TEST_JV_KEY}
    models:
      - name: judge
        kind: decision
        targets:
          - {provider: jv, model: jev-real, price: {input_per_million: 1, output_per_million: 4}}
      - name: embed-small
        kind: embedding
        targets:
          - {provider: emb, model: text-embedding-real, price: {input_per_million: 0.02}}
      - name: gpt
        targets:
          - {provider: o1, model: gpt-real, price: {input_per_million: 2, output_per_million: 8, cache_read_per_million: 0.5}}
          - {provider: o2, model: gpt-backup}
      - name: claude
        targets:
          - {provider: an, model: claude-real, price: {input_per_million: 3, output_per_million: 15, cache_read_per_million: 0.3, cache_write_per_million: 3.75}}
      - name: textonly
        targets:
          - {provider: o1, model: t1, capabilities: {vision: false}}
          - {provider: o2, model: t2}
%s
%s
`, hashOf("good"), hashOf("narrow"), catalogLine, e.o1.URL, e.o2.URL, e.an.URL, e.o2.URL, e.jv.URL, opts.policy, opts.extra)
	cfg, err := platform.ParseConfig([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	logger := opts.logger
	if logger == nil {
		logger = quiet
	}
	app, err := platform.Assemble(context.Background(), platform.Options{Config: cfg, Logger: logger,
		Secrets: secrets.New(secrets.WithBackend(fakeBackend{})), Plugins: []platform.Plugin{New()}})
	if err != nil {
		t.Fatal(err)
	}
	e.app = app
	e.srv = httptest.NewServer(app.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *gwEnv) do(t *testing.T, method, path string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+"/api/llm-gateway"+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var (
	oaAuth = map[string]string{"Authorization": "Bearer good", "Content-Type": "application/json"}
	anAuth = map[string]string{"X-Api-Key": "good", "Anthropic-Version": "2023-06-01", "Content-Type": "application/json"}
)

const (
	oaReq = `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`
	anReq = `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
)

func usageOf(t *testing.T, e *gwEnv) ([]Entry, []Total) {
	t.Helper()
	_, body := e.do(t, "GET", "/v1/usage", oaAuth, "")
	var out struct {
		Entries []Entry `json:"entries"`
		Totals  []Total `json:"totals"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("usage body %q: %v", body, err)
	}
	return out.Entries, out.Totals
}

func TestOpenAIRelayIsTransparentAndIsolatesCredentials(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	// The caller smuggles a caller_id in the body and its own key in a second header.
	body := `{"model":"gpt","caller_id":"someone-else","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`
	resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, body)
	if resp.StatusCode != 200 || got != openaiBody {
		t.Fatalf("response not relayed byte for byte: %d %q", resp.StatusCode, got)
	}
	if resp.Header.Get("X-Ratelimit-Remaining-Requests") != "99" {
		t.Fatal("upstream rate-limit header was not relayed")
	}
	_, hdr, sent := e.o1.snapshot()
	if hdr.Get("Authorization") != "Bearer upstream-o1-secret" {
		t.Fatalf("upstream credential: %q", hdr.Get("Authorization"))
	}
	if strings.Contains(sent, "good") || strings.Contains(fmt.Sprint(hdr), "good") {
		t.Fatal("the caller's Station key reached the upstream")
	}
	if !strings.Contains(sent, `"gpt-real"`) || !strings.Contains(sent, `"temperature":0.2`) || !strings.Contains(sent, `"caller_id":"someone-else"`) {
		t.Fatalf("model not rewritten or other fields not preserved: %s", sent)
	}
	entries, _ := usageOf(t, e)
	if len(entries) != 1 || entries[0].Row.SessionID != "caller-1" {
		t.Fatalf("attribution must come from the verified principal, got %+v", entries)
	}
}

func TestOpenAIUsageAndCost(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq)
	entries, totals := usageOf(t, e)
	en := entries[0]
	// 60 uncached + 40 cached + 20 out.
	if en.Row.Usage.UncachedInputTokens.Tokens != 60 || en.Row.Usage.CacheReadTokens.Tokens != 40 || en.Row.Usage.OutputTokens.Tokens != 20 {
		t.Fatalf("usage %+v", en.Row.Usage)
	}
	if en.Row.Usage.CacheWriteTokens.Provenance != "unknown" {
		t.Fatal("cache write was never reported and must stay unknown")
	}
	want := 60*2.0/1e6 + 40*0.5/1e6 + 20*8.0/1e6
	if en.CostUSD == nil || *en.CostUSD < want-1e-9 || *en.CostUSD > want+1e-9 {
		t.Fatalf("cost %v want %v", en.CostUSD, want)
	}
	if err := en.Row.Validate(); err != nil || len(totals) != 1 || totals[0].Calls != 1 {
		t.Fatalf("row invalid or totals wrong: %v %+v", err, totals)
	}
}

func sseHandler(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
			w.(http.Flusher).Flush()
		}
	}
}

const (
	oaChunk1 = "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"
	oaChunk2 = "data: {\"choices\":[{\"delta\":{\"content\":\"y\"},\"finish_reason\":\"stop\"}]}\n\n"
	oaUsage  = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4}}\n\n"
	oaDone   = "data: [DONE]\n\n"
)

func TestOpenAIStreamRelayedUntouchedWhenClientAsksForUsage(t *testing.T) {
	e := newGW(t, sseHandler(oaChunk1, oaChunk2, oaUsage, oaDone), openaiOK, anthropicOK, cfgOpts{})
	_, got := e.do(t, "POST", "/v1/chat/completions", oaAuth,
		`{"model":"gpt","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if got != oaChunk1+oaChunk2+oaUsage+oaDone {
		t.Fatalf("stream altered: %q", got)
	}
	entries, _ := usageOf(t, e)
	if entries[0].Row.Usage.UncachedInputTokens.Tokens != 10 || entries[0].Row.Usage.OutputTokens.Tokens != 4 || !entries[0].Streamed {
		t.Fatalf("stream usage %+v", entries[0])
	}
}

func TestOpenAIStreamUsageInjectedAndHiddenWhenClientDidNotAsk(t *testing.T) {
	e := newGW(t, sseHandler(oaChunk1, oaChunk2, oaUsage, oaDone), openaiOK, anthropicOK, cfgOpts{})
	_, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if got != oaChunk1+oaChunk2+oaDone {
		t.Fatalf("the usage chunk the gateway asked for must be hidden: %q", got)
	}
	_, _, sent := e.o1.snapshot()
	if !strings.Contains(sent, `"include_usage":true`) {
		t.Fatalf("upstream was not asked for usage: %s", sent)
	}
	entries, _ := usageOf(t, e)
	if entries[0].Row.Usage.OutputTokens.Tokens != 4 {
		t.Fatalf("usage lost: %+v", entries[0].Row.Usage)
	}
}

func TestStreamFirstByteIsNotBufferedBehindTheWholeResponse(t *testing.T) {
	release := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, oaChunk1)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, oaChunk2+oaDone)
	}
	e := newGW(t, h, openaiOK, anthropicOK, cfgOpts{})
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/llm-gateway/v1/chat/completions",
		strings.NewReader(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	for k, v := range oaAuth {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(resp.Body).ReadString('\n'); first <- l }()
	select {
	case l := <-first:
		if !strings.HasPrefix(l, "data: ") {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event did not arrive while the upstream was still streaming")
	}
	close(release)
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, oaChunk1)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	}
	e := newGW(t, h, openaiOK, anthropicOK, cfgOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/api/llm-gateway/v1/chat/completions",
		strings.NewReader(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	for k, v := range oaAuth {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled when the client left")
	}
}

func TestAnthropicRelayWithXAPIKeyAuth(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/messages", anAuth, anReq)
	if resp.StatusCode != 200 || got != anthropicBody {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	_, hdr, sent := e.an.snapshot()
	if hdr.Get("X-Api-Key") != "upstream-an-secret" || hdr.Get("Anthropic-Version") != "2023-06-01" || hdr.Get("Authorization") != "" {
		t.Fatalf("upstream headers %v", hdr)
	}
	if !strings.Contains(sent, `"claude-real"`) {
		t.Fatalf("model not rewritten: %s", sent)
	}
	entries, _ := usageOf(t, e)
	u := entries[0].Row.Usage
	if u.UncachedInputTokens.Tokens != 30 || u.CacheReadTokens.Tokens != 60 || u.CacheWriteTokens.Tokens != 5 || u.OutputTokens.Tokens != 7 {
		t.Fatalf("anthropic usage %+v", u)
	}
	want := 30*3.0/1e6 + 60*0.3/1e6 + 5*3.75/1e6 + 7*15.0/1e6
	if c := entries[0].CostUSD; c == nil || *c < want-1e-9 || *c > want+1e-9 {
		t.Fatalf("cost %v want %v", c, want)
	}
}

func TestAnthropicVersionDefaultedWhenAbsent(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	e.do(t, "POST", "/v1/messages", map[string]string{"X-Api-Key": "good"}, anReq)
	if _, hdr, _ := e.an.snapshot(); hdr.Get("Anthropic-Version") == "" {
		t.Fatal("anthropic-version missing upstream")
	}
}

func TestAnthropicStreamRelayedAndMetered(t *testing.T) {
	ev := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":3,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":9}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	e := newGW(t, openaiOK, openaiOK, sseHandler(ev...), cfgOpts{})
	_, got := e.do(t, "POST", "/v1/messages", anAuth, `{"model":"claude","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if got != strings.Join(ev, "") {
		t.Fatalf("stream altered: %q", got)
	}
	entries, _ := usageOf(t, e)
	u := entries[0].Row.Usage
	if u.UncachedInputTokens.Tokens != 12 || u.CacheReadTokens.Tokens != 3 || u.OutputTokens.Tokens != 9 {
		t.Fatalf("usage %+v", u)
	}
	if u.CacheWriteTokens.Provenance != "unknown" {
		t.Fatal("unreported cache write must stay unknown")
	}
}

func TestUpstreamClientErrorPassesThroughUntouched(t *testing.T) {
	const errBody = `{"error":{"message":"bad param","type":"invalid_request_error"}}`
	bad := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, errBody)
	}
	e := newGW(t, bad, openaiOK, anthropicOK, cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq)
	if resp.StatusCode != 400 || got != errBody {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	if n, _, _ := e.o2.snapshot(); n != 0 {
		t.Fatal("a request error must not fail over")
	}
}

func fail(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	}
}

func TestFailoverOnServerErrorThenCooldown(t *testing.T) {
	e := newGW(t, fail(503), openaiOK, anthropicOK, cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq)
	if resp.StatusCode != 200 || got != openaiBody {
		t.Fatalf("no failover: %d %q", resp.StatusCode, got)
	}
	_, _, sent := e.o2.snapshot()
	if !strings.Contains(sent, `"gpt-backup"`) {
		t.Fatalf("fallback model: %s", sent)
	}
	before, _, _ := e.o1.snapshot()
	e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq)
	if after, _, _ := e.o1.snapshot(); after != before {
		t.Fatal("a cooling-down target was called again")
	}
	entries, _ := usageOf(t, e)
	if entries[0].Outcome != OutcomeFailedOver || entries[1].Outcome != OutcomeOK {
		t.Fatalf("outcomes %s %s", entries[0].Outcome, entries[1].Outcome)
	}
}

func TestLastTargetErrorIsRelayedAndAllCooledDownIsNativeUnavailable(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, fail(500), cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/messages", anAuth, anReq)
	if resp.StatusCode != 500 || got != `{"error":"down"}` {
		t.Fatalf("sole target's error must be relayed as is: %d %q", resp.StatusCode, got)
	}
	resp, got = e.do(t, "POST", "/v1/messages", anAuth, anReq)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct{ Type string }
	}
	if err := json.Unmarshal([]byte(got), &env); err != nil || env.Type != "error" || env.Error.Type != "overloaded_error" {
		t.Fatalf("gateway error must use Anthropic's shape: %q", got)
	}
}

func TestCapabilityFiltering(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	img := `{"model":"textonly","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`
	if resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, img); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n, _, _ := e.o1.snapshot(); n != 0 {
		t.Fatal("a target marked vision:false was used for an image request")
	}
	if n, _, _ := e.o2.snapshot(); n != 1 {
		t.Fatal("capable target not used")
	}
}

func TestEndpointDialectMismatchIsRefusedNotTranslated(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(got, "model_not_available") {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	if n, _, _ := e.an.snapshot(); n != 0 {
		t.Fatal("request crossed protocols")
	}
}

func TestPolicyEnforceDeniesAndShadowAdmits(t *testing.T) {
	rules := `    policy:
      mode: %s
      rules:
        - {id: no-ci-premium, principal: "caller-*", model: "claude", effect: deny, reason: premium blocked}
        - {id: ask-tools, needs: tools, effect: ask}`
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{policy: fmt.Sprintf(rules, "enforce")})
	resp, got := e.do(t, "POST", "/v1/messages", anAuth, anReq)
	if resp.StatusCode != 403 || !strings.Contains(got, "permission_error") {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	if n, _, _ := e.an.snapshot(); n != 0 {
		t.Fatal("denied request reached the upstream")
	}
	tools := `{"model":"gpt","tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`
	if resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, tools); resp.StatusCode != 403 || !strings.Contains(got, "approval_required") {
		t.Fatalf("ask must refuse until approved: %d %q", resp.StatusCode, got)
	}
	shadow := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{policy: fmt.Sprintf(rules, "shadow")})
	if resp, _ := shadow.do(t, "POST", "/v1/messages", anAuth, anReq); resp.StatusCode != 200 {
		t.Fatalf("shadow mode must not change routing results: %d", resp.StatusCode)
	}
}

func TestAuthAndScopes(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	if resp, _ := e.do(t, "POST", "/v1/chat/completions", map[string]string{}, oaReq); resp.StatusCode != 401 {
		t.Fatalf("anonymous %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/v1/messages", map[string]string{"X-Api-Key": "wrong"}, anReq); resp.StatusCode != 401 {
		t.Fatalf("bad x-api-key %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/v1/chat/completions", map[string]string{"Authorization": "Bearer narrow"}, oaReq); resp.StatusCode != 403 {
		t.Fatalf("narrow scope %d", resp.StatusCode)
	}
	for _, c := range []*upstream{e.o1, e.o2, e.an} {
		if n, _, _ := c.snapshot(); n != 0 {
			t.Fatal("an unauthorized request reached an upstream")
		}
	}
}

func TestModelsListPerDialect(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	_, oa := e.do(t, "GET", "/v1/models", oaAuth, "")
	if !strings.Contains(oa, `"object":"list"`) || !strings.Contains(oa, `"gpt"`) || strings.Contains(oa, `"claude"`) {
		t.Fatalf("openai list %s", oa)
	}
	_, an := e.do(t, "GET", "/v1/models", anAuth, "")
	if !strings.Contains(an, `"has_more":false`) || !strings.Contains(an, `"claude"`) || strings.Contains(an, `"gpt"`) {
		t.Fatalf("anthropic list %s", an)
	}
}

func TestInvalidRequestsAnsweredInTheDialectsShape(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	for name, c := range map[string]struct {
		path, body string
		hdr        map[string]string
		want       int
	}{
		"not json":      {"/v1/chat/completions", `{`, oaAuth, 400},
		"no model":      {"/v1/chat/completions", `{"messages":[{"role":"user","content":"x"}]}`, oaAuth, 400},
		"no messages":   {"/v1/messages", `{"model":"claude","messages":[]}`, anAuth, 400},
		"unknown model": {"/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`, oaAuth, 404},
	} {
		resp, got := e.do(t, "POST", c.path, c.hdr, c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d %s", name, resp.StatusCode, got)
		}
	}
	if n, _, _ := e.o1.snapshot(); n != 0 {
		t.Fatal("invalid request reached the upstream")
	}
}

func TestConfigValidation(t *testing.T) {
	mk := func() Config {
		return Config{
			Providers: []ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: "http://x"}},
			Models:    []ModelConfig{{Name: "m", Targets: []TargetConfig{{Provider: "p", Model: "x"}}}},
		}
	}
	good := mk()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no providers":     func(c *Config) { c.Providers = nil },
		"unknown dialect":  func(c *Config) { c.Providers[0].Dialect = "magic" },
		"no base url":      func(c *Config) { c.Providers[0].BaseURL = "" },
		"no models":        func(c *Config) { c.Models = nil },
		"unknown provider": func(c *Config) { c.Models[0].Targets[0].Provider = "zzz" },
		"no targets":       func(c *Config) { c.Models[0].Targets = nil },
		"negative price":   func(c *Config) { c.Models[0].Targets[0].Price = &PriceConfig{InputPerMillion: -1} },
		"bad policy mode":  func(c *Config) { c.Policy.Mode = "yolo" },
		"bad effect":       func(c *Config) { c.Policy.Rules = []RuleConfig{{ID: "r", Effect: "maybe"}} },
		"rule no id":       func(c *Config) { c.Policy.Rules = []RuleConfig{{Effect: "deny"}} },
		"bad needs":        func(c *Config) { c.Policy.Rules = []RuleConfig{{ID: "r", Effect: "deny", Needs: "magic"}} },
	} {
		c := mk()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMissingProviderKeyFailsAtStartup(t *testing.T) {
	cfg, _ := platform.ParseConfig([]byte(`
auth: {mode: off}
plugins:
  llm-gateway:
    providers: [{id: p, dialect: openai, base_url: "http://x", api_key_env: STATION_TEST_KEY_THAT_IS_UNSET}]
    models: [{name: m, targets: [{provider: p, model: x}]}]
`))
	if _, err := platform.Assemble(context.Background(), platform.Options{Config: cfg, Logger: quiet, Plugins: []platform.Plugin{New()}}); err == nil {
		t.Fatal("empty provider key accepted")
	}
}

const embedBody = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"x","usage":{"prompt_tokens":8,"total_tokens":8}}`

func embedOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, embedBody)
}

func TestEmbeddingsRelayUsesSecretRefAndMeters(t *testing.T) {
	e := newGW(t, openaiOK, embedOK, anthropicOK, cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/embeddings", oaAuth, `{"model":"embed-small","input":["hello"]}`)
	if resp.StatusCode != 200 || got != embedBody {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	_, hdr, sent := e.o2.snapshot()
	if hdr.Get("Authorization") != "Bearer upstream-emb-secret" {
		t.Fatalf("api_key_ref not applied: %q", hdr.Get("Authorization"))
	}
	if !strings.Contains(sent, `"text-embedding-real"`) {
		t.Fatalf("model not rewritten: %s", sent)
	}
	entries, _ := usageOf(t, e)
	en := entries[0]
	if en.Operation != OpEmbed || en.Row.Usage.UncachedInputTokens.Tokens != 8 || en.Row.Usage.OutputTokens.Tokens != 0 {
		t.Fatalf("embedding usage %+v", en)
	}
	if en.CostUSD == nil || *en.CostUSD < 8*0.02/1e6-1e-12 || *en.CostUSD > 8*0.02/1e6+1e-12 {
		t.Fatalf("cost %v", en.CostUSD)
	}
}

func TestModelKindIsEnforcedBothWays(t *testing.T) {
	e := newGW(t, openaiOK, embedOK, anthropicOK, cfgOpts{})
	if resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"embed-small","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 400 || !strings.Contains(got, "model_not_available") {
		t.Fatalf("chat on an embedding model: %d %s", resp.StatusCode, got)
	}
	if resp, got := e.do(t, "POST", "/v1/embeddings", oaAuth, `{"model":"gpt","input":"x"}`); resp.StatusCode != 400 || !strings.Contains(got, "model_not_available") {
		t.Fatalf("embedding on a chat model: %d %s", resp.StatusCode, got)
	}
	if resp, _ := e.do(t, "POST", "/v1/embeddings", oaAuth, `{"model":"embed-small"}`); resp.StatusCode != 400 {
		t.Fatalf("missing input accepted: %d", resp.StatusCode)
	}
}

func TestEmbeddingScopeIsSeparate(t *testing.T) {
	e := newGW(t, openaiOK, embedOK, anthropicOK, cfgOpts{})
	// "narrow" holds only models.read.
	if resp, _ := e.do(t, "POST", "/v1/embeddings", map[string]string{"Authorization": "Bearer narrow"}, `{"model":"embed-small","input":"x"}`); resp.StatusCode != 403 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestEmbeddingModelCannotTargetAnthropicDialect(t *testing.T) {
	c := Config{
		Providers: []ProviderConfig{{ID: "a", Dialect: "anthropic", BaseURL: "http://x"}},
		Models:    []ModelConfig{{Name: "e", Kind: "embedding", Targets: []TargetConfig{{Provider: "a", Model: "x"}}}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("embedding model on a dialect with no embeddings accepted")
	}
}

func TestOneLogLinePerCallWithoutContent(t *testing.T) {
	var buf bytes.Buffer
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"gpt","messages":[{"role":"user","content":"my private prompt text"}]}`)
	e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
	var call, refused map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m["msg"] {
		case "llm.call":
			call = m
		case "llm.refused":
			refused = m
		}
	}
	if call == nil || refused == nil {
		t.Fatalf("missing llm.call or llm.refused in %s", buf.String())
	}
	if call["principal"] != "caller-1" || call["provider"] != "o1" || call["model"] != "gpt-real" || call["outcome"] != "ok" ||
		call["output_tokens"] != float64(20) || call["verified"] != true || call["request_id"] == "" {
		t.Fatalf("llm.call %v", call)
	}
	if refused["code"] != "model_not_found" || refused["status"] != float64(404) {
		t.Fatalf("llm.refused %v", refused)
	}
	for _, leaked := range []string{"my private prompt text", "upstream-o1-secret", "Bearer"} {
		if strings.Contains(buf.String(), leaked) {
			t.Fatalf("%q reached the log", leaked)
		}
	}
}
