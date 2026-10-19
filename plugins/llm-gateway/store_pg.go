package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hollis-labs/libs/llm/core/usageledger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore is the durable ledger store. It owns one table, llm_calls, in a
// database the operator dedicates to it (its own database and role on the
// shared cluster, per the ledger-home decision).
//
// Columns hold what queries and budgets need as real columns (who, what,
// outcome, cost, time); the disjoint usage and the price snapshot stay as
// jsonb exactly as the usageledger types serialize them, so a row decodes back
// to the same value. cost_usd is double precision: it is an estimate from
// configured rates, not a bill, and NULL means "not priced", never zero.
type pgStore struct {
	pool *pgxpool.Pool
}

// migrations are applied in order, once, under an advisory lock. Never edit an
// applied migration: append a new one.
var pgMigrations = []string{
	`CREATE TABLE llm_calls (
		id            text PRIMARY KEY,
		recorded_at   timestamptz NOT NULL,
		request_id    text NOT NULL,
		principal     text NOT NULL,
		verified      boolean NOT NULL,
		operation     text NOT NULL,
		dialect       text NOT NULL,
		alias         text NOT NULL,
		provider      text NOT NULL,
		model         text NOT NULL,
		status        integer NOT NULL,
		outcome       text NOT NULL,
		streamed      boolean NOT NULL,
		latency_ms    bigint NOT NULL,
		usage         jsonb NOT NULL,
		price         jsonb,
		cost_kind     text NOT NULL,
		cost_usd      double precision,
		cost_partial  boolean NOT NULL,
		detail        jsonb
	);
	CREATE INDEX llm_calls_recorded_at ON llm_calls (recorded_at);
	CREATE INDEX llm_calls_principal ON llm_calls (principal, recorded_at);
	CREATE INDEX llm_calls_target ON llm_calls (provider, model, recorded_at);
	CREATE INDEX llm_calls_request ON llm_calls (request_id);`,
	`ALTER TABLE llm_calls ADD COLUMN conversation_id text NOT NULL DEFAULT '';`,
	`ALTER TABLE llm_calls ADD COLUMN cost_bound jsonb;`,
}

const migrationLock = 7301160 // arbitrary, stable: serializes concurrent starters

// openPGStore connects, migrates and returns a store.
func openPGStore(ctx context.Context, dsn string) (*pgStore, error) {
	return openPGStoreMode(ctx, dsn, "auto")
}

func connectPGStore(ctx context.Context, dsn string) (*pgStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		// The error text can echo the DSN's host and user; the password is
		// never in pgx's messages, but keep the cause out of the chain anyway.
		return nil, fmt.Errorf("llm-gateway: ledger: invalid postgres dsn")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("llm-gateway: ledger: postgres unreachable: %w", scrub(err))
	}
	return &pgStore{pool: pool}, nil
}

func openPGStoreMode(ctx context.Context, dsn, mode string) (*pgStore, error) {
	if mode != "auto" && mode != "verify" {
		return nil, fmt.Errorf("llm-gateway: ledger: invalid schema mode")
	}
	s, err := connectPGStore(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if mode == "verify" {
		err = s.verifySchema(ctx)
	} else {
		err = s.migrate(ctx)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *pgStore) verifySchema(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("llm-gateway: ledger: begin schema verification failed")
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var count, first, last int
	if err = tx.QueryRow(ctx, `SELECT count(*),coalesce(min(version),0),coalesce(max(version),0) FROM llm_ledger_schema`).Scan(&count, &first, &last); err != nil {
		return fmt.Errorf("llm-gateway: ledger: schema unavailable; run station ledger-migrate with the migration identity")
	}
	if count != len(pgMigrations) || first != 1 || last != len(pgMigrations) {
		return fmt.Errorf("llm-gateway: ledger: schema does not match binary v%d; run station ledger-migrate with the matching binary and migration identity", len(pgMigrations))
	}
	// Resolve the full runtime row shape without reading any application rows.
	rows, err := tx.Query(ctx, `SELECT id,recorded_at,request_id,principal,verified,operation,dialect,alias,provider,model,status,outcome,streamed,latency_ms,usage,price,cost_kind,cost_usd,cost_partial,detail,conversation_id,cost_bound FROM llm_calls LIMIT 0`)
	if err != nil {
		return fmt.Errorf("llm-gateway: ledger: runtime schema is unavailable or incompatible")
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("llm-gateway: ledger: runtime schema verification failed")
	}
	return tx.Commit(ctx)
}

func scrub(err error) error { return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "\n", " ")) }

