package usagemonitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hollis-labs/libs/core/util/sqlite/sqlitekit"
	_ "modernc.org/sqlite"
)

var (
	ErrConflict = errors.New("revision or idempotency conflict")
	ErrNotFound = errors.New("account not found")
	ErrDisabled = errors.New("account disabled")
)

type Store struct{ db *sql.DB }

func OpenStore(ctx context.Context, path string) (*Store, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("usage database path must be absolute and clean")
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("usage database directory must be owner-only")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		var st os.FileInfo
		st, err = os.Lstat(path)
		if err == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
			err = errors.New("usage database must be a private regular file")
		}
	}
	if err != nil {
		return nil, err
	}
	opts := sqlitekit.WriterOptions()
	opts.Synchronous = "FULL"
	db, err := sqlitekit.OpenWriter(ctx, path, sqlitekit.OpenOptions{Options: opts})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var app, version int
	if err = tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	const identity = 0x5354554d
	if app != 0 && app != identity || version < 0 || version > 1 {
		return errors.New("foreign or unsupported usage database")
	}
	if version == 1 {
		if app != identity {
			return errors.New("missing usage database identity")
		}
		return tx.Commit()
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("refusing nonempty unversioned usage database")
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE accounts(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, document TEXT NOT NULL);
CREATE TABLE account_audit(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, actor TEXT NOT NULL, changed_at TEXT NOT NULL, document TEXT NOT NULL);
CREATE TABLE observations(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, status TEXT NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX observations_latest ON observations(account_id,observed_at DESC,sequence DESC);
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, model TEXT NOT NULL, cost_kind TEXT NOT NULL, cost_usd REAL, cost_partial INTEGER NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX events_account_time ON events(account_id,observed_at,sequence);
PRAGMA application_id=1398035789;
PRAGMA user_version=1;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func accountFrom(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Account, error) {
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
	return accountFrom(ctx, s.db, id)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	old, err := accountFrom(ctx, tx, a.ID)
	if errors.Is(err, ErrNotFound) {
		if expected != 0 {
			return a, ErrConflict
		}
	} else if err != nil {
		return a, err
	} else {
		if expected != old.Revision || a.Provider != old.Provider || a.Kind != old.Kind {
			return a, ErrConflict
		}
	}
	a.Revision = expected + 1
	raw, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO accounts(id,revision,document) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,document=excluded.document", a.ID, a.Revision, string(raw))
	if err != nil {
		return a, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO account_audit(account_id,actor,changed_at,document) VALUES(?,?,?,?)", a.ID, actor, time.Now().UTC().Format(time.RFC3339Nano), string(raw))
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

// Ingest acknowledges only a committed batch. Retrying the same source/ID and
// data is a no-op; reusing an ID with different data fails the whole batch.
func (s *Store) Ingest(ctx context.Context, account, source, actor string, b Batch) error {
	if !identifier.MatchString(source) || actor == "" {
		return errors.New("invalid source or actor")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	return tx.Commit()
}

func insertRecord(ctx context.Context, tx *sql.Tx, table, account, source, id, raw string, insert func() error) error {
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
	rows, err := s.db.QueryContext(ctx, "SELECT sequence,account_id,source,actor,collected_at,document FROM "+table+w+" ORDER BY sequence LIMIT ?", args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
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
	rows, err := s.db.QueryContext(ctx, "SELECT document FROM accounts ORDER BY id")
	if err != nil {
		return nil, err
	}
	views := []AccountView{}
	for rows.Next() {
		var raw string
		var a AccountView
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &a.Account); err != nil {
			rows.Close()
			return nil, err
		}
		views = append(views, a)
	}
	err = rows.Err()
	rows.Close()
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
	err := s.db.QueryRowContext(ctx, "SELECT sequence,account_id,source,actor,collected_at,document FROM observations WHERE account_id=?"+w+" ORDER BY observed_at DESC,sequence DESC LIMIT 1", args...).Scan(&r.Sequence, &r.AccountID, &r.Source, &r.Actor, &at, &raw)
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
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,source,model,cost_kind,count(*),sum(cost_usd),sum(cost_usd IS NULL),sum(cost_partial),
sum((SELECT coalesce(sum(json_extract(c.value,'$.tokens')),0) FROM json_each(document,'$.row.usage') c WHERE c.key!='dims') + (SELECT coalesce(sum(json_extract(d.value,'$.tokens')),0) FROM json_each(document,'$.row.usage.dims') d)),
sum((SELECT count(*) FROM json_each(document,'$.row.usage') c WHERE c.key!='dims' AND json_extract(c.value,'$.provenance')!='measured') + (SELECT count(*) FROM json_each(document,'$.row.usage.dims') d WHERE json_extract(d.value,'$.provenance')!='measured'))
FROM events`+w+" GROUP BY account_id,source,model,cost_kind ORDER BY account_id,source,model,cost_kind", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
