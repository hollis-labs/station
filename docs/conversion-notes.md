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
   name; no `scripts/scan-public` in the repo. Findings only, nothing changed.
10. **Postgres ledger** is a container database published on loopback and a tailnet address, with a Cerberus descriptor
    and a compose file beside the gateway config (not in this repo). Cutover must preserve it untouched.

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
`scripts/scan-public` is not in this repo; it lives in the Substrate repo (`scripts/scan-public`).
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

1. **Headless first-run setup.** Each unit starts with `GET /api/setup` -> `setup_required: true` and `POST /api/setup`
   creates the owner; keys are then issued with `POST /api/identity/keys` and shown once. At cutover that is four
   units: avatar-station, design-station, llm-gateway, usage-monitor. Steps per unit: start it on loopback only,
   run setup from the host (a command that reads the password from a file, not an argument), issue the keys for that
   unit's clients, write each key to a 0600 file named for the client, hand the files to the client's owner (never
   by mail or in a prompt), then open the unit to its clients. Doing this by hand over HTTP four times is error-prone
   and leaves the setup endpoint open between start and the first call; an offline bootstrap command for the identity
   module (create owner and first admin key without HTTP) is a framework candidate and Station is a strong case.
   Not invented here.
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

## Gateway store plan (ruled 2026-11-12; no store code written yet)

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

The live ledger reports PostgreSQL 17.11 (driver, read-only). The throwaway database for the ledger tests uses
`postgres:17` (the Substrate BOM's `ci.postgres_image`). Whether a container runtime is available on the worker is
not yet checked; the existing llm-gateway tests read `STATION_TEST_PG_DSN` and skip without it.

## Ledger schema mode (from the code; the container run confirms it)

- Default for an unstructured connection (`dsn_ref` / `dsn_env`, which is what the live gateway uses) is
  `schema_mode: auto`; a structured connection defaults to `verify`. The port does not change either default.
- `auto` runs `migrate`: it begins a transaction, takes the advisory lock, runs `CREATE TABLE IF NOT EXISTS
  llm_ledger_schema`, reads the version and applies any pending migrations. A runtime login without CREATE on the
  schema is expected to fail at that `CREATE TABLE IF NOT EXISTS` (Postgres checks the schema privilege before the
  existence test) with `llm-gateway: ledger: schema table: ...`, so startup stops. Recorded here as expected;
  the Postgres 17 container test records the real message.
- `verify` is read-only: it needs the `llm_ledger_schema` rows to be exactly v1..v3 and resolves the full `llm_calls`
  column list with `LIMIT 0`. A mismatch refuses to start and names `ledger-migrate`.
- The code carries three ledger migrations (v1 create, v2 `conversation_id`, v3 `cost_bound`). Whether any is pending
  on the live database depends on its `llm_ledger_schema` rows, which nobody on this side has read.
- Case (a): runtime login can create/alter: `auto` works as today. Case (b), the live shape: runtime login cannot
  create, so production is effectively verify-only and the owner's migrator runs `ledger-migrate` (SET ROLE to the
  owner) before a new binary that needs a newer version.
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
only the new code (`./cmd/station-next/...`, `./internal/...`, 0 issues).

## What the last port must do

- Delete `cmd/station`, `platform/` and `plugins/` once their roles live in `cmd/llm-gateway`, `cmd/usage-monitor` and
  the MCP host, and rename `cmd/station-next` to `cmd/station` (and `/station-next` in `.gitignore`).
- Widen the CI lint step from `./cmd/station-next/... ./internal/...` to `./...` (the 55 findings above disappear with
  the old code).
- Rewrite `AGENTS.md` and the README (both still say "not released") and the fleet descriptors for all four units.
