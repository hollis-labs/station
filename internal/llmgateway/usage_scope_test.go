package llmgateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func bearer(k string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + k, "Content-Type": "application/json"}
}

// A collector holds usage.read and nothing else. It must not read prompts or other callers' rows
// from /v1/usage; the full reader (usage.detail via llm-gateway.*) still does.
func TestUsageReadAloneSeesNeitherDetailNorOtherCallersRows(t *testing.T) {
	const prompt = "the-confidential-prompt-text"
	e := newGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":"answer-body-text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":9}}`)
	}, openaiOK, anthropicOK, cfgOpts{extra: `    ledger:
      capture:
        mode: full
        max_bytes: 65536`})
	if resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth,
		`{"model":"gpt","messages":[{"role":"user","content":"`+prompt+`"}]}`); resp.StatusCode != 200 {
		t.Fatalf("relay status %d", resp.StatusCode)
	}

	// The full reader sees the body in Detail (this is what the fix must keep working).
	_, full := e.do(t, "GET", "/v1/usage", oaAuth, "")
	if !strings.Contains(full, prompt) {
		t.Fatalf("a detail reader should see captured bodies: %s", full)
	}

	// usage.read alone: no Detail, no other caller's rows, totals are its own.
	_, got := e.do(t, "GET", "/v1/usage", bearer("collector"), "")
	for _, forbidden := range []string{prompt, "answer-body-text", `"detail"`, "caller-1"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("usage.read alone saw %q: %s", forbidden, got)
		}
	}
	var out struct {
		Entries []Entry `json:"entries"`
		Totals  []Total `json:"totals"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 0 || len(out.Totals) != 0 {
		t.Fatalf("a collector with no calls of its own saw rows: %s", got)
	}

	// Asking for another principal is refused, not silently narrowed.
	if resp, _ := e.do(t, "GET", "/v1/usage?principal=caller-1", bearer("collector"), ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other principal: status %d, want 403", resp.StatusCode)
	}
	// A key without the scope is still refused by the scope table.
	if resp, _ := e.do(t, "GET", "/v1/usage", bearer("narrow"), ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no usage scope: status %d, want 403", resp.StatusCode)
	}
	// The export stays the collector's path and still strips Detail.
	_, exp := e.do(t, "GET", "/v1/usage/export", bearer("collector"), "")
	if strings.Contains(exp, prompt) || !strings.Contains(exp, "caller-1") {
		t.Fatalf("export should list all callers without Detail: %s", exp)
	}
}

// Policy, budget and price are decided on the model the gateway parsed (the last duplicate wins in
// Go); an upstream may honour the first. A body that repeats a top-level key is refused before any
// of them, on every relay endpoint, and nothing reaches an upstream.
func TestRelayRefusesRepeatedTopLevelKeys(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	for name, tc := range map[string]struct {
		path string
		hdr  map[string]string
		body string
	}{
		"model twice":        {"/v1/chat/completions", oaAuth, `{"model":"gpt","model":"claude","messages":[{"role":"user","content":"hi"}]}`},
		"model in two cases": {"/v1/chat/completions", oaAuth, `{"model":"gpt","Model":"claude","messages":[{"role":"user","content":"hi"}]}`},
		"stream twice":       {"/v1/chat/completions", oaAuth, `{"model":"gpt","stream":false,"stream":true,"messages":[]}`},
		"anthropic":          {"/v1/messages", anAuth, `{"model":"claude","model":"gpt","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`},
		"responses":          {"/v1/responses", oaAuth, `{"model":"gpt","input":"a","input":"b"}`},
	} {
		resp, got := e.do(t, "POST", tc.path, tc.hdr, tc.body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, "repeats a top-level key") {
			t.Errorf("%s: status %d body %q, want 400 naming the repeated key", name, resp.StatusCode, got)
		}
	}
	calls := func() (n int) {
		for _, u := range []*upstream{e.o1, e.o2, e.an} {
			c, _, _ := u.snapshot()
			n += c
		}
		return n
	}
	if n := calls(); n != 0 {
		t.Fatalf("%d request(s) reached an upstream", n)
	}
	// A nested repeat is not a top-level one, and an ordinary body still relays.
	if resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth,
		`{"model":"gpt","messages":[{"role":"user","role":"user","content":"hi"}]}`); resp.StatusCode != 200 {
		t.Fatalf("a nested repeat was refused: %d", resp.StatusCode)
	}
}
