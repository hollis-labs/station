package usagemonitor

import (
	"context"
	"encoding/json"
	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProviderMeasurements(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		kind, raw string
		count     int
		used      *float64
	}{
		{"claude", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null}`, 1, number(0)},
		{"claude", `{"five_hour":{"resets_at":null}}`, 1, nil},
		{"codex", `{"rate_limit":{"primary_window":{"used_percent":17,"reset_at":1900000000},"secondary_window":null}}`, 1, number(17)},
		{"opencode-go", `{"usage":{"rolling":{"usagePercent":0.5,"resetInSec":3600},"monthly":{"used":15,"limit":60}}}`, 2, number(0.5)},
		{"agy", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"display_name":"Gemini Models","buckets":[{"bucket_id":"five-hour","remaining_fraction":0.75},{"bucket_id":"disabled","remaining_fraction":1,"disabled":true}]}]}}}`, 1, number(25)},
		{"claude", `{}`, 0, nil},
	} {
		t.Run(tc.kind+tc.raw, func(t *testing.T) {
			w, err := parseWindows(tc.kind, []byte(tc.raw), now)
			if err != nil {
				t.Fatal(err)
			}
			if len(w) != tc.count {
				t.Fatalf("windows: %+v", w)
			}
			if tc.count > 0 {
				if tc.used == nil {
					if w[0].Used != nil || w[0].Remaining != nil {
						t.Fatal("unknown became measured zero")
					}
				} else if w[0].Used == nil || *w[0].Used != *tc.used {
					t.Fatalf("wrong units: %+v", w)
				}
			}
		})
	}
	if _, err := parseWindows("agy", []byte(`{"status":"ERROR","command":{"name":"usage"}}`), now); err == nil {
		t.Fatal("failed agy command accepted")
	}
	w, _ := parseWindows("claude", []byte(`{"five_hour":{"utilization":101}}`), now)
	if (Observation{ID: "x", ObservedAt: now, Status: "ok", Windows: w}).validate() == nil {
		t.Fatal("invalid percentage accepted")
	}
}

func TestGatewayPagedProjectionAndRetry(t *testing.T) {
	s, _ := testStore(t)
	e := testEvent("first")
	e.CostUSD = number(2)
	entry := llmgateway.Entry{ID: e.ID, Row: e.Row, CostUSD: e.CostUSD, Outcome: "ok"}
	second := entry
	second.ID = "second"
	reserved := entry
	reserved.ID = "pending"
	reserved.Outcome = llmgateway.OutcomeReserved
	calls := 0
	c := &collector{source: Source{ID: "gateway", AccountID: "claude", Collector: "gateway", GatewayURL: "https://gateway.example/api/llm-gateway"}, store: s,
		resolve: func(context.Context, string) (string, error) { return "secret", nil },
		client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path != "/api/llm-gateway/v1/usage/export" {
				t.Fatal(r.URL)
			}
			page := llmgateway.UsageExport{Entries: []llmgateway.Entry{entry, reserved}, NextBefore: "older"}
			if r.URL.Query().Get("before") == "older" {
				page = llmgateway.UsageExport{Entries: []llmgateway.Entry{second}}
			}
			raw, _ := json.Marshal(page)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}, nil
		})}}
	for range 2 {
		if err := c.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Events(t.Context(), Query{Limit: 10})
	if err != nil || len(page.Items) != 2 || calls != 4 {
		t.Fatal(page, err, calls)
	}
	for _, r := range page.Items {
		if r.Data.Row.CostKind != "subscription_equivalent" {
			t.Fatal("subscription mispriced", r)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCredentialRotationAndSafeFailureHistory(t *testing.T) {
	s, _ := testStore(t)
	secret := "token-one"
	status := 200
	calls := 0
	c := &collector{source: Source{ID: "claude-native", AccountID: "claude", Collector: "claude", CredentialRef: "file:///unused"}, store: s,
		resolve: func(context.Context, string) (string, error) { return secret, nil },
		client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer "+secret {
				t.Fatal("credential not reread")
			}
			if r.URL.Host != "api.anthropic.com" {
				t.Fatal(r.URL)
			}
			body := `{"five_hour":{"utilization":42}}`
			if status != 200 {
				body = secret + " secret upstream error"
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}}
	if err := c.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	secret = "token-two"
	status = 401
	if err := c.Collect(t.Context()); err == nil || err.Error() != "authentication_required" {
		t.Fatal(err)
	}
	v, err := s.Accounts(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || v[0].LastSuccess.Data.Windows[0].Used == nil || *v[0].LastSuccess.Data.Windows[0].Used != 42 || !v[0].Stale || v[0].Latest.Data.ErrorCode != "authentication_required" {
		t.Fatalf("bad failure recovery: %+v", v)
	}
}

func TestNativeCredentialsReadOnlyExtraction(t *testing.T) {
	for _, tc := range []struct{ format, raw, token, account string }{
		{"claude", `{"claudeAiOauth":{"accessToken":"abc"}}`, "abc", ""},
		{"codex", `{"tokens":{"access_token":"def","account_id":"acct"}}`, "def", "acct"},
	} {
		token, account, err := credential(tc.raw, tc.format)
		if err != nil || token != tc.token || account != tc.account {
			t.Fatal(token, account, err)
		}
	}
	if _, _, err := credential(`{"OPENAI_API_KEY":"key"}`, "codex"); err == nil {
		t.Fatal("API key treated as subscription OAuth")
	}
}
