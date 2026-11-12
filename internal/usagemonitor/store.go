package usagemonitor

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

var (
	ErrConflict = errors.New("revision or idempotency conflict")
	ErrNotFound = errors.New("account not found")
	ErrDisabled = errors.New("account disabled")
)

// Migrations is the usage monitor's schema, one goose migration for the database module.
//
//go:embed migrations
var Migrations embed.FS

// Store is the usage monitor's data on the service database (the database module's SQLite, which has
// one write connection). Reads go through Reader(); every write is a WriteTx whose callback only reads,
// decides and writes, so it can be restarted after contention.
type Store struct{ st *store.Store }

func NewStore(st *store.Store) *Store { return &Store{st: st} }

func accountFrom(ctx context.Context, q store.Querier, id string) (Account, error) {
	var a Account
	var raw string
	err := q.QueryRowContext(ctx, "SELECT document FROM accounts WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(raw), &a)
	return a, err
}
func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	return accountFrom(ctx, s.st.Reader(), id)
}

// PutAccount uses compare-and-swap and retains an audit trail. Identity is
// immutable: repointing an account would relabel its already recorded history.
func (s *Store) PutAccount(ctx context.Context, a Account, expected int64, actor string) (Account, error) {
	if err := a.validate(); err != nil {
		return a, err
	}
	if expected < 0 || actor == "" {
		return a, ErrConflict
	}
	next := a
	next.Revision = expected + 1
	raw, err := json.Marshal(next)
	if err != nil {
		return a, err
	}
	err = s.st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		old, err := accountFrom(ctx, tx, a.ID)
		if errors.Is(err, ErrNotFound) {
			if expected != 0 {
				return ErrConflict
			}
		} else if err != nil {
			return err
		} else if expected != old.Revision || a.Provider != old.Provider || a.Kind != old.Kind {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO accounts(id,revision,document) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,document=excluded.document", next.ID, next.Revision, string(raw)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO account_audit(account_id,actor,changed_at,document) VALUES(?,?,?,?)", next.ID, actor, time.Now().UTC().Format(time.RFC3339Nano), string(raw))
		return err
	})
	if err != nil {
		return a, err
	}
	return next, nil
}

