# Conversion notes: Station onto Substrate (draft)

Survey only, no code converted. Data is in use: it is migrated, never moved aside, never deleted.

## Gaps

1. **Brief versus reality (stopped, reported).** Brief: wave 1, fresh data, no DB, no UI. Found: SQLite and Postgres
   stores, a deployed fleet unit, an MCP host. Driver confirmed station is in use (usage.db about 47 MB, about 84,000
   observations, live Postgres ledger). Resolved: treat as in-use, cutover plan first.
2. **MCP host has no Substrate counterpart** (DECISION, owner; used in production by avatar-station and
   design-station, so it cannot be dropped). `architecture.md` section 2: A keep libs `mcp-host` as the engine inside a
   Substrate service, B framework gap (an MCP-proxy module). Substrate `mcp` and `pluginhost` cover tool registration
   and HTTP-forwarded plugins, not a YAML-configured proxy of `process` MCP logical servers served at per-server paths.
   Unverified: whether `httphost` can mount handlers on config-chosen paths and have `ScopeTable` know them.
3. **Auth model for service clients** (DECISION, owner). `architecture.md` section 3. Whether Substrate offers
   per-client hashed service keys with scopes, rotation and a management API is not verified.
4. **Two stores in one binary: ruled** (`architecture.md` section 5). Module on SQLite (management store plus identity,
   one goose schema); Postgres ledger stays app-owned. Framework candidate, not a blocker: a second named store in the
   `database` module.
5. **BOM does not pin `libs/llm/core`** (confirmed by running `substrate pins check --app .` at Substrate main
   2248c52 on a scratch copy: `go.mod: unpinned: github.com/hollis-labs/libs/llm/core is a libs module the bom does not
   pin (add it to app_libs or drop the import)`; `pins sync` stops on the same line). The llm-gateway imports
   costcalc, guard, llmtypes, modelsdev, pricesource, quota and usageledger from it, so dropping the import is not an
   option. A one-line Substrate BOM PR (`app_libs`, v0.4.0) is needed; the driver lands it. `pgx/v5`, `go-keyring`,
   `modernc.org/sqlite` and `x/oauth2` did not produce a finding (third-party pins are checked for the BOM's own
   `third_party` list only). Evidence of the upgrade itself: with the Substrate dependency added, Go selected the BOM
   versions (core/util v0.6.0, plugin-mcp v0.6.0, security/auth v0.3.0) and the current Station tree passes
   `go build ./...`, `go vet ./...` and `go test -count=1 ./...` unchanged.
6. **No pre-migration hook** (known gap): the legacy guard cannot be used here anyway because the data must be read,
   not refused. A one-way migrator that reads the old SQLite files must run before the database module opens a new
   schema.
7. **`AGENTS.md` is stale.** It says "not released, no consumers"; `deploy.yaml` and the live host say otherwise.
   Fix in the same change as the port.
8. **Two MCP-host instances with different builds** (avatar-station 2026-10-15, design-station 2026-11-05) run from
   binaries outside this repo's `deploy.yaml`; the fleet descriptor covers only the gateway and the usage monitor. Cutover
   needs descriptors for all four.
9. **Hygiene:** provider API keys inline in design-station's config env block (move to the `secrets` module); the
   design-station paths have empty `bearer_tokens` (open today); `deploy.yaml` holds an absolute personal path and a host
   name; no `scripts/scan-public` in the repo. Findings only, nothing changed (all since fixed; see the hygiene table).
10. **Postgres ledger** is a container database published on loopback and a tailnet address, with a Cerberus descriptor
    and a compose file beside the gateway config (not in this repo). Cutover must preserve it untouched.
