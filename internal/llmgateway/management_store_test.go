package llmgateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func managementFixture() ManagementDocument {
	return ManagementDocument{SchemaVersion: 1, Gateway: Config{
		Providers: []ProviderConfig{{ID: "local", Dialect: "openai", BaseURL: "http://127.0.0.1:9999/v1", APIKeyRef: "keychain://station/provider"}},
		Models:    []ModelConfig{{Name: "chat", Targets: []TargetConfig{{Provider: "local", Model: "first"}, {Provider: "local", Model: "second", Tags: []string{"fallback"}}}}},
		Policy:    PolicyConfig{Mode: "enforce", DefaultEffect: "deny", Rules: []RuleConfig{{ID: "app", Principal: "app", Effect: "allow"}, {ID: "deny", Effect: "deny"}}},
		Budgets:   []BudgetConfig{{ID: "daily", MaxRequests: 5, Window: "day"}},
	}}
}

func TestManagementRevisionsPersistAndRollback(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(managementTempDir(t), "gateway.db")
	s, err := OpenManagementStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	doc := managementFixture()
	r1, err := s.Save(ctx, doc, 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Active(ctx); !errors.Is(err, ErrNoActiveRevision) {
		t.Fatalf("staging activated: %v", err)
	}
	if err := s.Activate(ctx, r1.ID, 0, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenManagementStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.Active(ctx)
	if err != nil || !reflect.DeepEqual(*got.Document, doc) {
		t.Fatalf("reopen lost configuration/order: %+v %v", got, err)
	}
	disabled := false
	doc.Gateway.Providers[0].Enabled = &disabled
	doc.Gateway.Models[0].Enabled = &disabled
	r2, err := s.Save(ctx, doc, r1.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	still, _ := s.Active(ctx)
	if still.ID != r1.ID {
		t.Fatal("staged revision changed active config")
	}
	if err := s.Activate(ctx, r2.ID, r1.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Active(ctx)
	if err != nil || !reflect.DeepEqual(*got.Document, doc) {
		t.Fatalf("enablement lost: %v", err)
	}
	if err := s.Activate(ctx, r1.ID, r1.ID, "stale"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale activation: %v", err)
	}
	if err := s.Activate(ctx, r1.ID, r2.ID, "rollback"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Active(ctx)
	if got.ID != r1.ID || got.Document.Gateway.Models[0].Enabled != nil {
		t.Fatal("rollback did not restore full revision")
	}
	var previous, selected int64
	var actor string
	if err := s.st.QueryRowContext(ctx, "SELECT previous_revision,revision,actor FROM gateway_activations ORDER BY id DESC LIMIT 1").Scan(&previous, &selected, &actor); err != nil {
		t.Fatal(err)
	}
	if previous != r2.ID || selected != r1.ID || actor != "rollback" {
		t.Fatal("rollback audit incorrect")
	}
	history, err := s.History(ctx, r2.ID, 1)
	if err != nil || len(history) != 1 || history[0].ID != r1.ID || history[0].Document != nil {
		t.Fatalf("history pagination: %+v %v", history, err)
	}
}

func TestManagementConcurrentWriters(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	a, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*ManagementStore{a.ManagementStore, b.ManagementStore} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Save(t.Context(), managementFixture(), 0, "editor")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	succeeded, conflicted := false, false
	for err := range errs {
		switch {
		case err == nil:
			if succeeded {
				t.Fatal("both stale writes succeeded")
			}
			succeeded = true
		case errors.Is(err, ErrRevisionConflict):
			conflicted = true
		default:
			t.Fatal(err)
		}
	}
	if !succeeded || !conflicted {
		t.Fatal("expected one successful editor and one conflict")
	}
}

func TestManagementValidationAndAtomicity(t *testing.T) {
	s, err := OpenManagementStore(t.Context(), filepath.Join(managementTempDir(t), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	r, err := s.Save(t.Context(), managementFixture(), 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ManagementDocument){
		"future document":  func(d *ManagementDocument) { d.SchemaVersion++ },
		"unknown provider": func(d *ManagementDocument) { d.Gateway.Models[0].Targets[0].Provider = "missing" },
		"invalid policy":   func(d *ManagementDocument) { d.Gateway.Policy.Rules[0].Effect = "maybe" },
		"invalid budget":   func(d *ManagementDocument) { d.Gateway.Budgets[0].MaxRequests = -1 },
		"credential header": func(d *ManagementDocument) {
			d.Gateway.Providers[0].Headers = map[string]string{"Authorization": "Bearer raw-secret"}
		},
		"URL credential": func(d *ManagementDocument) { d.Gateway.Providers[0].BaseURL = "https://user:" + "secret@example.com" },
		"literal key":    func(d *ManagementDocument) { d.Gateway.Providers[0].APIKeyRef = "raw-secret" },
		"client keys": func(d *ManagementDocument) {
			d.Clients = json.RawMessage(`[{"id":"app","sha256":"x","scopes":["llm-gateway.chat"]}]`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := managementFixture()
			change(&d)
			if _, err := s.Save(t.Context(), d, r.ID, "operator"); err == nil {
				t.Fatal("invalid revision accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Save(ctx, managementFixture(), r.ID, "operator"); err == nil {
		t.Fatal("cancelled write succeeded")
	}
	// Force failure after INSERT to exercise transaction rollback, not just validation.
	if _, err := s.st.ExecContext(t.Context(), "CREATE TRIGGER fail_head BEFORE UPDATE ON gateway_state BEGIN SELECT RAISE(ABORT, 'injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(t.Context(), managementFixture(), r.ID, "operator"); err == nil {
		t.Fatal("injected transaction failure ignored")
	}
	var leaked bool
	if err := s.st.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM gateway_revisions WHERE id>?)", r.ID).Scan(&leaked); err != nil || leaked {
		t.Fatalf("partial revision leaked: %v", err)
	}
	st, err := s.Status(t.Context())
	if err != nil || st.LatestRevision != r.ID || st.ActiveRevision != 0 {
		t.Fatalf("failed writes changed pointers: %+v %v", st, err)
	}
	if err := s.Activate(t.Context(), r.ID+100, 0, "operator"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing revision accepted: %v", err)
	}
}

func TestManagementRejectsCorruption(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	s, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Save(t.Context(), managementFixture(), 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.ExecContext(t.Context(), "UPDATE gateway_revisions SET sha256='corrupt' WHERE id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(t.Context(), r.ID, 0, "operator"); err == nil {
		t.Fatal("corrupt revision activated")
	}
}

func TestManagementStrictDecode(t *testing.T) {
	raw, _ := json.Marshal(managementFixture())
	if _, err := DecodeManagementDocument(append(raw, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing document accepted")
	}
	raw = append([]byte(`{"typo":true,`), raw[1:]...)
	if _, err := DecodeManagementDocument(raw); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// A revision stored by the old gateway carries hashed client keys. It still reads, and keeps its
// checksum, so migrated history stays verifiable; the next revision staged from it drops them.
func TestManagementReadsLegacyRevisionWithClients(t *testing.T) {
	ctx := t.Context()
	s, err := OpenManagementStore(ctx, filepath.Join(managementTempDir(t), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := managementFixture()
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// Splice the old shape in: {"schema_version":1,"gateway":{...},"clients":[...]}
	text := string(raw[:len(raw)-1]) + `,"clients":[{"id":"app","sha256":"` + hashOf("old-token") + `","scopes":["llm-gateway.chat"]}]}`
	sum := hashOf(text)
	if _, err := s.st.ExecContext(ctx, "INSERT INTO gateway_revisions(id,created_at,actor,document_version,document,sha256) VALUES(1,'2026-10-17T00:00:00Z','operator',1,?,?)", text, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.ExecContext(ctx, "UPDATE gateway_state SET latest_revision=1, active_revision=1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Active(ctx)
	if err != nil || got.SHA256 != sum || len(got.Document.Clients) == 0 {
		t.Fatalf("legacy revision did not read as stored: %+v %v", got, err)
	}
	// Saving the same document unchanged is refused: new documents do not name clients.
	if _, err := s.Save(ctx, *got.Document, 1, "operator"); err == nil {
		t.Fatal("a document with client keys was staged")
	}
	got.Document.Clients = nil
	if _, err := s.Save(ctx, *got.Document, 1, "operator"); err != nil {
		t.Fatalf("the same configuration without clients should stage: %v", err)
	}
}

func managementTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
