package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/llm/core/usageledger"
)

// ---- JEV ------------------------------------------------------------------

const jevAnswers = `{"urgent":{"type":"choice","choice":"yes","probabilities":{"yes":0.9,"no":0.1},"confidence":0.9},"on_topic":{"type":"noul","noul":0.8}}`
const jevResponse = `{"model":"jev-real","answers":` + jevAnswers + `,"usage":{"input_tokens":120,"output_tokens":9}}`

func jevOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, jevResponse)
}

// The option order inside questions is read by JEV; the relay must not reorder it.
const jevReq = `{"state":"ticket text","model":"judge","questions":{"urgent":{"type":"choice","instructions":"is it urgent","criteria":{"zebra":"z","apple":"a"}},"on_topic":{"type":"noul","instructions":"on topic?"}}}`

func TestJEVDecisionRelayPreservesOrderMetersAndKeepsAnswers(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	resp, got := e.do(t, "POST", "/v1/systemone", oaAuth, jevReq)
	if resp.StatusCode != 200 || got != jevResponse {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	_, hdr, sent := e.jv.snapshot()
	if hdr.Get("Authorization") != "Bearer upstream-jev-secret" {
		t.Fatalf("credential %q", hdr.Get("Authorization"))
	}
	if !strings.Contains(sent, `"model":"jev-real"`) {
		t.Fatalf("model not rewritten: %s", sent)
	}
	if !strings.Contains(sent, `"criteria":{"zebra":"z","apple":"a"}`) {
		t.Fatalf("option order was not preserved: %s", sent)
	}
	entries, totals := usageOf(t, e)
	en := entries[0]
	if en.Operation != OpDecide || en.Row.Usage.UncachedInputTokens.Tokens != 120 || en.Row.Usage.OutputTokens.Tokens != 9 {
		t.Fatalf("entry %+v", en)
	}
	if string(en.Detail) != jevAnswers {
		t.Fatalf("decision answers not kept: %s", en.Detail)
	}
	want := 120*1.0/1e6 + 9*4.0/1e6
	if en.CostUSD == nil || *en.CostUSD < want-1e-12 || *en.CostUSD > want+1e-12 || totals[0].Calls != 1 {
		t.Fatalf("cost %v totals %+v", en.CostUSD, totals)
	}
}

func TestJEVKindAndScopeAndValidation(t *testing.T) {
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{})
	if resp, got := e.do(t, "POST", "/v1/chat/completions", oaAuth, `{"model":"judge","messages":[{"role":"user","content":"x"}]}`); resp.StatusCode != 400 || !strings.Contains(got, "model_not_available") {
		t.Fatalf("chat on a decision model: %d %s", resp.StatusCode, got)
	}
	if resp, _ := e.do(t, "POST", "/v1/systemone", oaAuth, `{"model":"gpt","state":"s","questions":{"q":{}}}`); resp.StatusCode != 400 {
		t.Fatalf("decision on a chat model: %d", resp.StatusCode)
	}
	for name, body := range map[string]string{
		"no state":     `{"model":"judge","questions":{"q":{}}}`,
		"empty state":  `{"model":"judge","state":"","questions":{"q":{}}}`,
		"no questions": `{"model":"judge","state":"s","questions":{}}`,
		"array":        `{"model":"judge","state":"s","questions":[1]}`,
	} {
		if resp, _ := e.do(t, "POST", "/v1/systemone", oaAuth, body); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, "POST", "/v1/systemone", map[string]string{"Authorization": "Bearer narrow"}, jevReq); resp.StatusCode != 403 {
		t.Fatalf("scope: %d", resp.StatusCode)
	}
	if n, _, _ := e.jv.snapshot(); n != 0 {
		t.Fatal("an invalid or unauthorized decision reached JEV")
	}
}