11. **httphost rewrites the gateway's OpenAI-shaped errors (found at the start of cmd/llm-gateway; reported, waiting).**
    `httphost.Handler` wraps every route and every mount in `apierr.Normalize`, which turns a JSON error body of the form
    `{"error":{"code":"...","message":"..."}}` (status 400 and above) into the flat `{code, message}`. The gateway
    exists to relay OpenAI- and Anthropic-compatible APIs, and its OpenAI-dialect errors are exactly that nested
    shape: `{"error":{"message","type","param","code"}}` (`plugins/llm-gateway/dialects.go`). Run against
    `apierr.Normalize` (probe, not committed): a 404 with `"code":"model_not_found"` comes out as
    `{"code":"model_not_found","message":"The model does not exist"}`, so `error.type`, `error.param` and the nested
    `error` object are gone, and an OpenAI SDK that reads `error.message` no longer finds it. Anthropic-shaped errors
    (no `error.code`) and success and streaming responses are unaffected. There is no option on `Mount`, `Routes` or
    `Options` to skip it. Not invented around: the gateway keeps its dialect errors as they are. What is needed from the
    framework is an opt-out for a mount or route (a `Raw`/`NoNormalize` flag, or Normalize skipping a response that
    carries a header the handler sets), or a scoping rule (normalize only the framework's own routes). Until then the
    gateway's relay routes cannot serve OpenAI clients faithfully through `httphost`.

    A `Raw` flag on `Mount` alone would not be enough for the gateway as it is served today. Clients call
    `/api/llm-gateway/v1/chat/completions`, `/v1/messages` and the rest under `/api/llm-gateway`, and
    `httphost.checkMount` refuses any mount at or under `/api`, `/healthz` or `/mcp`. Either the opt-out has to
    reach routes registered through `Options.Routes` (a per-route flag, or the mux wrapped in a marker the
    normalizer honours), or the relay moves to a mount outside `/api` (for example `/llm-gateway/v1/...`), which
    changes the base URL of every gateway client. Station has no preference between the two beyond the first not
    changing any client; it is the framework's call.

## Client list (live facts via the driver, read-only; ids and scopes only)

| Client | Surface | Auth today | Scope today |
|---|---|---|---|
| `station-operator` | gateway: everything incl. management | gateway keys file | `llm-gateway.*` |
| `station-smoke` | gateway chat, models, embed, decide | gateway keys file | `llm-gateway.chat`, `.models.read`, `.embed`, `.decide` |
| `usage-operator` | usage-monitor read, manage, collect | usage keys file | `usage-monitor.read`, `.manage`, `.collect` |
| `usage-reader` | usage-monitor read | usage keys file | `usage-monitor.read` |
| `gateway-reader` (token file beside the usage config) | the gateway reads the usage monitor as a client | usage keys file | read (name implies) |
| Tether gateway entries `design_*` and `tripo_*` | design-station `/design-suite`, `/tripo` on 8110 | none (empty bearer_tokens) | none |
| Tether gateway / fabric agents for avatar | avatar-station `/avatar` on 8097 | per-server `bearer_tokens` | n/a |
| Cerberus units | `avatar-station`, `design-station`, `station-llm-gateway`, `station-usage-monitor` | n/a | n/a |
| Health probes | gateway `18082/healthz`, usage `18083/healthz` | none | none |

Repo-visible gateway routes: `/v1/chat/completions`, `/responses`, `/messages`, `/embeddings`, `/systemone`, `/models`,
`/catalog`, `/usage`, `/usage/export`, and management routes (status, revisions, activations, validate, activate,
clients, config resources). Usage-monitor routes: accounts, sources, observations, events, summary, ingest, collect.

Owner decision (2026-11-12): each service is its own issuer through the identity module; clients hold identity-issued
API keys; Station's own client-key registry and the per-server `bearer_tokens` go. Manifest app names are `station`,
`llm-gateway`, `usage-monitor`, so the gateway and usage-monitor scope strings do not change (`llm-gateway.chat`,
`usage-monitor.read`, ...); the MCP host gets one scope per logical server, `station.<id>`, derived from the id in
config. Every client still changes once, because stored hashes cannot be carried over:

| Client | New credential | Scope | Who changes it |
|---|---|---|---|
| `station-operator` | key from the llm-gateway service | `llm-gateway.*` (wildcard verified, see below) | operator at cutover |
| `station-smoke` | key from the llm-gateway service | `llm-gateway.chat`, `.models.read`, `.embed`, `.decide` | operator |
| `usage-operator` | key from the usage-monitor service | `usage-monitor.read`, `.manage`, `.collect` | operator |
| `usage-reader` | key from the usage-monitor service | `usage-monitor.read` | operator |
| gateway reading the usage monitor | key from the usage-monitor service, in a 0600 file | `usage-monitor.read` | operator |
| Tether entries `design_*` and `tripo_*` | key from the design-station unit | `station.design-suite`, `station.tripo` (or `station.*`) | Tether's agent, routed by the driver |
| Tether entry avatar | key from the avatar-station unit | `station.avatar` | Tether's agent |

For the MCP paths this adds auth where design-station has none today.

## Cutover outline (draft, from the plan template; no rehearsal yet)

Four deployables, so four cutovers, each with its own rollback. Old instances keep running until each new one is
verified. Proposed order, least live-critical first: 1 usage-monitor (the gateway reads it as a client), 2 gateway
(live clients, Postgres ledger), 3 avatar-station, 4 design-station (daily use). Reason: matches the existing
`deploy.yaml` restart order for the first two; MCP instances last because the port changes their auth.

1. **Data** (gateway and usage only; the MCP instances have no Station database):
   back up `gateway.db` and `usage.db` (reuse `deploy.yaml` `sqlite_backup`, 0600) and snapshot the Postgres ledger.
   One-way migration to the new schema; rehearse on copies on a throwaway host; compare counts before and after
   (gateway: gateway_revisions 10, gateway_activations 9, gateway_usage 0; usage: accounts 11, observations about
   84,000 and growing, events 67; Postgres: llm_calls and llm_ledger_schema) and `PRAGMA integrity_check`; compare
   usage totals by principal and model. Nothing deleted or moved aside. usage-monitor is written continuously, so the
   migration needs a write pause or a final delta pass: decision.
2. **Clients**: the table above; each is run against the new build. Credentials stored only as hashes cannot be
   re-minted by me: re-issue is a named owner action.
3. **Config and supervision**: four descriptors/units to hand to Cerberus (flags like `-api-only`, `-api-config`,
   `-gateway-db`, `-config`, `-http-addr` change meaning; the avatar `env:` block and the design-suite inline provider
   keys move to config/secrets refs; HOLLIS_AUTH_STATION is replaced by an auth mode key). Map every key in a table
   (to write after the decisions).
4. **Deploy**: build once in CI; `fleet-install deploy station --commit <sha> --dry-run` first per unit; the current
   `deploy.yaml` covers only gateway and usage-monitor, so avatar-station and design-station need entries (they run two
   different builds today). Health: `/healthz` on each unit.
5. **Rollback per unit**: relink the previous release; usage-monitor and gateway: restore the `.pre-<version>.db`
   backups (0600) and keep the Postgres snapshot; usage written after cutover is lost unless merged back (decision);
   avatar/design: relink the previous binary and restore the previous config (no data owned by Station).
6. **Verify live**: key auth, one write (a gateway call that records a ledger row), one read (`/v1/usage`), an unknown
   Host refused, one MCP tool call through each of `/avatar`, `/design-suite`, `/tripo` via the Tether gateway.
7. **Approval**: driver and owner before any deploy; Cerberus owns the descriptors.

## Env, flags, paths, HTTP

To be filled after decisions 2, 3 and 5. Inventory so far: the only settings env names are `HOLLIS_AUTH_STATION`,
`dsn_env` / `api_key_env` indirections named in config, and test helpers (`STATION_TEST_PG_DSN` etc.). Flags:
`-config`, `-http-addr`, `-api-config`, `-gateway-db`, `-api-only`, `-state-dir`, `-log-format`, `-log-level`.

## Hygiene: exact files and replacements (the literal values are deliberately not repeated here)

`scan-public --ref HEAD --no-allowlist` (run from the Substrate checkout against this clone). Start of the port:
**tree 17 findings, history 18, NOT clean.** After the test-fixture changes below: **tree 11 findings in 7 files**
(see the remaining list). History is untouched and waits for the owner's program-wide answer.

Done in this branch (tests only, behaviour unchanged, tests pass): secret-shaped and key-shaped fixtures are assembled
at run time from parts (`auth_logging_test.go` bearer value, `capture_translate_test.go` provider key,
`ledger_test.go` and `management_store_test.go` URL credentials); the private address in `auth_dial_test.go` became an
RFC 5737 documentation address (the test only needs "non-loopback, non-wildcard"); the agent-box provider id in
`budget_test.go` became `local-llama`.

Remaining, by file (replacement when the file is rewritten for the port):

| File | Finding | Replacement |
|---|---|---|
| `deploy.yaml` | agent-box host name; personal home paths in ExecStart and symlinks | replaced wholesale by the new fleet descriptors; use `%h` or a placeholder |
| `docs/llm-gateway-adoption.md`, `docs/llm-gateway-management.md` | cloud host name, agent-box host name, Tailscale mention, personal home paths | rewrite with `gateway.example.test` and `$HOME`-style placeholders |
| `usage-monitor.example.yaml` (one line) | cloud host name in an example URL | `https://gateway.example.test/api/llm-gateway` |
| `internal/secretref/resolve_test.go` (2 lines), `station.yaml` (1 line) | secret-assignment rule matches `<NAME>_TOKEN: keychain://...`, which is a reference, not a credential | allowlist candidates with that reason; run-time assembly does not help because the scanner matches the assignment shape |

History: the same patterns, first added in the commits the scanner lists; no rewrite, no allowlist until the owner rules.
CI runs `go run github.com/hollis-labs/substrate/cmd/substrate scan-public --tree` (the framework command, tree only until the
one program-wide history pass); the copied script was deleted.
gitleaks reported nothing in tree or history (driver check); `station.yaml` line 61 is a keychain reference.

## Usage-monitor cutover: write pause (planned, not delta)

Stop the old unit, migrate, verify counts, start the new unit. The window is measured on a copy made with the SQLite
online backup (the driver provides it when I am ready). What is lost on rollback after the new unit has collected:
everything the new unit recorded between its start and the rollback, since the restored database is the pre-cutover
backup; that is bounded by the time between start and rollback. How long collection can pause without a gap that
matters is not known: collectors need their pull windows and the ingest API needs client retry behaviour, both to be
confirmed. The owner approves the loss window with the plan.

## Verified during the MCP-host port (cmd/station-next, tests in `cmd/station-next/boot_test.go`)

- **Wildcard scopes work**: a key with `station.*` is accepted on `/echo` and `/other`; libs `auth` matches `<app>.*`
  (and `*`). So `station-operator` can hold `llm-gateway.*`. A key for `station.echo` is refused (403) on `/other`
  and a key scoped to another app (`llm-gateway.chat`) is refused on `/echo`. Unknown key 401, no credential 401.
- **A mount without a scope is refused** (403 `route_not_mapped`). Removing the scope registration from
  `internal/host` makes the acceptance test fail (checked once on purpose).
- **Shutdown order is explicit in code and asserted**: HTTP drains, then logical servers stop (last started first),
  then the modules stop. The test records `http-drained, logical-servers-stopped, modules-stopping` and checks no child
  outlives `serve`. A real-binary run on a throwaway directory: `SIGTERM` exit code 0, child gone.
- **Data directory per unit** (`STATION_DATA_DIR`, else the XDG data home for `station`): `station.db` (identity
  tables only), `identity-signing.key` (0600), the `secrets/` store; the directory is 0700 and the database 0600.
  avatar-station and design-station set different `STATION_DATA_DIR` values in their descriptors. This is the known
  run-time path gap (no hook), worked around in `main` as Basalisk and Parallax do.
- **Host check**: requests for a Host other than loopback or `station.allowed_hosts` get 403.

## Open points found while porting

1. **First-run setup is offline (resolved by the framework, pin d453ac8).** `identity.setup` defaults to `offline`:
   `POST /api/setup` is always refused (403 `setup_offline`) and `GET /api/setup` answers
   `{setup_required, setup_over_http}`. The owner and the first keys are created with the binary's own commands
   before it serves: `<binary> identity setup --username NAME` (password from a no-echo prompt or the first line of
   stdin, never an argument or the environment; refuses when an owner exists) and `<binary> identity key create
   --name N --scope S... (--out PATH | --stdout)` (a new 0600 file, never overwritten). They run through the normal
   boot, so the data directory, database, signing key and secrets are the server's, and they DO create those files;
   `plan()` starts them with the write module set and read-only commands still create nothing. Tests create the
   owner with the command (in process through the root command, and with the real binary in the SIGTERM test);
   `TestAcceptance` asserts the HTTP setup is refused and `setup_required` flips once the command ran. The four
   per-unit first runs are scripted in `scripts/first-run` (see "First-run script").
2. **Secrets: legacy mapping not carried.** The old resolver chained the `secrets` store with a `tether` legacy mapping
   and the api-projection credential resolver (its own keychain service, `helper://`). The new service resolves env
   references through the `secrets` module's resolver only, so references provisioned only under the old
   api-projection service must be re-provisioned in the new store at cutover (`station secret set` has no
   counterpart yet in `cmd/station-next`; the module's resolver has `Set`). Whether the module configures the same
   `tether` legacy mapping is not verified.
3. **serve.stdio is refused** by `internal/settings` with a message naming the key until the program-wide stdio
   question is answered. The two live units do not use it.
4. **`bearer_tokens` is refused** with a message naming the identity-key replacement.
5. **Temporary name.** The new service lives in `cmd/station-next` until the gateway and usage-monitor have their own
   binaries; then the old `cmd/station`, `platform/` and `plugins/` roles are removed and the directory is renamed.

## Gateway store plan (ruled 2026-11-12; written and verified below)

- **gateway.db**: one-way copy into the new SQLite file (old file untouched, old unit stopped during the copy).
  Tables and current counts: `gateway_revisions` 10, `gateway_activations` 9, `gateway_usage` 0, plus `gateway_state`.
  `application_id` (1398032215) and `user_version` (1) are replaced by goose versioning; the new file's schema is one
  squashed goose migration. Verify counts, row-level equality of the revision and activation JSON, and
  `PRAGMA integrity_check` before and after.
- **Ledger (Postgres)**: no data moves, no dump, no rehearsal against the live ledger. Proof by tests against a throwaway
  Postgres container: the ported gateway connects with a runtime role that cannot alter schema; it verifies the schema
  version; it refuses to start on a mismatch; `ledger-migrate` works with the migrator role. Plus a test that compares
  the old and the new insert for the same call, column for column.
- **Found while reading the code**: the live gateway config uses `dsn_ref` (an unstructured connection), and the code's
  default for that shape is `schema_mode: auto` (README: "existing dsn_ref and dsn_env configurations retain automatic
  startup migration"). The runtime login of today's unit therefore may migrate the ledger at start. The port keeps the
  ledger code unchanged, so that behaviour is unchanged; moving to the structured connection with `schema_mode: verify`
  and a separate migrator is a hardening choice for the owner, not part of the port. The role test above must run both
  shapes to say which one the live config is. (Live role privileges: ask the driver, read-only.)
- **DSN reference**: today `ledger.postgres.dsn_ref: keychain://station/ledger-dsn` (shape per `api.example.yaml`; the
  live value is the same name per the driver). It moves to the secrets module's reference form (`keychain://` is a
  scheme of the module's resolver; the keychain service name is the module's `secrets.service`, libs default unless
  set): the exact new line is decided when the gateway config key set is written; at cutover the value is re-provisioned
  in the new store by the operator. Not verified: whether the module's default keychain service finds the existing item.
- **Usage-monitor**: module on SQLite, `usage.db` copied one way under a write pause (see the cutover section).

## Findings about the engine (libs mcp-host), not Station's to fix

1. **A child busy inside a call is not reaped.** mcp-host's stop sequence (close session, wait 5 s, SIGTERM, wait 5 s,
   SIGKILL) did not end a child that was inside a 60 s tool call within the 12 s Station allows; the child outlived the
   host process (observed with the real binary and a stand-in child; an idle child exits). Station bounds shutdown
   (`shutdownGrace` 5 s for HTTP, `childStopTimeout` 12 s for children, total at most 17 s), logs the ids still stopping,
   and relies on the supervisor reaping the unit's cgroup (systemd's default `KillMode=control-group`). Operator
   consequence: the unit's stop timeout must exceed 17 s, and the Cerberus descriptor must not use `KillMode=process`.
   Upstream request for libs: kill the child process (or its group) when the stop deadline passes.
