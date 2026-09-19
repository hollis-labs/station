// Package atlastest is a fake Tesseract for testing the Atlas server without a
// running daemon: an httptest server that answers the three read routes Atlas
// uses (recall, current-by-key, namespace listing) from an in-memory set of
// revisions, plus builders for the revisions.
//
// It speaks the HTTP door's shapes, verified against the live service on
// 2026-09-19: recall returns {results, manifest} with results_total and
// next_cursor, a trailing /* in a namespace expands to its sub-namespaces but
// does not include the namespace itself, and a missing key is a 404 carrying
// {code: not_found}.
package atlastest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

// Fake is a running fake Tesseract.
type Fake struct {
	srv *httptest.Server

	mu    sync.Mutex
	revs  []tesseract.Revision
	calls map[string]int
}

// New starts a fake holding revs and stops it when the test ends.
func New(t testing.TB, revs ...tesseract.Revision) *Fake {
	t.Helper()
	f := &Fake{revs: revs, calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/memory/recall", f.recall)
	mux.HandleFunc("GET /v1/knowledge/current", f.current)
	mux.HandleFunc("GET /v1/namespaces/list", f.namespaces)
	mux.HandleFunc("GET /v1/health/readiness", func(w http.ResponseWriter, _ *http.Request) {
		f.count("readiness")
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.srv.URL }

// Close stops the fake early, to test a Tesseract that has gone away.
func (f *Fake) Close() { f.srv.Close() }

// Set replaces the revisions the fake serves.
func (f *Fake) Set(revs ...tesseract.Revision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revs = revs
}

// Calls returns how many times a route was hit: "recall", "current",
// "namespaces" or "readiness".
func (f *Fake) Calls(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[route]
}

func (f *Fake) count(route string) {
	f.mu.Lock()
	f.calls[route]++
	f.mu.Unlock()
}

func (f *Fake) snapshot() []tesseract.Revision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tesseract.Revision(nil), f.revs...)
}

func matches(namespace, pattern string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(namespace, prefix+"/")
	}
	return namespace == pattern
}

