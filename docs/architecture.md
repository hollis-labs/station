# Station architecture (conversion draft)

DRAFT, survey only. No code has been converted. Facts below come from reading `station-previous` (main at 31c6c83)
and from the driver's read-only look at the live host. Open decisions are marked DECISION.

## 1. Decisions before code (guide section 0)

| Question | Answer |
|---|---|
| Migrated data or fresh start? | **Migrated, no loss.** The brief said fresh; the live host shows otherwise (usage.db about 47 MB, about 84,000 observations, still written; gateway.db with revisions and activations; a live Postgres ledger). A cutover plan precedes code (see `conversion-notes.md`, section Cutover outline). |
| Binary / variant | One `station` binary today with subcommands `serve`, `validate`, `list`, `secret`, `client`, `gateway-store`, `ledger-migrate`, `version`. Two deployed units run it via symlinks named `station-llm-gateway` and `station-usage-monitor`, both with `-api-only`. DECISION (owner): `service` vs `app-host`, depends on 2 below. |
| Who issues identity? | Today: libs `security/auth` with static/registry client keys (SHA-256 only), plus Tether JWTs verified offline against a JWKS (`auth.jwt`), posture `off|permissive|enforce` (`HOLLIS_AUTH_STATION` default enforce). No human accounts, no UI. DECISION (owner): see 3. |
| Hygiene (public repo) | (a) design-station's provider keys are keychain references in the live config (corrected by the driver from the live file): six `keychain://design-suite/<NAME>` entries that already exist under service `hollis-labs`, so nothing inline and nothing moves; (b) `deploy.yaml` and the existing `docs/llm-gateway-adoption.md` and `docs/llm-gateway-management.md` carry absolute personal home paths (and a host name) already in the tree and in history, so `scan-public` will not be clean until they are rewritten, and history needs a decision; (c) the gateway ledger database is also published on a tailnet address, which must not appear in docs or config here; (d) `scripts/scan-public` was not in this repo; CI now runs the framework command `substrate scan-public --tree`. |
| Routes and scopes | Plugin routes are mounted under `/api/<plugin>/` each with a required scope (list in the client section). Station's own scopes are named per plugin (`llm-gateway.*`, usage-monitor read/manage/ingest/collect). No open mounts today. The MCP logical servers authenticate through mcp-host's `bearer_tokens`/allowed origins. |
| Background work to drain | llm-gateway: async ledger writer (`store_async.go`), reservations, catalog refresh. usage-monitor: collectors (Claude, Codex, Antigravity, OpenCode Go) and paginated gateway import. mcp-host: supervised subprocesses with restart. All need a defined stop order. |
| UI needs | None. No GUI agent. |
| BOM gaps | See the pins table below. |

## 2. DECISION: how the MCP-host half is ported

The MCP-host half is used in production (confirmed read-only on the live host; dropping it is not an option). Four
deployables come from this repo, in three shapes:

| Unit | Shape | Notes |
|---|---|---|
| avatar-station | `station serve -config <avatar>.yaml -http-addr 127.0.0.1:8097` (MCP host) | One logical server `avatar` (process transport, `/avatar`, `bearer_tokens` set). The child gets its settings through an `env:` block (workflow path, Hadron URL and token file, output/model dirs, budget caps, its own HTTP address, origins, public URL). Built 2026-10-15. |
| design-station | same command, `127.0.0.1:8110` (MCP host) | Two process logical servers: `design-suite` (`/design-suite`, a Python venv entry point whose env block carries six provider keys as `keychain://design-suite/...` references) and `tripo` (`/tripo`, a shell wrapper, args `mcp --toolset full`). `allowed_origins` and `bearer_tokens` are empty on both. Different build (2026-11-05) from avatar-station. In daily use: the Tether gateway entries for `design_*` and `tripo_*` tools point at it. No Station-owned database. |
| station-llm-gateway | `-api-only`, loopback 18082, auth enforce with a keys file | See data inventory. |
| station-usage-monitor | `-api-only`, loopback 18083 (also reached by a public name through the local TLS proxy), auth enforce with a keys file | See data inventory. |

