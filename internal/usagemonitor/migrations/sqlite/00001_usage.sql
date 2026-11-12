-- +goose Up
-- The usage monitor's SQLite state, squashed from the old usage.db format (user_version 1):
-- accounts with a revision and an audit trail, and the two append-only record tables. Sequences
-- are kept when history is copied from the old file.
CREATE TABLE accounts(id TEXT PRIMARY KEY, revision INTEGER NOT NULL, document TEXT NOT NULL);
CREATE TABLE account_audit(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, actor TEXT NOT NULL, changed_at TEXT NOT NULL, document TEXT NOT NULL);
CREATE TABLE observations(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, status TEXT NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX observations_latest ON observations(account_id,observed_at DESC,sequence DESC);
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL REFERENCES accounts(id), source TEXT NOT NULL, record_id TEXT NOT NULL, actor TEXT NOT NULL, collected_at TEXT NOT NULL, observed_at INTEGER NOT NULL, model TEXT NOT NULL, cost_kind TEXT NOT NULL, cost_usd REAL, cost_partial INTEGER NOT NULL, document TEXT NOT NULL, UNIQUE(account_id,source,record_id));
CREATE INDEX events_account_time ON events(account_id,observed_at,sequence);

-- +goose Down
DROP TABLE events;
DROP TABLE observations;
DROP TABLE account_audit;
DROP TABLE accounts;