2. **No GET event stream**: the Streamable HTTP handler answers POST only (405 on GET), so server-initiated
   notifications over a standing GET are not offered. Same as before the port.
3. **`mcphost.Run` cannot be mounted**: it owns its listener and signal handling; the port composes the engine's public
   parts instead (`bootstrap.BuildRegistry`, `serving.BuildMCPServer`, `serving.MountHTTP`). A `Handlers(registry)` entry
   point in mcp-host would remove the duplication. Not a blocker.
4. **Test isolation**: tests that leave a busy child behind (the in-flight test) must not share a name-based process
   check; leak checks use the exact binary path.

## Test database for the ledger proof

The live ledger reports PostgreSQL 17.11 (driver, read-only). The proof ran against a throwaway `postgres:17` container
(17.11, the Substrate BOM's `ci.postgres_image`) on `127.0.0.1:55432` with trust authentication, removed afterwards.
The tests read `STATION_TEST_PG_DSN` (a superuser URL for that container, for example
`postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable`) and skip without it. Each proof test makes its own
database and role names and drops them.

## Ledger schema mode (from the code; the container run confirms it)

- Default for an unstructured connection (`dsn_ref` / `dsn_env`, which is what the live gateway uses) is
  `schema_mode: auto`; a structured connection defaults to `verify`. The port does not change either default.
- `auto` runs `migrate`: it begins a transaction, takes the advisory lock, runs `CREATE TABLE IF NOT EXISTS
  llm_ledger_schema`, reads the version and applies any pending migrations. A runtime login without CREATE on the
  schema is expected to fail at that `CREATE TABLE IF NOT EXISTS` (Postgres checks the schema privilege before the
  existence test) with `llm-gateway: ledger: schema table: ...`, so startup stops. Recorded here as expected;
  the container run recorded the real message: `llm-gateway: ledger: schema table: ERROR: permission denied for schema
  public (SQLSTATE 42501)`, and the schema versions were unchanged afterwards (`TestPGLiveRoleLayout`).
- `verify` is read-only: it needs the `llm_ledger_schema` rows to be exactly v1..v3 and resolves the full `llm_calls`
  column list with `LIMIT 0`. A mismatch refuses to start and names `ledger-migrate`.
- The code carries three ledger migrations (v1 create, v2 `conversation_id`, v3 `cost_bound`). Whether any is pending
  on the live database depends on its `llm_ledger_schema` rows, which nobody on this side has read.
- Case (a): runtime login can create/alter: `auto` works as today. Case (b), the live shape: runtime login cannot
  create, so production is effectively verify-only and the owner's migrator runs `ledger-migrate` (SET ROLE to the
  owner) before a new binary that needs a newer version.
