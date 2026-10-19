package llmgateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageExportPaginatesEqualTimestampsAndStripsDetails(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = newMemStore(2000)
			if backend == "sqlite" {
				var err error
				store, err = openSQLiteStore(t.Context(), filepath.Join(managementTempDir(t), "export.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer store.Close()
			at := time.Now().UTC()
			entries := make([]Entry, 1007)
			for i := range entries {
				entries[i] = sampleEntry(fmt.Sprintf("id-%04d", i), "app", "anthropic", OutcomeOK, at, nil)
				entries[i].Detail = json.RawMessage(`{"answer":"private"}`)
			}
			if err := store.Add(t.Context(), entries...); err != nil {
				t.Fatal(err)
			}
			gateway := &Gateway{ledger: NewLedger(store, slog.New(slog.NewTextHandler(io.Discard, nil)))}
			seen := map[string]bool{}
			cursor := ""
			for range 10 {
				w := httptest.NewRecorder()
				gateway.UsageExportHandler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/usage/export?before="+url.QueryEscape(cursor), nil))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				var page UsageExport
				if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				for _, e := range page.Entries {
					if seen[e.ID] {
						t.Fatal("duplicate export", e.ID)
					}
					seen[e.ID] = true
					if len(e.Detail) != 0 {
						t.Fatal("payload detail exported")
					}
				}
				cursor = page.NextBefore
				if cursor == "" {
					break
				}
			}
			if len(seen) != len(entries) {
				t.Fatalf("lost equal-time rows: %d/%d", len(seen), len(entries))
			}
			original, err := store.Recent(t.Context(), Query{Limit: 1})
			if err != nil || len(original[0].Detail) == 0 {
				t.Fatal("export mutated canonical ledger", err)
			}
			w := httptest.NewRecorder()
			gateway.UsageExportHandler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/usage/export?before=invalid", nil))
			if w.Code != 400 {
				t.Fatal("bad cursor accepted")
			}
		})
	}
}