func TestJEVModelCannotBeChat(t *testing.T) {
	c := Config{
		Providers: []ProviderConfig{{ID: "j", Dialect: "jev", BaseURL: "http://x"}},
		Models:    []ModelConfig{{Name: "m", Targets: []TargetConfig{{Provider: "j", Model: "x"}}}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("a chat model on the jev dialect was accepted")
	}
}

func TestPolicyCanTargetDecisions(t *testing.T) {
	pol := `    policy:
      mode: enforce
      rules:
        - {id: no-decisions-for-narrow, operation: decision, principal: "caller-*", effect: deny}`
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{policy: pol})
	if resp, _ := e.do(t, "POST", "/v1/systemone", oaAuth, jevReq); resp.StatusCode != 403 {
		t.Fatalf("decision not denied: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq); resp.StatusCode != 200 {
		t.Fatalf("chat must be unaffected: %d", resp.StatusCode)
	}
}

// ---- store contract ------------------------------------------------------

func sampleEntry(id, principal, provider, outcome string, at time.Time, cost *float64) Entry {
	u := usageledger.NewUsage()
	u.UncachedInputTokens = usageledger.Component{Tokens: 10, Provenance: usageledger.ProvenanceMeasured}
	u.OutputTokens = usageledger.Component{Tokens: 5, Provenance: usageledger.ProvenanceMeasured}
	e := Entry{ID: id, Alias: "a", Operation: OpChat, Dialect: "openai", RequestID: "req-" + id, Status: 200,
		Outcome: outcome, Streamed: true, LatencyMS: 42, Verified: true, CostUSD: cost,
		Row: usageledger.Row{SessionID: principal, MessageID: "req-" + id, Provider: provider, Model: "m", Usage: u, RecordedAt: at}}
	if cost != nil {
		e.Row.Price = &usageledger.PriceSnapshot{InputPerMillion: 1, OutputPerMillion: 2, Source: "t",
			UnknownRates: usageledger.UnknownRates{Reasoning: true}}
		e.Row.CostKind = usageledger.CostKindAPIEstimated
	}
	return e
}

func f64(v float64) *float64 { return &v }

// runStoreContract is what every Store must satisfy.
func runStoreContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	es := []Entry{
		sampleEntry("e1", "alice", "p1", OutcomeOK, base, f64(0.5)),
		sampleEntry("e2", "bob", "p1", OutcomeOK, base.Add(time.Minute), nil),
		sampleEntry("e3", "alice", "p2", OutcomeFailedOver, base.Add(2*time.Minute), nil),
		sampleEntry("e4", "alice", "p1", OutcomeOK, base.Add(3*time.Minute), f64(0.25)),
	}
	es[3].Detail = json.RawMessage(`{"k":{"v":1}}`)
	if err := s.Add(ctx, es...); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, es[0], es[1]); err != nil { // a retry must be a no-op
		t.Fatal(err)
	}
	all, err := s.Recent(ctx, Query{})
	if err != nil || len(all) != 4 {
		t.Fatalf("recent: %d entries, %v", len(all), err)
	}
	for i, e := range all {
		if e.ID != es[i].ID {
			t.Fatalf("order: got %s at %d, want %s", e.ID, i, es[i].ID)
		}
	}
	// Round trip fidelity: the entry comes back as it went in.
	got := all[3]
	if got.CostUSD == nil || *got.CostUSD != 0.25 || got.Row.Price == nil || got.Row.Price.Source != "t" ||
		!got.Row.Price.UnknownRates.Reasoning || got.Row.CostKind != usageledger.CostKindAPIEstimated {
		t.Fatalf("price/cost lost: %+v", got)
	}
	if got.Row.Usage.OutputTokens.Tokens != 5 || got.Row.Usage.CacheWriteTokens.Provenance != usageledger.ProvenanceUnknown {
		t.Fatalf("usage lost or unknown became measured: %+v", got.Row.Usage)
	}
	if string(got.Detail) != `{"k":{"v":1}}` && !jsonEqual(got.Detail, `{"k":{"v":1}}`) {
		t.Fatalf("detail: %s", got.Detail)
	}
	if all[1].CostUSD != nil || all[1].Row.Price != nil {
		t.Fatal("an unpriced entry came back priced (nil must stay nil, not zero)")
	}
	if !got.Row.RecordedAt.Equal(es[3].Row.RecordedAt) || got.Operation != OpChat || !got.Verified || !got.Streamed || got.LatencyMS != 42 {
		t.Fatalf("fields: %+v", got)
	}
	if r, _ := s.Recent(ctx, Query{Limit: 2}); len(r) != 2 || r[0].ID != "e3" || r[1].ID != "e4" {
		t.Fatalf("limit must keep the newest, oldest first: %+v", r)
	}
	if r, _ := s.Recent(ctx, Query{Principal: "bob"}); len(r) != 1 || r[0].ID != "e2" {
		t.Fatalf("principal filter: %+v", r)
	}
	if r, _ := s.Recent(ctx, Query{Since: base.Add(90 * time.Second)}); len(r) != 2 {
		t.Fatalf("since filter: %d", len(r))
	}
	tot, err := s.Totals(ctx, Query{})
	if err != nil || len(tot) != 1 {
		t.Fatalf("totals %+v %v (only ok calls count, so only p1)", tot, err)
	}
	if tot[0].Calls != 3 || tot[0].Input != 30 || tot[0].Output != 15 || tot[0].UnpricedCalls != 1 || tot[0].CostUSD != 0.75 {
		t.Fatalf("totals %+v", tot[0])
	}
	if tot, _ := s.Totals(ctx, Query{Principal: "bob"}); len(tot) != 1 || tot[0].Calls != 1 || tot[0].UnpricedCalls != 1 {
		t.Fatalf("filtered totals %+v", tot)
	}
}