func (f *Fake) recall(w http.ResponseWriter, r *http.Request) {
	f.count("recall")
	var req struct {
		Namespaces  []string `json:"namespaces"`
		Query       string   `json:"query"`
		Limit       int      `json:"limit"`
		Cursor      string   `json:"cursor"`
		PayloadMode string   `json:"payload_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "validation_error", "message": err.Error()})
		return
	}

	type hit struct {
		rev   tesseract.Revision
		score float64
	}
	var hits []hit
	terms := strings.Fields(strings.ToLower(req.Query))
	for _, rev := range f.snapshot() {
		in := false
		for _, ns := range req.Namespaces {
			if matches(rev.Namespace, ns) {
				in = true
				break
			}
		}
		if !in {
			continue
		}
		score := 1.0
		if len(terms) > 0 {
			haystack := strings.ToLower(rev.MemoryKey + " " + rev.Payload.Summary + " " + rev.Payload.Body)
			score = 0
			for _, term := range terms {
				if strings.Contains(haystack, term) {
					score++
				}
			}
			if score == 0 {
				continue
			}
		}
		hits = append(hits, hit{rev, score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

	offset := 0
	if req.Cursor != "" {
		raw, err := base64.StdEncoding.DecodeString(req.Cursor)
		if err == nil {
			offset, _ = strconv.Atoi(strings.TrimPrefix(string(raw), "o:"))
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 30
	}
	end := min(offset+limit, len(hits))
	if offset > end {
		offset = end
	}
	page := hits[offset:end]

	results := make([]map[string]any, 0, len(page))
	for _, h := range page {
		entry := map[string]any{"revision": shape(h.rev, req.PayloadMode)}
		if len(terms) > 0 {
			entry["score"] = h.score
		}
		results = append(results, entry)
	}
	var next any
	if end < len(hits) {
		next = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("o:%d", end)))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"facets":  map[string]any{},
		"manifest": map[string]any{
			"results_total":    len(hits),
			"results_returned": len(page),
			"truncated":        end < len(hits),
			"truncation_reason": func() string {
				if end < len(hits) {
					return "limit"
				}
				return ""
			}(),
			"next_cursor": next,
		},
	})
}

// shape projects a revision the way payload_mode does: keys is identity only,
// summary drops the body and data, full carries everything.
func shape(rev tesseract.Revision, mode string) tesseract.Revision {
	switch mode {
	case "keys":
		return tesseract.Revision{
			RevisionID: rev.RevisionID, ItemID: rev.ItemID, Domain: rev.Domain,
			Namespace: rev.Namespace, MemoryKey: rev.MemoryKey, CreatedAt: rev.CreatedAt,
		}
	case "summary":
		rev.Payload.Body = ""
		rev.Payload.Data = nil
	}
	return rev
}

func (f *Fake) current(w http.ResponseWriter, r *http.Request) {
	f.count("current")
	ns, key := r.URL.Query().Get("namespace"), r.URL.Query().Get("key")
	for _, rev := range f.snapshot() {
		if rev.Namespace == ns && rev.MemoryKey == key {
			writeJSON(w, http.StatusOK, rev)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"code":    "not_found",
		"details": nil,
		"message": fmt.Sprintf("memory not found: no current revision for %s/%s", ns, key),
	})
}

func (f *Fake) namespaces(w http.ResponseWriter, r *http.Request) {
	f.count("namespaces")
	prefix := r.URL.Query().Get("prefix")
	seen := map[string]bool{}
	var items []map[string]string
	for _, rev := range f.snapshot() {
		if strings.HasPrefix(rev.Namespace, prefix) && !seen[rev.Namespace] {
			seen[rev.Namespace] = true
			items = append(items, map[string]string{"namespace": rev.Namespace})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": false})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Opt adjusts a revision under construction.
type Opt func(*tesseract.Revision)

// Summary sets the summary.
func Summary(s string) Opt { return func(r *tesseract.Revision) { r.Payload.Summary = s } }

// Body sets the body.
func Body(s string) Opt { return func(r *tesseract.Revision) { r.Payload.Body = s } }

// Tags replaces the tag list.
func Tags(tags ...string) Opt { return func(r *tesseract.Revision) { r.Tags = tags } }

// AddTags appends to the tag list.
func AddTags(tags ...string) Opt {
	return func(r *tesseract.Revision) { r.Tags = append(r.Tags, tags...) }
}

// Data merges keys into the record's data.
func Data(kv map[string]any) Opt {
	return func(r *tesseract.Revision) {
		m := map[string]any{}
		_ = json.Unmarshal(r.Payload.Data, &m)
		for k, v := range kv {
			m[k] = v
		}
		r.Payload.Data, _ = json.Marshal(m)
	}
}

// State merges keys into consumer_state.
func State(kv map[string]any) Opt {
	return func(r *tesseract.Revision) {
		m := map[string]any{}
		_ = json.Unmarshal(r.ConsumerState, &m)
		for k, v := range kv {
			m[k] = v
		}
		r.ConsumerState, _ = json.Marshal(m)
	}
}

// NoState removes consumer_state entirely.
func NoState() Opt { return func(r *tesseract.Revision) { r.ConsumerState = nil } }

// Created sets created_at.
func Created(ts string) Opt { return func(r *tesseract.Revision) { r.CreatedAt = ts } }

// Relationships sets data.relationships.
func Relationships(rels ...map[string]any) Opt {
	return Data(map[string]any{"relationships": rels})
}

// Rel builds one relationship entry.
func Rel(typ, target string) map[string]any { return map[string]any{"type": typ, "target": target} }

// Record builds a class record in its class namespace, with conforming
// defaults: the atlas, class and visibility:portfolio tags, a name, an empty
// relationships list and review_status draft. Options then override.
func Record(class, key string, opts ...Opt) tesseract.Revision {
	c, ok := contract.ClassByKey(class)
	if !ok {
		panic("atlastest: unknown class " + class)
	}
	rev := base(c.Namespace(), key)
	rev.Tags = []string{"atlas", c.Tag(), "visibility:portfolio"}
	Data(map[string]any{"id": key, "name": "Name of " + key, "relationships": []any{}})(&rev)
	State(map[string]any{"review_status": "draft"})(&rev)
	for _, o := range opts {
		o(&rev)
	}
	return rev
}

// Doc builds a document in namespace (a document namespace under the Atlas
// root, or the root itself).
func Doc(namespace, key string, opts ...Opt) tesseract.Revision {
	rev := base(namespace, key)
	rev.Tags = []string{"atlas"}
	rev.Facets.Kind = "doc"
	for _, o := range opts {
		o(&rev)
	}
	return rev
}

func base(namespace, key string) tesseract.Revision {
	var rev tesseract.Revision
	rev.RevisionID = "rev-" + namespace + "/" + key
	rev.ItemID = "item-" + namespace + "/" + key
	rev.Domain = "knowledge"
	rev.Namespace = namespace
	rev.MemoryKey = key
	rev.Status = "canonical"
	rev.CreatedAt = "2026-09-01T00:00:00Z"
	rev.Confidence = 0.9
	rev.Payload.Summary = "Summary of " + key
	rev.Payload.Body = "Body of " + key
	rev.Author.AgentID = "test"
	rev.SessionID = "session-test"
	rev.Facets.Kind = "note"
	rev.Facets.Source = "manual"
	return rev
}