- `verify` refusals recorded (same test): with v3 missing and with a v4 row present the runtime login gets
  `llm-gateway: ledger: schema does not match binary v3; run llm-gateway ledger-migrate with the matching binary and
  migration identity`. The port carries three migrations: there is no v4 and no schema change.
- Recommendation for the owner: set `schema_mode: verify` explicitly and keep the migrator credential out of the
  serve process, so a role change cannot turn into a startup failure. Not changed by this port.

## Ports, data directories and health (from the tree read)

### Port of every unit

The manifest default is `127.0.0.1:18084`, a port no live service holds; every unit sets its own in its
`SUBSTRATE_CONFIG` file. Another application on the same host holds 8085, so the earlier default was wrong.

| Unit | Port | Same as live? |
|---|---|---|
| avatar-station (cmd/station) | 8097 | yes, the live port |
| design-station (cmd/station) | 8110 | yes |
| llm-gateway (cmd/llm-gateway) | 18082 | yes |
| usage-monitor (cmd/usage-monitor) | 18083 | yes |

A new unit takes its live port only at cutover, when the old unit is stopped. Until then it runs on a throwaway
port from the migration range and a temporary data directory.

### Data directories

- With no `STATION_DATA_DIR` the directory is the XDG data home of the app name: `~/.local/share/station` for
  `cmd/station`. It exists on the host, is empty and was last changed on 2026-09-30. The live Station units keep their
  state elsewhere: `~/.local/state/station/{gateway,usage}` (databases, backups) and `~/.config/station` (unit
  configuration, tokens). So the new MCP-host unit's default directory is not a live one, but the gateway and usage
  databases are, and `llm-gateway` / `usage-monitor` default to `~/.local/share/llm-gateway` and
  `~/.local/share/usage-monitor`, which do not exist. Those units are not live until cutover.
