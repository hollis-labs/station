package llmgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hollis-labs/libs/llm/core/costcalc"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

// Outcomes recorded in the ledger.
const (
	OutcomeReserved    = "reserved" // admitted durably; outcome not yet reconciled
	OutcomeOK          = "ok"
	OutcomeUpstreamErr = "upstream_error" // the upstream answered with an error status; relayed as is
	OutcomeFailedOver  = "failed_over"    // this attempt failed and the next target was tried
	OutcomeInterrupted = "interrupted"    // the stream broke after bytes were sent
	OutcomeCanceled    = "canceled"       // the client went away
)

// Record is one call's outcome as the relay saw it.
type Record struct {
	Caller    Caller
	RequestID string
	Alias     string
	Operation Operation
	Dialect   string
	Target    *target
	Usage     *Usage
	Price     *usageledger.PriceSnapshot
	// Detail is the call's small structured result worth keeping: for a
	// decision, the answers (labels, scores, probabilities). It is the
	// upstream's own output, never a prompt.
	Detail   json.RawMessage
	Status   int
	Outcome  string
	Streamed bool
	Latency  time.Duration
}

// Entry is one ledger record plus its computed cost.
type Entry struct {
	// ID is unique per entry (not per request: a failed-over request has one
	// entry per attempt). Durable stores use it to make a retried write a no-op.
	ID             string          `json:"id"`
	Row            usageledger.Row `json:"row"`
	Alias          string          `json:"alias"`
	Operation      Operation       `json:"operation"`
	Dialect        string          `json:"dialect"`
	RequestID      string          `json:"request_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Status         int             `json:"status"`
	Outcome        string          `json:"outcome"`
	Streamed       bool            `json:"streamed"`
	LatencyMS      int64           `json:"latency_ms"`
	// Verified is false when the caller was not authenticated (auth off).
	Verified bool `json:"verified"`
	// CostUSD is nil when the target has no configured price or the upstream
	// reported no usage; nil is not free.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// CostPartial marks a cost that is a lower bound (some tokens unpriced).
	CostPartial bool `json:"cost_partial,omitempty"`
	// Detail is Record.Detail.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// Total aggregates successful calls for one provider and model.
type Total struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Calls    int     `json:"calls"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	CostUSD  float64 `json:"cost_usd"`
	// UnpricedCalls counts calls with missing or partial costs, so CostUSD reads as a lower bound.
	UnpricedCalls int `json:"unpriced_calls"`
}

// Query selects ledger entries. Zero fields select everything.
type Query struct {
	ID string // internal exact-entry lookup; not exposed by the HTTP query parser
	// Limit is the read cap; -1 requests the complete retained history for internal budget seeding.
	// The HTTP usage endpoint accepts only bounded positive limits.
	Limit     int
	Principal string
	Since     time.Time
}

// Store is where ledger entries live. Add must be idempotent on Entry.ID.
type Store interface {
	Add(ctx context.Context, entries ...Entry) error
	// Recent returns matching entries, newest last.
	Recent(ctx context.Context, q Query) ([]Entry, error)
	// Totals aggregates the successful calls matching q.
	Totals(ctx context.Context, q Query) ([]Total, error)
	Close()
}

// Ledger is the gateway's usage record: it builds each entry (identity, usage,
// price, cost), logs one line, and hands the entry to a Store.
type Ledger struct {
	logger *slog.Logger
	store  Store
	sink   sinkStats
}

// sinkStats is implemented by stores that buffer; /v1/usage surfaces it so a
// backlog or a drop is visible rather than silent.
type sinkStats interface {
	Stats() SinkStats
}

// SinkStats describes a buffering store.
type SinkStats struct {
	Queued  int    `json:"queued"`
	Dropped uint64 `json:"dropped"`
	Failed  uint64 `json:"write_failures"`
}

// NewLedger returns a ledger over store.
func NewLedger(store Store, logger *slog.Logger) *Ledger {
	if logger == nil {
		logger = slog.Default()
	}
	l := &Ledger{logger: logger, store: store}
	if s, ok := store.(sinkStats); ok {
		l.sink = s
	}
	return l
}

