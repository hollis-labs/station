package llmgateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

// UsageCursor is an exclusive (time, ID) boundary, including equal-time rows.
type UsageCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}
type UsageExport struct {
	Entries    []Entry    `json:"entries"`
	NextBefore string     `json:"next_before"`
	Buffer     *SinkStats `json:"buffer,omitempty"`
}

// UsageExportHandler exports bounded pages without prompts or decision details.
// It is an eventually consistent history scan, not a transactional snapshot.
// Consumers must rescan to reconcile in-flight reservations and delayed writes.
func (g *Gateway) UsageExportHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q := Query{Limit: 500}
		values := r.URL.Query()
		for k, v := range values {
			if k != "before" || len(v) != 1 {
				openaiDialect{}.WriteError(w, 400, "invalid_cursor", "only one before cursor is supported")
				return
			}
		}
		if raw := values.Get("before"); raw != "" {
			if len(raw) > 1024 {
				openaiDialect{}.WriteError(w, 400, "invalid_cursor", "invalid usage cursor")
				return
			}
			b, err := base64.RawURLEncoding.DecodeString(raw)
			var cursor UsageCursor
			if err != nil || json.Unmarshal(b, &cursor) != nil || cursor.At.Year() < 2000 || cursor.At.Year() > 2200 || cursor.ID == "" || len(cursor.ID) > 256 {
				openaiDialect{}.WriteError(w, 400, "invalid_cursor", "invalid usage cursor")
				return
			}
			q.Before = &cursor
		}
		entries, err := g.ledger.store.Recent(r.Context(), q)
		if err != nil {
			openaiDialect{}.WriteError(w, 503, "ledger_unavailable", "usage history unavailable")
			return
		}
		out := UsageExport{Entries: entries}
		if out.Entries == nil {
			out.Entries = []Entry{}
		}
		if len(entries) == q.Limit {
			raw, _ := json.Marshal(UsageCursor{At: entries[0].Row.RecordedAt, ID: entries[0].ID})
			out.NextBefore = base64.RawURLEncoding.EncodeToString(raw)
		}
		// Clone before stripping payload details; memory stores may share slices.
		out.Entries = append([]Entry{}, out.Entries...)
		for i := range out.Entries {
			out.Entries[i].Detail = nil
		}
		if g.ledger.sink != nil {
			stats := g.ledger.sink.Stats()
			out.Buffer = &stats
		}
		writeJSON(w, 200, out)
	})
}