- `validate`, `list`, `--help`, `help`, `completion` and `secret check` now start only the modules that open no file
  (`config`, `logging`, `cli`, plus `secrets` for `secret check`) and prepare no directory. Only `serve` (the default
  command) and `secret set` create anything. `TestReadOnlyCommandsCreateNothing` runs the real binary with an empty
  HOME and no `STATION_DATA_DIR` and asserts nothing appears; `secret set` is the positive control.
- `TestMain` of `cmd/station-next` points HOME and the XDG variables at a scratch root for the whole package and
  unsets `STATION_DATA_DIR`; `TestDefaultDataDirIsNotLive` fails if a run without the variable could resolve outside
  that root. Every test that starts the app still sets an explicit temporary directory. The old `cmd/station`,
  `platform` and `plugins` tests already use `t.TempDir()`; they are replaced by this port.

### Health

`/healthz` is reachable without a key: `httphost` registers it outside the auth guard (`TestAcceptance` asserts an
anonymous 200). The health module does not cache or rate-limit. Each logical server's check (`ListTools`, 3 s
timeout) is therefore cached for 5 seconds, failures included, and concurrent callers share one run
(`internal/host/healthcache.go`, `healthcache_test.go`). A crashed child shows as unhealthy within 5 seconds.

### deploy.yaml

The target id and host alias are unchanged; they are the fleet-deploy inventory, allowlisted in
`scan-public.allow`. Only the two `ExecStart` lines changed, from the home path to `%h`. `systemd-analyze --user
verify` accepted a scratch copy of both units with their drop-ins and `HOME` pointing at a scratch directory holding
the two executables (exit 0). As a control, with the executables removed it reported the `%h`-expanded path as not
executable (exit 1), so the specifier was expanded.

## Known state: lint on the old code

`golangci-lint` v2.11.4 (`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 run`, whole
repository) reports 55 issues: 49 errcheck, 4 staticcheck, 2 unused. All are in `cmd/station`, `platform/` and
`plugins/`, the code the remaining ports replace. A clean export of the base commit (31c6c83) shows the same 55, so they
are existing debt, not from this port, and they are deliberately not fixed in code that is about to be deleted. CI lints
only the new code (`./cmd/station-next/...`, `./cmd/llm-gateway/...`, `./cmd/usage-monitor/...`, `./internal/...`, 0 issues).

## What the last port must do

- Delete `cmd/station`, `platform/` and `plugins/` once their roles live in `cmd/llm-gateway`, `cmd/usage-monitor` and
  the MCP host, and rename `cmd/station-next` to `cmd/station` (and `/station-next` in `.gitignore`).
- Widen the CI lint step from `./cmd/station-next/... ./internal/...` to `./...` (the 55 findings above disappear with
  the old code).
- Delete `internal/llmgateway/ledger_compat_test.go` (it imports `plugins/llm-gateway` and `platform`), `scripts/` mentions
  of `station client`, and the old `api.example.yaml` / `usage-monitor.example.yaml` shapes.
- Flip `relayWired` in `cmd/llm-gateway/serve.go` once the framework's raw-error opt-out is pinned (Gap 11) and move the
  relay routes onto it; update the Gap 11 text and the evidence.
- Rewrite `AGENTS.md` and the README (both still say "not released") and the fleet descriptors for all four units.

## llm-gateway port: what is written and verified (cmd/llm-gateway, internal/llmgateway)

**Shape.** `internal/llmgateway` is the old `plugins/llm-gateway` with no dependency on `platform`: `Start(ctx, host, cfg,
opts)` returns routes and a `Close`; the host supplies the logger, secret resolution and a redaction scope. The relay, policy,
budgets, catalog, ledger, reservations and capture code is carried over unchanged apart from lint fixes and the changes below.
`cmd/llm-gateway` is the service: manifest (`app: llm-gateway`, modules httphost, secrets, auth, database, identity, cli),
default listen address `127.0.0.1:18085` (the live gateway keeps 18082 until cutover), auth `enforce`, a loopback-only bind
refused before anything listens, Host check (`internal/hostguard`), unit data directory from `LLM_GATEWAY_DATA_DIR` (database
`llm-gateway.db`, signing key, secrets). The gateway configuration (providers, models, policy, budgets, ledger, catalog) is
the active revision in the service database, as it was in the old managed mode; `serve` refuses to start without one.

**Changed behaviour (deliberate).**
- Client keys are issued by the identity module (scopes `llm-gateway.*` as before). `/clients` routes, `station client`
  and the management credential scope are gone; a stored legacy `clients` field is kept in old revisions, ignored, and a
  new document naming clients is refused with a message pointing at the identity module.
- Management errors use the framework's flat `{code, message, details}` shape (`details.request_id`).
- `ledger.sqlite.path` is refused: the SQLite ledger lives in the service database.
- The relay routes are **not mounted** (`relayWired = false`, each route is marked `Raw`): see Gap 11. `serve` logs a warning
  with the count. Until the framework has a per-route opt-out and Station pins it, the new binary serves the management API
  and health only, and nothing may be cut over to it.
- Shutdown: `shutdownGrace` 15 s (a stream that outlives it is cut) plus `closeTimeout` 10 s (ledger flush, catalog stop), so
  the unit's stop timeout must exceed 25 s.
- Not ported: Tether-JWT verification from the old `auth.jwt` block (the live config uses keys only), the old middleware
  chain explanation (`station validate` printed it), inprocess transport and tool-level scopes (they belong to the MCP host,
  not the gateway), and `gateway-store import` (it read an `api.yaml`; use `stage` with a document, or `migrate-from`).
- Known default outside the data directory: the catalog's cache directory defaults to `~/.cache/go-modelsdev`
  (a library default, shared with other tools). Tests redirect `HOME`/`XDG_*`; a unit may set `catalog.cache_dir`.

**Old to new, commands.**