func (s *pgStore) migrate(ctx context.Context) error {
	return s.migrateAs(ctx, "")
}

func (s *pgStore) migrateAs(ctx context.Context, owner string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("llm-gateway: ledger: begin migration: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if owner != "" {
		var safe bool
		if err := tx.QueryRow(ctx, `SELECT NOT (rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls) FROM pg_roles WHERE rolname=$1`, owner).Scan(&safe); err != nil || !safe {
			return fmt.Errorf("llm-gateway: ledger: owner must be a non-login, non-administrative role")
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{owner}.Sanitize()); err != nil {
			return fmt.Errorf("llm-gateway: ledger: migration identity cannot assume owner role")
		}
	}
	// Runtime lookup puts pg_catalog first. DDL must target the configured
	// application schema rather than attempting to create objects in pg_catalog.
	var schemas []string
	if err := tx.QueryRow(ctx, `SELECT current_schemas(false)`).Scan(&schemas); err != nil {
		return fmt.Errorf("llm-gateway: ledger: migration schema lookup failed")
	}
	var schema string
	for _, candidate := range schemas {
		if candidate != "pg_catalog" && candidate != "information_schema" && !strings.HasPrefix(candidate, "pg_temp_") {
			schema = candidate
			break
		}
	}
	if schema == "" {
		return fmt.Errorf("llm-gateway: ledger: migration requires an existing application schema")
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = "+pgx.Identifier{schema}.Sanitize()+", pg_catalog"); err != nil {
		return fmt.Errorf("llm-gateway: ledger: migration schema selection failed")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("llm-gateway: ledger: migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS llm_ledger_schema (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("llm-gateway: ledger: schema table: %w", err)
	}
	var current int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM llm_ledger_schema`).Scan(&current); err != nil {
		return fmt.Errorf("llm-gateway: ledger: read schema version: %w", err)
	}
	if current > len(pgMigrations) {
		return fmt.Errorf("llm-gateway: ledger: database schema v%d is newer than this binary (v%d)", current, len(pgMigrations))
	}
	for v := current; v < len(pgMigrations); v++ {
		if _, err := tx.Exec(ctx, pgMigrations[v]); err != nil {
			return fmt.Errorf("llm-gateway: ledger: migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO llm_ledger_schema (version) VALUES ($1)`, v+1); err != nil {
			return fmt.Errorf("llm-gateway: ledger: record migration %d: %w", v+1, err)
		}
	}
	return tx.Commit(ctx)
}

const insertEntry = `INSERT INTO llm_calls
	(id, recorded_at, request_id, principal, verified, operation, dialect, alias, provider, model,
	 status, outcome, streamed, latency_ms, usage, price, cost_kind, cost_usd, cost_partial, detail, conversation_id, cost_bound)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
	ON CONFLICT (id) DO NOTHING`

func (s *pgStore) Add(ctx context.Context, entries ...Entry) error {
	return addPG(ctx, s.pool, entries...)
}

type pgBatcher interface {
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

func addPG(ctx context.Context, db pgBatcher, entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range entries {
		usage, err := json.Marshal(e.Row.Usage)
		if err != nil {
			return err
		}
		var price any
		if e.Row.Price != nil {
			b, err := json.Marshal(e.Row.Price)
			if err != nil {
				return err
			}
			price = b
		}
		var detail any
		if len(e.Detail) > 0 {
			detail = []byte(e.Detail)
		}
		var bound any
		if e.CostBound != nil {
			raw, err := json.Marshal(e.CostBound)
			if err != nil {
				return err
			}
			bound = raw
		}
		batch.Queue(insertEntry, e.ID, e.Row.RecordedAt, e.RequestID, e.Row.SessionID, e.Verified, string(e.Operation),
			e.Dialect, e.Alias, e.Row.Provider, e.Row.Model, e.Status, e.Outcome, e.Streamed, e.LatencyMS,
			usage, price, string(e.Row.CostKind), e.CostUSD, e.CostPartial, detail, e.ConversationID, bound)
	}
	res := db.SendBatch(ctx, batch)
	defer res.Close()
	for range entries {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("insert ledger entry: %w", err)
		}
	}
	return nil
}

