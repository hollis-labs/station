-- +goose Up
-- The gateway's SQLite state, squashed from the old gateway.db format (user_version 1):
-- configuration revisions (immutable), the single state row, the activation audit and the
-- SQLite usage ledger. Revisions keep their stored document text and checksum, so history
-- migrated from the old file stays verifiable.
CREATE TABLE gateway_revisions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 created_at TEXT NOT NULL, actor TEXT NOT NULL CHECK(length(actor)>0),
 document_version INTEGER NOT NULL CHECK(document_version=1),
 document TEXT NOT NULL CHECK(json_valid(document)), sha256 TEXT NOT NULL
);
CREATE TABLE gateway_state (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 latest_revision INTEGER REFERENCES gateway_revisions(id),
 active_revision INTEGER REFERENCES gateway_revisions(id)
);
INSERT INTO gateway_state(singleton) VALUES(1);
CREATE TABLE gateway_activations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, activated_at TEXT NOT NULL, actor TEXT NOT NULL,
 previous_revision INTEGER REFERENCES gateway_revisions(id),
 revision INTEGER NOT NULL REFERENCES gateway_revisions(id)
);
CREATE TABLE gateway_usage (
 id TEXT PRIMARY KEY NOT NULL, recorded_at INTEGER NOT NULL,
 principal TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
 outcome TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
 cost_usd REAL, entry TEXT NOT NULL CHECK(json_valid(entry))
);
CREATE INDEX gateway_usage_time ON gateway_usage(recorded_at,id);
CREATE INDEX gateway_usage_principal ON gateway_usage(principal,recorded_at);
CREATE INDEX gateway_usage_target ON gateway_usage(provider,model,recorded_at);

-- +goose Down
DROP TABLE gateway_usage;
DROP TABLE gateway_activations;
DROP TABLE gateway_state;
DROP TABLE gateway_revisions;