| Old | New |
|---|---|
| `station serve -api-only -api-config api.yaml -gateway-db gateway.db` | `llm-gateway serve` with `SUBSTRATE_CONFIG` and `LLM_GATEWAY_DATA_DIR` |
| `station validate -api-only -api-config ... -gateway-db ...` | `llm-gateway validate` (assembles the active revision without listening; needs the existing database) |
| `station gateway-store stage\|show\|status\|history\|activate -db PATH ...` | `llm-gateway gateway-store stage\|show\|status\|history\|activate` (flags `--input --expected --actor --revision --output --before`) |
| `station gateway-store import -api-config` | dropped; `gateway-store migrate-from --old-db COPY` copies a whole old `gateway.db` |
| `station ledger-migrate -dsn-ref ... -owner-role ...` | `llm-gateway ledger-migrate --dsn-ref ... --owner-role ...` (same flags) |
| `station secret set\|check` | `llm-gateway secret set\|check` (stdin only) |
| `station client issue\|revoke\|list` | `llm-gateway identity key create` offline, `/api/identity/keys` online |

**Copying the old gateway database.** `gateway-store migrate-from --old-db COPY` copies an online-backup copy of the old
`gateway.db` into an empty service database in one transaction: revisions verbatim (ids, document text, sha256), the state
row, the activation audit and the usage rows. It refuses a file that is not an old gateway database (application id and
user_version), refuses a destination that already has rows, checks the counts and recomputes every revision checksum before
it commits, and prints the report. A service refuses to open an old `gateway.db` as its own database
(`RefuseLegacyDatabase`, also when the old file's header change is still in its `-wal`). Tests: `legacy_migrate_test.go`,
`cmd/llm-gateway/store_test.go`.

**Postgres ledger proof (container, 17.11).** `TestPGLiveRoleLayout`, `TestLedgerMigrateCommand` and
`TestPGInsertMatchesOldGatewayColumnForColumn` ran against a throwaway Postgres 17.11 with the live role layout (owner
NOLOGIN; migrator LOGIN, NOINHERIT, member of the owner WITH SET; runtime group NOLOGIN; runtime login without CREATE):
- `ledger-migrate` as the migrator login (SET ROLE to the owner) took an empty schema to v1, v2, v3; the objects belong to
  the owner role; a rerun is a no-op; the migrator cannot create objects without SET ROLE. The real binary did the same with
  the migration DSN read from the unit's secrets store.
- The runtime login holds INSERT, SELECT, UPDATE on `llm_calls` and SELECT on `llm_ledger_schema`, and nothing more
  (no DELETE, no CREATE, no ALTER).
- The ported gateway started in `verify` mode as the runtime login, served a request and wrote its `llm_calls` row.
- `verify` refuses with a missing v3 and with a v4 row (message above); `auto` as the runtime login fails with
  `permission denied for schema public` and changes nothing. So in the live shape (case b) the unit only works in verify mode,
  and the migrator runs `ledger-migrate` before a binary that needs a newer version. Not verified: which of the two the
  live config is; that needs the live role privileges (read-only, via the driver).
- No v4: the port carries the three migrations of the old code. `store_pg.go` differs from the old file in three lines:
  two operator messages (they name `llm-gateway ledger-migrate`) and a lint fix on a `defer`.
- Old versus new insert: the same OpenAI and Anthropic requests through the old plugin and through the port wrote the same
  `llm_calls` values in the 18 columns compared (all but id, recorded_at, request_id, latency_ms); the test fails when one
  value is changed (checked).

**Store concurrency.** The management store and the SQLite ledger use `Reader()` for reads and `WriteTx` for writes (callbacks
only read, decide and write, so a restart after contention is safe); no statement runs inside an open cursor.
`TestSQLiteLedgerOneReadConnection` runs four writers and four readers with `ReadConns: 1`; the package's `go test -race`
passed six runs in a row.

**What is verified, by test.** The real binary: offline `identity setup` and `identity key create`, `gateway-store stage` and
`activate`, `validate`, `serve` on a loopback port, a read (`/status`: running revision 1) and a write (`PUT /config` with the
precondition stages revision 2, read back through `/models?revision=2`) through the management API; refused: no key (401),
an unknown key (401), a key without the scope (403 `scope_required`), a foreign Host (403), the unmounted relay route and the
removed clients route (403 `route_not_mapped`); SIGTERM exits 0; no credential in the log. Binding to 0.0.0.0, `[::]`,
`:0`, a documentation address and a tailnet address is refused before listening (the check was removed once and the
test failed). The read-only commands create nothing under an empty HOME (positive control: `secret set`).

## usage-monitor port: what is written and verified (cmd/usage-monitor, internal/usagemonitor)

**Shape.** `internal/usagemonitor` is the old `plugins/usage-monitor` on `database.From(app)` (libs core/store, one write
connection): reads go through `Reader()`, writes (`PutAccount`, `Ingest`) through `WriteTx`, so callbacks are re-runnable.
`Start(ctx, host, cfg, opts)` returns routes; `Service.Run(ctx)` is the collector loop. `cmd/usage-monitor` is its own unit:
default `127.0.0.1:18086` (live unit: 18083), database `usage-monitor.db`, data dir from `USAGE_MONITOR_DATA_DIR`.

**Old to new.**

| Old | New |
|---|---|
| `station serve` with the usage-monitor plugin | `usage-monitor serve` |
| `database:` in the usage config | removed (the module owns the database) |
| bearer client registry | identity API keys: `usage-monitor.read`, `.ingest`, `.admin` scopes per route |
| push source `principal: <client id>` | `principal: key:<id>` (the key id comes from `GET /api/identity/keys`) |
| nested/OpenAI-style errors | flat `{code, message}` (`apierr`) |
| old `usage.db` in place | `usage-monitor migrate-from --old-db <copy>`; the service refuses an old-format file |

**Verified (real binary, throwaway data dir, `-race`).** Setup and keys, serve, ingest as the bound key (200), a second
ingest-scoped key refused `source_forbidden`, stale account revision 409, 401/403/foreign-Host refusals, unmapped route 403,
SIGTERM exit 0, no credential in the log. `migrate-from` carries accounts, audit, observations and events with sequences
kept, in one transaction, verifying a per-table digest and `integrity_check` against the old file; a second run and a
missing source are refused. `internal/usagemonitor` passes `-race -count=6`, including collectors writing while the API
reads on one read connection.

**Cutover (write pause).** Stop the old unit, `migrate-from` a copy, check counts, start the new unit. Counts seen: accounts
11, observations about 84k, events 67. Rollback loses what the new unit recorded after start. Shutdown is a 10 s drain plus
10 s for collectors, so the service manager's stop timeout must be above 20 s. Re-provision: reporter keys (push sources are
bound to `key:<id>`), the gateway collector's credential_ref and gateway URL, and the operator and reader keys
(`scripts/first-run usage-monitor`).

