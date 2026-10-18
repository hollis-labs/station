# Gateway standards and model intelligence

Reviewed 2026-10-18 against merged Station PRs #7, #9, #11, #13 and #14, the published
`libs/llm/core v0.4.0`, and the official sources linked below. This is the working
contract and gap map for Torque epic **EP-20261018-0001**. Tasks own progress;
this document does not certify unimplemented provider features.

## Provider API direction

| Provider surface | Current documentation | Station coverage and next work |
|---|---|---|
| OpenAI | [Responses is recommended; Chat Completions remains supported](https://developers.openai.com/api/docs/guides/migrate-to-responses) | POST Responses and Chat Completions with SSE exist. Keep both; legacy `/completions` is not implemented. Stored response lifecycle, account affinity and other native features need explicit contracts. |
| Claude | [Messages, token counting, models and files; Bearer auth and workspace selection](https://platform.claude.com/docs/en/api/overview) | Messages exists. The current `x-api-key` adapter remains supported upstream; multi-workspace selection and newer auth patterns need review. Token counting/files are not gateway routes. |
| Gemini | [Interactions is recommended alongside generateContent and other APIs](https://ai.google.dev/api); [OpenAI compatibility remains available](https://ai.google.dev/gemini-api/docs/openai) | Current route uses OpenAI-compatible chat. Native Interactions/generateContent/streaming are separate protocol work, not a base-URL substitution. |
| OpenRouter | [Responses API](https://openrouter.ai/docs/api/reference/responses/overview) | OpenAI-dialect relay supports POST Responses; each configured route still needs verified model/operation support. |
| Hugging Face | [Chat Completion](https://huggingface.co/docs/inference-providers/tasks/chat-completion) | OpenAI-compatible chat path exists. Provider/model capability and billing details must retain their own identity. |
| OpenCode | [Console inference exposes multiple API families](https://opencode.ai/v2/docs/console/api/inference); [Go access](https://opencode.ai/v2/docs/console/go/) | Installed Zen/Go routes use explicit operations and conversation IDs. Audit newer Console paths/auth independently; shared model IDs do not establish API compatibility or workspace entitlement. |

Responses IDs, remote files, signatures and stored interaction state belong to an
upstream account/protocol. Transparent bytes alone do not make cross-provider
failover safe for stateful requests. Support must be advertised per route and
operation, backed by fixtures and timestamped live observations. Hosted agents,
batch, realtime and media operations require their own scope decisions; a provider
having an endpoint does not mean the gateway supports it.

## Catalog contract implemented in this change

`GET /v1/catalog` retains `providers`, `models`, `ttl_seconds` and optional
`fetched_at`, adding `schema_version: 1` and `status`:

```json
{
  "source": "models.dev",
  "state": "fresh",
  "refresh_enabled": true,
  "fetched_at": "2026-10-18T15:00:00Z",
  "age_seconds": 60,
  "stale_after": "2026-10-18T15:15:00Z"
}
```

`state` is `empty`, `fresh` or `stale`. Timestamps are omitted when unavailable.
Freshness uses the configured TTL. Disabling background refresh does not make an
old snapshot fresh; manual refresh still works. Status loads an existing disk
cache before reporting its timestamp, including the first read after restart.
`POST /v1/catalog/refresh` also returns the version and status on success.
`refresh_enabled` describes configuration. `status.sync` reports whether the
worker is running, whether a refresh is in flight, attempts/failures,
`last_attempt`, `last_finished`, `last_success`, `next_attempt`, safe error classes
and upstream HTTP status. `source_id` hashes the exact source URL; raw URLs,
response bodies and filesystem paths are excluded. Freshness is independent of
refresh success: a failed manual refresh can coexist with fresh cached data.
`next_attempt` is eligibility, not a promise when the worker is stopped. A missing
value means immediately eligible. Only catalog data/fetch time persist; process
attempt counters reset on restart. Freshness is not provider health, entitlement
or a guarantee that every model has metadata.

Provider identity resolution follows these rules:

1. Explicit `catalog_provider` is authoritative; missing data stays missing.
2. Otherwise use the configured provider ID, followed by the existing conventional
   vendor alias (for example `openai-work` → `openai`, `gemini-work` → `google`,
   `opencode-go-claude` → `opencode-go`). Prefer an explicit mapping for custom IDs.
3. Never infer commercial identity from `dialect`. An OpenAI-compatible local
   server does not inherit OpenAI prices or model metadata.

Price precedence is target override, then the configured price file (local
provider ID before mapped identity), then models.dev for the resolved provider.
This preserves account-specific prices while preventing a missing aggregator model
from borrowing a lab's rate. `price_file` is loaded at startup; catalog refresh
does not reload it. `POST /v1/catalog/prices/reload` reloads that configured path
without fetching models.dev, protected by `llm-gateway.catalog.write`. Success
returns schema version and status; invalid/missing files return 422 with safe
health, or 409 if no file is configured. Reads continue using the previous table
while reloads serialize; failed reloads preserve it. A bad startup file still
fails initialization. Files must be regular JSON price tables no larger than
8 MiB; malformed/trailing JSON, missing/null `entries`, invalid rates and unknown
fields are rejected. An explicit `entries: []` clears overrides. Publish edits
with an atomic rename (for example `pricesource.SaveFile`). Local filesystem
reads are bounded by size but cannot be interrupted by a context; canceled
reloads do not publish once cancellation is observed.

`status.price_file` reports `disabled`, `empty`, or `loaded`, load attempts,
consecutive failures, safe error code, last successful load time and entry count.
`as_of` retains the operator-supplied price date, separate from load time. A failed
reload leaves state `loaded` with the old load time and an error. No file watcher
or periodic price-file reload runs. Changing its configured path follows normal
management revision/apply semantics. Existing wildcard entries remain explicit
operator overrides. `/v1/models` target entries expose `catalog_matched` and the actual
matched `catalog_provider`; a price override can exist without a catalog match.

## Observed gaps and work queue

The deployed config enables warmup and refresh every 15 minutes; its cache was
observed updating during this review. The repository default remains 90 seconds.
The pinned v0.4.0 client keeps last-good data on failed/invalid/empty refreshes,
serializes publication and applies bounded failure backoff. Successful refreshes
run on `catalog.ttl`. Each cycle has at most three HTTP attempts; permanent HTTP
errors and invalid payloads are not retried within that cycle. Failure delays
start at `catalog.retry_min` (default 1s), double to `catalog.retry_max` (default
1m), and reset on success. Both must be positive and max must be at least min.
`catalog.refresh_timeout` (default 30s) bounds waiting/fetching/retries, including
warmup; cache filesystem publication is not context-interruptible.
`catalog.max_response_bytes` defaults to 64 MiB; zero selects the default and
values above 1 GiB or below zero are rejected. These settings persist in SQLite
management revisions. Stopping the plugin cancels the worker and waits for its
work, subject to the host's shutdown deadline.

Schema 2 caches are bound to the exact source URL, including query parameters.
Old unbound `catalog.json` files are ignored by Station: the first startup after
this upgrade must fetch a new snapshot (use warmup when required). No source
ownership is inferred from an old file. Subsequent offline starts load the matching
source-bound snapshot. Changing the URL cannot reuse another source's data.
Price lookups use the library's atomic model/fetch-time read; Station no longer
keeps a second models.dev price cache.

The [models.dev contract](https://raw.githubusercontent.com/anomalyco/models.dev/dev/README.md)
now includes canonical model identity, structured-output support, input limits,
reasoning controls and additional pricing dimensions. Specialized model types are
included by the default `type=all` endpoint in the pinned library. A custom
`catalog.url` is used unchanged. `/v1/catalog` exposes canonical identity,
description, type/status, `provider_metadata`, reasoning controls, interleaving,
experimental modes, input limits, and expanded pricing. Provider bodies, headers
and API URLs are descriptive source data; Station never applies them to inference
requests. Sharing a canonical model ID does not merge provider prices or access.

Catalog `cost` omits unknown core rates; explicit zero means free. Audio rates and
context tiers remain available as metadata. `cost_currency` is `USD` and
`cost_unit` is `per_million_tokens`. The effective five-component `price_snapshot`
carries `unknown_rates`; without tier selection, context-dependent rates remain
unknown. Audio usage is not priced by this projection. Consumers must inspect
these flags rather than assuming an omitted numeric field means zero.

Usage entries retain `cost_partial`; aggregate `unpriced_calls` includes partial
costs as well as calls with no cost. Such totals are lower bounds. This does not
make budgeting fail closed on unknown prices. Catalog prices are estimates, not
subscription invoices or verified account access. Historical snapshots are not
rewritten. Unbound metadata caches are not loaded; newly fetched snapshots
preserve the distinction between absent fields and explicit zero/false values.

Catalog `capabilities.unknown` distinguishes absent flags from explicit false,
including `structured_output`. `/v1/models` target capability booleans and routing
still use the existing admission behavior; they are not a tri-state guarantee.
Adding another source must preserve identity, observation time, precedence and
unknown values.

| Work | Torque task | Scope |
|---|---|---|
| Provider identity and freshness | CW-20261018-0021 | This implementation |
| Current schema and unknown rates | CW-20261018-0022 | v0.3.0 adopted; metadata and partial costs preserved |
| Resilient recurring sync | CW-20261018-0023 | v0.4.0 adopted; safe source health, bounded sync and explicit price-file reload |
| Shared HTTP/MCP catalog and estimates | CW-20261018-0024 | Versioned DTOs, filtering/pagination, future sources, estimate completeness |
| Native API and stateful route conformance | CW-20261018-0025 | Protocol coverage, operation metadata, account affinity |
| Policy explanation and trusted facts | CW-20261018-0026 | Same decision path for preflight/enforcement; capabilities, privacy and budgets |
| SQLite-managed deployment | CW-20261018-0027 | Complete: live management verified; PostgreSQL usage retained |
| Discovery API-family validation | CW-20261017-0011 | Existing task; explicit routes already avoid mixed-family discovery |
| Provider headers | CW-20261017-0013 | Existing task; auth/version/workspace/beta headers and precedence |
| Unknown capabilities/readiness | CW-20261017-0019 | Warmup is implemented; uncataloged capabilities still default permissively |
| Versioned configuration/conformance | CW-20261017-0020 | Management document version exists; exported schema and reusable probes remain |
| Budget/accounting correctness | CW-20261017-0021 | Durable request admission and opt-in tokenized embedding bounds implemented; general bounds, metering and recovery remain |
| Diagnostics/telemetry | CW-20261017-0022 | Usage components, correlation, queue/persistence health |
| Activity events | CW-20261017-0023 | Workflow/lifecycle facts, separate from telemetry |

Budget seeding reads the full retained ledger window rather than only the newest
1,000 entries. Budget admission is independent of policy allow rules and shadow
mode. Counter failures fail closed, all dispatched outcomes count toward request budgets, and
reported costs from canceled/interrupted attempts contribute to spend. Missing
or partial historical cost blocks the matching spend window; known zero remains
free. Prices are captured before each attempt, including an unknown price.

With budgets configured, SQL admission now atomically checks the full window and
persists a reservation before dispatch. Completion replaces it with usage in one
transaction, bypassing the asynchronous queue. Requests count in their admission
window across processes/restarts; crashes leave outstanding rows counted. A pending
or incomplete cost blocks another matching spend attempt until settlement or window
expiry. These rows remain inspectable as `outcome: reserved` in usage reads.

Matching spend budgets now reject missing or unknown base rates before dispatch
(`503 budget_price_unknown`), without consuming quota. Embeddings require input;
generation requires input and output. The check uses the pinned attempt snapshot,
honors model/principal filters, and cannot be bypassed by policy or a free tag.
Rate validation uses `usageledger.PriceSnapshot.Validate`, including non-finite
values. Unknown optional components remain distinct and can still produce partial
cost on settlement. Legacy inline prices preserve their existing zero semantics;
price files can represent each missing rate explicitly.

Spend budgets can now opt into `require_cost_bound`. The initial versioned target
contract, `cost_bound_method: tokenized_embeddings_v1`, supports native OpenAI
embedding requests containing token-ID arrays. Exact token counts and a captured
flat input rate produce a microdollar bound checked atomically before dispatch.
The operator must establish input-only billing for the actual account/endpoint;
model discovery never infers this declaration. Unsupported request shapes fail
closed with `422 budget_cost_unbounded`. Limits round down, costs round up, and
overflowing histories fail closed. The persisted bound survives SQL restart and
settlement; incomplete or mismatching usage still blocks other spend. No parallel
USD holds are released based only on the bound. PostgreSQL schema v3 adds the
nullable bound column; existing rows remain intact, and older binaries refuse v3.

CW-20261017-0021 still owns general chat/tool/multimodal bounds, request-specific
rate/feature coverage, complete metering and operator/provider-assisted recovery.
Default observed-spend budgets can still overshoot on a single call. Strict bounds
are at configured rates, not a promise about subscription debit or provider bills.
All workers must use the reservation path and consistent budget configuration.
Memory storage is single-process only and stops budgeted admission at capacity.
Usage totals retain their successful-call contract; budget counters also count
outstanding and failed attempts. No costs here are subscription debits or bills.

Provider documentation reviewed on 2026-10-18 reinforces that the current flat
base rates are insufficient for a general upper bound. OpenAI's generated-token
cap includes reasoning, but does not bound every other charge
([reasoning guide](https://developers.openai.com/api/docs/guides/reasoning),
[pricing](https://developers.openai.com/api/docs/pricing)). Anthropic documents
distinct cache-write durations and feature pricing, while token counting is an
estimate without caching logic
([pricing](https://platform.claude.com/docs/en/about-claude/pricing),
[token counting](https://platform.claude.com/docs/en/build-with-claude/token-counting)).
The initial bound deliberately uses only tokenized embedding requests, whose
token-array input and input-token pricing are documented in the
[embedding API](https://developers.openai.com/api/reference/resources/embeddings/methods/create)
and [embedding guide](https://developers.openai.com/api/docs/guides/embeddings).
Future methods must define supported shapes and rate coverage before admitting
them under strict budgets.

Effective target capability flags still use booleans with permissive defaults for
uncataloged models. The catalog-match flag exposes missing data but does not yet
establish tri-state
capabilities, structured-output/reasoning admission or complete context limits.
Those belong to the existing capability/conformance tasks and shared schema work.

Station's HTTP metadata exists today; the gateway has no MCP catalog or estimate
surface yet. The planned MCP adapter must reuse the HTTP service contract and use
[structured tool output](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)
and [resources](https://modelcontextprotocol.io/specification/2025-11-25/server/resources)
where appropriate. Each consuming app should use the shared catalog rather than
start another independent metadata sync.

Older Tether gateway epics retain separate migration/consumer requirements. They
must not be marked complete wholesale just because Station implements the basic
relay. App launch/adoption remains CW-20261017-0032 with Chrispian and the planner.
