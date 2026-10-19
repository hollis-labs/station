# Usage monitor

Station's `usage-monitor` plugin stores subscription allowance observations and
individual usage events. It exposes an authenticated HTTP API for other apps.
It uses Station's app-host platform, shared auth and secret resolution,
`core/util/sqlite/sqlitekit`, and `llm/core/usageledger` token and cost contracts.
CLIProxyAPI is not a dependency.

## Run in Station

Start from [`usage-monitor.example.yaml`](../../usage-monitor.example.yaml).
Create a private directory (0700) for the SQLite database; the database is 0600.
Use separate Station client identities with only the scopes each app requires.
Raw client tokens do not belong in YAML; `keys_file` contains the same SHA-256
client records used by Station's gateway. Provider credentials remain secret
references in local bootstrap configuration. They are never returned by this API.

Validate the configured installation with:

```sh
station validate -api-only -api-config /private/station/usage/api.yaml
```

The managed service launch command is:

```sh
station serve -api-only -api-config /private/station/usage/api.yaml
```

Deploy and run that command through Cerberus, following the existing Station
gateway resource pattern. This plugin can also share the gateway's Station
process: add its configuration beside `llm-gateway`. The plugin remains available
when `-gateway-db` supplies managed gateway config and client identities.
Validation opens/migrates the usage store and seeds missing accounts, but does
not poll providers or launch `agy`. Polling starts when the host serves and stops
before storage closes. Use SQLite's online backup facility or stop the managed
service before copying the database; copying only a live `.db` omits its WAL.

The example accounts start disabled. Enable them through the management API
after setting their source credentials. Bootstrap account definitions only
create missing accounts. They do not overwrite API edits on restart.

## Collectors

| Collector | Reads | Credential/setup |
| --- | --- | --- |
| `claude` | Anthropic OAuth usage windows | Token secret or `credential_format: claude` for the CLI credential JSON |
| `codex` | ChatGPT account usage windows | Token secret or `credential_format: codex` for CLI auth JSON; account ID forwarded when present |
| `opencode-go` | Go usage endpoint | Workspace API key as a secret reference |
| `agy` | `agy -p /usage --output-format json` | Explicit absolute executable; existing sign-in belonging to the service user |
| `gateway` | Station's paginated usage export | Dedicated Station key with `llm-gateway.usage.read` |
| `push` | Normalized batches from another app/device | Exact configured `principal` with `usage-monitor.ingest` |

Native subscription collectors read allowance, not individual token history.
An `agy` source refers to the configured executable's active local account.
CLI credential files are read again each poll to observe token rotations; this
service does not perform OAuth refresh, write CLI credentials, sign in, or import
browser cookies. Expired credentials produce an `authentication_required`
observation. The owning CLI remains responsible for renewal.

Claude/Codex subscription endpoints and agy's report are provider-specific
contracts, not a universal billing API. Claude requires a usage-capable OAuth
credential. API keys are not substituted for subscription sign-ins. Unsupported
response shapes stay unknown/error, never a fabricated unused allowance.
Claude, Codex, OpenCode Go, and agy allowance reads were verified against live
accounts on 2026-10-19. Regression fixtures preserve the observed response
shapes with synthetic values; verify account bindings for each deployment.

