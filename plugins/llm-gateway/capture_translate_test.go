package llmgateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIResponsesRelayAndStream(t *testing.T) {
	const respJSON = `{"id":"resp_1","object":"response","model":"gpt-real","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello responses"}]}],"usage":{"input_tokens":50,"output_tokens":12,"input_tokens_details":{"cached_tokens":10},"output_tokens_details":{"reasoning_tokens":4}}}`
	e := newGW(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("stream") != "" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respJSON)
	}, openaiOK, anthropicOK, cfgOpts{})

	reqBody := `{"model":"gpt","instructions":"be concise","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	resp, got := e.do(t, "POST", "/v1/responses", oaAuth, reqBody)
	if resp.StatusCode != 200 || got != respJSON {
		t.Fatalf("status=%d body=%q", resp.StatusCode, got)
	}
	_, _, upBody := e.o1.snapshot()
	if !strings.Contains(upBody, `"model":"gpt-real"`) {
		t.Fatalf("upstream model was not rewritten: %s", upBody)
	}

	_, usageBody := e.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	for _, want := range []string{
		`"operation":"responses"`,
		`"input_tokens":50`,
		`"output_tokens":12`,
		`"uncached_input_tokens":{"tokens":40,"provenance":"measured"}`,
		`"cache_read_tokens":{"tokens":10,"provenance":"measured"}`,
	} {
		if !strings.Contains(usageBody, want) {
			t.Fatalf("missing %s in usage: %s", want, usageBody)
		}
	}

	// Streaming Responses API (event: response.completed carries usage).
	es := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"usage\":{\"input_tokens\":30,\"output_tokens\":8}}}\n\n")
		fl.Flush()
	}, openaiOK, anthropicOK, cfgOpts{})

	sresp, sbody := es.do(t, "POST", "/v1/responses", oaAuth, `{"model":"gpt","stream":true,"input":"hello"}`)
	if sresp.StatusCode != 200 || !strings.Contains(sbody, "response.completed") {
		t.Fatalf("stream status=%d body=%q", sresp.StatusCode, sbody)
	}
	_, susage := es.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	if !strings.Contains(susage, `"input_tokens":30`) || !strings.Contains(susage, `"output_tokens":8`) {
		t.Fatalf("streaming responses usage not metered: %s", susage)
	}
}

func TestLedgerFullCaptureSanitizesSecretsAndPII(t *testing.T) {
	extra := `    ledger:
      capture:
        mode: full
        max_bytes: 65536`
	e := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Upstream response accidentally echoes a known provider secret, an inline OpenAI key, and PII.
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":"Reach alice@example.com or use upstream-o1-secret / sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":9}}`)
	}, openaiOK, anthropicOK, cfgOpts{extra: extra})

	req := `{
		"model":"gpt",
		"messages":[
			{"role":"system","content":"Leader prompt for bob@example.com using secret upstream-an-secret"},
			{"role":"user","content":"Call 415-555-0199"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Uses key upstream-emb-secret","parameters":{"type":"object"}}}]
	}`
	resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, req)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	_, u := e.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	// Raw secrets and PII must never appear in the ledger snapshot.
	for _, forbidden := range []string{
		"upstream-o1-secret",
		"upstream-an-secret",
		"upstream-emb-secret",
		"sk-proj-abcdefghijklmnopqrstuvwxyz0123456789",
		"alice@example.com",
		"bob@example.com",
	} {
		if strings.Contains(u, forbidden) {
			t.Fatalf("unsanitized secret or PII %q leaked into ledger detail: %s", forbidden, u)
		}
	}
	// Structured sections and redaction markers must be present.
	for _, want := range []string{
		`"leader":`,
		`"tools":`,
		`"request":`,
		`"response":`,
		`"redaction":`,
		`"sensitivity":"secret"`,
		`REDACTED`,
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("expected %s in ledger detail: %s", want, u)
		}
	}
}

func TestLedgerFullCaptureSSEEventsSanitized(t *testing.T) {
	extra := `    ledger:
      capture:
        mode: full`
	e := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"secret upstream-o1-secret and carol@example.com\"}}]}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}, openaiOK, anthropicOK, cfgOpts{extra: extra})

	resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, u := e.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	if strings.Contains(u, "upstream-o1-secret") || strings.Contains(u, "carol@example.com") {
		t.Fatalf("unsanitized secret/PII in captured SSE events: %s", u)
	}
	if !strings.Contains(u, `"sse_events":[`) {
		t.Fatalf("expected sse_events in ledger detail: %s", u)
	}
}

func TestLedgerFullCaptureOversizedPayloadFailsClosedToTombstone(t *testing.T) {
	extra := `    ledger:
      capture:
        mode: full
        max_bytes: 128`
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{extra: extra})

	bigText := strings.Repeat("x", 300)
	req := fmt.Sprintf(`{"model":"gpt","messages":[{"role":"user","content":%q}]}`, bigText)
	resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, req)
	if resp.StatusCode != 200 {
		t.Fatalf("oversized capture must not fail the relay: status %d", resp.StatusCode)
	}
	_, u := e.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	if strings.Contains(u, bigText) {
		t.Fatalf("oversized request payload should have been replaced with a tombstone: %s", u)
	}
	for _, want := range []string{`"request_error_class":"oversized"`, `"sanitize_error":"oversized"`} {
		if !strings.Contains(u, want) {
			t.Fatalf("expected %s in capture report: %s", want, u)
		}
	}
}