// where builds the filter clause and its arguments from a Query.
func where(q Query, extra string) (string, []any) {
	var conds []string
	var args []any
	if q.Before != nil {
		args = append(args, q.Before.At, q.Before.ID)
		conds = append(conds, "(recorded_at,id)<($1,$2)")
	}
	if extra != "" {
		conds = append(conds, extra)
	}
	if q.ID != "" {
		args = append(args, q.ID)
		conds = append(conds, fmt.Sprintf("id = $%d", len(args)))
	}
	if q.Principal != "" {
		args = append(args, q.Principal)
		conds = append(conds, fmt.Sprintf("principal = $%d", len(args)))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		conds = append(conds, fmt.Sprintf("recorded_at >= $%d", len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *pgStore) Recent(ctx context.Context, q Query) ([]Entry, error) {
	return recentPG(ctx, s.pool, q)
}

type pgReader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func recentPG(ctx context.Context, db pgReader, q Query) ([]Entry, error) {
	w, args := where(q, "")
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	limitSQL := ""
	if q.Limit != -1 {
		args = append(args, limit)
		limitSQL = fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT id, recorded_at, request_id, principal, verified, operation, dialect,
		alias, provider, model, status, outcome, streamed, latency_ms, usage, price, cost_kind, cost_usd, cost_partial, detail, conversation_id, cost_bound
		FROM (SELECT * FROM llm_calls%s ORDER BY recorded_at DESC, id DESC%s) recent
		ORDER BY recorded_at, id`, w, limitSQL), args...)
	if err != nil {
		return nil, fmt.Errorf("query ledger: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var (
			e             Entry
			op, costKind  string
			usage, price  []byte
			detail, bound []byte
		)
		if err := rows.Scan(&e.ID, &e.Row.RecordedAt, &e.RequestID, &e.Row.SessionID, &e.Verified, &op, &e.Dialect,
			&e.Alias, &e.Row.Provider, &e.Row.Model, &e.Status, &e.Outcome, &e.Streamed, &e.LatencyMS,
			&usage, &price, &costKind, &e.CostUSD, &e.CostPartial, &detail, &e.ConversationID, &bound); err != nil {
			return nil, fmt.Errorf("scan ledger row: %w", err)
		}
		e.Operation, e.Row.CostKind, e.Row.MessageID = Operation(op), usageledger.CostKind(costKind), e.RequestID
		e.Row.RecordedAt = e.Row.RecordedAt.UTC()
		if err := json.Unmarshal(usage, &e.Row.Usage); err != nil {
			return nil, fmt.Errorf("decode ledger usage: %w", err)
		}
		if price != nil {
			var p usageledger.PriceSnapshot
			if err := json.Unmarshal(price, &p); err != nil {
				return nil, fmt.Errorf("decode ledger price: %w", err)
			}
			e.Row.Price = &p
		}
		if detail != nil {
			e.Detail = detail
		}
		if bound != nil {
			if err := json.Unmarshal(bound, &e.CostBound); err != nil {
				return nil, fmt.Errorf("decode admission cost bound: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) Totals(ctx context.Context, q Query) ([]Total, error) {
	w, args := where(q, "outcome = 'ok'")
	rows, err := s.pool.Query(ctx, `SELECT provider, model, count(*),
		coalesce(sum((usage->'uncached_input_tokens'->>'tokens')::bigint
			+ (usage->'cache_read_tokens'->>'tokens')::bigint
			+ (usage->'cache_write_tokens'->>'tokens')::bigint), 0),
		coalesce(sum((usage->'output_tokens'->>'tokens')::bigint), 0),
		coalesce(sum(cost_usd), 0),
		count(*) FILTER (WHERE cost_usd IS NULL OR cost_partial)
		FROM llm_calls`+w+` GROUP BY provider, model ORDER BY provider, model`, args...)
	if err != nil {
		return nil, fmt.Errorf("query ledger totals: %w", err)
	}
	defer rows.Close()
	var out []Total
	for rows.Next() {
		var t Total
		if err := rows.Scan(&t.Provider, &t.Model, &t.Calls, &t.Input, &t.Output, &t.CostUSD, &t.UnpricedCalls); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *pgStore) Close() { s.pool.Close() }
