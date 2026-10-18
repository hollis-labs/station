package llmgateway

import (
	"context"
	"sync"
)

// memStore is a bounded in-memory Store: the default, and a test double. It
// loses everything on restart; use the Postgres store for a record that lasts.
type memStore struct {
	mu      sync.Mutex
	max     int
	entries []Entry
	seen    map[string]bool
}

func newMemStore(max int) *memStore {
	if max <= 0 {
		max = 1000
	}
	return &memStore{max: max, seen: map[string]bool{}}
}

func (m *memStore) Add(_ context.Context, entries ...Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if m.seen[e.ID] {
			continue // idempotent, like the durable stores
		}
		m.seen[e.ID] = true
		if len(m.entries) == m.max {
			delete(m.seen, m.entries[0].ID)
			copy(m.entries, m.entries[1:])
			m.entries = m.entries[:m.max-1]
		}
		m.entries = append(m.entries, e)
	}
	return nil
}

func matches(e Entry, q Query) bool {
	if q.ID != "" && e.ID != q.ID {
		return false
	}
	if q.Principal != "" && e.Row.SessionID != q.Principal {
		return false
	}
	return q.Since.IsZero() || !e.Row.RecordedAt.Before(q.Since)
}

func (m *memStore) Recent(_ context.Context, q Query) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for _, e := range m.entries {
		if matches(e, q) {
			out = append(out, e)
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}

func (m *memStore) Totals(_ context.Context, q Query) ([]Total, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := map[string]*Total{}
	var order []string
	for _, e := range m.entries {
		if e.Outcome != OutcomeOK || !matches(e, q) {
			continue
		}
		k := e.Row.Provider + "\x00" + e.Row.Model
		t := by[k]
		if t == nil {
			t = &Total{Provider: e.Row.Provider, Model: e.Row.Model}
			by[k] = t
			order = append(order, k)
		}
		u := e.Row.Usage
		t.Calls++
		t.Input += u.UncachedInputTokens.Tokens + u.CacheReadTokens.Tokens + u.CacheWriteTokens.Tokens
		t.Output += u.OutputTokens.Tokens
		if e.CostUSD != nil {
			t.CostUSD += *e.CostUSD
		}
		if e.CostUSD == nil || e.CostPartial {
			t.UnpricedCalls++
		}
	}
	out := make([]Total, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	return out, nil
}

func (m *memStore) Close() {}
