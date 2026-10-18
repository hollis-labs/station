# Station

Station is a prototype built on
[`github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host`](https://github.com/hollis-labs/libs/tree/main/mcp/plugin-mcp/mcp-host):
the generic dual-transport MCP plugin host library. Station itself is thin —
flag parsing, its own config, and two starter virtual MCPs — proving the
library is feature-complete enough to build a real product on top of.

> **Pre-release.** Station is unreleased, not deployed, and has no outside
> consumers. It's being built in the open: the code, the docs, and this
> README describe what exists today, not a pitch for what's planned.
> Interfaces and behavior change without notice, and there are no
> compatibility guarantees yet.

## Status

Built on `plugin-mcp v0.4.0`. Three working virtual MCPs (`echo`, process-mode;
`clock`, inprocess-mode) run simultaneously, each independently reachable,
each recovering from a real process kill — see `cmd/station/smoke_test.go`.

## Quickstart

```bash
go build -o bin/echo-server ./plugins/echo-server
go build -o bin/clock-plugin ./plugins/clock-plugin
go build -o station ./cmd/station

./station validate                    # check station.yaml without running anything
./station list                        # see what's configured
./station serve -http-addr :8080      # or just `./station` — serve is the default
```

This serves `echo` over stdio and `clock` over HTTP at `/clock`
simultaneously. Point an MCP client at station's stdio for `echo`'s tools,
or at `http://localhost:8080/clock` for `clock`'s.

## Service layer

Station exposes MCP logical servers and opt-in HTTP plugin APIs. The LLM
gateway serves API-powered agents and tools directly, with no MCP dependency.
The CLI is the operator/ops surface:

- `station` / `station serve` — the daemon; bare invocation defaults to
  this, matching Tangent's "no serve subcommand, bare invocation just
  serves" pattern.
- `station validate [-config PATH]` — validate MCP configuration without
  spawning processes. API validation also assembles plugins and can resolve
  secrets, fetch metadata, and connect/migrate the ledger. Exit 0/1.
- `station list [-config PATH]` — print every configured logical server:
  id, name, transport, and (for `inprocess`) its manifest-declared tool
  names, without spawning or dialing anything.
- `station gateway-store` — import, stage, inspect and select SQLite gateway revisions.
- `station client` — issue/list/revoke hashed credentials in YAML or a gateway database.
- `station version` — print the build version.

There's no management API and no MCP tools for operating Station
itself (adding a logical server, restarting one, etc.) — that's
config-file-driven and CLI-inspected, on purpose, the same way Tangent's
own plugin install/list/remove stays CLI-only rather than becoming
agent-callable tools. An agent builds with Station by editing
`station.yaml` and running `validate`/`list` to check its work, the same
way an operator would.

## Atlas and other first-party plugins

The Portfolio Atlas server (read-only Atlas tools over Tesseract) moved to
[`station-plugins`](https://github.com/hollis-labs/station-plugins), the home of
first-party plugins that are not Station's own starters. Build it there and add
it to `station.yaml` as a process-mode logical server; its README has the
config, including the exposure note (Atlas holds `visibility:private` records:
use loopback or `bearer_tokens`).

## Local HTTP gateway dogfooding

Use `station serve -api-only -api-config /absolute/path/to/api.yaml` to run
only the HTTP API. No MCP config, demo server, or MCP listener is loaded.
See [the adoption guide](docs/llm-gateway-adoption.md) for the local endpoint,
per-app credentials, conversation headers, native API families, and live probes.

## SQLite gateway state

The optional SQLite store backs the LLM gateway management API.
It stores versioned gateway configuration (providers, ordered
model routes/fallbacks, capabilities/prices, policy rules, budgets and catalog
settings), hashed client identities/scopes, immutable revision history and an
activation audit. Usage can share the same database via `ledger.store: sqlite`.
Provider credentials and PostgreSQL DSNs remain secret references; resolved
secrets are never imported. The models.dev download cache remains managed by the
catalog library; per-call price snapshots are retained in the usage ledger.

SQLite uses an application ID and transactional schema version, foreign keys,
WAL, FULL synchronous writes and a bounded busy timeout. Use an absolute database
path inside an existing owner-only directory on a local filesystem. Database
files are created mode 0600; public files and symlinks are refused. Back up a live
store with SQLite's backup API or `.backup`, not by copying just the main file
while its WAL is open. A snapshot includes credential hashes and potentially
captured usage details; keep it private.

Import creates a **staged** revision without contacting providers or resolving
secrets. For example, with an existing API configuration:

```bash
install -d -m 700 "$HOME/.local/state/station/gateway"
DB="$HOME/.local/state/station/gateway/gateway.db"
station gateway-store import -db "$DB" -api-config /private/api.yaml \
  -expected 0 -actor operator
station gateway-store status -db "$DB"
station gateway-store activate -db "$DB" -revision 1 -expected 0 -actor operator
station validate -api-only -api-config /private/api.yaml -gateway-db "$DB"
```

For a Cerberus-managed service, add `-gateway-db /absolute/path/gateway.db` to its
serve arguments and deploy/reload through Cerberus. Standalone serve accepts the
same flag. Startup reads one selected revision and logs its ID. Missing, corrupt,
incompatible or unactivated databases fail startup; no YAML fallback occurs.
The API bootstrap file still owns the listener, auth mode/JWT and other plugins,
and must retain an `llm-gateway` block (it can be `{}`). Its gateway settings and
client keys/keys_file are replaced by the selected revision. Auth must be enforced.

To edit configuration, export a revision into a new private JSON file:

```bash
station gateway-store show -db "$DB" -output /private/gateway-edit.json
# Edit the document, retaining schema_version: 1.
station gateway-store stage -db "$DB" -input /private/gateway-edit.json \
  -expected 1 -actor operator
station gateway-store activate -db "$DB" -revision 2 -expected 1 -actor operator
station validate -api-only -api-config /private/api.yaml -gateway-db "$DB"
```

`-expected` is the **latest** revision for import/stage/client edits and the
**active** revision for activation. A stale value is rejected, including across
processes. `history` lists revision metadata (`-before ID` paginates); `show
-revision ID` exports a particular document. Unknown JSON fields, unsupported
document versions, invalid references/rules/budgets and ambiguous client hashes
are rejected before a revision is stored. The document is typed JSON, with
snake_case fields matching the YAML configuration.

Activation selects the revision for the **next startup**; it does not hot-reload
the running gateway or prove credentials/upstreams work. Validate before reloading
and run the existing gateway smoke probes afterward. To roll back, activate a
previous revision with the current active ID as `-expected`, then validate/reload.
Rollback restores client credentials too: an old revision can re-enable a
revoked key. Usage records are independent of configuration rollback.

Providers and model aliases accept `enabled: false` (default true). Disabled
providers skip secret resolution/discovery and are removed from runtime targets;
disabled aliases and aliases with no enabled targets are absent from model lists
and cannot be invoked. Their definitions remain in the stored document for later
re-enablement. Fallback and policy order survive storage unchanged.

Client operations also stage revisions, preserving other pending configuration:

```bash
station client issue -gateway-db "$DB" -expected 2 -actor operator \
  -id my-app -output /private/my-app.token \
  -scopes llm-gateway.chat,llm-gateway.models.read
station client list -gateway-db "$DB"
station client revoke -gateway-db "$DB" -expected 3 -actor operator -id my-app
```

Use the returned revision ID for activation, then validate/reload. The list is
from the latest revision and reports both latest and active IDs. Token material
is only written to a new 0600 file, never stdout; SQLite retains SHA-256 hashes.
Changes to the old YAML registry have no effect when `-gateway-db` is selected.

For usage in the same SQLite file, select this ledger configuration in the
stored document (or under the YAML plugin when not using managed configuration):

```yaml
ledger:
  store: sqlite
  sqlite:
    path: /absolute/private/gateway.db
  capture:
    mode: off
```

Remove `ledger.postgres` settings when selecting SQLite. Existing PostgreSQL
history is **not migrated automatically**; changing the backend starts reading
that backend's records, affecting usage reports and budget counters. Preserve
and migrate historical entries before a live cutover. SQLite uses the same
bounded asynchronous usage writer as PostgreSQL: committed batches are atomic
and idempotent, but queued records can still be lost on crash/overflow.

Starting with `-gateway-db` also enables the [management API](docs/llm-gateway-management.md)
under `/api/llm-gateway/v1/management`. It uses the portfolio
`Authorization: Bearer` pattern and dedicated management scopes through the
shared auth library. Operators can stage configuration and credential changes,
validate them, inspect history, and select a revision for the next service reload.
Status distinguishes latest, selected and actually running revisions; selection
does not hot-reload the gateway. Existing chat credentials should receive only
inference/model-read scopes. YAML-only deployments do not expose management routes.

## Plugin platform and the LLM gateway

Station's core hosts MCP logical servers and, when asked, one HTTP API
assembled with `app-host` (`platform/`). Product features are plugins that
register through a small hook set: `Mount` (routes under `/api/<plugin>/`, each
with a required auth scope), `UseMiddleware` (ordered by phase, with a declared
failure mode), `AddTransport` (extra listeners) and `OnStop`. A plugin runs only
when its block exists in the API config. See `api.example.yaml`.

`plugins/llm-gateway` is a transparent relay, not an envelope gateway
(CW-20261013-0030). A request arriving on a provider-native endpoint goes to an
upstream speaking the same protocol and the bytes pass through; the gateway only
swaps in the hub-held credential, rewrites the `model` field (and, for OpenAI
streams, asks the upstream for usage and hides that chunk again unless the
client asked), and reads usage from the response. Endpoints, under
`/api/llm-gateway`:

- `POST /v1/chat/completions` and `POST /v1/responses` (OpenAI), base URL `.../api/llm-gateway/v1`
- `POST /v1/messages` (Anthropic), base URL `.../api/llm-gateway`, key via `x-api-key`
- `POST /v1/embeddings` (OpenAI), for models with `kind: embedding`
- `POST /v1/systemone` (JEV), for models with `kind: decision`. A decision model is an LLM like the others (provider, target, policy, guard, ledger); only its dialect differs. Question order is preserved byte for byte, and the answers are kept in the ledger
- `GET /v1/models` (shape follows the `anthropic-version` header; decision models are not listed; includes resolved capabilities, pricing, context/output limits, modalities, and model metadata)
- `POST /v1/catalog/prices/reload`: reload the configured local price table with last-good preservation, using `llm-gateway.catalog.write`
- `GET /v1/catalog?provider=&model=&family=` and `POST /v1/catalog/refresh`: centralized model catalog (`llm/core/modelsdev` + `llm/core/pricesource`) cached on disk and refreshed in the background with a default `90s` TTL (`catalog.ttl` in `api.yaml`)
- `GET /v1/usage?limit=&principal=&since=`

See the [standards and model-intelligence contract](docs/llm-gateway-standards.md)
for provider API direction, catalog freshness, identity/price precedence, and
the tracked capability/budget/MCP gaps. Catalog reads report `schema_version: 1`
and `status` (`empty`, `fresh`, `stale`), with `status.sync` worker/retry health
and `status.price_file` reload health; model targets report `catalog_matched`
and their resolved `catalog_provider`. A wire dialect does not establish a
provider's prices or capabilities. Custom provider IDs should set an explicit
`catalog_provider`; an explicit mapping never falls back to another provider.

Pattern: a `Dialect` adapter per wire protocol, `provider` + `target` with
`ProviderCapabilities` resolved from `models.dev` and operator overrides (a request
needing tools or vision skips targets lacking it), automatic `pricesource` price
resolution (`target.price` override -> `catalog.price_file` -> `models.dev`), optional per-target `translate: true` for explicit `openai` <-> `anthropic`
chat and streaming SSE translation (same-dialect requests remain a zero-translation
byte relay), an `llm.invoke` decision through `security/policy` (shadow by default,
ask refuses until approved), and `llm/core` guard for breaker and cooldown with
failover before the first byte. Caller identity comes only from the verified
principal; a `caller_id` in a request is ignored. Auth is `security/auth`: Tether-issued EdDSA JWTs verified offline against a
cached JWKS (`auth.jwt`), plus static hashed keys. Provider keys are secret
references, not config: `printf %s "$KEY" | station secret set keychain://openai/work`,
then `api_key_ref: keychain://openai/work` (`station secret check <ref>` shows
where it resolves, never the value).

Logging: JSON to stderr through `security/redact` (known secrets are matched
exactly, credential shapes by rule). One `http` line per request (verified
principal, route pattern, status, duration, request ID; no query string, header
or body), one `auth` event per guarded request, one `policy` event per
`llm.invoke` decision, one `llm.call` line per relayed call (provider, model,
outcome, tokens, cost) and `llm.refused` for what the gateway turns away.
Prompts and completions are never written to stderr logs. Flags: `-log-format`, `-log-level`.

Ledger: every call (and every failed-over attempt) is an entry with identity,
routing, outcome, disjoint usage, price snapshot and cost. `ledger.store: sqlite`
uses the gateway SQLite database described above. `ledger.store: postgres`
makes it durable: an idempotent, migrated `llm_calls` table in a database and role
dedicated to it, written off the request path through a bounded queue with retry. If
the database is down, calls still succeed, entries wait and retry, and a dropped
entry is counted (`buffer.dropped` in `/v1/usage`) and logged with enough to recover
it. Setting `ledger.capture.mode: full` passes the leader/system prompt, tool
definitions, request body and response (or bounded SSE events) through
`security/redact` `v0.2.0` `Sanitizer` (sharing the resolved-secret `redact.Scope`
and PII rules, failing closed to an `oversized` tombstone when a payload exceeds
`ledger.capture.max_bytes`) and stores them in `detail` alongside the redaction
audit report.

Budgets use `llm/core/quota` window and limit math over `day`, `week`, `month`
or rolling windows, scoped by `total` or verified `principal` (`max_usd` and/or
`max_requests`). Admission reads the complete retained ledger window and commits
an `outcome: reserved` row before dispatch. PostgreSQL serializes check-and-insert
with a transaction advisory lock; SQLite uses an immediate transaction. All
matching budgets must admit before any row is written. The lock is released before
contacting the provider. All workers sharing a ledger must use this reservation
path and consistent budget configuration for cross-process request limits.

Configured budgets enforce independently of policy mode and ordered allow rules.
Exhaustion returns `429 budget_exceeded` with `Retry-After` when known. Store
failures return `503 budget_unavailable`. Unresolved, missing or partial cost in
a matching spend window returns `503 budget_cost_unknown`; a known zero cost
remains free. A `free` tag cannot bypass a budget; use budget model filters to
exclude a separate local alias when appropriate. Every fallback needs its own
reservation and consumes another request slot.

A matching spend budget also requires usable base token rates before dispatch:
input for embeddings; input and output for chat, Responses and decisions.
Absent snapshots, unknown required rates, and invalid rates return
`503 budget_price_unknown` without writing a reservation or contacting the
provider. This applies to each selected target, including fallbacks; it does not
automatically select a different target on a pricing refusal. Known zero rates
remain valid. Request-only budgets and calls outside the spend budget's filters
do not require prices. Fix the configured override or refresh/reload the relevant
price source before retrying; this error has no time-based `Retry-After`.

This checks the same captured snapshot that settlement uses. Missing optional
cache/reasoning rates can still make actual usage partially priced and block
subsequent spend through `budget_cost_unknown`. The legacy inline `price` object
treats zero input/output as known free rates and zero optional rates as unknown;
use `catalog.price_file` when omitted and explicitly zero components must be
distinguished. Catalog tiered pricing without a resolved flat rate fails this
base-rate check. A known base rate does not certify freshness, account-specific
billing, feature charges, or complete pricing for the request.

Completion atomically replaces the reservation with its usage and outcome,
including failures and cancellations. ID, admission time, identity and captured
price stay fixed. Identical settlement retries are safe; conflicting retries are
refused. Budgeted writes bypass the asynchronous usage queue. A crash, ambiguous
commit or settlement failure leaves the reservation counted until settlement or
window expiry; it is never assumed free. The retained row remains visible in
`/v1/usage` even after its window expires. `recorded_at` for reserved attempts is
admission time, so a long call settles into the window in which it started.

Request limits now cover in-flight attempts across processes and restarts with
SQL storage. Spend limits conservatively block overlapping attempts while cost
is unresolved; they do **not** yet bound a single attempt's dollar cost. Priced
per-call bounds and complete metering remain CW-20261017-0021. Usage summary
totals still aggregate successful calls; budget counters also include outstanding
and other recorded outcomes.

Memory storage is only for one process and loses state on restart. With budgets
it refuses admission when history capacity is full instead of evicting accounting
records; use SQLite or PostgreSQL for shared use. Keep all ledger rows for at least
the longest configured budget window. Do not delete unresolved rows to unblock
spend without independently establishing the actual usage. No operator settlement
API or provider-billing recovery job is included yet. Deployments with no budgets
retain asynchronous usage recording and its existing loss/overflow limits.

Targets carry `tags` (`local`, `free`, `paid`) and `best_effort: true`
(`CW-20261011-0099`: fast 1.5s header timeout + threshold-1 breaker so a sleeping or
rebooting tailnet llama host fails over in milliseconds and is skipped while open),
and `GET /v1/models` exposes `kind`, `reachable`, `tags` and per-target breaker state.

Not yet: gemini native dialect. Tether remains the production gateway while Station's
gateway is validated in shadow/parallel.

```bash
OPENAI_API_KEY=... ANTHROPIC_API_KEY=... station serve -config station.yaml -api-config api.yaml
station validate -config station.yaml -api-config api.yaml
```

## Depending on mcp-host

Station depends on the real, published
[`github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host`](https://github.com/hollis-labs/libs/tree/main/mcp/plugin-mcp/mcp-host)
(`mcp/plugin-mcp` `v0.4.0` as of this writing) — no `replace` directive needed. Both repos
are also listed in `~/dev/hollis-labs/go.work` for convenience when
developing them together locally; that workspace entry overrides
resolution to the local checkout, but `go.mod`'s own `require` line is a
real version, so a build outside this workspace (`GOWORK=off`, or any
other machine) fetches it from GitHub like any other dependency.

## Config

Station's own `station.yaml` configures its two starter virtual MCPs and the
GitHub pilot. See
[mcp-host's README](https://github.com/hollis-labs/libs/tree/main/mcp/plugin-mcp/mcp-host#config) for the
full config schema, the process/inprocess transport split, and how to add a
new logical server — Station's config is just an instance of that schema,
nothing Station-specific about the format itself.

## Package map

- `cmd/station/main.go` — the whole binary: subcommand dispatch
  (`serve`/`validate`/`list`/`version`), flag parsing, config loading, then
  a single call into `mcphost.Run` for `serve`. Everything host-shaped
  lives in the library.
- `platform/` — plugin platform: hook registry, stock middleware, auth wiring, app-host assembly.
- `plugins/llm-gateway` — the llm-gateway plugin (imports libs and `platform` only).
- `plugins/echo-server` — process-mode starter virtual MCP: a real,
  standalone MCP server with one `echo` tool.
- `plugins/clock-plugin` — inprocess-mode starter virtual MCP: a
  plugin-sdk-dialect subprocess with one `now` tool.
- `cmd/station/main_test.go` — builds the real binary and drives it with a
  real MCP client over real stdio.
- `cmd/station/smoke_test.go` — the two-virtual-MCPs acceptance test:
  both plugins running simultaneously under one station process, each
  independently reachable, each killed and confirmed to recover.

The two starter plugins remain demonstrations; the Atlas server is the first
real capability. Adding another follows the same wiring.

## Commands

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

No CI, no Makefile — these three are the only gate. `go test ./...` builds
real binaries and spawns/kills real processes, so expect it to take several
seconds — that's the point, not a flake.

## Known gaps

Everything in [mcp-host's README](https://github.com/hollis-labs/libs/tree/main/mcp/plugin-mcp/mcp-host#known-gaps-and-boundaries)
"Known gaps and boundaries" applies here too — Station doesn't work around
any of them, it just consumes the library as-is. Station-specific: the two
starter plugins are demonstration tools. The Atlas server is read-only: its
write tools need Tesseract to refuse a duplicate key and a stale `supersedes`
first (it accepts both silently today), and Station is not deployed, so no
agent reaches `/atlas` yet.

## License

MIT License — see [`LICENSE`](./LICENSE). © 2026 Chrispian Burks / Hollis Labs.
