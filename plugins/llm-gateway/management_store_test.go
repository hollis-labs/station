package llmgateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/hollis-labs/station/platform"
)

func managementFixture() ManagementDocument {
	return ManagementDocument{SchemaVersion: 1, Gateway: Config{
		Providers: []ProviderConfig{{ID: "local", Dialect: "openai", BaseURL: "http://127.0.0.1:9999/v1", APIKeyRef: "keychain://station/provider"}},
		Models:    []ModelConfig{{Name: "chat", Targets: []TargetConfig{{Provider: "local", Model: "first"}, {Provider: "local", Model: "second", Tags: []string{"fallback"}}}}},
		Policy:    PolicyConfig{Mode: "enforce", DefaultEffect: "deny", Rules: []RuleConfig{{ID: "app", Principal: "app", Effect: "allow"}, {ID: "deny", Effect: "deny"}}},
		Budgets:   []BudgetConfig{{ID: "daily", MaxRequests: 5, Window: "day"}},
	}, Clients: []platform.KeyConfig{{ID: "app", SHA256: hashOf("test-token"), Scopes: []string{ScopeChat, ScopeModelRead}}}}
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
	defer s.Close()
	got, err := s.Active(ctx)
	if err != nil || !reflect.DeepEqual(*got.Document, doc) {
		t.Fatalf("reopen lost configuration/order: %+v %v", got, err)
	}
	disabled := false
	doc.Gateway.Providers[0].Enabled = &disabled
	doc.Gateway.Models[0].Enabled = &disabled
	doc.Clients = nil // revocation is part of the same configuration revision
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
		t.Fatalf("enablement/revocation lost: %v", err)
	}
	if err := s.Activate(ctx, r1.ID, r1.ID, "stale"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale activation: %v", err)
	}
	if err := s.Activate(ctx, r1.ID, r2.ID, "rollback"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Active(ctx)
	if got.ID != r1.ID || got.Document.Clients[0].ID != "app" {
		t.Fatal("rollback did not restore full revision")
	}
	var previous, selected int64
	var actor string
	if err := s.db.QueryRowContext(ctx, "SELECT previous_revision,revision,actor FROM gateway_activations ORDER BY id DESC LIMIT 1").Scan(&previous, &selected, &actor); err != nil {
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
	defer a.Close()
	b, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*ManagementStore{a, b} {
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
	defer s.Close()
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
		"URL credential":     func(d *ManagementDocument) { d.Gateway.Providers[0].BaseURL = "https://user:secret@example.com" },
		"literal key":        func(d *ManagementDocument) { d.Gateway.Providers[0].APIKeyRef = "raw-secret" },
		"duplicate identity": func(d *ManagementDocument) { d.Clients = append(d.Clients, d.Clients[0]) },
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
	if _, err := s.db.Exec("CREATE TRIGGER fail_head BEFORE UPDATE ON gateway_state BEGIN SELECT RAISE(ABORT, 'injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(t.Context(), managementFixture(), r.ID, "operator"); err == nil {
		t.Fatal("injected transaction failure ignored")
	}
	var leaked bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM gateway_revisions WHERE id>?)", r.ID).Scan(&leaked); err != nil || leaked {
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

func TestManagementRejectsCorruptionAndFutureSchemas(t *testing.T) {
	path := filepath.Join(managementTempDir(t), "gateway.db")
	s, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Save(t.Context(), managementFixture(), 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE gateway_revisions SET sha256='corrupt' WHERE id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(t.Context(), r.ID, 0, "operator"); err == nil {
		t.Fatal("corrupt revision activated")
	}
	if _, err := s.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if next, err := OpenManagementStore(t.Context(), path); err == nil {
		next.Close()
		t.Fatal("future DB schema opened")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 999 {
		t.Fatal("future schema was changed")
	}
}

func TestManagementPrivateFilesAndStrictDecode(t *testing.T) {
	dir := managementTempDir(t)
	path := filepath.Join(dir, "gateway.db")
	s, err := OpenManagementStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("database not private")
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenManagementStore(t.Context(), link); err == nil {
		s.Close()
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenManagementStore(t.Context(), path); err == nil {
		s.Close()
		t.Fatal("public database accepted")
	}
	raw, _ := json.Marshal(managementFixture())
	if _, err := DecodeManagementDocument(append(raw, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing document accepted")
	}
	raw = append([]byte(`{"typo":true,`), raw[1:]...)
	if _, err := DecodeManagementDocument(raw); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func managementTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
