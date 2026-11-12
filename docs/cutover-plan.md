# Cutover plan: the live Station units move to the Substrate build

Status: **written and rehearsed on copies of the live databases; not run.** Nothing here touches a live unit until the owner
approves the cutover (section 13). The rehearsal record is section 10; what it could not cover is listed there.

Related: `docs/conversion-notes.md` (what changed and why, the gaps, the old-to-new tables), `docs/architecture.md`,
`deploy/*.yaml` (the installer descriptors), `.github/workflows/build.yml` (three builds, one artifact per binary).

## 0. The ledger (answered first)

**The PostgreSQL ledger is kept exactly as it is: not migrated, not moved to SQLite.** The new gateway reads the same database with the
same login, in `verify` mode, and the code carries the same three ledger schema versions (v1 to v3) as the old build, so the cutover
applies no ledger migration and changes no ledger object. What the gateway reads is the **active revision's** ledger block, which
`gateway-store migrate-from` copies verbatim: on the 2026-11-12 copy it is `store: postgres`, a structured connection (host, port, database,
runtime login `..._r2`), `schema_mode: verify` and `password_ref: file://...` (the issued credential file, which resolves unchanged; no keychain
entry is involved). The old `api.yaml` may name a different reference (a keychain DSN); it is not read by the new build, and the owner should
confirm which of the two the old unit really used before the window (`llm-gateway validate` against the migrated revision answers it, since it opens the ledger).

**Rollback for the ledger is nothing to undo.** Rows the new gateway wrote are valid for the old build (the same columns, proved by
`TestPGInsertMatchesOldGatewayColumnForColumn`, 18 columns compared on the #28 head); the old unit simply reads and appends to the same
tables. A `pg_dump -Fc` is taken in step zero as a safety copy, not because the plan needs a restore. What is lost on rollback is in section 11.

## 1. What changes for the owner

Read this first. Everything else is how it is done.

**You type four passwords, one per unit.** Each unit is its own identity issuer, so each needs its own owner, created offline with
`<unit> identity setup` at a terminal (usage-monitor, llm-gateway, avatar-station, design-station). The order that lets you do all four in
one sitting: before the window, prepare everything else; then at the terminal run the four `identity setup` commands back to back (each
prompts twice, nothing is echoed), then the key creation and installs follow in the window order of section 6 without you. A unit's
owner can be created while its old unit still runs (it only creates the new data directory), so the four passwords can be typed before the
first stop and the window itself needs no password. Use the same passphrase manager entry per unit; none is ever passed as an argument.

**Station is three binaries now, and four things you could do before are gone, on purpose.**

1. **Shared bearer tokens and client-key files.** The gateway's `clients.yaml`/`keys_file`, the usage monitor's keys file, the
   client hashes inside `gateway.db` and the MCP host's per-server `bearer_tokens` are removed. Each unit is its own identity
   provider; every caller holds an **identity API key** with scopes (`llm-gateway.chat`, `usage-monitor.read`, `station.avatar`, ...).
   Every old token stops working at cutover; every caller gets a new key (section 12).
2. **`station client ...` and the `/clients` management routes.** Keys are issued with `<unit> identity key create` offline, or by
   a signed-in administrator over the API (`/api/identity/keys`).
3. **Tether-issued JWTs on the gateway.** `auth.jwt` is not ported. The live gateway had no `jwt` block, so no live caller is affected.
4. **Open design-station mounts.** `design-suite` and `tripo` had empty `bearer_tokens`: anyone on loopback could call them. After the
   port each caller needs a key with `station.design-suite` or `station.tripo`. The Tether gateway's `design_*` and `tripo_*`
   entries are the callers; they are updated at cutover (section 12).

**Also different:**
- One command per binary replaces `station serve -api-only -api-config ...`: `llm-gateway serve`, `usage-monitor serve`,
  `station serve`. Flags are gone; each unit reads one override file (`SUBSTRATE_CONFIG`) and one data directory.
- Management and usage API errors are the flat `{code, message}`; the relay's errors are the provider's own, untouched.
- Usage push sources are bound to `key:<id>` principals (the key id from `GET /api/identity/keys`), not a client name.
- Ports are unchanged: 8097 (avatar), 8110 (design), 18082 (gateway), 18083 (usage). All are loopback only.
- The data is copied, not shared: gateway revisions and usage history are copied into new databases by `migrate-from`; the old
  files are never written (rollback keeps them).
- The two MCP-host binaries are plain files today; the installer keeps each as `<name>.pre-fleet-install`.
- Shutdown takes longer: the service manager's stop timeout is 40 s (gateway), 35 s (usage), 30 s (MCP hosts); no `KillMode=process`.
- The credential store: six gateway provider keys are copied from the legacy keyring location (section 6, step 7).

## 2. What runs today (read from the host by the driver, names only)

| Unit (Cerberus user unit) | Port | Command today | Data and config today |
|---|---|---|---|
| `station-usage-monitor` | 18083 | old `station serve -api-only -api-config <usage api.yaml>` | `usage.db` (47 MB: 11 accounts, about 84,700 observations, 67 events, 24 audit rows) under the old state directory; keys file mode 0600 |
| `station-llm-gateway` | 18082 | old `station serve -api-only -api-config <gateway api.yaml> -gateway-db <gateway.db>` | `gateway.db` (10 revisions, 9 activations, usage 0); keys file mode 0600; ledger on the dedicated PostgreSQL 17.11 container |
| `avatar-station` | 8097 | old `station serve -config <avatar-station.yaml> -http-addr 127.0.0.1:8097` | no database; one logical server `avatar` with `bearer_tokens`; its settings in an `env:` block |
| `design-station` | 8110 | the other (5 November) build, `serve -config <design-station.yaml> -http-addr 127.0.0.1:8110` | no database; logical servers `design-suite` and `tripo`, empty `bearer_tokens` |

Besides the installer's `20-fleet-exec.conf`, the gateway and usage units carry `10-neutral-cwd.conf` (WorkingDirectory in the old state
directory) and `30-auth-posture.conf` (`HOLLIS_AUTH_STATION=permissive`). Both are harmless to the new build (proved:
conversion-notes, "Two live drop-ins") and stay in place as part of the rollback. A reverse proxy forwards the gateway's and the usage
monitor's public names to the loopback ports. The ledger runtime login cannot create or alter anything; schema changes need the separate
migrator login; versions 1 to 3 are applied (verify-only). A legacy login `station` exists with no privileges on the ledger tables.