Protocol references reviewed during implementation:
[Claude OAuth usage](https://github.com/steipete/CodexBar/blob/main/docs/claude.md),
[Codex account usage](https://github.com/steipete/CodexBar/blob/main/docs/codex.md),
[OpenCode Go](https://github.com/steipete/CodexBar/blob/main/docs/opencode.md), and
[agy quota reports](https://github.com/steipete/CodexBar/blob/main/docs/antigravity.md).

The gateway source requires this branch's `/v1/usage/export` endpoint. It scans
retained records in pages of 500, preserving gateway entry IDs and filtering by
the account's provider ID (which must match the gateway's configured provider
ID). Pages commit independently; a failed scan can be retried without duplicate
usage. Repeated full scans reconcile delayed writes and completed reservations.
Reserved attempts are excluded until finalized. Scan time and page count are
bounded (95 seconds, 200 pages); exhausting either is visible as a failed
collection. Gateway buffer failures are surfaced. This cannot recover usage the
gateway never persisted, or usage outside that gateway. The gateway remains the
authoritative record; these entries are a read projection.

Provider-wide billing reports, CLI transcript history import, and a UI are not
included. Other providers/apps can send usage events now through the push API.
Do not send the same calls through multiple sources and then sum those sources:
deduplication is within an account/source, not a guess across independent records.

## HTTP contract

Base path: `/api/usage-monitor/v1`. Every route requires a verified bearer
principal and its scope, including when the host is permissive or auth is off.
Reads are operator-wide, across configured accounts; issue this scope only to
apps intended to see the portfolio. Responses carry `Cache-Control: no-store`.

| Method/path | Scope | Result |
| --- | --- | --- |
| `GET /accounts` | `usage-monitor.read` | Accounts, latest successful quota measurement and freshness |
| `PUT /accounts/{id}` | `usage-monitor.manage` | Create or revise account metadata; disable collection |
| `GET /sources` | `usage-monitor.read` | Source identities, most recent attempt and staleness; no secret refs |
| `GET /observations` | `usage-monitor.read` | Paginated immutable quota/error history |
| `GET /events` | `usage-monitor.read` | Paginated immutable token/cost history |
| `GET /summary` | `usage-monitor.read` | Cost and token subtotals grouped by account, source, model and cost kind |
| `POST /sources/{id}/ingest` | `usage-monitor.ingest` | Commit a batch for that source's account; exact principal binding also required |
| `POST /sources/{id}/collect` | `usage-monitor.collect` | Run one configured collector; concurrent refreshes do not overlap |

`usage-monitor.*` grants all route scopes, but does not bypass a push source's
principal binding. Account management cannot change connection URLs, executables,
secret references, or source/principal bindings: those remain local operator
configuration. Account provider/kind are immutable to prevent relabeling history.

Account edit body (`expected_revision: 0` creates a new account):

```json
{
  "expected_revision": 1,
  "account": {
    "id": "claude-personal", "provider": "anthropic",
    "kind": "subscription", "label": "Claude", "plan": "Max",
    "enabled": true
  }
}
```

A stale revision returns 409. Edits retain the authenticated actor in the audit
table. Accounts are disabled rather than deleted, preserving their history.

History queries accept `account_id`, `source`, RFC3339 `since` (inclusive) and
`until` (exclusive), `after` (sequence cursor), and `limit` (1–500, default 100).
Responses have `items`, `next_after`, and `has_more`. Follow `next_after` with the
same filters until `has_more` is false. Cursors follow ingestion sequence, so a
late historical import is not skipped. Summary accepts the same filters but no
nonzero cursor; it summarizes the full filtered history regardless of `limit`.

## Data semantics

An observation contains `id`, `observed_at`, `status` (`ok`, `unknown`, `error`),
optional `error_code`, and `windows`. Each window has `id`, optional `model`,
`unit` (`percent`, `tokens`, `requests`, `credits`, `usd`), nullable `used`,
`limit`, `remaining`, and nullable `resets_at`. Percentages use 0–100.
Window values are gauges and may overlap: do not add samples together or infer
token totals from a percentage change. Unknown quantities/resets remain null.
There is no conversion between subscription percentages and universal tokens.

Failed observations preserve earlier successful measurements and their original
timestamps. Account quota freshness follows the source of its last successful
quota reading. `/sources` separately exposes every source's latest health,
including gateway imports. An account with no quota measurement remains stale;
a successful gateway import reports `unknown` quota with `usage_only`.

Minimal quota batch:

```json
{
  "observations": [{
    "id": "sample-20261019-001", "observed_at": "2026-10-19T00:00:00Z",
    "status": "ok", "windows": [{
      "id": "five_hour", "unit": "percent", "used": 25,
      "limit": 100, "remaining": 75, "resets_at": null
    }]
  }]
}
```

Use the actual observation timestamp, not the example. Ingest accepts up to 500
observations/events and 4 MiB per batch. Acknowledgement means SQLite committed.
Retry identical `(account, source, id)` data safely; changed data at the same ID
returns 409 and rolls back the entire batch. A different observation needs a new
ID. Source and actor are assigned by the server, not trusted from request bodies.

Usage `events` contain `id`, `row` (the shared `usageledger.Row` JSON contract),
nullable `cost_usd`, `cost_partial`, and `outcome`. A row records provider, model,
session/message IDs, recorded time, disjoint usage components and cost kind.
Construct Go usage with `usageledger.NewUsage()` so unset components have
`provenance: unknown`; plain zero-valued components are invalid. Token history
does not require model prices. Each event can carry its historical price snapshot.

Cost kinds distinguish `api_billed`, `api_estimated`, `subscription_equivalent`,
and `local_compute`. Subscription accounts reject API billing labels. Only
`api_billed` represents an actual provider bill. Summary `known_cost_usd` is null
when no costs are known; `unpriced_events` and `partial_events` expose incomplete
coverage. `known_tokens` includes measured/estimated components and extensions;
`uncertain_token_components` counts estimated/unknown components. Keep source and
cost-kind groups separate when presenting totals.

No prompts, completions, raw provider responses, or credential values are stored.
Only safe error categories are persisted. Provider HTTP redirects are refused;
responses and subprocess output are bounded. No inference is needed to collect
allowance data.

## Verification

Run the repository checks: `gofmt -l .`, `go vet ./...`, and
`go test -race -count=1 ./...`. The focused package covers transaction rollback,
restart persistence, idempotency, concurrent revisions, source binding,
auth-mode bypass prevention, null-vs-zero semantics, credential rotation,
failure retention, and gateway replay. Gateway export tests cover equal-time
pagination and payload stripping against memory, SQLite, and PostgreSQL stores.
The full Station suite needs loopback sockets and its optional PostgreSQL tests
need an isolated test database, never the live ledger.