// Ingest acknowledges only a committed batch. Retrying the same source/ID and
// data is a no-op; reusing an ID with different data fails the whole batch.
func (s *Store) Ingest(ctx context.Context, account, source, actor string, b Batch) error {
	if !identifier.MatchString(source) || actor == "" {
		return errors.New("invalid source or actor")
	}
	return s.st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		a, err := accountFrom(ctx, tx, account)
		if err != nil {
			return err
		}
		if !a.Enabled {
			return ErrDisabled
		}
		if err = b.validate(a); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, o := range b.Observations {
			o.ObservedAt = o.ObservedAt.UTC()
			for i := range o.Windows {
				if t := o.Windows[i].ResetsAt; t != nil {
					v := t.UTC()
					o.Windows[i].ResetsAt = &v
				}
			}
			raw, _ := json.Marshal(o)
			if err = insertRecord(ctx, tx, "observations", account, source, o.ID, string(raw), func() error {
				_, e := tx.ExecContext(ctx, "INSERT INTO observations(account_id,source,record_id,actor,collected_at,observed_at,status,document) VALUES(?,?,?,?,?,?,?,?)", account, source, o.ID, actor, now, o.ObservedAt.UnixMilli(), o.Status, string(raw))
				return e
			}); err != nil {
				return err
			}
		}
		for _, e := range b.Events {
			e.Row.RecordedAt = e.Row.RecordedAt.UTC()
			raw, marshalErr := json.Marshal(e)
			if marshalErr != nil {
				return marshalErr
			}
			if err = insertRecord(ctx, tx, "events", account, source, e.ID, string(raw), func() error {
				_, x := tx.ExecContext(ctx, "INSERT INTO events(account_id,source,record_id,actor,collected_at,observed_at,model,cost_kind,cost_usd,cost_partial,document) VALUES(?,?,?,?,?,?,?,?,?,?,?)", account, source, e.ID, actor, now, e.Row.RecordedAt.UnixMilli(), e.Row.Model, e.Row.CostKind, e.CostUSD, e.CostPartial, string(raw))
				return x
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertRecord(ctx context.Context, tx store.Querier, table, account, source, id, raw string, insert func() error) error {
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT document FROM "+table+" WHERE account_id=? AND source=? AND record_id=?", account, source, id).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return insert()
	}
	if err != nil {
		return err
	}
	if existing != raw {
		return ErrConflict
	}
	return nil
}

type Query struct {
	Account string
	Source  string
	After   int64
	Limit   int
	Since   time.Time
	Until   time.Time
}

func (q Query) where() (string, []any) {
	w := " WHERE sequence>?"
	args := []any{q.After}
	if q.Account != "" {
		w += " AND account_id=?"
		args = append(args, q.Account)
	}
	if q.Source != "" {
		w += " AND source=?"
		args = append(args, q.Source)
	}
	if !q.Since.IsZero() {
		w += " AND observed_at>=?"
		args = append(args, q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		w += " AND observed_at<?"
		args = append(args, q.Until.UnixMilli())
	}
	return w, args
}

func readPage[T any](ctx context.Context, s *Store, table string, q Query) (Page[Record[T]], error) {
	page := Page[Record[T]]{Items: []Record[T]{}, NextAfter: q.After}
	if q.Limit < 1 || q.Limit > 500 || q.After < 0 {
		return page, errors.New("invalid page bounds")
	}
	w, args := q.where()
	args = append(args, q.Limit+1)
	rows, err := s.st.Reader().QueryContext(ctx, "SELECT sequence,account_id,source,actor,collected_at,document FROM "+table+w+" ORDER BY sequence LIMIT ?", args...)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r Record[T]
		var at, raw string
		if err = rows.Scan(&r.Sequence, &r.AccountID, &r.Source, &r.Actor, &at, &raw); err != nil {
			return page, err
		}
		if len(page.Items) == q.Limit {
			page.HasMore = true
			break
		}
		if r.CollectedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return page, err
		}
		if err = json.Unmarshal([]byte(raw), &r.Data); err != nil {
			return page, err
		}
		page.Items = append(page.Items, r)
		page.NextAfter = r.Sequence
	}
	return page, rows.Err()
}
func (s *Store) Observations(ctx context.Context, q Query) (Page[Record[Observation]], error) {
	return readPage[Observation](ctx, s, "observations", q)
}
func (s *Store) Events(ctx context.Context, q Query) (Page[Record[Event]], error) {
	return readPage[Event](ctx, s, "events", q)
}

func (s *Store) Accounts(ctx context.Context, staleAfter time.Duration) ([]AccountView, error) {
	rows, err := s.st.Reader().QueryContext(ctx, "SELECT document FROM accounts ORDER BY id")
	if err != nil {
		return nil, err
	}
	views := []AccountView{}
	for rows.Next() {
		var raw string
		var a AccountView
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &a.Account); err != nil {
			_ = rows.Close()
			return nil, err
		}
		views = append(views, a)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range views {
		v := &views[i]
		v.LastSuccess, err = s.latest(ctx, v.ID, "", true)
		if err != nil {
			return nil, err
		}
		// Quota freshness follows the source of this measurement. An unrelated
		// gateway import must not mark a fresh provider quota reading stale.
		source := ""
		if v.LastSuccess != nil {
			source = v.LastSuccess.Source
		}
		v.Latest, err = s.latest(ctx, v.ID, source, false)
		if err != nil {
			return nil, err
		}
		v.Stale = v.LastSuccess == nil || time.Since(v.LastSuccess.Data.ObservedAt) > staleAfter || (v.Latest != nil && v.Latest.Data.Status != "ok")
	}
	return views, nil
}

func (s *Store) latest(ctx context.Context, id, source string, success bool) (*Record[Observation], error) {
	w := ""
	args := []any{id}
	if source != "" {
		w += " AND source=?"
		args = append(args, source)
	}
	if success {
		w += " AND status='ok'"
	}
	var r Record[Observation]
	var at, raw string
	err := s.st.Reader().QueryRowContext(ctx, "SELECT sequence,account_id,source,actor,collected_at,document FROM observations WHERE account_id=?"+w+" ORDER BY observed_at DESC,sequence DESC LIMIT 1", args...).Scan(&r.Sequence, &r.AccountID, &r.Source, &r.Actor, &at, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.CollectedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(raw), &r.Data)
	return &r, err
}

type Total struct {
	AccountID                string   `json:"account_id"`
	Source                   string   `json:"source"`
	Model                    string   `json:"model"`
	CostKind                 string   `json:"cost_kind"`
	Events                   int64    `json:"events"`
	KnownCostUSD             *float64 `json:"known_cost_usd"`
	UnpricedEvents           int64    `json:"unpriced_events"`
	PartialEvents            int64    `json:"partial_events"`
	KnownTokens              int64    `json:"known_tokens"`
	UncertainTokenComponents int64    `json:"uncertain_token_components"`
}

func (s *Store) Totals(ctx context.Context, q Query) ([]Total, error) {
	if q.After != 0 {
		return nil, fmt.Errorf("summary does not accept a cursor")
	}
	w, args := q.where()
	// Usage components are disjoint in usageledger. Include extension dimensions
	// and report uncertain components instead of describing a lower bound as exact.
	rows, err := s.st.Reader().QueryContext(ctx, `SELECT account_id,source,model,cost_kind,count(*),sum(cost_usd),sum(cost_usd IS NULL),sum(cost_partial),
sum((SELECT coalesce(sum(json_extract(c.value,'$.tokens')),0) FROM json_each(document,'$.row.usage') c WHERE c.key!='dims') + (SELECT coalesce(sum(json_extract(d.value,'$.tokens')),0) FROM json_each(document,'$.row.usage.dims') d)),
sum((SELECT count(*) FROM json_each(document,'$.row.usage') c WHERE c.key!='dims' AND json_extract(c.value,'$.provenance')!='measured') + (SELECT count(*) FROM json_each(document,'$.row.usage.dims') d WHERE json_extract(d.value,'$.provenance')!='measured'))
FROM events`+w+" GROUP BY account_id,source,model,cost_kind ORDER BY account_id,source,model,cost_kind", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Total{}
	for rows.Next() {
		var t Total
		if err = rows.Scan(&t.AccountID, &t.Source, &t.Model, &t.CostKind, &t.Events, &t.KnownCostUSD, &t.UnpricedEvents, &t.PartialEvents, &t.KnownTokens, &t.UncertainTokenComponents); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