## Pin d453ac8: what Station adopts and what it does not

- **Adopted:** (1) offline `identity setup` and `identity key create`, in the write set of `plan()`; (2) offline setup
  default, with tests and the first-run script; (5) `substrate scan-public --tree` in CI, copy deleted.
- **(3) Local trust for stdio MCP: not applicable.** `mcp.StdioOptions{LocalTrust, LocalScopes}` belongs to Substrate's
  own `mcp` module. Station does not use that module: its MCP serving is the libs mcp-host engine mounted over HTTP, and
  `serve.stdio` of a logical server is refused. Serving a logical server over stdio would be code in the engine
  wrapper, with its own guard; it is not needed by any live unit, so nothing is built and nothing is invented.
- **(4) `httphost.Stopping`: not adopted, on purpose.** Station has no long-lived handler of its own (no SSE,
  WebSocket or long poll; the libs handler is POST-only). An in-flight tool call is a bounded request that the 5 s grace
  period lets finish; cancelling it the moment a stop begins would abort short calls on every restart. The busy-child
  orphan finding is unchanged by this pin.
- **(6) Queue and `database.sqlite.read_pool` options:** no workaround to remove; not used.
- **Not in this pin** and not needed: the Host check in `httphost` (the `internal/hostguard` copy stays), the
  cross-process open-and-migrate lock, `queue.reservation_secs`, the MCP JSON-response option.

## First-run script

`scripts/first-run UNIT` sets one unit up offline, for `avatar-station`, `design-station`, `llm-gateway` or
`usage-monitor`: the owner (`identity setup`, password from the first line of a 0600 file through stdin) and one new
0600 key file per client (`identity key create --out`, never overwritten), with the scopes in the client table above.
It reads everything from the environment (`FIRST_RUN_BIN`, `SUBSTRATE_CONFIG`, `FIRST_RUN_DATA_DIR`,
`FIRST_RUN_PASSWORD_FILE`, `FIRST_RUN_KEYS_DIR`), requires an explicit absolute data directory (it sets the variable the
binary reads, so a forgotten one cannot reach the live directory), refuses a loose-mode password file or keys
directory, and a second run changes nothing (setup refuses when an owner exists).
`TestFirstRunScript` runs it with the real binary for avatar-station and design-station against throwaway data
directories and also covers the refusals. The llm-gateway and usage-monitor entries are in the script and get the same
test when those binaries exist. At cutover the operator runs it once per unit before the unit serves on its live port,
then hands each key file to its client's owner.

## Management `/clients` routes: callers and what replaces them

The old gateway stored hashed client keys inside its configuration revisions and managed them with
`/api/llm-gateway/v1/management/clients` (scope `llm-gateway.management.credentials`). The owner decision is that the
identity module issues keys, so these routes, the `clients` list inside a revision document and the
`station client issue|revoke|list` command are not ported.

| Caller found in this repository | What it does | Replacement |
|---|---|---|
| `station client issue\|revoke\|list` (`cmd/station/client.go`, offline, SQLite or keys file) | issue, revoke, list hashed gateway keys | `llm-gateway identity key create` (offline); list and revoke through `GET /api/identity/keys` and `DELETE /api/identity/keys/{id}` with an owner session |
| `scripts/gateway-management-smoke.py` (`GET /clients`) | checks the list shows only id and scopes | the identity list route; the smoke script drops that check |
| `docs/llm-gateway-management.md` route table | documents the five routes | rewritten for the identity routes |
| `GET /clients` | list `{id, scopes}` | `GET /api/identity/keys` |
| `POST /clients` | issue, token shown once | `POST /api/identity/keys` or the offline command |
| `PUT /clients` (change scopes, keep the token) | edit scopes in place | no in-place edit in identity: create a new key with the new scopes, hand it over, revoke the old one |
| `POST /clients/rotate` | new token, same scopes | create a new key with the same scopes, revoke the old one |
| `DELETE /clients?id=` | stage a revocation | `DELETE /api/identity/keys/{id}` |

Two things the replacement does not cover, for the owner: identity has no offline list or revoke command (revoking
needs an owner session over HTTP), and a revision no longer records who held which key, so the audit of key changes
moves from the revision history to identity's own records. Live callers of these routes are not known from the
repository (the `station-operator` client holds `llm-gateway.*` and could call them); the driver is asked who does.

## Live facts received from the driver (2026-11-12)