func TestCrossProtocolTranslationOpenAIToAnthropic(t *testing.T) {
	extra := `      - name: claude-via-oa
        targets:
          - {provider: an, model: claude-real, translate: true, price: {input_per_million: 3, output_per_million: 15}}`
	e := newGW(t, openaiOK, openaiOK, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected /v1/messages, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_99","type":"message","role":"assistant","model":"claude-real","content":[{"type":"text","text":"translated hello"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}],"stop_reason":"tool_use","usage":{"input_tokens":25,"output_tokens":14,"cache_read_input_tokens":5}}`)
	}, cfgOpts{extra: extra})

	oaReqWithTools := `{
		"model":"claude-via-oa",
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"Weather in SF?"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object"}}}]
	}`
	resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReqWithTools)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	_, _, anBody := e.an.snapshot()
	for _, wantUp := range []string{`"model":"claude-real"`, `"system":"You are helpful"`, `"input_schema":`} {
		if !strings.Contains(anBody, wantUp) {
			t.Fatalf("expected %s in translated Anthropic request: %s", wantUp, anBody)
		}
	}
	for _, wantResp := range []string{
		`"object":"chat.completion"`,
		`"model":"claude-via-oa"`,
		`"finish_reason":"tool_calls"`,
		`"content":"translated hello"`,
		`"name":"get_weather"`,
		`"arguments":"{\"city\":\"SF\"}"`,
	} {
		if !strings.Contains(got, wantResp) {
			t.Fatalf("expected %s in translated OpenAI response: %s", wantResp, got)
		}
	}

	// Verify streaming SSE translation from Anthropic upstream -> OpenAI client.
	es := newGW(t, openaiOK, openaiOK, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s1\",\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello \"}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		fl.Flush()
	}, cfgOpts{extra: extra})

	sresp, sgot := es.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"claude-via-oa","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if sresp.StatusCode != 200 {
		t.Fatalf("stream status=%d body=%s", sresp.StatusCode, sgot)
	}
	for _, wantChunk := range []string{
		`"object":"chat.completion.chunk"`,
		`"content":"hello "`,
		`"content":"world"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(sgot, wantChunk) {
			t.Fatalf("missing %s in translated stream: %s", wantChunk, sgot)
		}
	}
	_, susage := es.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	if !strings.Contains(susage, `"input_tokens":20`) || !strings.Contains(susage, `"output_tokens":6`) {
		t.Fatalf("translated stream usage not metered: %s", susage)
	}
}

func TestCrossProtocolTranslationAnthropicToOpenAI(t *testing.T) {
	extra := `      - name: gpt-via-an
        targets:
          - {provider: o1, model: gpt-real, translate: true, price: {input_per_million: 2, output_per_million: 8}}`
	e := newGW(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("expected /v1/chat/completions, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":"hi from openai","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":40,"completion_tokens":12}}`)
	}, openaiOK, anthropicOK, cfgOpts{extra: extra})

	anReqWithTools := `{
		"model":"gpt-via-an",
		"max_tokens":256,
		"system":"Be brief",
		"messages":[{"role":"user","content":[{"type":"text","text":"search go"}]}],
		"tools":[{"name":"search","description":"Search docs","input_schema":{"type":"object"}}]
	}`
	resp, got := e.do(t, "POST", "/v1/messages", anAuth, anReqWithTools)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	_, _, o1Body := e.o1.snapshot()
	for _, wantUp := range []string{`"model":"gpt-real"`, `"role":"system"`, `"content":"Be brief"`, `"max_tokens":256`} {
		if !strings.Contains(o1Body, wantUp) {
			t.Fatalf("expected %s in translated OpenAI request: %s", wantUp, o1Body)
		}
	}
	for _, wantResp := range []string{
		`"type":"message"`,
		`"model":"gpt-via-an"`,
		`"stop_reason":"tool_use"`,
		`"text":"hi from openai"`,
		`"type":"tool_use"`,
		`"name":"search"`,
	} {
		if !strings.Contains(got, wantResp) {
			t.Fatalf("expected %s in translated Anthropic response: %s", wantResp, got)
		}
	}

	// Verify streaming SSE translation from OpenAI upstream -> Anthropic client.
	es := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_s1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello \"}}]}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_s1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"anthropic\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":18,\"completion_tokens\":4}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}, openaiOK, anthropicOK, cfgOpts{extra: extra})

	sresp, sgot := es.do(t, "POST", "/v1/messages", anAuth, `{"model":"gpt-via-an","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if sresp.StatusCode != 200 {
		t.Fatalf("stream status=%d body=%s", sresp.StatusCode, sgot)
	}
	for _, wantEvent := range []string{
		`event: message_start`,
		`event: content_block_delta`,
		`"text":"hello "`,
		`"text":"anthropic"`,
		`event: message_delta`,
		`event: message_stop`,
	} {
		if !strings.Contains(sgot, wantEvent) {
			t.Fatalf("missing %s in translated Anthropic stream: %s", wantEvent, sgot)
		}
	}
	_, susage := es.do(t, "GET", "/v1/usage?limit=1", oaAuth, "")
	if !strings.Contains(susage, `"input_tokens":18`) || !strings.Contains(susage, `"output_tokens":4`) {
		t.Fatalf("translated Anthropic stream usage not metered: %s", susage)
	}
}

func TestCrossProtocolTranslationConfigValidation(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "jv", Dialect: "jev", BaseURL: "http://localhost:8080/v1"},
		},
		Models: []ModelConfig{
			{
				Name: "judge",
				Kind: "decision",
				Targets: []TargetConfig{
					{Provider: "jv", Model: "jev-1", Translate: true},
				},
			},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "translate is not supported") {
		t.Fatalf("expected translate validation error, got %v", err)
	}
}
