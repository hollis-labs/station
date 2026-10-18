package llmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// sqliteStore shares the versioned gateway database format with management
// state. Entry JSON preserves usage provenance and price snapshots; indexed
// columns support principal/time filtering and totals without scanning JSON.
type sqliteStore struct{ state *ManagementStore }

func openSQLiteStore(ctx context.Context, path string) (*sqliteStore, error) {
	s, err := OpenManagementStore(ctx, path)
	if err != nil {
		return nil, err
	}
	return &sqliteStore{state: s}, nil
}

func (s *sqliteStore) Add(ctx context.Context, entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.state.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range entries {
		if e.ID == "" || e.Row.RecordedAt.IsZero() {
			return errors.New("ledger entry requires ID and timestamp")
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		u := e.Row.Usage
		_, err = tx.ExecContext(ctx, `INSERT INTO gateway_usage(id,recorded_at,principal,provider,model,outcome,input_tokens,output_tokens,cost_usd,entry)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, e.ID, e.Row.RecordedAt.UnixNano(), e.Row.SessionID, e.Row.Provider, e.Row.Model, e.Outcome,
			u.UncachedInputTokens.Tokens+u.CacheReadTokens.Tokens+u.CacheWriteTokens.Tokens, u.OutputTokens.Tokens, e.CostUSD, string(raw))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sqliteWhere(q Query, extra string) (string, []any) {
	var clauses []string
	var args []any
	if extra != "" {
		clauses = append(clauses, extra)
	}
	if q.Principal != "" {
		clauses = append(clauses, "principal=?")
		args = append(args, q.Principal)
	}
	if !q.Since.IsZero() {
		clauses = append(clauses, "recorded_at>=?")
		args = append(args, q.Since.UnixNano())
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (s *sqliteStore) Recent(ctx context.Context, q Query) ([]Entry, error) {
	w, args := sqliteWhere(q, "")
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	args = append(args, limit)
	rows, err := s.state.db.QueryContext(ctx, "SELECT entry FROM (SELECT entry,recorded_at,id FROM gateway_usage"+w+" ORDER BY recorded_at DESC,id DESC LIMIT ?) ORDER BY recorded_at,id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var raw string
		var e Entry
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Totals(ctx context.Context, q Query) ([]Total, error) {
	w, args := sqliteWhere(q, "outcome='ok'")
	rows, err := s.state.db.QueryContext(ctx, `SELECT provider,model,count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(cost_usd),0),sum(cost_usd IS NULL) FROM gateway_usage`+w+` GROUP BY provider,model ORDER BY provider,model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Total{}
	for rows.Next() {
		var total Total
		if err := rows.Scan(&total.Provider, &total.Model, &total.Calls, &total.Input, &total.Output, &total.CostUSD, &total.UnpricedCalls); err != nil {
			return nil, err
		}
		out = append(out, total)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Close() { _ = s.state.Close() }
