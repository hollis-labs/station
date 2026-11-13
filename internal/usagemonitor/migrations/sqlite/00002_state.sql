-- +goose Up
-- One row says how this database began: 'migrated' (history copied from an old usage.db by migrate-from)
-- or 'init' (an explicit start with no history). `serve` refuses a database without it, so a unit that
-- was only given an owner and keys cannot be started and seed accounts ahead of the migration.
CREATE TABLE service_state(key TEXT PRIMARY KEY, value TEXT NOT NULL, set_at TEXT NOT NULL);

-- +goose Down
DROP TABLE service_state;
