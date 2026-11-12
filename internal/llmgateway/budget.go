package llmgateway

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hollis-labs/libs/llm/core/quota"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

const (
	unitUSDMicros quota.Unit = "usd_micros"
	unitRequests  quota.Unit = "requests"
	unitUnpriced  quota.Unit = "unpriced_calls"
)

// BudgetStatus is the result of checking every configured budget that applies
// to a call. Spend math and windows live here in the quota module; policy
// receives this summary as facts on llm.invoke.
type BudgetStatus struct {
	// State is "ok", "exhausted", "unknown" (incomplete spend history), or
	// "price_unknown" (this attempt lacks usable base token rates), or
	// "unbounded" (a required cost bound cannot be established).
	State   string
	Unknown []string
	// Exhausted lists the IDs of budgets that refused the call.
	Exhausted []string
	// RetryAfter is the longest wait among exhausted budgets.
	RetryAfter time.Duration
	// RemainingUSD is the lowest remaining USD budget across matching USD
	// limits, or nil when no USD budget applies.
	RemainingUSD *float64
}

type compiledBudget struct {
	cfg       BudgetConfig
	window    quota.Window
	maxMicros int64
	maxReqs   int64
}

func (b compiledBudget) matches(callerID, model string) bool {
	if b.cfg.Principal != "" && !glob(b.cfg.Principal, callerID) {
		return false
	}
	if b.cfg.Model != "" && !glob(b.cfg.Model, model) {
		return false
	}
	return true
}

func (b compiledBudget) key(callerID string) string {
	if b.cfg.Scope == "principal" {
		if callerID == "" {
			callerID = "anonymous"
		}
		return "budget=" + b.cfg.ID + "/principal=" + callerID
	}
	return "budget=" + b.cfg.ID
}

func (b compiledBudget) usdLimit(callerID string) quota.Limit {
	return quota.Limit{
		Key:    b.key(callerID),
		Unit:   unitUSDMicros,
		Window: b.window,
		Max:    b.maxMicros,
	}
}

func (b compiledBudget) reqLimit(callerID string) quota.Limit {
	return quota.Limit{
		Key:    b.key(callerID),
		Unit:   unitRequests,
		Window: b.window,
		Max:    b.maxReqs,
	}
}

// Budgets tracks call counts and USD spend over calendar or rolling windows,
// seeded from the ledger store so restarts do not reset active windows.
type Budgets struct {
	cfgs    []BudgetConfig
	store   Store
	clock   quota.Clock
	horizon time.Duration
	items   []compiledBudget
	seeded  *quota.SeededStore
	limiter *quota.Limiter

	mu      sync.Mutex
	seen    map[string]bool
	failure error
}

func parseBudgetWindow(raw string) (quota.Window, time.Duration, error) {
	switch raw {
	case "", "month":
		return quota.Calendar(quota.Month, time.UTC), 32 * 24 * time.Hour, nil
	case "week":
		return quota.Calendar(quota.Week, time.UTC), 8 * 24 * time.Hour, nil
	case "day":
		return quota.Calendar(quota.Day, time.UTC), 2 * 24 * time.Hour, nil
	default:
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return quota.Window{}, 0, fmt.Errorf("window %q must be day, week, month or a positive duration", raw)
		}
		return quota.Rolling(d), d + time.Hour, nil
	}
}

func usdToMicros(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	m := int64(math.Round(usd * 1e6))
	if m == 0 {
		return 1
	}
	return m
}

