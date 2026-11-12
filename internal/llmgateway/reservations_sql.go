package llmgateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/libs/core/store"
	"github.com/jackc/pgx/v5"
)

// One lock serializes admission and settlement across gateway processes using
// this ledger. READ COMMITTED takes the history snapshot AFTER acquiring it.
// It is independent of the schema migration lock and held only for local SQL.
const budgetAdmissionLock int64 = 7301161

func (s *pgStore) Reserve(ctx context.Context, e *Entry, since time.Time, check func([]Entry) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackReservation(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", budgetAdmissionLock); err != nil {
		return err
	}
	existing, err := recentPG(ctx, tx, Query{ID: e.ID, Limit: 1})
	if err != nil {
		return err
	}
	if len(existing) != 0 {
		return errSettlementConflict
	}
	entries, err := recentPG(ctx, tx, Query{Since: since, Limit: -1})
	if err != nil {
		return err
	}
	if err = check(entries); err != nil {
		return err
	}
	if err = addPG(ctx, tx, *e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *pgStore) Settle(ctx context.Context, e Entry) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackReservation(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", budgetAdmissionLock); err != nil {
		return err
	}
	entries, err := recentPG(ctx, tx, Query{ID: e.ID, Limit: 1})
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return errSettlementConflict
	}
	done, err := validateSettlement(entries[0], e)
	if err != nil || done {
		return err
	}
	usage, err := json.Marshal(e.Row.Usage)
	if err != nil {
		return err
	}
	var detail any
	if len(e.Detail) > 0 {
		detail = []byte(e.Detail)
	}
	_, err = tx.Exec(ctx, `UPDATE llm_calls SET status=$2,outcome=$3,streamed=$4,latency_ms=$5,
 usage=$6,cost_kind=$7,cost_usd=$8,cost_partial=$9,detail=$10 WHERE id=$1`,
		e.ID, e.Status, e.Outcome, e.Streamed, e.LatencyMS, usage, string(e.Row.CostKind), e.CostUSD, e.CostPartial, detail)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Reserve and Settle run in one write transaction, which the store restarts from scratch on
// contention, so the callbacks below only read the transaction and decide.
func (s *sqliteStore) Reserve(ctx context.Context, e *Entry, since time.Time, check func([]Entry) error) error {
	return s.st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		existing, err := recentSQLite(ctx, tx, Query{ID: e.ID, Limit: 1})
		if err != nil {
			return err
		}
		if len(existing) != 0 {
			return errSettlementConflict
		}
		entries, err := recentSQLite(ctx, tx, Query{Since: since, Limit: -1})
		if err != nil {
			return err
		}
		if err = check(entries); err != nil {
			return err
		}
		return addSQLite(ctx, tx, *e)
	})
}

func (s *sqliteStore) Settle(ctx context.Context, e Entry) error {
	return s.st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		entries, err := recentSQLite(ctx, tx, Query{ID: e.ID, Limit: 1})
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return errSettlementConflict
		}
		done, err := validateSettlement(entries[0], e)
		if err != nil || done {
			return err
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		u := e.Row.Usage
		_, err = tx.ExecContext(ctx, `UPDATE gateway_usage SET outcome=?,input_tokens=?,output_tokens=?,cost_usd=?,entry=? WHERE id=?`,
			e.Outcome, u.UncachedInputTokens.Tokens+u.CacheReadTokens.Tokens+u.CacheWriteTokens.Tokens, u.OutputTokens.Tokens, e.CostUSD, string(raw), e.ID)
		return err
	})
}

func rollbackReservation(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