What Substrate has: module `mcp` (go-mcp server plus Streamable HTTP at `/mcp`, `RegisterChecked` typed tools,
`UseToolFilter`, `ServeStdio` with a token file, scope `<app>.mcp`) and module `pluginhost` (libs plugin-host
subprocess plugins from reviewed Specs, started all-or-nothing, forwarding routes `/api/plugins/{id}/...`, scope
`<app>.plugins`, health check). No module reads a YAML list of logical servers and proxies them as MCP servers; that is
`mcp-host`'s job. Station's served paths are `/avatar`, `/design-suite`, `/tripo` (one per logical server, set in
config), not `/mcp` or `/api/plugins/...`.

| Option | What it is | Cost / what it lacks |
|---|---|---|
| A. Keep libs `mcp-host` as the engine inside a Substrate service | Station becomes a Substrate app. Substrate supplies: `config` (SUBSTRATE_CONFIG file, merged), `logging`, `health` (`/healthz`), `httphost` (listener, Host check, bind check against the auth mode, SIGINT/SIGTERM handling through `Serve`), `auth` (guard and ScopeTable) with the served paths mounted through httphost so the guard applies, `cli`, `secrets`, and (if used) `database`. The app keeps: the `mcp-host` engine (load logical servers, spawn and supervise process/inprocess children, session routing, per-server stdio/HTTP serving), the logical-server config schema (`logical_servers:` as the app's own config block), the child env-resolution (`secretref`), and the ledger/usage plugins. | The mount must be an `httphost.Mount` whose handler is mcp-host's per-server handler. Per-server `bearer_tokens` and `allowed_origins` are replaced by the auth guard and scopes on those paths (e.g. `station.avatar`, `station.design-suite`, `station.tripo`), so every caller (fabric agents through the Tether gateway) must present a Substrate-verified credential instead of the per-server bearer; **design-station's paths are open today** (empty `bearer_tokens`), so enforcing scopes is a behaviour change that needs a named owner for the gateway entries. mcp-host serves stdio itself, which collides with `mcp.ServeStdio` only if both are used. The libs `mcp-host`/`go-mcp`/`plugin-sdk` tree stays a dependency (inside the pinned plugin-mcp module). mcp-host owns the process lifecycle, so the stop order (HTTP drain, then children) must be proven with a real SIGTERM test. Open question for Substrate: can httphost host a handler on paths chosen by app config (three dynamic path prefixes), and can `ScopeTable` be fed those paths at start? (not verified). |
| B. Map onto Substrate `pluginhost` + `mcp` | Logical servers become reviewed `libhost.Spec`s; tools registered as typed tools on `mcp.Server(app)`. | **Framework Gap, not invented here.** pluginhost forwards HTTP to plugin-host subprocesses; it does not expose a stdio MCP server child (design-suite, tripo, avatar are existing MCP servers) as MCP tools nor serve them at per-server paths; no `serve.stdio`, no `inprocess` catalog. Would need a Substrate module "mcp proxy over logical servers", to be requested and landed first. |

Recommendation (not a decision): A. B only after a Substrate task exists for an MCP-proxy module.
Decision needed from the owner: A, or hold for B.

## 3. DECISION: auth model for service clients

| Option | What changes for clients | Notes |
|---|---|---|
| i. Trust Tether issuer only (`auth.jwks_url`, audience `station`) | Every client must present a Tether JWT. All static/registry client keys stop working. | Matches the existing `auth.jwt` path in `api.yaml`; clients holding only a key must move. |
| ii. Substrate `auth` with fixed service keys | Clients keep a bearer key but it is re-issued in Substrate's format and scopes; `station client issue/revoke` is replaced or dropped. | Is key hash storage per-client with scopes and rotation available as a Substrate feature? Not verified: Gap if not. The gateway `/clients` management API (issue, rotate, revoke, stored in gateway.db) has no Substrate counterpart. |
| iii. `identity` module (own accounts) | Adds accounts, setup, login, API keys (`/api/token`). | Station has no humans or UI; likely overkill, but it supplies API keys and pairing. |

Whatever is chosen, scope names change to `<app>.<verb>` (today `llm-gateway.*`), which every client key and the gateway
management scopes must follow. Existing client credentials are stored as hashes and cannot be silently re-minted;
re-issuing is a client-side action with a named owner (cutover).

## 4. Data inventory (from code; live values pending driver)

| Store | Engine | Schema owner / tool | Credential | Written by |
|---|---|---|---|---|
| `gateway.db` tables `gateway_revisions`, `gateway_state`, `gateway_activations`, `gateway_usage` | SQLite, WAL, `PRAGMA application_id=1398032215`, `user_version=1`, hand-rolled `CREATE TABLE` in `management_store.go` | llm-gateway plugin on open; `station gateway-store import|stage|activate|show|status|history` | file mode must be private (binary refuses otherwise) | station-llm-gateway (usage entries, revisions, activations); management API `/api/llm-gateway/...`; CLI `gateway-store` |
| Postgres ledger: `llm_calls`, `llm_ledger_schema` (versioned) | PostgreSQL via pgx directly (`store_pg.go`, `reservations_sql.go`) | `station ledger-migrate` (`MigratePostgresLedger`), separate migrator login that may `SET ROLE` a non-login owner; runtime verifies schema (`schema_mode: verify`) and never repairs | runtime role: USAGE on schema, SELECT on `llm_ledger_schema`, SELECT/INSERT/UPDATE on `llm_calls`; password via `password_ref` (file:// or keychain://) | station-llm-gateway when `ledger.store: postgres` |
| `usage.db` tables `accounts`, `account_audit`, `observations`, `events` | SQLite via sqlitekit, `application_id=1398035789`, `user_version=1`, `CREATE TABLE` in `plugins/usage-monitor/store.go` | usage-monitor plugin on open (no separate tool) | private file mode | station-usage-monitor (collectors, ingest API, gateway import) |
| Client key registry | YAML file (`auth.keys_file`, absolute, SHA-256 only) or gateway.db (hashed) | `station client issue|revoke` | hashes only; issued token written to a private file | operator via CLI; gateway `/clients` API |
| Secrets | OS keychain (`keychain://...`, zalando/go-keyring) or `credfile`/`file://` refs; `station secret set|check` | `internal/secretref` | n/a | operator |
| `station.yaml`, `api.yaml` (gateway, usage), `usage-monitor.example.yaml` | YAML in `~/.config/station/{gateway,usage}/` on the deploy host | operator / Cerberus | none inline (refs only) | operator |

Observations: user_version/application_id are the only schema versioning (no goose); the guide wants one embedded goose
set via the `database` module, so a one-way data migration is needed for both SQLite files whether or not the schema is
otherwise kept. The `deploy.yaml` already takes `sqlite_backup` preflights of both DBs (backups 0600) which can be
reused for rehearsal.

## 5. RULED (2026-11-12): gateway stores. Module on SQLite, ledger stays an app-owned Postgres connection

The options considered were: (1) ledger through the module with the management store app-owned, (2) both in Postgres,
(3) two instances in the module (a framework Gap). The ruling is the inverse of option 1:

- The llm-gateway's `database` module instance is **SQLite**. It holds the gateway management store
  (`gateway_revisions`, `gateway_state`, `gateway_activations`, `gateway_usage`) and the identity module's tables in
  one file in the unit's data directory, with **one squashed goose schema** registered through
  `database.UseMigrationFS`. `application_id` and `user_version` are replaced by goose versioning.
- The **Postgres ledger stays an app-owned connection**: the existing pgx store, schema and `llm_ledger_schema`
  versioning, separate migrator role and `ledger-migrate` flow, unchanged.

Justification for the one store opened outside the module (guide section 4 allows this when justified): the ledger is an
append-only external record with its own role separation (a runtime role without schema rights, a separate migrator
role that may `SET ROLE` the owner), not application state. It outlives the application database, is shared by tooling
outside the gateway, and its schema is versioned by its own table. Moving it into the module would make the module's
migrator own identity tables and would make sign-in and key verification depend on the ledger database being up. With
the module on SQLite the gateway authenticates on local state, like every other app. The ledger needs no data
migration and no schema-version reconciliation with goose: same database, schema and roles before and after.

Consequences recorded in the plan (`conversion-notes.md`): `gateway.db` is copied one way into the new file (old file
untouched); the ledger is proved with a test against a throwaway Postgres; the DSN reference moves to the secrets
module's reference form; usage-monitor is a module on SQLite with a one-way copy of `usage.db` under a write pause.
`usage.db` is a separate unit and file from `gateway.db`.

Framework candidate (not a blocker): a second named store in the `database` module.

## 6. Pins (substrate `bom/bom.yaml` on main vs Station `go.mod`)

| Module | Station | BOM | Status |
|---|---|---|---|
| libs/app/app-host | v0.1.0 | v0.1.0 | ok |
| libs/core/util | v0.4.0 | v0.6.0 | differs (sync would raise) |
| libs/mcp/plugin-mcp | v0.4.0 | v0.6.0 | differs; mcp-host, go-mcp, plugin-sdk, api-projection sit in this module (path suffix), so it is pinned |
| libs/security/auth | v0.1.0 | v0.3.0 | differs |
| libs/security/policy, redact, secrets | v0.1.0, v0.2.0, v0.1.0 | same | ok |
| libs/ui/ui-go | v0.1.0 (indirect) | v0.1.0 | ok |
| **libs/llm/core** | v0.4.0 | not listed (neither `libs` nor `app_libs`) | **not pinned: needs a Substrate BOM PR** (llm-gateway uses costcalc, guard, llmtypes, modelsdev, pricesource, quota, usageledger) |
| libs/core/panics | v0.1.0 (indirect) | not listed | indirect only; check whether `pins check` flags it |
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | v1.8.0 | ok |
| gopkg.in/yaml.v3 | v3.0.1 | v3.0.1 | ok |
| **github.com/jackc/pgx/v5** | v5.10.0 | not listed | third-party, not pinned (needed if the ledger stays on pgx) |
| **modernc.org/sqlite** | v1.60.1 | not listed | likely brought by `core/store` or the database module; confirm via `pins check` |
| **github.com/zalando/go-keyring** | v0.2.8 | not listed | used for `keychain://` secret refs; the `secrets` module may replace this |
| golang.org/x/oauth2 | v0.35.0 | not listed | indirect; used by usage-monitor collectors |
| spf13/cobra | not used (flag-based CLI) | v1.10.2 | the `cli` module brings cobra; the subcommands move to it |

Not yet run: `substrate pins check --app .` (the app has no Substrate dependency yet, so it will fail until the port
starts). The above is by reading.

## 7. Target layout, scopes, config keys, dropped features

Layout (module `github.com/hollis-labs/station`, one Go module, three binaries):

| Path | What |
|---|---|
| `cmd/station-next` (becomes `cmd/station`) | MCP host: avatar-station and design-station are two units of this binary |
| `cmd/llm-gateway` | the LLM gateway service (section 11) |
| `cmd/usage-monitor` | usage monitor (not written yet) |
| `internal/appboot`, `internal/appdirs`, `internal/appcmd` | shared boot hook and command plan, per-app data directory, shared `secret` command |
| `internal/host`, `internal/settings` | the MCP engine inside a Substrate service |
| `internal/llmgateway` | gateway package: relay, policy, budgets, catalog, ledger, management store |
| `internal/hostguard` | Basalisk's Host/proxy-header guard (until the framework has one) and the loopback bind rule |

Scopes: MCP host `station.<logical-server-id>`; gateway `llm-gateway.chat`, `.embed`, `.decide`, `.models.read`, `.usage.read`,
`.catalog.write`, `.management.read|write|activate`; usage monitor unchanged. Config keys per unit are in
`docs/conversion-notes.md` (env, flags, paths). Dropped: client-key registry and per-server `bearer_tokens`, `/clients`
management routes and `station client`, `ledger.sqlite.path`, Tether-JWT verification (`auth.jwt`), `serve.stdio` (refused),
inprocess transport and tool-level scopes (not used by any live unit; porting them means carrying `plugin_policy.go` and a
tool filter on the MCP mount).

## 8. Test and evidence plan for the MCP-host port (in order of risk)

The spike (`spike/`, untracked, never committed, deleted when the real service replaces it) proved a guarded
config-chosen mount, 401/403/200, an unmapped mount refused, and a clean SIGTERM that stops the child, with the
repo's echo-server. The remaining items, each with a test or a recorded check:

1. **Station on plugin-mcp v0.6.0 (done).** With the Substrate dependency resolved Go selects the BOM versions;
   `go build`, `go vet` and `go test -race -count=1 ./...` pass on the unchanged code.
2. **The three production child shapes started the way production starts them**, with stand-in values only (never the
   production keys or config files): avatar-server (a binary with an `env:` block of paths, URLs, token-file and budget
   settings plus its own HTTP address), the design-suite Python venv entry point (env block with provider keys, which
   become `secrets` references), the tripo shell wrapper (args `mcp --toolset full`). Checks: env resolved through the
   `secrets` module, the child sees the value, no value in logs or `/healthz`; the child is spawned with the same
   command shape.
3. **A long-lived streaming session through httphost** (SSE; `Serve` sets no write timeout).
4. **Child crash and supervision** (`process.supervise`): kill the child under a live mount, expect the mount to
   recover or fail visibly, and `/healthz` to say which.
5. **Shutdown order made explicit in code, with a test**: stop accepting, drain HTTP, close sessions, stop children,
   then modules; asserted by a test that records the order.
6. **stdio serving of a logical server** (`serve.stdio`): see section 9.
7. **inprocess transport** (plugin-sdk child, the clock plugin) and its initialization policy.
8. **Tool-level scopes** (`ScopeTable.Tool`) and per-caller tool filtering.
9. **Real-binary run**: setup, sign-in or key issue, a write, a read, a refused request and SIGTERM, on a throwaway
   data directory and port 18110 (the migration port range is 18110-18119; the ledger container takes another port in it).

## 9. serve.stdio today (record, not a decision)

In this repo `serve.stdio: true` is set on the starter `echo` logical server in `station.yaml` and exercised by
`cmd/station/smoke_test.go`; README says to point an MCP client at Station's stdio for echo's tools. At most one logical
server may set it (mcp-host config validation). I found no use of `serve.stdio` in the two live units: avatar-station and
design-station serve over HTTP paths. Who launches a stdio Station today is not visible from the repo; the driver can
check the live configs and the Tether gateway entries. Whether a locally launched stdio process needs an identity key or
runs under filesystem trust is a program-wide question with the owner and is not settled here.

## 10. Evidence status of the plan in section 8 (cmd/station-next)

| Item | Status | Where |
|---|---|---|
| 1. plugin-mcp v0.6.0 | done | `go test -race ./...` on the whole module |
| 2. Three production child shapes with stand-ins | done | `TestProductionChildShapes`: a binary with an env block and a token reference (avatar), a Python entry point with a provider-key reference (design-suite), a shell wrapper with `mcp --toolset full` (tripo); references resolve through the secrets module, a child never sees another's secret, no stand-in value in the log |
| 3. Long response through httphost | done, with a finding | `TestLongResponseOutlivesHeaderTimeout`: a 12 s tool call answers past the 10 s header timeout. The libs handler offers no GET event stream (405), so a long POST response is the long-lived form |
| 4. Child crash and supervision | done | `TestChildCrashIsSupervised`: both children SIGKILLed, both mounts answer again with new pids, no client reconnect. `/healthz` carries one `mcp:<id>` check per logical server |
| 5. Shutdown order explicit and bounded | done | HTTP drains (5 s) then logical servers stop together (12 s), then modules; asserted by `TestAcceptance` and `TestLongResponseOutlivesHeaderTimeout`; real binary exits 0 on SIGTERM with a 60 s call in flight after 17 s |
| 6. stdio | blocked | program-wide question; `serve.stdio` is refused with a message |
| 7. inprocess transport | not done | the clock plugin's initialization policy (`plugin_policy.go`) is not ported; the live units do not use it |
| 8. Tool-level scopes / per-caller tool filtering | not done | not used by the live units |
| 9. Real binary on a throwaway directory | done | see the mail of 2026-11-12 13:02Z and `TestRealBinarySIGTERMWithCallInFlight` |

## 11. The gateway service (cmd/llm-gateway)

```
client (key from identity) -> Host check -> session/bearer -> request id + x-api-key bridge -> framework guard (scope table)
  -> management routes (flat errors)      -> internal/llmgateway.ManagementStore -> service SQLite (database module)
  -> relay routes (Raw, NOT MOUNTED yet)  -> Gateway -> upstream providers; ledger: memory | SQLite (service DB) | Postgres (pgx, app-owned)
```

- Configuration is data: the active revision in `gateway_revisions` (immutable, sha256-checked), selected through
  `gateway_state`/`gateway_activations`; staged and activated offline (`gateway-store`) or through the management API.
- Stores: one goose schema (`internal/llmgateway/migrations/sqlite`) on `database.From(app)`, which has one write connection:
  reads through `Reader()`, writes through `WriteTx` with re-runnable callbacks. The Postgres ledger is unchanged (three code
  migrations, advisory lock, `auto` or `verify`) and is administered with `llm-gateway ledger-migrate` through SET ROLE.
- Loopback only by Station's rule (`RequireLoopbackBind`), before anything listens. The gateway is reached from other
  machines through a local TLS proxy listed in `llm-gateway.allowed_hosts`; reaching it directly from another host would be an
  owner decision and a change to this rule.
- Relay routes: served under `/api/llm-gateway/v1/` (client base URLs unchanged). The framework would rewrite OpenAI-shaped error bodies
  into its flat shape, so the prefix is declared in `httphost.Options.RawPrefixes` (Substrate 0f2899e): the relay's responses are untouched,
  while the guard's and the scope table's refusals on the same paths stay flat (Gap 11).
- Shutdown: drain HTTP (15 s) then close the gateway (10 s); a unit stop timeout above 25 s.

## 12. The usage service (cmd/usage-monitor)

Own binary and unit, same boot path as the gateway (`internal/appboot`): bind check first (loopback only), then the
database module on SQLite, then `internal/usagemonitor` routes behind `auth.ScopeTable`, wrapped by the Host guard and
the identity middleware. Collectors run beside the server (`Service.Run`) and stop after the drain. The store uses the
single write connection through `WriteTx`; an old `usage.db` is refused at start and copied by `migrate-from`.


## Bind rule

Station units listen on loopback only, by Station's own rule: in `enforce` mode the framework's bind check accepts
every address (the tailnet and wildcard limits apply only to `off` and `permissive`). `serve` therefore calls
`hostguard.RequireLoopbackBind` on `httphost.addr` first, before parsing the `station:` block, starting a logical
server or listening; `TestServeRefusesNonLoopbackBind` covers wildcard, empty-host, documentation and tailnet
addresses. This is separate from the Host-header check (`hostguard.Handler`), which refuses non-loopback peers
for unlisted names. No unit is meant to be reached from another machine today. If the gateway later serves
other machines' agents, that is an owner decision and a change to this rule, not a config edit.