// NewBudgets compiles cfg and binds a quota.Limiter backed by a SeededStore
// that reads historical entries from store on first check.
func NewBudgets(cfgs []BudgetConfig, store Store, clock quota.Clock) (*Budgets, error) {
	if err := validateBudgets(cfgs); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = quota.ClockFunc(func() time.Time { return time.Now().UTC() })
	}
	var (
		items   []compiledBudget
		horizon = 24 * time.Hour
	)
	for _, c := range cfgs {
		w, h, err := parseBudgetWindow(c.Window)
		if err != nil {
			return nil, fmt.Errorf("llm-gateway: budget %q: %w", c.ID, err)
		}
		if h > horizon {
			horizon = h
		}
		maxMicros := usdToMicros(c.MaxUSD)
		if c.RequireCostBound {
			maxMicros, _ = scaledMicros(c.MaxUSD, 1000000, false)
		}
		items = append(items, compiledBudget{
			cfg:       c,
			window:    w,
			maxMicros: maxMicros,
			maxReqs:   c.MaxRequests,
		})
	}
	b := &Budgets{
		cfgs: append([]BudgetConfig(nil), cfgs...), store: store, clock: clock, horizon: horizon,
		items: items,
		seen:  map[string]bool{},
	}
	seedFn := func(ctx context.Context, since time.Time) ([]quota.SeedEntry, error) {
		if len(b.items) == 0 {
			return nil, nil
		}
		if store == nil {
			return nil, fmt.Errorf("budget history store unavailable")
		}
		entries, err := store.Recent(ctx, Query{Since: since, Limit: -1})
		if err != nil {
			return nil, err
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		var out []quota.SeedEntry
		sums := map[quota.Counter]int64{}
		seen := map[string]bool{}
		for _, e := range entries {
			if e.ID != "" && (b.seen[e.ID] || seen[e.ID]) {
				continue
			}
			if e.ID != "" {
				seen[e.ID] = true
			}
			for _, seed := range b.entriesFor(e) {
				// quota's counters use int64. Refuse an unrepresentable retained
				// history rather than let accumulated charges wrap and reopen spend.
				if seed.Entry.Amount < 0 || sums[seed.Counter] > math.MaxInt64-seed.Entry.Amount {
					return nil, fmt.Errorf("budget history exceeds counter capacity")
				}
				sums[seed.Counter] += seed.Entry.Amount
				out = append(out, seed)
			}
		}
		for id := range seen {
			b.seen[id] = true
		}
		return out, nil
	}
	b.seeded = quota.NewSeededStore(seedFn, horizon, clock)
	b.limiter = quota.New(b.seeded, quota.WithClock(clock))
	return b, nil
}

func (b *Budgets) entriesFor(e Entry) []quota.SeedEntry {
	callerID := e.Row.SessionID
	if callerID == "" {
		callerID = "anonymous"
	}
	at := e.Row.RecordedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var out []quota.SeedEntry
	for _, item := range b.items {
		if !item.matches(callerID, e.Alias) {
			continue
		}
		if item.maxMicros > 0 && (e.CostUSD == nil || e.CostPartial || !validBudgetUSD(*e.CostUSD)) {
			out = append(out, quota.SeedEntry{Counter: item.unknownLimit(callerID).Counter(), Entry: quota.Entry{At: at, Amount: 1}})
		}
		if item.maxMicros > 0 && e.CostUSD != nil && validBudgetUSD(*e.CostUSD) {
			amount := usdToMicros(*e.CostUSD)
			if item.cfg.RequireCostBound {
				amount, _ = scaledMicros(*e.CostUSD, 1000000, true)
				if e.CostBound != nil && !e.CostPartial {
					// A verified tokenized embedding settles the exact input count;
					// retain its integer charge instead of re-rounding a float total.
					amount = max(amount, e.CostBound.MaxCostMicros)
				}
			}
			out = append(out, quota.SeedEntry{
				Counter: item.usdLimit(callerID).Counter(),
				Entry:   quota.Entry{At: at, Amount: amount},
			})
		}
		if item.maxReqs > 0 {
			out = append(out, quota.SeedEntry{
				Counter: item.reqLimit(callerID).Counter(),
				Entry:   quota.Entry{At: at, Amount: 1},
			})
		}
	}
	return out
}

// Record updates in-memory budget counters immediately when a call finishes so
// subsequent checks see the spend without waiting for async store flushes.
func (b *Budgets) Record(e Entry) error {
	if b == nil || len(b.items) == 0 {
		return nil
	}
	b.mu.Lock()
	if e.ID != "" {
		if b.seen[e.ID] {
			b.mu.Unlock()
			return nil
		}
		b.seen[e.ID] = true
	}
	seeds := b.entriesFor(e)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range seeds {
		if err := b.seeded.Record(ctx, s.Counter, s.Entry); err != nil {
			// A partially applied record cannot safely be retried. Stop admission
			// until restart reconstructs every counter from durable history.
			b.mu.Lock()
			b.failure = err
			b.mu.Unlock()
			return err
		}
	}
	return nil
}

