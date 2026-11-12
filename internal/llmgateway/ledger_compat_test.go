package llmgateway

// Old versus new, for the cutover: the same requests through the old plugin (plugins/llm-gateway under
// platform) and through this package write the same llm_calls columns, value for value. This file
// imports the old code and is deleted with it (docs/conversion-notes.md, "What the last port must do").

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hollis-labs/station/platform"
	oldgw "github.com/hollis-labs/station/plugins/llm-gateway"
)

func TestPGInsertMatchesOldGatewayColumnForColumn(t *testing.T) {
	ctx := context.Background()
	dsn := pgDSN(t)
	resetPG(t, dsn)
	t.Setenv("COMPAT_LEDGER_DSN", dsn)
	t.Setenv("COMPAT_O_KEY", "upstream-o-secret")
	t.Setenv("COMPAT_A_KEY", "upstream-a-secret")
	o, a := newUpstream(t, openaiOK), newUpstream(t, anthropicOK)
	y := fmt.Sprintf(`
auth:
  keys:
    - {id: caller-1, sha256: %s, scopes: ["llm-gateway.*"]}
plugins:
  llm-gateway:
    catalog: {refresh: false, cache_dir: %q}
    providers:
      - {id: o1, dialect: openai, catalog_provider: openai, base_url: "%s/v1", api_key_env: COMPAT_O_KEY}
      - {id: an, dialect: anthropic, catalog_provider: anthropic, base_url: "%s/v1", api_key_env: COMPAT_A_KEY}
    models:
      - name: gpt
        targets:
          - {provider: o1, model: gpt-real, price: {input_per_million: 2, output_per_million: 8, cache_read_per_million: 0.5}}
      - name: claude
        targets:
          - {provider: an, model: claude-real, price: {input_per_million: 3, output_per_million: 15, cache_read_per_million: 0.3, cache_write_per_million: 3.75}}
    ledger:
      store: postgres
      postgres: {dsn_env: COMPAT_LEDGER_DSN}
`, hashOf("good"), t.TempDir(), o.URL, a.URL)

	oldCfg, err := platform.ParseConfig([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	oldApp, err := platform.Assemble(ctx, platform.Options{Config: oldCfg, Logger: quiet, Plugins: []platform.Plugin{oldgw.New()}})
	if err != nil {
		t.Fatal(err)
	}
	oldSrv := httptest.NewServer(oldApp.Handler())
	newApp, err := assembleYAML(ctx, y, testOptions{logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	newSrv := httptest.NewServer(newApp.Handler())
	t.Cleanup(func() {
		oldSrv.Close()
		newSrv.Close()
		_ = oldApp.Close(ctx)
		_ = newApp.Close(ctx)
	})

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx) //nolint:errcheck
	count := func() (n int) {
		_ = admin.QueryRow(ctx, "SELECT count(*) FROM llm_calls").Scan(&n)
		return n
	}
	send := func(base, path, body string) {
		t.Helper()
		before := count()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		resp, err := doJSON(base, req)
		if err != nil || resp != 200 {
			t.Fatalf("%s %s = %d %v", base, path, resp, err)
		}
		for deadline := time.Now().Add(8 * time.Second); count() == before; {
			if time.Now().After(deadline) {
				t.Fatalf("no ledger row for %s %s", base, path)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// Old first, then new, for each request, so the rows pair up in recorded order.
	send(oldSrv.URL, "/api/llm-gateway/v1/chat/completions", oaReq)
	send(newSrv.URL, "/api/llm-gateway/v1/chat/completions", oaReq)
	send(oldSrv.URL, "/api/llm-gateway/v1/messages", anReq)
	send(newSrv.URL, "/api/llm-gateway/v1/messages", anReq)

	rows, err := admin.Query(ctx, `SELECT (to_jsonb(llm_calls) - 'id' - 'recorded_at' - 'request_id' - 'latency_ms')::text FROM llm_calls ORDER BY recorded_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if len(got) != 4 {
		t.Fatalf("%d rows, want 4", len(got))
	}
	for i := 0; i < 4; i += 2 {
		if got[i] != got[i+1] {
			t.Errorf("old and new rows differ:\n old %s\n new %s", got[i], got[i+1])
		}
	}
	var cols int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name='llm_calls'").Scan(&cols); err != nil {
		t.Fatal(err)
	}
	t.Logf("compared %d columns per row (minus id, recorded_at, request_id, latency_ms) for an OpenAI and an Anthropic call", cols-4)
}

func doJSON(base string, req *http.Request) (int, error) {
	r, err := http.NewRequest(req.Method, base+req.URL.Path, req.Body)
	if err != nil {
		return 0, err
	}
	r.Header = req.Header.Clone()
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