func newEntryID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Add records one outcome and returns the constructed entry.
func (l *Ledger) Add(rec Record) Entry {
	price := rec.Price
	if price == nil && rec.Target != nil {
		price = rec.Target.price
	}
	row := usageledger.Row{
		SessionID:  rec.Caller.ID,
		MessageID:  rec.RequestID,
		Provider:   rec.Target.provider.id,
		Model:      rec.Target.model,
		Usage:      toLedgerUsage(rec.Usage),
		Price:      price,
		RecordedAt: time.Now().UTC(),
	}
	if row.SessionID == "" {
		row.SessionID = "anonymous"
	}
	e := Entry{ID: newEntryID(), Alias: rec.Alias, Operation: rec.Operation, Dialect: rec.Dialect, RequestID: rec.RequestID, ConversationID: rec.Caller.ConversationID,
		Status: rec.Status, Outcome: rec.Outcome, Streamed: rec.Streamed, LatencyMS: rec.Latency.Milliseconds(),
		Verified: rec.Caller.Known, Detail: rec.Detail}
	if price != nil && rec.Usage != nil {
		row.CostKind = usageledger.CostKindAPIEstimated
		c := costcalc.PriceRow(row)
		total := c.Total()
		e.CostUSD, e.CostPartial = &total, c.Partial()
	}
	e.Row = row
	if rec.Target.reservation != nil {
		// Keep the attempt in its admission window, including across a restart.
		e.ID = rec.Target.reservation.ID
		e.Row.RecordedAt = rec.Target.reservation.Row.RecordedAt
		e.Row.SessionID = rec.Target.reservation.Row.SessionID
	}
	l.log(e)
	// The caller's request has been answered; the write gets its own bounded
	// context rather than the request's, which may already be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if rec.Target.reservation != nil {
		journal, ok := l.store.(reservationStore)
		if !ok {
			err = errReservationStore
		} else {
			err = journal.Settle(ctx, e)
		}
	} else {
		err = l.store.Add(ctx, e)
	}
	if err != nil {
		l.logger.Error("ledger.write_failed", "request_id", e.RequestID, "entry_id", e.ID, "err", err)
	}
	return e
}

// log writes the one structured line every call gets. It carries identity,
// routing, outcome, size and cost, and never a prompt, a completion, a header
// or a credential.
func (l *Ledger) log(e Entry) {
	u := e.Row.Usage
	attrs := []any{
		"entry_id", e.ID, "request_id", e.RequestID, "principal", e.Row.SessionID, "verified", e.Verified,
		"conversation_id", e.ConversationID,
		"operation", string(e.Operation), "dialect", e.Dialect, "alias", e.Alias,
		"provider", e.Row.Provider, "model", e.Row.Model, "status", e.Status, "outcome", e.Outcome,
		"streamed", e.Streamed, "latency_ms", e.LatencyMS,
		"input_tokens", u.UncachedInputTokens.Tokens, "cache_read_tokens", u.CacheReadTokens.Tokens,
		"cache_write_tokens", u.CacheWriteTokens.Tokens, "output_tokens", u.OutputTokens.Tokens,
		"usage_provenance", string(u.TotalProvenance()),
	}
	if e.CostUSD != nil {
		attrs = append(attrs, "cost_usd", *e.CostUSD, "cost_partial", e.CostPartial)
	}
	level := slog.LevelInfo
	if e.Outcome != OutcomeOK {
		level = slog.LevelWarn
	}
	l.logger.Log(context.Background(), level, "llm.call", attrs...)
}

// Snapshot returns matching entries and totals, plus buffer stats when the
// store buffers.
func (l *Ledger) Snapshot(ctx context.Context, q Query) ([]Entry, []Total, *SinkStats, error) {
	entries, err := l.store.Recent(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	totals, err := l.store.Totals(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	var stats *SinkStats
	if l.sink != nil {
		s := l.sink.Stats()
		stats = &s
	}
	return entries, totals, stats, nil
}

// Close flushes and releases the store.
func (l *Ledger) Close() { l.store.Close() }

// toLedgerUsage maps reported usage onto the disjoint ledger usage. A
// dimension the upstream did not report stays unknown, never measured zero.
func toLedgerUsage(u *Usage) usageledger.Usage {
	out := usageledger.NewUsage()
	if u == nil {
		return out
	}
	measured := func(n int64) usageledger.Component {
		return usageledger.Component{Tokens: n, Provenance: usageledger.ProvenanceMeasured}
	}
	out.UncachedInputTokens = measured(u.Input)
	out.OutputTokens = measured(u.Output)
	if u.CacheRead != nil {
		out.CacheReadTokens = measured(*u.CacheRead)
	}
	if u.CacheWrite != nil {
		out.CacheWriteTokens = measured(*u.CacheWrite)
	}
	return out
}