func jsonEqual(a json.RawMessage, b string) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal([]byte(b), &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
}

func TestMemStoreContract(t *testing.T) { runStoreContract(t, newMemStore(100)) }

func TestMemStoreEvictsOldestAndKeepsIdempotency(t *testing.T) {
	s := newMemStore(2)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		_ = s.Add(ctx, sampleEntry(id, "p", "p", OutcomeOK, now, nil))
	}
	r, _ := s.Recent(ctx, Query{})
	if len(r) != 2 || r[0].ID != "b" {
		t.Fatalf("%+v", r)
	}
}

// ---- postgres ------------------------------------------------------------

func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("STATION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("STATION_TEST_PG_DSN not set; skipping Postgres tests")
	}
	return dsn
}

func resetPG(t *testing.T, dsn string) {
	t.Helper()
	s, err := openPGStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE llm_calls`); err != nil {
		t.Fatal(err)
	}
}

func TestPGStoreContractAndMigrationIdempotent(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	s, err := openPGStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s2, err := openPGStore(context.Background(), dsn) // a second start migrates nothing and does not fail
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
	runStoreContract(t, s)
}

func TestPGStoreRefusesNewerSchema(t *testing.T) {
	dsn := pgDSN(t)
	s, err := openPGStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _ = s.pool.Exec(context.Background(), `INSERT INTO llm_ledger_schema (version) VALUES (999)`)
	defer s.pool.Exec(context.Background(), `DELETE FROM llm_ledger_schema WHERE version = 999`) //nolint:errcheck
	if _, err := openPGStore(context.Background(), dsn); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("downgrade not refused: %v", err)
	}
}

func TestPGDSNErrorsDoNotEchoPassword(t *testing.T) {
	_, err := openPGStore(context.Background(), "postgres://u:hunter2secret@127.0.0.1:1/db?connect_timeout=1")
	if err == nil || strings.Contains(err.Error(), "hunter2secret") {
		t.Fatalf("%v", err)
	}
}

// failingStore fails the first n Add calls.
type failingStore struct {
	*memStore
	mu    sync.Mutex
	fails int
	calls int
}

func (f *failingStore) Add(ctx context.Context, es ...Entry) error {
	f.mu.Lock()
	f.calls++
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return errors.New("database down")
	}
	f.mu.Unlock()
	return f.memStore.Add(ctx, es...)
}

func TestAsyncStoreRetriesWithoutLossOrDuplication(t *testing.T) {
	inner := &failingStore{memStore: newMemStore(100), fails: 2}
	a := newAsyncStore(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Close()
	for _, id := range []string{"x1", "x2", "x3"} {
		_ = a.Add(context.Background(), sampleEntry(id, "p", "p", OutcomeOK, time.Now(), nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := a.Recent(context.Background(), Query{})
	if len(r) != 3 {
		t.Fatalf("entries after retries: %d", len(r))
	}
	if st := a.Stats(); st.Failed != 2 || st.Dropped != 0 || st.Queued != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAsyncStoreCountsAndLogsDropsWhenFull(t *testing.T) {
	var buf bytes.Buffer
	block := &failingStore{memStore: newMemStore(10), fails: 1 << 30}
	a := newAsyncStore(block, slog.New(slog.NewJSONHandler(&buf, nil)))
	for i := 0; i < asyncQueue+asyncBatch+50; i++ {
		_ = a.Add(context.Background(), sampleEntry(fmt.Sprint("d", i), "p", "p", OutcomeOK, time.Now(), nil))
	}
	st := a.Stats()
	if st.Dropped == 0 {
		t.Fatalf("a full queue must drop and say so: %+v", st)
	}
	if !strings.Contains(buf.String(), "ledger.entry_dropped") || !strings.Contains(buf.String(), `"principal":"p"`) {
		t.Fatal("a dropped entry must be logged with enough to recover it")
	}
	done := make(chan struct{})
	go func() { a.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung while the database was down")
	}
}

// End to end: calls are recorded to Postgres and survive a restart.
func TestLedgerSurvivesRestartOnPostgres(t *testing.T) {
	dsn := pgDSN(t)
	resetPG(t, dsn)
	t.Setenv("TEST_LEDGER_DSN", dsn)
	extra := `    ledger:
      store: postgres
      postgres: {dsn_env: TEST_LEDGER_DSN}`
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{extra: extra})
	e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq)
	e.do(t, "POST", "/v1/systemone", oaAuth, jevReq)
	waitEntries := func(env *gwEnv, n int) []Entry {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			entries, _ := usageOf(t, env)
			if len(entries) >= n {
				return entries
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("ledger never reached %d entries", n)
		return nil
	}
	waitEntries(e, 2)
	// Stop the first gateway (flushes the queue), then start a fresh one.
	e.srv.Close()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.app.Close(stopCtx)

	e2 := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{extra: extra})
	entries := waitEntries(e2, 2)
	if entries[0].Row.SessionID != "caller-1" || entries[1].Operation != OpDecide || string(entries[1].Detail) == "" {
		t.Fatalf("restarted ledger %+v", entries)
	}
	_, body := e2.do(t, "GET", "/v1/usage?principal=nobody", oaAuth, "")
	if !strings.Contains(body, `"entries":[]`) && !strings.Contains(body, `"entries":null`) {
		t.Fatalf("principal filter over the API: %s", body)
	}
	if !strings.Contains(body, `"buffer"`) {
		t.Fatalf("buffer stats missing: %s", body)
	}
}

func TestLedgerConfigValidation(t *testing.T) {
	base := func() Config {
		return Config{
			Providers: []ProviderConfig{{ID: "p", Dialect: "openai", BaseURL: "http://x"}},
			Models:    []ModelConfig{{Name: "m", Targets: []TargetConfig{{Provider: "p", Model: "x"}}}},
		}
	}
	for name, l := range map[string]LedgerConfig{
		"unknown store":     {Store: "sqlite"},
		"postgres no dsn":   {Store: "postgres"},
		"postgres both":     {Store: "postgres", Postgres: PostgresConfig{DSNRef: "a://b", DSNEnv: "X"}},
		"dsn without store": {Postgres: PostgresConfig{DSNEnv: "X"}},
		"negative history":  {History: -1},
	} {
		c := base()
		c.Ledger = l
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := base()
	ok.Ledger = LedgerConfig{Store: "postgres", Postgres: PostgresConfig{DSNRef: "keychain://station/ledger"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPGConversationSurvivesRestart(t *testing.T) {
	dsn := pgDSN(t)
	s, err := openPGStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLedger(s, quiet)
	l.Add(Record{Caller: Caller{ID: "session-test-app", Known: true, ConversationID: "station-session-proof"}, RequestID: "session-proof", Target: &target{provider: &provider{id: "test"}, model: "test"}, Outcome: OutcomeOK})
	l.Close()
	s, err = openPGStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := s.Recent(context.Background(), Query{Principal: "session-test-app"})
	if err != nil || len(entries) == 0 {
		t.Fatal("missing persisted conversation", err)
	}
	if entries[len(entries)-1].ConversationID != "station-session-proof" {
		t.Fatal("conversation lost on restart")
	}
}