// Check evaluates all budgets matching caller and model, returning the
// combined budget_state facts for policy admission.
func (b *Budgets) Check(ctx context.Context, caller Caller, model string) (BudgetStatus, error) {
	return b.check(ctx, caller, model, nil)
}

func (b *Budgets) check(ctx context.Context, caller Caller, model string, bound *CostBound) (BudgetStatus, error) {
	status := BudgetStatus{State: "ok"}
	if b == nil || len(b.items) == 0 {
		return status, nil
	}
	callerID := caller.ID
	if !caller.Known || callerID == "" {
		callerID = "anonymous"
	}
	for _, item := range b.items {
		if !item.matches(callerID, model) {
			continue
		}
		b.mu.Lock()
		failure := b.failure
		b.mu.Unlock()
		if failure != nil {
			return status, failure
		}
		exhausted := false
		if item.maxMicros > 0 {
			unknown, err := b.limiter.Check(ctx, item.unknownLimit(callerID), 1)
			if err != nil {
				return status, err
			}
			if !unknown.Allowed {
				status.Unknown = append(status.Unknown, item.cfg.ID)
			}
			amount := int64(1)
			if item.cfg.RequireCostBound && bound != nil {
				amount = bound.MaxCostMicros
			}
			dec, err := b.limiter.Check(ctx, item.usdLimit(callerID), amount)
			if err != nil {
				return status, err
			}
			remUSD := float64(dec.Remaining) / 1e6
			if status.RemainingUSD == nil || remUSD < *status.RemainingUSD {
				status.RemainingUSD = &remUSD
			}
			if !dec.Allowed {
				exhausted = true
				if dec.RetryAfter > status.RetryAfter {
					status.RetryAfter = dec.RetryAfter
				}
			}
		}
		if item.maxReqs > 0 {
			dec, err := b.limiter.Check(ctx, item.reqLimit(callerID), 1)
			if err != nil {
				return status, err
			}
			if !dec.Allowed {
				exhausted = true
				if dec.RetryAfter > status.RetryAfter {
					status.RetryAfter = dec.RetryAfter
				}
			}
		}
		if exhausted {
			status.State = "exhausted"
			status.Exhausted = append(status.Exhausted, item.cfg.ID)
		}
	}
	if len(status.Unknown) > 0 && status.State != "exhausted" {
		status.State = "unknown"
	}
	return status, nil
}

// A single unknown/partial cost makes a matching spend window incomplete.
// Keeping these events in quota gives them the same window expiry as spend.
func (b compiledBudget) unknownLimit(callerID string) quota.Limit {
	return quota.Limit{Key: b.key(callerID), Unit: unitUnpriced, Window: b.window, Max: 1}
}

func validBudgetUSD(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(math.MaxInt64)/1e6
}

// checkPrice is a prerequisite, not an estimate or upper bound. Every inference
// operation needs input pricing; generation also needs output pricing. Optional
// cache/reasoning rates are checked by the usage pricer when those tokens occur.
// Neither permissive capability defaults nor a "free" route tag establish rates.
func (b *Budgets) checkPrice(caller Caller, model string, op Operation, price *usageledger.PriceSnapshot) BudgetStatus {
	status := BudgetStatus{State: "ok"}
	if b == nil || usableBasePrice(op, price) {
		return status
	}
	principal := caller.ID
	if !caller.Known || principal == "" {
		principal = "anonymous"
	}
	for _, item := range b.items {
		if item.maxMicros > 0 && item.matches(principal, model) {
			status.State = "price_unknown"
			status.Unknown = append(status.Unknown, item.cfg.ID)
		}
	}
	return status
}

func usableBasePrice(op Operation, price *usageledger.PriceSnapshot) bool {
	if price == nil || price.Validate() != nil || price.UnknownRates.UncachedInput {
		return false
	}
	switch op {
	case OpEmbed:
		return true
	case OpChat, OpResponses, OpDecide:
		return !price.UnknownRates.Output
	default:
		return false
	}
}