## 3. Principles

- **The old files are never written.** `migrate-from` opens its source read-only on a copy taken with the old unit stopped. Rollback
  is: stop the new unit, remove its drop-in, start the old build on the old files (section 11).
- **New data directories, new configuration files, new link paths.** `~/.local/share/{llm-gateway,usage-monitor,avatar-station,design-station}`,
  `~/.config/station/{gateway,usage}/substrate.yaml` and `~/.config/station/{avatar,design}-station.substrate.yaml`, and
  `~/.local/libexec/station-gateway/{llm-gateway,usage-monitor}`. The old links, old `20-` drop-ins and the old release stay; the new
  drop-ins are `30-*-exec.conf` and sort after the old one. The two MCP-host units have no drop-in directory today, so the `station` descriptor's own `20-fleet-exec.conf` is new for them (dry run 2026-11-12); their rollback is removing it and restoring `<name>.pre-fleet-install`. The installer targets are the two in `deploy/station.yaml` (avatar and design); `--target` takes the id written there.
- **No open setup window.** `POST /api/setup` is refused (403); the owner and keys are created offline **before** a unit starts. The
  first check after start is that `GET /api/setup` answers `{"setup_required":false}`.
- **Never print a secret.** Keys go to mode-0600 files with `--out`; passwords are typed at a prompt; the keyring copy is a pipe.
- **The installer comes last.** `fleet-install deploy` restarts the unit, and `serve` refuses a missing database, so the restart is
  only safe after the migration. The installer's preflight then backs up the **migrated** database.
