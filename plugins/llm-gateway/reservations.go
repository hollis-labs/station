package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"time"

	"github.com/hollis-labs/libs/llm/core/quota"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

var (
	errReservationStore   = errors.New("ledger does not support atomic reservations")
	errBudgetRefused      = errors.New("budget admission refused")
	errSettlementConflict = errors.New("reservation settlement conflict")
)

// reservationStore serializes read/check/insert across every writer to a store.
// A reservation is a usage row with outcome=reserved and unknown cost. Settlement
// replaces that row atomically, preserving its identity, rates and admission time.
// The callback uses quota for all window and limit math; storage owns locking.
type reservationStore interface {
	Reserve(context.Context, *Entry, time.Time, func([]Entry) error) error
	Settle(context.Context, Entry) error
}

// snapshotStore is an immutable view read under the store's admission lock.
// It deliberately does not implement reservationStore: quota must evaluate this
// exact transaction snapshot rather than open another transaction or cache it.
type snapshotStore struct{ entries []Entry }

func (s snapshotStore) Recent(_ context.Context, q Query) ([]Entry, error) {
	var out []Entry
	for _, e := range s.entries {
		if matches(e, q) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (snapshotStore) Add(context.Context, ...Entry) error {
	return errors.New("read-only budget snapshot")
}
func (snapshotStore) Totals(context.Context, Query) ([]Total, error) {
	return nil, errors.New("read-only budget snapshot")
}
func (snapshotStore) Close() {}

func (b *Budgets) checkEntries(ctx context.Context, caller Caller, model string, entries []Entry) (BudgetStatus, error) {
	view, err := NewBudgets(b.cfgs, snapshotStore{entries}, b.clock)
	if err != nil {
		return BudgetStatus{}, err
	}
	return view.Check(ctx, caller, model)
}

// CheckStored is a fresh preflight view. Reserve repeats it under the durable
// lock, so a successful preflight is never authority to dispatch on its own.
func (b *Budgets) CheckStored(ctx context.Context, caller Caller, model string) (BudgetStatus, error) {
	if b == nil || len(b.items) == 0 {
		return BudgetStatus{State: "ok"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if b.store == nil {
		return BudgetStatus{}, errReservationStore
	}
	entries, err := b.store.Recent(ctx, Query{Since: b.clock.Now().Add(-b.horizon), Limit: -1})
	if err != nil {
		return BudgetStatus{}, err
	}
	return b.checkEntries(ctx, caller, model, entries)
}

// Reserve records an admitted upstream attempt before dispatch. Request budgets
// consume one slot; spend budgets see unresolved cost until settlement, so a
// second matching spend attempt fails closed rather than spending concurrently.
// This does not bound the first attempt's dollar cost. A priced, bounded estimate
// is required before this can promise a hard dollar ceiling.
func (b *Budgets) Reserve(ctx context.Context, caller Caller, requestID, alias string, op Operation, dialect string, target *target, stream bool) (*Entry, BudgetStatus, error) {
	status := BudgetStatus{State: "ok"}
	if b == nil || len(b.items) == 0 {
		return nil, status, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	journal, ok := b.store.(reservationStore)
	if !ok {
		return nil, status, errReservationStore
	}
	principal := caller.ID
	if !caller.Known || principal == "" {
		principal = "anonymous"
	}
	e := Entry{ID: newEntryID(), RequestID: requestID, ConversationID: caller.ConversationID, Alias: alias, Operation: op, Dialect: dialect, Outcome: OutcomeReserved, Verified: caller.Known, Streamed: stream,
		Row: usageledger.Row{SessionID: principal, MessageID: requestID, Provider: target.provider.id, Model: target.model, Price: target.price}}
	// The store stamps admission time after acquiring its lock. Otherwise time
	// spent waiting for a contended lock could charge the previous window.
	e.Row.RecordedAt = b.clock.Now().UTC()
	err := journal.Reserve(ctx, &e, e.Row.RecordedAt.Add(-b.horizon), func(entries []Entry) error {
		var err error
		now := b.clock.Now().UTC().Truncate(time.Microsecond)
		e.Row.RecordedAt = now
		view := Budgets{cfgs: b.cfgs, clock: quota.ClockFunc(func() time.Time { return now })}
		status, err = view.checkEntries(ctx, caller, alias, entries)
		if err != nil {
			return err
		}
		if status.State != "ok" {
			return errBudgetRefused
		}
		return nil
	})
	if errors.Is(err, errBudgetRefused) {
		return nil, status, nil
	}
	if err != nil {
		return nil, status, err
	}
	return &e, status, nil
}

// validateSettlement makes completion retries idempotent and rejects changing
// the reservation's identity, rates or window. Unknown usage stays unknown.
func validateSettlement(before, after Entry) (already bool, err error) {
	switch after.Outcome {
	case OutcomeOK, OutcomeCanceled, OutcomeInterrupted, OutcomeFailedOver, OutcomeUpstreamErr:
	default:
		return false, errSettlementConflict
	}
	if after.ID == "" {
		return false, errSettlementConflict
	}
	if before.Outcome != OutcomeReserved {
		a, e1 := json.Marshal(before)
		b, e2 := json.Marshal(after)
		if e1 != nil || e2 != nil || !sameReservationJSON(a, b) {
			return false, errSettlementConflict
		}
		return true, nil
	}
	a, b := before, after
	a.Status = b.Status
	a.Outcome = b.Outcome
	a.Streamed = b.Streamed
	a.LatencyMS = b.LatencyMS
	a.Row.Usage = b.Row.Usage
	a.Row.CostKind = b.Row.CostKind
	a.CostUSD = b.CostUSD
	a.CostPartial = b.CostPartial
	a.Detail = b.Detail
	x, e1 := json.Marshal(a)
	y, e2 := json.Marshal(b)
	if e1 != nil || e2 != nil || !sameReservationJSON(x, y) {
		return false, errSettlementConflict
	}
	return false, nil
}

func (a *asyncStore) Reserve(ctx context.Context, e *Entry, since time.Time, check func([]Entry) error) error {
	// These writes must bypass the queue: dispatch requires a durable receipt.
	s, ok := a.inner.(reservationStore)
	if !ok {
		return errReservationStore
	}
	return s.Reserve(ctx, e, since, check)
}
func (a *asyncStore) Settle(ctx context.Context, e Entry) error {
	s, ok := a.inner.(reservationStore)
	if !ok {
		return errReservationStore
	}
	return s.Settle(ctx, e)
}

func (m *memStore) Reserve(ctx context.Context, e *Entry, since time.Time, check func([]Entry) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.seen[e.ID] {
		return errSettlementConflict
	}
	var entries []Entry
	for _, entry := range m.entries {
		if !entry.Row.RecordedAt.Before(since) {
			entries = append(entries, entry)
		}
	}
	if err := check(entries); err != nil {
		return err
	}
	// Never evict an unresolved reservation to admit another attempt.
	if len(m.entries) >= m.max {
		return errors.New("memory budget history capacity exhausted; use a durable ledger")
	}
	m.entries = append(m.entries, *e)
	m.seen[e.ID] = true
	return nil
}
func (m *memStore) Settle(ctx context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, old := range m.entries {
		if old.ID == e.ID {
			done, err := validateSettlement(old, e)
			if err != nil || done {
				return err
			}
			m.entries[i] = e
			return nil
		}
	}
	return errSettlementConflict
}

// PostgreSQL jsonb canonicalizes object order and numeric notation. Compare the
// JSON values without float64 conversion so a retry cannot lose integer precision.
func sameReservationJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return normalizeReservationNumbers(value), err
	}
	left, err := decode(a)
	if err != nil {
		return false
	}
	right, err := decode(b)
	return err == nil && reflect.DeepEqual(left, right)
}

type reservationNumber struct{ exact string }

func normalizeReservationNumbers(v any) any {
	switch value := v.(type) {
	case json.Number:
		if r, ok := new(big.Rat).SetString(string(value)); ok {
			return reservationNumber{r.RatString()}
		}
	case map[string]any:
		for key, element := range value {
			value[key] = normalizeReservationNumbers(element)
		}
	case []any:
		for i, element := range value {
			value[i] = normalizeReservationNumbers(element)
		}
	}
	return v
}
