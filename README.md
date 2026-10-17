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

**MCP is the only agent-facing interface** — matching Tangent's own
convention (verified against its source and docs; see `AGENTS.md`). The
CLI is an operator/ops surface, not a second agent API:

- `station` / `station serve` — the daemon; bare invocation defaults to
  this, matching Tangent's "no serve subcommand, bare invocation just
  serves" pattern.
- `station validate [-config PATH]` — parse and validate a config file,
  no side effects. Exit 0/1.
- `station list [-config PATH]` — print every configured logical server:
  id, name, transport, and (for `inprocess`) its manifest-declared tool
  names, without spawning or dialing anything.
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
- `GET /v1/catalog?provider=&model=&family=` and `POST /v1/catalog/refresh`: centralized model catalog (`llm/core/modelsdev` + `llm/core/pricesource`) cached on disk and refreshed in the background with a default `90s` TTL (`catalog.ttl` in `api.yaml`)
- `GET /v1/usage?limit=&principal=&since=`

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
routing, outcome, disjoint usage, price snapshot and cost. `ledger.store: postgres`
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

Budgets and local routes: `budgets` uses `llm/core/quota` (`SeededStore` + `Limiter`)
over `day`, `week`, `month` or rolling windows, scoped by `total` or verified
`principal` (`max_usd` and/or `max_requests`), seeded from the ledger on startup
and updated live on every call. Spend math stays in `quota` and passes `budget_state`
(`ok` or `exhausted`) and `exhausted_budgets` as facts into the `llm.invoke` policy
decision (`CW-20261010-0030`): shadow mode logs `would_block=true` without changing
routing, while enforce mode returns `429 budget_exceeded` with `Retry-After` (unless
a rule such as `budget_state: exhausted, tag: free, effect: allow` permits a free
local target). Targets carry `tags` (`local`, `free`, `paid`) and `best_effort: true`
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
