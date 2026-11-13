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

**Rollback for the ledger is nothing to undo.** Rows the new gateway wrote are valid for the old build (the same columns: the old-versus-new comparison, 18 columns, passed on the #28 head; its test was deleted with the old gateway in #29, so
nothing on `main` guards the claim any more. **The cutover therefore deploys the build of `2f6f6b0f` (main after #29) or of a later commit
whose ledger code is byte-identical to it** (`git diff 2f6f6b0f..<commit> -- internal/llmgateway/store_pg.go internal/llmgateway/ledger.go
internal/llmgateway/migrations` is empty: checked for the head of the pull request that carries this paragraph, and to be checked again on
the merge commit); otherwise the comparison is re-proved first. The `2f6f6b0f` build is run 43336002026 with artifacts
`station-2f6f6b0fd821`, `llm-gateway-2f6f6b0fd821`, `usage-monitor-2f6f6b0fd821`; it has no `init` mark or validate checks, so the build to
deploy is the later one: its run and three artifacts are written into section 5, step 1 when it exists); the old unit simply
reads and appends to the same tables. A `pg_dump -Fc` is taken in step zero as a safety copy, not because the plan needs a restore. What is lost on rollback is in section 11.

## 1. What changes for the owner

Read this first. Everything else is how it is done.

**You type four passwords, one per unit.** Each unit is its own identity issuer, so each needs its own owner, created offline with
`<unit> identity setup` at a terminal (usage-monitor, llm-gateway, avatar-station, design-station). **One order, used everywhere in this plan:** per unit, stop, backup, `migrate-from`, the owner's `identity setup`, keys, install. The four
units are run one after another in one sitting, and you type each unit's password (twice, nothing echoed) when its window reaches
the `identity setup` step. Use one passphrase manager entry per unit; none is ever passed as an argument. Nothing creates a new
data directory before that unit's `migrate-from` (step zero 1 checks this), and `serve` refuses a database that neither
`migrate-from` nor an explicit `init` has marked (section 3).

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
- Usage push sources are bound to `key:<id>` principals (the key id that `identity key create --out` prints), not a client name.
- Ports are unchanged: 8097 (avatar), 8110 (design), 18082 (gateway), 18083 (usage). All are loopback only.
- The data is copied, not shared: gateway revisions and usage history are copied into new databases by `migrate-from`; the old
  files are never written (rollback keeps them).
- The two MCP-host binaries are plain files today; the installer keeps each as `<name>.pre-fleet-install`.
- Shutdown takes longer: the service manager's stop timeout is 40 s (gateway), 35 s (usage), 30 s (MCP hosts); no `KillMode=process`.
- The credential store: six gateway provider keys are copied from the legacy keyring location (section 6, llm-gateway step 4).

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
  drop-ins of the gateway and usage units are `30-*-exec.conf` and sort after the old one. The two MCP-host units have no drop-in directory today, so the `station` descriptor's own `20-fleet-exec.conf` is new for them (dry run 2026-11-12); their rollback is removing it and restoring `<name>.pre-fleet-install`. That copy is taken only if the install root has no current
release, so each MCP target has its own root (`releases/station-avatar`, `releases/station-design`); the old Station release lives elsewhere. The installer targets are the two in `deploy/station.yaml` (avatar and design); `--target` takes the id written there.
- **No open setup window.** `POST /api/setup` is refused (403); the owner and keys are created offline **before** a unit starts. The
  first check after start is that `GET /api/setup` answers `{"setup_required":false}`.
- **Never print a secret.** Keys go to mode-0600 files with `--out`; passwords are typed at a prompt; the keyring copy is a pipe.
- **The installer comes last.** `fleet-install deploy` restarts the unit, and `serve` refuses a missing database, so the restart is
  only safe after the migration. `usage-monitor serve` also refuses a database that neither `migrate-from` nor `usage-monitor init` has
  marked (a row written in the same transaction as the copied history): a unit given only an owner and keys cannot start, seed its
  configured accounts and make the migration refuse ("already has rows"). The gateway refuses to start with no active revision. The
  installer's preflight then backs up the **migrated** database.
- **No `station` deploy before the gateway and usage cutovers are accepted** (the gate is in section 6). The two MCP hosts have separate
  install roots, so a later rollback of one does not depend on the other.
- **Order:** usage-monitor, then the gateway (it has live clients), then avatar-station, then design-station.

## 4. Step zero: before anything else, with nothing changed

Only these commands create anything: `identity setup`, `identity key create`, `migrate-from`, `usage-monitor init` and `gateway-store stage|activate|migrate-from`
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

1. Merge the pull request (CI green on `main`). Record the commit and the run id of the `push` build on `main` (one run, three artifacts:
   `station-<sha12>`, `llm-gateway-<sha12>`, `usage-monitor-<sha12>`); that run id is the `--run` of every deploy, and the sha is the one section 0
   compares against `2f6f6b0f`.
2. Rehearse on a fresh copy in a scratch directory with `HOME`, `XDG_*` and every `*_DATA_DIR` pointed there (section 10).
   Counts match; the copy's checksum unchanged.
3. Prepare, without applying: the four override files (section 6, step 3), the proxy edit (section 9), the Cerberus descriptor edit
   (section 8). Tell the callers of section 12 which window to expect.
4. **Dry runs of the installer**, by the driver, against the published artifacts: `fleet-install deploy <app> --repo hollis-labs/station --run <id> --dry-run`
   (for `station`, `--target` names avatar-station or design-station: two targets match the host). Each must print the checksum check,
   the new link, its drop-in (`30-*-exec.conf` for the gateway and usage; `20-fleet-exec.conf` for the two `station` targets), the restart
   and the `/healthz` poll, and skip the preflight while the migrated database does not exist. For each `station` target it must also say
   its new install root; the dry run does not print the `<name>.pre-fleet-install` copy (section 10), the first real deploy makes it.
   Passed on 2026-11-12 against earlier artifacts of this branch (conversion-notes); repeat on the published ones.
5. **The ledger answer and the principal list, on the rehearsal copy** (driver, scratch directory, never the live units): `gateway-store
   migrate-from` the day's copy of `gateway.db` into a scratch data directory, then `llm-gateway validate` with the ledger's
   credential file reachable. Expected: either `llm-gateway: OK, revision <n>, <r> route(s), 0 live key(s)` (the ledger opened in
   verify mode as the runtime login, so its password reference resolves) or the principal lines (`names principal ..., which no issued key can produce`) and nothing
   else; a secret or connection error is the ledger answer and must be settled before the window. Then `gateway-store show --output`
   and list every `principal` in `policy.rules` and `budgets`. On the 2026-11-12 copy (revision 10): policy `enforce`, default effect
   `allow`, **no rules and no budgets**, so nothing names a principal and nothing needs rewriting; the ledger block is structured with a
   `file://` password reference and no `dsn_ref`/`dsn_env` (a keychain DSN in the old `api.yaml` is not what the revision applies).
6. **The window does not start until every holder of a key is known and ready to swap it.** `nanite` is another application's caller: its owner and
   Nanite's agent are told it needs a new key at the cutover, and the new key must be handed over (and the old one retired) in the window. Same for any
   other relay client the log pass finds.
7. Read section 12 against what is found on the host: every caller gets a key, or is retired. Do the relay-client log pass (section 12)
   now.

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
4. **`migrate-from`**: `$NEWBIN/usage-monitor migrate-from --old-db <the backup copy>`. It creates the new database and the mark `serve`
   requires, in the same transaction as the copy. Expect `accounts 11`, `account_audit 24`, `observations ~84,700`, `events 67`, matching
   digests, `integrity_check ok`, exit 0 (rehearsed: 2.4 to 2.6 s). A second run is refused.
5. **Identity, offline**: the owner runs `$NEWBIN/usage-monitor identity setup --username <owner>` **himself at a terminal** (his password for this
   unit); then `identity key create --name usage-operator --scope usage-monitor.read --scope usage-monitor.manage --scope usage-monitor.collect --out <file>`,
   `usage-reader` (`usage-monitor.read`), and one key per push reporter with `usage-monitor.ingest`. `identity key create --out` prints the
   new key's id (`k_...`) on stdout: record it, and set that reporter's source `principal: key:<id>` in the override file. A source copied
   from the old `api.yaml` still names a client name; replace every one. Then `usage-monitor validate`: it fails, naming the source, for any push source whose
   principal no live key can produce (the reporter would be refused with `source_forbidden`). Expect `usage-monitor: OK, <n> route(s), <k> live key(s)`.
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
5. **Identity, offline**: the owner runs `llm-gateway identity setup --username <owner>` **himself at a terminal** (his password for this unit);
   then the keys of section 12, each `--out <file>`: `station-operator`, `station-management`, `station-smoke`, `usage-collector`, `nanite`
   and any other relay client found by the log pass. Write the usage-collector key to the file the usage monitor's eight gateway sources name as
   `credential_ref`. Note each printed key id (`k_...`).
6. **Principal globs.** `llm-gateway gateway-store show --output <new file>` (mode 0600, not committed) and read `policy.rules` and `budgets`:
   every `principal` there is matched against the caller's principal id (`policy.go`, `budget.go`, `relay.go`), and callers are `key:<id>`
   from now on. A rule or budget naming an old client id stops matching without any error at request time: an allow falls to the default
   effect, a budget stops applying. On the 2026-11-12 copy there are none (section 5, step 5); repeat it on the revision actually activated. For
   each one: rewrite it to `key:<id>` (or a glob such as `key:*`), or drop it. **Remove the `clients` array from the edited file first**
   (`show` carries the old revision's client hashes; `stage` refuses a document that names clients), then
   `gateway-store stage --input <file> --expected <latest> --actor <name>` and `activate --revision <n> --expected <active> --actor <name>`.
7. `llm-gateway validate` (resolves the references, opens the ledger in verify mode as the runtime login, and does not listen). It also fails,
   naming the rule or budget and its pattern, when the active revision names a principal that no live key can produce; so run it after step 5.
   Expect `llm-gateway: OK, revision <n>, <r> route(s), <k> live key(s)`. A ledger password error here is the answer to section 0.
8. **Install, last**: `fleet-install deploy llm-gateway ...` (link `~/.local/libexec/station-gateway/llm-gateway`, `30-llm-gateway-exec.conf`,
   `TimeoutStopSec=40`). Read the effective `ExecStart` (`systemctl --user cat`) before trusting the unit.

**Acceptance gate before the MCP hosts.** The usage monitor and the gateway are accepted when every check of section 7 for them has passed
(the gateway row in the ledger, a collector pull recorded, a push ingest with its own key) and the driver says so. Only then does a
`station` deploy start: it is a separate decision, not the next line of a script.

**avatar-station, design-station** (identity data directory only, no application data; one unit at a time)

1. Stop the unit. Convert its YAML to the `station:` block of a new override file (`examples/station.yaml`): `logical_servers` keep their
   schema; `bearer_tokens`, `allowed_origins` and the listen flag are removed; the avatar `env:` block is unchanged except that
   secrets are references. `station validate` prints the logical servers without spawning anything.
2. Identity, offline: the owner runs `station identity setup --username <owner>` himself at a terminal (his password for this unit); then the key
   `tether-avatar` (`station.avatar`) or `tether-design` (`station.design-suite`, `station.tripo`), `--out <file>`.
3. Install, last: `fleet-install deploy station --target <the avatar or design target id written in `deploy/station.yaml`> ...`; the plain file at the old
   path is kept as `<name>.pre-fleet-install` (it exists because each target has its own, empty install root; confirm the file is there
   before the unit is judged). `WorkingDirectory` stays as the Cerberus unit has it.
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
   a row appears in the PostgreSQL ledger; `GET /api/llm-gateway/v1/management/status` shows `running_revision` equal to the revision activated in step 6 (10 if nothing was restaged) and `restart_required: false`.
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
| Installer dry runs on the host (nothing changed) | all four targets passed on earlier artifacts of this branch; the four were repeated on 2026-11-13 against `workflow_dispatch` run 43338479890 (head `b177773`). Each `station` target now reads `install: would place .../releases/station-avatar/<version>/station` (or `station-design`) and `dropin: would write .../<unit>.service.d/20-fleet-exec.conf`, with no state in the new root. **The dry run prints no line about keeping the old file as `<name>.pre-fleet-install`**: that copy is made by the first real deploy, so it is proved only then (hand copy of step zero 3 stays). The gateway and usage runs print `preflight ... does not exist yet, skipped`, the `30-` drop-in, the restart and the health poll; nothing was created under `releases/` |
| Rollback, both shapes, on throwaway units | MCP shape: old, new, old again; gateway/usage shape: old, new, old again (section 11) |
| Revision 10 on the copy | policy `enforce`/default `allow`, no rules, no budgets; five clients; ledger block structured with a `file://` password reference (section 12) |

**Not covered:** a run on the live host; the relay against live providers; the live proxy's header behaviour; the first real
`fleet-install deploy` (link, drop-in merge, the `.pre-fleet-install` rollback); a Cerberus apply; the legacy keyring copy against the real
keyring; the unreaped-busy-child behaviour under a real stop.

## 11. Rollback

Before the first deploy there is nothing to roll back: the old unit is stopped and its files are untouched. If a new unit misbehaves,
stop it, then undo exactly what **its** deploy added. Each unit has a different drop-in; the installer's own `rollback` needs a previous
release, and on a first deploy of the gateway or the usage monitor there is none, so **rollback is by hand** for all four:

| Unit | Drop-in the deploy wrote (remove it) | Also | Then |
|---|---|---|---|
| usage-monitor | `30-usage-monitor-exec.conf` | nothing: the old link and the old `20-fleet-exec.conf` were never touched | `systemctl --user daemon-reload`, start the old unit |
| llm-gateway | `30-llm-gateway-exec.conf` | nothing, as above | `daemon-reload`, start the old unit |
| avatar-station | `20-fleet-exec.conf` (the descriptor's; the unit had no drop-in before) | remove the symlink at the old path and `mv <path>.pre-fleet-install <path>` | `daemon-reload`, start the old unit |
| design-station | `20-fleet-exec.conf` | the same, for its own path | `daemon-reload`, start the old unit |

If the first MCP deploy fails its own health check, the installer's automatic rollback relinks the old binary but leaves `20-fleet-exec.conf`, so
the old binary starts with a bare `serve`; this table recovers it (remove the drop-in, put the kept file back). For the MCP hosts both parts are needed: removing only the drop-in starts the new binary's link with the old arguments, and restoring only the
file leaves the new `ExecStart`. Read `ls -l <path>` before the `mv`. The hand copy of step zero 3 is the second safety. Each MCP target has its
own install root, so one host's rollback never depends on the other.

Then: the old links, `20-fleet-exec.conf`, configuration and databases (`usage.db`, `gateway.db`, the keys files) are as they were. The
gateway's old build reads the same PostgreSQL ledger (the new gateway added no schema version); rows the new gateway wrote there stay and are
valid (section 0 for the limit of that claim).

**What is lost on rollback:** everything the new usage unit recorded after it started (the old `usage.db` is the pre-cutover copy), and the
gateway revisions and activations made after cutover; keys issued by the new units stop working and the old tokens work again. The unit's
stop-to-start pause is the only other gap. The owner accepts this window with the plan.

**Rehearsed** on 2026-11-13 on two throwaway user units (not the live ones; the deploy was simulated by hand, the commands are the table's):
the MCP shape went from the old arguments to the new `serve` and, after the table's steps, back to the old arguments; the gateway/usage shape
likewise by removing the `30-` drop-in alone. Removing a `30-*` file from the MCP shape left the new `ExecStart` running (the failure this table
exists to prevent). The first real `fleet-install deploy` is still not covered (section 10).

## 12. Callers and what changes for each

**Facts read on 2026-11-12 (structure only).** `ledger.capture.mode` is `off`, so no prompt text is captured. The ledger is PostgreSQL; the
active revision's block is structured with a `file://` password reference and no `dsn_ref` (the keychain DSN the driver read is in the old
`api.yaml`, which the new build does not read: section 0). Auth: the driver read a keys file holding two principals,
`station-operator` and `station-smoke`. **But the live gateway runs with `-gateway-db`, and the old build then ignores the keys file and
authenticates with the active revision's own `clients`** (old tree, `cmd/station/gateway_store.go`: `cfg.Auth.Keys = r.Document.Clients;
cfg.Auth.KeysFile = ""`). Revision 10 on the rehearsal copy names **five** clients (ids and scopes only; the hashes are not read):

| Client in revision 10 | Scopes | Becomes |
|---|---|---|
| `station-operator` | `chat`, `models.read`, `embed`, `decide`, `usage.read`, `catalog.write` | key `station-operator`, the same scopes (no management, no usage detail) |
| `station-management` | the same six plus `management.read`, `.write`, `.activate`, `.credentials` | key `station-management`: `management.read`, `.write`, `.activate` (no `.credentials`; `/clients` is gone), and the six if it still calls the relay |
| `station-smoke` | `chat`, `models.read`, `embed`, `decide` | key `station-smoke`, the same |
| `station-usage-monitor` | `usage.read` | key `usage-collector`, `usage.read` only |
| `nanite` | `chat`, `models.read` | key `nanite`, the same: a real relay client, found by name |

Confirmed by the driver from the live process's command line on 2026-11-12: the gateway unit runs `serve -api-only -api-config <...>/gateway/api.yaml -gateway-db <...>/gateway/gateway.db`, so these five clients are what authenticates live calls; the two-principal keys file is a file the live build ignores.

**Usage scopes after the port.** `GET /v1/usage/export` (what the usage monitor's collector reads) needs `llm-gateway.usage.read` and never
carries request or response bodies. `GET /v1/usage` shows a `usage.read`-only key its own rows without Detail; every caller's rows with
Detail need `llm-gateway.usage.detail`, a scope no old client held, so no key gets it by default. Turning `ledger.capture.mode` to `full` is a
decision for the owner; grant `usage.detail` only knowing that its holder can then read every caller's prompts.

**Management authority.** `llm-gateway.management.write` plus `.activate` is equivalent to reading the process's secrets (a staged
configuration can point a provider at any `base_url` with any `api_key_env` or secret reference as its key). Holder today and after the
port: `station-management` only. Do not give that pair to another key without deciding it is allowed to read every credential the gateway
can resolve.

| Caller | Before | After | Who |
|---|---|---|---|
| The owner, by hand | keys files, `station client`, SQLite import commands | `<unit> identity ...`, `llm-gateway gateway-store ...`, signed-in API | owner |
| `station-operator`, `station-management` (gateway) | clients of the active revision | identity keys, scopes as in the table above | operator, at cutover |
| `station-smoke` (gateway) | client of the active revision | identity key, `chat`, `models.read`, `embed`, `decide` | operator |
| The relay's clients (`nanite`, OpenAI and Anthropic SDKs, the JEV decision client) | clients of the active revision; base URL `https://<public name>/api/llm-gateway` | **same URL**, a new key each with the scopes it uses; Anthropic clients may send `x-api-key` | see the note below |
| The usage monitor's eight `gateway` collectors | the `station-usage-monitor` client's token, from one file | one `usage-collector` key (`llm-gateway.usage.read` only), written to that file | operator |
| `usage-operator`, `usage-reader` | usage keys-file entries (the usage unit has no revision) | identity keys on the usage unit | operator |
| Usage push reporters (`principal`) | client names | one key each (`usage-monitor.ingest`), `principal: key:<id>` | each reporter's owner |
| Tether gateway entries `design_*`, `tripo_*` | no authentication | identity key `tether-design` (`station.design-suite`, `station.tripo`) | the driver, at cutover |
| Tether / fabric agents for avatar | per-server bearer token | identity key `tether-avatar` (`station.avatar`) | the driver |
| Health probes | unauthenticated `/healthz` | unchanged | n/a |
| Cerberus | the four registry entries | section 8 | whoever owns the registry |
| Scripts | `scripts/gateway-smoke.py`, `gateway-management-smoke.py` | use the new keys; no `/clients` | operator |

**Who the relay's clients are.** The revision names `nanite` (chat and models); it, `station-operator` and `station-smoke` are the relay's
possible holders, and `station-management` is for management. Nothing on the primary host that names the gateway's address was found, so
who holds `nanite`'s key, and whether a person or program uses the operator's or smoke's for chat, is not known from this host. Where to look:
the other hosts' configs and environment (anything that sets an OpenAI- or Anthropic-style base URL to the public name), the public name's
reverse-proxy access log (source address and user agent per request, before the window), and the gateway's own ledger
(`GET /v1/usage/export` by `principal` shows which key each call used, and when). Do the log pass before the window so every holder is
known and none is cut off. Every old key value stops working at cutover.

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
