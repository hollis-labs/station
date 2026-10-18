# Gateway standards and model intelligence

Reviewed 2026-10-18 against merged Station PRs #7, #9, #11 and #13, the published
`libs/llm/core v0.3.0`, and the official sources linked below. This is the working
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
`refresh_enabled` describes configuration, not a worker heartbeat. Freshness is
not provider health, entitlement or a guarantee that every model has metadata.

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
does not reload it. Existing wildcard price-file entries remain explicit operator
overrides. `/v1/models` target entries expose `catalog_matched` and the actual
matched `catalog_provider`; a price override can exist without a catalog match.

## Observed gaps and work queue

The deployed config enables warmup and refresh every 15 minutes; its cache was
observed updating during this review. The repository default remains 90 seconds.
Existing periodic sync is useful, but the shared refresher drops errors and can
retry immediately after an expired-cache failure. Library robustness needs work
before treating this as an operationally mature source pipeline.

The [models.dev contract](https://raw.githubusercontent.com/anomalyco/models.dev/dev/README.md)
now includes canonical model identity, structured-output support, input limits,
reasoning controls and additional pricing dimensions. Specialized model types are
included by the default `type=all` endpoint in the pinned v0.3.0 library. A custom
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
rewritten. Unversioned metadata caches retain ambiguous zeros/false flags as
unknown until a successful refresh recovers their presence.

Catalog `capabilities.unknown` distinguishes absent flags from explicit false,
including `structured_output`. `/v1/models` target capability booleans and routing
still use the existing admission behavior; they are not a tri-state guarantee.
Adding another source must preserve identity, observation time, precedence and
unknown values.

| Work | Torque task | Scope |
|---|---|---|
| Provider identity and freshness | CW-20261018-0021 | This implementation |
| Current schema and unknown rates | CW-20261018-0022 | v0.3.0 adopted; metadata and partial costs preserved |
| Resilient recurring sync | CW-20261018-0023 | Last-good snapshots, bounded retries/fetches, failure/recovery visibility |
| Shared HTTP/MCP catalog and estimates | CW-20261018-0024 | Versioned DTOs, filtering/pagination, future sources, estimate completeness |
| Native API and stateful route conformance | CW-20261018-0025 | Protocol coverage, operation metadata, account affinity |
| Policy explanation and trusted facts | CW-20261018-0026 | Same decision path for preflight/enforcement; capabilities, privacy and budgets |
| SQLite-managed deployment | CW-20261018-0027 | Deploy merged management APIs; retain live PostgreSQL history |
| Discovery API-family validation | CW-20261017-0011 | Existing task; explicit routes already avoid mixed-family discovery |
| Provider headers | CW-20261017-0013 | Existing task; auth/version/workspace/beta headers and precedence |
| Unknown capabilities/readiness | CW-20261017-0019 | Warmup is implemented; uncataloged capabilities still default permissively |
| Versioned configuration/conformance | CW-20261017-0020 | Management document version exists; exported schema and reusable probes remain |
| Budget/accounting correctness | CW-20261017-0021 | Priority 1: reservations, counter failures, failed-but-billable calls, unknown spend |
| Diagnostics/telemetry | CW-20261017-0022 | Usage components, correlation, queue/persistence health |
| Activity events | CW-20261017-0023 | Workflow/lifecycle facts, separate from telemetry |

Budget enforcement currently checks before execution without reserving capacity,
counts successful outcomes, and ignores some counter errors. An earlier allow
rule can also bypass the later exhausted-budget rule. These are correctness gaps,
not hard spend guarantees. Rate snapshots are currently resolved at completion,
so a concurrent catalog refresh can change the rate used for a call. The budget
task owns fixing admission, snapshot timing, reconciliation and durable restart
behavior together.

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