- **No `station` deploy before the gateway and usage cutovers are accepted.** The `station` release directory is shared by the two MCP
  hosts and keeps three versions.
- **Order:** usage-monitor, then the gateway (it has live clients), then avatar-station, then design-station.

## 4. Step zero: before anything else, with nothing changed

Only these commands create anything: `identity setup`, `identity key create`, `migrate-from` and `gateway-store stage|activate|migrate-from`
(the data directory, the database and the signing key), and `secret set` (the directory only). `serve`, `validate`, the `gateway-store`
reads, `--help`, a bare command and **any line that is not exactly one of the commands above with the flags it needs** create nothing and
start no module that opens a file; `serve` and `validate` also refuse a data directory with no database before anything runs. The tests
prove this with the real binaries over an empty home (`TestReadOnlyCommandsCreateNothing`). Even so, always export the data directory
variable first: the commands that do create resolve the live directory of the app name without it.

1. List the data homes as the service's user, changing nothing: `find ~/.local/share/{llm-gateway,usage-monitor,avatar-station,design-station} -ls`.
   None of the four may exist. If one does, stop and report: something has already booted the new build there.
2. **Backups proven complete before anything is stopped.** The driver takes an online-backup copy (SQLite `.backup`) of `usage.db` and `gateway.db`
   into a private directory (mode 0600) while the old units run; `sqlite3 <copy> 'pragma integrity_check'` prints `ok`; row counts match the
   figures in section 2 (or the day's figures). For the ledger, a `pg_dump -Fc` of the ledger database is taken and `pg_restore --list`
   reads it. Record `sha256sum` of every backup.
3. Copy aside, once, the two MCP-host binaries and the three old configuration files (`api.yaml` of each service, the two MCP-host YAML
   files) and the old keys files; they are the rollback.
4. Confirm none of `~/.config/station/gateway/substrate.yaml`, `usage/substrate.yaml`, `avatar-station.substrate.yaml`, `design-station.substrate.yaml` exists yet.

## 5. Before the window (anyone, no downtime)

1. Merge the pull request (CI green on `main`). Record the commit and the run ids of the three builds (`station`, `llm-gateway`,
   `usage-monitor`); each is the `--run` of its deploy.
2. Rehearse on a fresh copy in a scratch directory with `HOME`, `XDG_*` and every `*_DATA_DIR` pointed there (section 10).
   Counts match; the copy's checksum unchanged.
3. Prepare, without applying: the four override files (section 6, step 3), the proxy edit (section 9), the Cerberus descriptor edit
   (section 8). Tell the callers of section 12 which window to expect.
4. **Dry runs of the installer**, by the driver, against the published artifacts: `fleet-install deploy <app> --repo hollis-labs/station --run <id> --dry-run`
   (for `station`, `--target` names avatar-station or design-station: two targets match the host). Each must print the checksum check,
   the new link, the `30-` drop-in, the restart and the `/healthz` poll, and skip the preflight while the migrated database does not exist.
   Passed on 2026-11-12 against earlier artifacts of this branch (conversion-notes); repeat on the published ones.
5. Read section 12 against what is found on the host: every caller gets a key, or is retired.

## 6. The window (host-run, in this order, per unit)

Every command runs as the service's user with its `HOME`. New binaries are extracted from the artifacts into a scratch directory
(`$NEWBIN`); they are **not installed yet**. For each unit export its own pair first, so every host-run command places the database,
signing key and secrets exactly as the service will:

```sh
export SUBSTRATE_CONFIG=$HOME/.config/station/usage/substrate.yaml     # the unit's override file
export USAGE_MONITOR_DATA_DIR=$HOME/.local/share/usage-monitor         # LLM_GATEWAY_DATA_DIR / STATION_DATA_DIR for the others
```

**usage-monitor**

1. **Stop** the unit through Cerberus (`service-stop`), not by killing the process; confirm no process remains. This begins the write
   pause: nothing is collected or ingested until step 6.
2. **Fresh backup** of `usage.db` with the online backup (the old unit is stopped; step zero 2 proved the procedure), `integrity_check` ok,
   sha256 recorded.
3. **Config**: write the override file (mode 0600) from `examples/usage-monitor.yaml`: `httphost.addr` `127.0.0.1:18083`,
   `identity.trusted_proxy: true`, `usage-monitor.allowed_hosts: [<public name>]`, and the accounts/sources from the old `api.yaml`
   (the `database:` key is gone). Run `usage-monitor config` and read the effective values.
4. **`migrate-from`**: `$NEWBIN/usage-monitor migrate-from --old-db <the backup copy>`. Expect `accounts 11`, `account_audit 24`,
   `observations ~84,700`, `events 67`, matching digests, `integrity_check ok`, exit 0 (rehearsed: 2.4 to 2.6 s). A second run is refused.
5. **Identity, offline**: the owner runs `$NEWBIN/usage-monitor identity setup --username <owner>` **himself at a terminal**; then
   `identity key create --name usage-operator --scope usage-monitor.read --scope usage-monitor.manage --scope usage-monitor.collect --out <file>` and
   `usage-reader` (`usage-monitor.read`), plus one key per push reporter with `usage-monitor.ingest`. Set each reporter's source
   `principal: key:<id>` (ids from `GET /api/identity/keys`) and restart for it to take effect.
6. **Install, last**: `fleet-install deploy usage-monitor --repo hollis-labs/station --run <id>`. Its preflight backs up the migrated
   `usage-monitor.db`; it links `~/.local/libexec/station-gateway/usage-monitor`, writes `30-usage-monitor-exec.conf` and restarts the unit.
   The pause ends when `/healthz` answers.

**llm-gateway** (the same shape; the pause is the migration, under a second, plus stop and start)

1. Stop the unit. Fresh `gateway.db` backup; `integrity_check` ok.
2. Override file from `examples/llm-gateway.yaml`: `httphost.addr` `127.0.0.1:18082`, `identity.trusted_proxy: true`,
   `llm-gateway.allowed_hosts: [<public name>]`.
3. `llm-gateway gateway-store migrate-from --old-db <the backup copy>`: expect revisions 10, state 1, activations 9, usage 0, latest and
   active revision 10 (rehearsed 0.04 to 0.06 s). The active revision is the live configuration, carried verbatim.
4. **Keyring**: `scripts/copy-legacy-secrets llm-gateway keychain://openai/work keychain://anthropic/work keychain://gemini/work
   keychain://openrouter/work keychain://huggingface/work keychain://typesafe/jev` (with `COPY_BIN`, `SUBSTRATE_CONFIG`, `COPY_DATA_DIR`
   set). Each value moves through a pipe and is verified by length; the legacy entries stay. Nothing else is copied: `opencode/zen`,
   the design-suite keys and the ledger's `file://` password reference already resolve. Run by the driver; never with `COPY_OVERWRITE`.
5. `llm-gateway validate` (resolves the references and opens the ledger in verify mode as the runtime login; it does not listen).
6. Identity, offline: the owner runs `identity setup` himself; keys `station-operator` (`llm-gateway.*`), `station-smoke`
   (`chat`, `models.read`, `embed`, `decide`), `usage-collector` (`llm-gateway.usage.read` only) and one per relay client (section 12).
   Write the usage-collector key to the file the usage monitor's eight gateway sources name as `credential_ref`.
7. **Install, last**: `fleet-install deploy llm-gateway ...` (link `~/.local/libexec/station-gateway/llm-gateway`, `30-llm-gateway-exec.conf`,
   `TimeoutStopSec=40`). Read the effective `ExecStart` (`systemctl --user cat`) before trusting the unit.

**avatar-station, design-station** (no database; one unit at a time)

1. Stop the unit. Convert its YAML to the `station:` block of a new override file (`examples/station.yaml`): `logical_servers` keep their
   schema; `bearer_tokens`, `allowed_origins` and the listen flag are removed; the avatar `env:` block is unchanged except that
   secrets are references. `station validate` prints the logical servers without spawning anything.
2. Identity, offline: the owner runs `identity setup`; keys `tether-avatar` (`station.avatar`) or `tether-design` (`station.design-suite`,
   `station.tripo`).
3. Install, last: `fleet-install deploy station --target <avatar-station or design-station target> ...`; the plain file at the old path is kept as
   `<name>.pre-fleet-install`. `WorkingDirectory` stays as the Cerberus unit has it.
4. Update the Tether gateway entry of the tools (section 12) to present the new key, then check one tool call end to end.

## 7. Checks right after each start

0. The migration report (`migrate-from` output) carries `source_sha256` and `source_unchanged: true`: the old file is only ever read as
   bytes (a private copy is what SQLite opens), so its sha256 and its directory listing are what they were. Record the report.

1. `GET /healthz` is 200 on the unit's port.
2. `GET /api/setup` answers `{"setup_required":false}` and `POST /api/setup` answers 403. If setup is required, stop the unit now.
3. No credential: 401. A key without the scope: 403. A `Host` that is not loopback or listed: 403. An old token as a bearer: 401.
4. `GET /api/me` with a key reports `"mode":"enforce"`.
5. Through the public name via the proxy: gateway `GET /api/llm-gateway/v1/models` with an authorised key lists models and is 401 without one;
   the usage monitor's `GET /api/usage-monitor/v1/accounts` lists the 11 accounts; a request to the public name with a different `Host` is 403.
6. Gateway only: one chat request through each live dialect reaches its provider; an upstream error body arrives as the provider sent it;
   a row appears in the PostgreSQL ledger; `GET /api/llm-gateway/v1/management/status` shows `running_revision` 10 and `restart_required: false`.
7. Usage only: `observations` count not lower than at migration; the next gateway collector pull records an observation; a push reporter's
   ingest succeeds with its own key and is refused (`source_forbidden`) with another.
8. MCP hosts: each logical server's `/healthz` check is up; a tool call through the Tether gateway with the new key succeeds, and without a key is 401.
9. A SIGTERM (a stop through Cerberus) exits within the drain; the journal has no panic and no credential.

## 8. The Cerberus descriptors

The units are regenerated from Cerberus's registry by an apply. The installer's drop-ins reset `ExecStart` and win, but the registry is the
record. In the same window, and by whoever owns it:

| Item | Today | After |
|---|---|---|
| command | `station serve -api-only -api-config ... [-gateway-db ...]` | `llm-gateway serve`, `usage-monitor serve`, `station serve` (settings from `SUBSTRATE_CONFIG`) |
| environment | `PATH`, `HOLLIS_AUTH_STATION` (drop-in) | `SUBSTRATE_CONFIG` and the unit's `*_DATA_DIR`; `HOLLIS_AUTH_*` is ignored by the new build |
| stop timeout | none | `TimeoutStopSec` 40 / 35 / 30; no `KillMode=process` (an unreaped busy child is Cerberus task CW-20261112-0057) |
| working directory | old state directory (gateway, usage) | not relevant to the binary |
| health | `/healthz` on the four ports | unchanged |

## 9. The proxy

The gateway and the usage monitor are reached by a public name through a local TLS proxy that forwards to the loopback port. The new
units serve that name when it is in `allowed_hosts`, the peer is loopback and the request carries **exactly one** `X-Forwarded-Proto: https`;
other forwarding headers are dropped. Confirm before the window that the proxy sends that header once (the tests prove the refusals;
the live proxy's behaviour is read, not assumed). No proxy change is needed otherwise.

## 10. Rehearsal record

| What | Result |
|---|---|
| `usage-monitor migrate-from` on the driver's online-backup copy of the live `usage.db` (47 MB), three runs | 11 accounts, 24 audit rows, 84,697 observations, 67 events; digests identical; `integrity_check` ok; SQL `EXCEPT` both ways 0; sequences equal; 2.40 s, 2.57 s, 2.44 s |
| `llm-gateway gateway-store migrate-from` on the copy of the live `gateway.db`, three runs | revisions 10, state 1, activations 9, usage 0; `EXCEPT` 0/0 on all four tables; 0.06 s, 0.06 s, 0.04 s |
| PostgreSQL 17.11 ledger under the live role layout (throwaway container) | `ledger-migrate` via SET ROLE gives v1..v3, rerun a no-op; the runtime login (INSERT/SELECT/UPDATE on `llm_calls`, SELECT on `llm_ledger_schema`) starts in verify mode and writes; verify refuses a missing v3 and a v4; auto as the runtime login fails without changing anything; old and new inserts identical across 18 columns |
| Real binaries, throwaway data directories, empty working directory | setup, keys, write, read, refusals (no key, wrong key, missing scope, foreign Host, unmapped route, non-loopback bind), SIGTERM exit 0, no credential in the log |
| Relay | upstream OpenAI-style error byte for byte with headers; framework refusals flat; fails without `RawPrefixes` |
| Public name through the proxy (gateway and usage) | served only from a loopback peer with one `X-Forwarded-Proto: https`; the collector key reaches the usage export |
| Ambient `HOLLIS_AUTH_*=permissive` | changes nothing (401 without a credential; `/api/me` says `enforce`) |
| Keyring copy | an isolated gnome-keyring with fake values: copied and verified by length, existing entry left alone, missing entry reported, legacy entry kept, no value in any output |
| Installer dry runs on the host (nothing changed) | all four targets passed on earlier artifacts of this branch |

**Not covered:** a run on the live host; the relay against live providers; the live proxy's header behaviour; the first real
`fleet-install deploy` (link, drop-in merge, the `.pre-fleet-install` rollback); a Cerberus apply; the legacy keyring copy against the real
keyring; the unreaped-busy-child behaviour under a real stop.

## 11. Rollback

Before the first deploy there is nothing to roll back: the old unit is stopped and its files are untouched. If a new unit misbehaves:

1. Stop the unit. Remove its `30-*-exec.conf` drop-in (or use the installer's rollback), `systemctl --user daemon-reload`.
   For the MCP hosts, restore `<name>.pre-fleet-install` to the old path.
2. Start the old unit: the old links, `20-fleet-exec.conf`, configuration and databases (`usage.db`, `gateway.db`, the keys files) are as
   they were. The gateway's old build reads the same PostgreSQL ledger (the new gateway added no schema version); rows the new gateway
   wrote there stay and are valid.
3. **What is lost on rollback:** everything the new usage unit recorded after it started (the old `usage.db` is the pre-cutover copy), and
   the gateway revisions and activations made after cutover; keys issued by the new units stop working and the old tokens work again.
   The unit's stop-to-start pause is the only other gap. The owner accepts this window with the plan.

Do not run a `station` deploy while a rollback may still be wanted: its release directory keeps three versions.

## 12. Callers and what changes for each

**Facts read from the live gateway on 2026-11-12 (structure only):** `ledger.capture.mode` is `off`; the ledger store is PostgreSQL
with a keychain DSN reference; auth mode is `enforce` with a separate keys file (mode 0600) that holds two principals:
`station-operator` (`llm-gateway.*`) and `station-smoke` (`chat`, `models.read`, `embed`, `decide`). So today no key holds
`usage.read` except through the operator's wildcard, and no prompt text is captured.

**Usage scopes after the port.** `GET /v1/usage/export` (what the usage monitor's collector reads) needs `llm-gateway.usage.read` and never
carries request or response bodies. `GET /v1/usage` shows a `usage.read`-only key its own rows without Detail; every caller's rows with
Detail need `llm-gateway.usage.detail`, which today only `station-operator` holds, through `llm-gateway.*`. The new `usage-collector`
key holds `usage.read` alone. Turning `ledger.capture.mode` to `full` is a decision for the owner, taken knowing that every
`llm-gateway.*` key can then read every caller's prompts.

**Management authority.** `llm-gateway.management.write` plus `.activate` is equivalent to reading the process's secrets (a staged
configuration can point a provider at any `base_url` with any `api_key_env` or secret reference as its key). Holders today and after
the port: `station-operator` only, through its wildcard. Do not issue that pair to another key without deciding it is allowed to read
every credential the gateway can resolve.

| Caller | Before | After | Who |
|---|---|---|---|
| The owner, by hand | keys files, `station client`, SQLite import commands | `<unit> identity ...`, `llm-gateway gateway-store ...`, signed-in API | owner |
| `station-operator` (gateway) | keys-file entry, `llm-gateway.*` | identity key, same scopes | operator, at cutover |
| `station-smoke` (gateway) | keys-file entry | identity key, `chat`, `models.read`, `embed`, `decide` | operator |
| The relay's clients (OpenAI and Anthropic SDKs, the JEV decision client) | keys-file entries; base URL `https://<public name>/api/llm-gateway` | **same URL**, a new key each with the scopes it uses; Anthropic clients may send `x-api-key` | see the note below |
| The usage monitor's eight `gateway` collectors | the operator's token, from one file | one `usage-collector` key (`llm-gateway.usage.read` only), written to that file | operator |
| `usage-operator`, `usage-reader` | usage keys-file entries | identity keys on the usage unit | operator |
| Usage push reporters (`principal`) | client names | one key each (`usage-monitor.ingest`), `principal: key:<id>` | each reporter's owner |
| Tether gateway entries `design_*`, `tripo_*` | no authentication | identity key `tether-design` (`station.design-suite`, `station.tripo`) | the driver, at cutover |
| Tether / fabric agents for avatar | per-server bearer token | identity key `tether-avatar` (`station.avatar`) | the driver |
| Health probes | unauthenticated `/healthz` | unchanged | n/a |
| Cerberus | the four registry entries | section 8 | whoever owns the registry |
| Scripts | `scripts/gateway-smoke.py`, `gateway-management-smoke.py` | use the new keys; no `/clients` | operator |

**Who the relay's clients are.** On the live host only two keys exist, so every relay call today comes from `station-operator` or
`station-smoke`; each real client therefore holds one of those two, and after the cutover each gets its own key (the old key values stop
working). No config on the primary host that names the gateway's address was found, so the holders cannot be listed from that host.
Where else to look: the other hosts' configs and environment (anything that sets an OpenAI- or Anthropic-style base URL to the public
name), the public name's reverse-proxy access log (source address and user agent per request, before the window), and the gateway's own
ledger (`GET /v1/usage/export` by `principal` shows which of the two keys each call used, and when). Do the log pass before the window so
every holder is known and none is cut off.

The `/clients` routes had no caller found beyond the operator by hand; they are removed without a replacement route.

## 13. Left in place, downtime, approval

- **Left in place:** the old state directories and databases, the old configuration and keys files, the old links, drop-ins and `station`
  release, the legacy keyring entries, `<name>.pre-fleet-install`, and the legacy PostgreSQL login `station` until it is removed after the
  cutover is accepted (it has no privileges on the ledger tables). Cleaning up is a follow-up after a quiet period, not part of the window.
- **Downtime:** per unit, from its stop to its health check. usage-monitor: the migration is about 2.5 s plus stop, key creation and
  start; the gateway: the migration is under a second plus the keyring copy, key creation and start; the MCP hosts: no data, stop to start.
  Nothing is queued: a request in the window is refused. What the old usage unit would have collected during its pause is not recoverable.
- **Unavailable afterwards:** shared bearer tokens, `station client`, the `/clients` routes, `auth.jwt`, `serve.stdio`, the inprocess
  transport and tool-level scopes, `ledger.sqlite.path`, the `-api-only`/`-api-config`/`-gateway-db` flags.
- **Approval:** the owner approves the cutover. Until then, nothing here is run on the host by anyone.