- **Ledger.** The live runtime login cannot create or alter anything in the ledger database; schema changes need the
  separate migrator login; versions 1 to 3 are applied. The live case is therefore verify-only for the runtime login (the
  proof's steps 3 and 5). There is no version 4, so the cutover does not need the migrator's credential (nobody has found
  where its reference was recorded). The legacy login named `station` has no privileges on the ledger tables and is removed
  after cutover.
- **Callers.** The live gateway listens on 127.0.0.1:18082, auth mode enforce with a `keys_file` and no `jwt` block, so every
  caller presents a key from that file; no Tether-issued JWT. `auth.jwt` is not ported. The keys file is one YAML list of
  `{id, sha256, scopes}`; each entry becomes a new identity API key with `llm-gateway.*` scopes in the re-provisioning list
  (names and holders pending from the driver).
- **Relay gate.** The relay routes stay unreachable in every build (`relayWired` false) until a test proves an upstream
  OpenAI error arrives byte for byte and a framework refusal arrives flat.

## Rehearsal on the live online-backup copies (2026-11-12, scratch directories, copies of the copies)

Method: each run takes a fresh `.backup` of the driver's copy, an empty data directory, then `migrate-from`. Content is
proved by the command's in-transaction digest and `integrity_check`, then independently by SQL `EXCEPT` in both directions
on every table (column lists from the old schema) and by comparing `sqlite_sequence`.

| Unit | Rows copied | EXCEPT old-new / new-old | Sequences | Pause (3 runs, wall time of `migrate-from`) |
|---|---|---|---|---|
| usage-monitor (47,456,256 B) | accounts 11, account_audit 24, observations 84,697, events 67 | 0 / 0 on all four tables | audit 24, observations 84,697, events 67, equal | 2.40 s, 2.57 s, 2.44 s |
| llm-gateway (131,072 B) | revisions 10, state 1, activations 9, usage 0; latest and active revision 10 | 0 / 0 on all four tables | revisions 10, activations 9, equal | 0.06 s, 0.06 s, 0.04 s |

The pause for a cutover is the migration time plus the old unit's stop and the new unit's start; the migration itself is
under three seconds on the live data. The driver's copies were only read; the scratch copies were deleted afterwards.

## Who is meant to hold each client key (from the code and docs; the driver confirms with the owner)

| Old client (live) | Meant holder (from code) | New key, issued by | Scopes |
|---|---|---|---|
| `station-operator` (gateway) | the owner by hand: management (`gateway-store`, management API), and the live relay clients named in `docs/llm-gateway-adoption.md` | llm-gateway | `llm-gateway.*` |
| `station-smoke` (gateway) | `scripts/gateway-smoke.py` | llm-gateway | `chat`, `models.read`, `embed`, `decide` |
| `usage-operator` (usage) | the owner by hand (account edits, manual collect) | usage-monitor | `read`, `manage`, `collect` |
| `usage-reader` (usage) | dashboards and read-only callers | usage-monitor | `read` |
| new: `usage-collector` | the usage monitor's own `gateway` collector, via `credential_ref` | llm-gateway | `llm-gateway.usage.read` |

**The gateway collector.** The collector calls `GET <gateway_url>/v1/usage/export`, which needs `llm-gateway.usage.read`. The live
gateway holds only `station-operator` (`llm-gateway.*`) and `station-smoke` (no usage scope), so if a gateway collector is
configured live it presents the operator's token, or it is not configured. The driver reads the live usage config structure
(source ids and collectors, no values) to say which. In the new design it holds its own key `usage-collector` with the one
scope it needs, issued by the gateway and stored in a file or keychain reference named by `credential_ref`. An earlier draft of
`scripts/first-run` created a `gateway-usage-reader` key on the usage unit; that was wrong (the usage unit's keys do not
authorise calls to the gateway) and is replaced by `usage-collector` on the gateway unit.

**Push reporters** (`principal`) are not in the live keys file; each needs a key on the usage unit with `usage-monitor.ingest`,
and its source `principal` becomes `key:<id>`.

## Stop timeouts for the service manager (recorded here, written into the deploy descriptors' drop-ins)

| Unit | Drain | Close | `TimeoutStopSec` must exceed |
|---|---|---|---|
| llm-gateway | 15 s | 10 s | 25 s (the driver asks for the drop-in in the descriptor) |
| usage-monitor | 10 s | 10 s collectors | 20 s |
| MCP host (avatar, design) | engine stop | busy child may outlive it | above 17 s; no `KillMode=process` (Cerberus CW-20261112-0057) |

## Public name, proxy and the keychain (from the live usage config structure, driver 2026-11-12)

**Live gateway collectors.** The live usage monitor has 12 sources (11 accounts): four direct (claude, codex, agy, opencode-go with
a keychain reference) and eight `gateway` collectors (opencode-go, opencode-go-claude, openai-work, anthropic-work,
gemini-work, openrouter, huggingface, jev), each with a file credential reference and `gateway_url` on the gateway's public name.
They present the operator's token today. At cutover each source's `credential_ref` points at the new `usage-collector` key
(one key may serve all eight; the driver says whether the eight references name one file or several).

**Who arrives by the public name.** From `docs/llm-gateway-adoption.md`: every relay client (OpenAI and Anthropic SDKs, the JEV
decision client, the usage monitor's collectors) uses `https://<public name>/api/llm-gateway`; Caddy terminates TLS on the tailnet
address and forwards to the loopback listener. Only the host-local diagnostic root is plain loopback
(`http://127.0.0.1:18082/api/llm-gateway/v1`). The usage monitor itself is documented only as loopback 18083 (health and keys
file); no document gives it a public name. If the driver finds one in the proxy config, the usage unit needs the same two
settings.

**What the ported gateway needs.** `llm-gateway.allowed_hosts: [<public name>]` and `identity.trusted_proxy: true` in the unit's
override file. `TestServedThroughTheTLSProxyName` runs the real binary and proves: a listed Host with a loopback peer and exactly
one `X-Forwarded-Proto: https` is served (200); the same Host with no header, `http`, or two values is refused (403); an unlisted
Host is refused; a key without the scope is refused; a loopback Host still works. Mutating the allowed list makes the first case
fail. The usage-collector request (`GET /v1/usage/export` with `llm-gateway.usage.read`) rides the same path but is a relay
route, so its test is added with the relay wiring (the proxied-name check, then the 200 with the collector key).

**keychain:// on Linux.** The usage monitor resolves references through the secrets module's resolver, which is libs
security/secrets over go-keyring: on Linux it talks to the Secret Service (gnome-keyring, KeePassXC) over D-Bus, so `keychain://`
is supported when the user session's bus is reachable (the unit's `DBUS_SESSION_BUS_ADDRESS`/`XDG_RUNTIME_DIR` must be set; a
user unit has them). Entry location: service `hollis-labs`, account `<namespace>/<key>` (the module's `secrets.service` can
change the service). **Difference from the old build:** the old platform added a read-through to the legacy service `tether`,
account `provider-api-key:<namespace>/<key>`; the Substrate secrets module has no option for that. A value that exists only in
the legacy location (a value stored by an older Tether tool) will not resolve in the new unit. The cutover step: for each keychain
reference in use (`keychain://station/opencode-go` for usage, `keychain://station/ledger-dsn` for the gateway), the driver
checks where the entry lives (attributes only, not values); if it is under `tether`, the owner re-enters it with
`<unit> secret set <ref>` (stdin, never printed) or it moves to a `file://` reference. A positive-control check, `<unit> secret
check <ref>`, confirms resolution before the old unit is stopped. Framework ask: `secrets.legacy` config for the read-through.
