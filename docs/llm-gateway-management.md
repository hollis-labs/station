# LLM gateway management API

This API edits the gateway state held in the unit's database (`llm-gateway.db` in its data directory). The service needs an active
revision to start: stage and activate one with `llm-gateway gateway-store` (see `examples/gateway.json`); a previous build's
`gateway.db` is copied in once with `llm-gateway gateway-store migrate-from`.

Base URL on the dogfood host, enabled and verified on 2026-10-18:

```text
https://gateway.example.test/api/llm-gateway/v1/management
```

The Cerberus resource `station-llm-gateway` runs `llm-gateway serve` with `LLM_GATEWAY_DATA_DIR` set in its drop-in.
SQLite owns gateway configuration. Client keys are issued by the identity module
(`/api/identity/keys`), not by this API. The bootstrap `SUBSTRATE_CONFIG` file owns
listener and module settings. PostgreSQL remains the usage ledger; this cutover did not
migrate or discard usage history.

The host-local dedicated credential is
`$HOME/.config/station/gateway/tokens/management.token` (0600).
It belongs to `station-management`; give adopting integrations their own identity
and only their needed scopes. The existing operator and smoke credentials cannot
access management. See the [adoption guide](llm-gateway-adoption.md) for provisioning.

All requests use `Authorization: Bearer <station-client-token>`. Use the portfolio
client pattern: read the token from a private file or approved secret mechanism,
attach it server-side, and keep TLS verification enabled. Management does not
accept the Anthropic `x-api-key` bridge or browser cookies. Do not put an operator
token in frontend configuration, URLs, logs, task text or command arguments.
Management remains closed to unverified or underscoped callers even if a custom
embedding host uses permissive/off authentication; the managed CLI requires
enforced authentication.

## Scopes

| Scope | Grants |
|---|---|
| `llm-gateway.management.read` | Configuration, status and history reads |
| `llm-gateway.management.write` | Configuration validation and staged edits |
| `llm-gateway.management.activate` | Select a stored revision for the next service reload |

**`management.write` plus `management.activate` is the authority to read the process's secrets.** A holder can
stage a configuration that points a provider at any `base_url` with any `api_key_env` or secret reference as its
key, activate it, and the next start sends that credential to a host of the holder's choosing. Treat the pair like
the ability to read every credential the service can resolve; do not give it to a caller you would not give those to.

Literal provider `headers` are never returned: the management GET and `gateway-store show` show the header names
with the value `[redacted]`. Writing a document back with the placeholder keeps the stored value (it is restored
from the revision being edited, named by `If-Match` or `--expected`); a placeholder with nothing to restore is
refused.

`GET /v1/usage` shows a key holding only `llm-gateway.usage.read` its own rows without Detail; the separate
`llm-gateway.usage.detail` scope (also covered by `llm-gateway.*`) shows every caller's rows with Detail, which with
`ledger.capture.mode: full` is the request and response bodies. The usage collector's key needs only `usage.read`
and reads `GET /v1/usage/export`, which never carries Detail.

The existing `llm-gateway.*` and `*` wildcards include management authority.
Ordinary app keys with chat/model-read scopes cannot use these routes. An operator
can issue a dedicated management key with `identity key create` (offline) or
`POST /api/identity/keys`. No reload is needed for a key.

Configuration write access is administrative: it controls upstream endpoints,
secret references, local paths, policies and budgets. Give it only to trusted
operator integrations. Configuration editors are not untrusted tenants. Stronger identity/approval patterns can later
replace the verifier while retaining these route-level authorization boundaries.

## Revision and application contract

- **Latest revision:** most recently staged configuration, including pending edits.
- **Selected revision:** durable selection for the next service startup.
- **Running revision:** snapshot this process actually loaded at startup.

GET configuration and resources default to the latest revision. Add
`?revision=ID` to inspect an older snapshot. Read responses include `revision` and
an `ETag`, such as `"12"`. Configuration writes must send
that latest revision's ETag as `If-Match`. They return **201**, a new ETag and
`{"revision":13,"state":"staged"}`. The edit preserves other parts of the
document.

`GET /status` returns an ETag for the **selected** revision, plus:

```json
{
  "schema_version": 1,
  "latest_revision": 13,
  "selected_revision": 12,
  "running_revision": 12,
  "restart_required": false,
  "staged_changes": true,
  "apply_mode": "service_reload"
}
```

Use that selected-revision ETag with `POST /activate` and `{"revision":13}`.
Success is **202** and reports `selected_revision`, `running_revision` and
`restart_required`. Selection does **not** hot-reload the process, restart a
service, test provider credentials or prove that a future startup will succeed.
It only validates the stored document and commits the selection and audit record.
It can select an older revision for rollback.

An operator then runs `llm-gateway validate` (with the unit's `SUBSTRATE_CONFIG` and `LLM_GATEWAY_DATA_DIR`), restarts the unit, and checks
`/status` again. Show a change as applied only when the running revision matches
the intended selected revision. If validation fails, select the previous working
revision before reloading. Keys issued or revoked through the identity module take effect at once and do not
need a reload.

A stale `If-Match` returns **412** without writing anything. Read the new state
and reconcile the edit; do not blindly retry it with the newest revision number.
Missing `If-Match` returns **428**. Only one strong numeric ETag is accepted:
wildcards, weak tags and lists return **400**. This protects concurrent editors,
including edits made through the CLI. No client-supplied actor field is accepted:
revision and activation audit actors are the verified bearer principal.

## Routes

All paths below are relative to the management base URL. POST/PUT bodies require
`Content-Type: application/json`; their maximum size is 4 MiB. Unknown fields,
trailing JSON and null resource bodies are rejected. Configuration documents use
`schema_version: 1`; field names match the gateway's snake_case YAML names.

| Method | Path | Body / result |
|---|---|---|
| GET | `/status` | Latest, selected and running revisions; reload state |
| GET | `/config` | `{revision, schema_version, gateway}` |
| PUT | `/config` | `{schema_version: 1, gateway: {...}}`; replaces gateway settings atomically |
| POST | `/validate` | Same body as config PUT; validates without saving or contacting providers |
| GET / PUT | `/providers` | `{providers: [...]}` on PUT; GET also includes revision/schema version |
| GET / PUT | `/models` | `{models: [...]}`; includes ordered targets/fallbacks |
| GET / PUT | `/policy` | `{policy: {...}}`; rules retain their supplied order |
| GET / PUT | `/budgets` | `{budgets: [...]}`; `[]` clears budgets explicitly |
| GET | `/revisions` | Immutable revision metadata; no document |
| GET | `/activations` | Selection audit: actor, time, previous revision and selected revision |
| POST | `/activate` | `{revision: ID}`; select for the next reload, as described above |

Resource PUT replaces the named resource, not the whole document. To add/remove
providers and dependent model targets together, use `/config` so cross-reference
validation and the commit are atomic. Model names and provider IDs may contain
slashes; they stay in JSON rather than URL path segments. Disable an entry with
`enabled: false` and retain its other fields. Disabling every route leaves
management available. Fallback order is the order of `models[].targets`.
The existing configuration rules still require at least one provider, plus a model
or provider model discovery. Disabling entries retains those definitions;
deleting all providers is rejected. Empty collection reads use `[]`.

History routes accept `?before=ID&limit=N` (1–100, default 100). They return
`{items, next_before}`. A nonzero cursor can lead to an empty final page. Revision
metadata has a document checksum. Secret reference
names and environment variable names are visible in configuration; resolved
provider secrets are never returned.

Requests and responses are not logged, and all management responses carry `Cache-Control: no-store`.

Validation returns `valid: true`, `validation: "configuration_only"` and
`runtime_checked: false`. It checks the same typed configuration rules as the
store. Secret resolution, provider availability, metadata warmup and ledger
connectivity belong to startup validation and the existing live smoke probes.
Changing the ledger backend does not copy history; migrate existing usage before
switching it so usage reports and budget seeds remain meaningful.

## Example: stage a model disable

This reads a token privately and stages a change. It deliberately leaves selection
to the caller's explicit apply action. The caller should preserve a 412 conflict
for review rather than overwrite the competing change.

```python
import json
from pathlib import Path
import urllib.request

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None

opener = urllib.request.build_opener(NoRedirect)
base = "https://gateway.example.test/api/llm-gateway/v1/management"
token = Path("/private/station-management.token").read_text().strip()
headers = {"Authorization": "Bearer " + token}
request = urllib.request.Request(base + "/models", headers=headers)
with opener.open(request, timeout=30) as response:
    etag = response.headers["ETag"]
    models = json.load(response)["models"]

model = next(m for m in models if m["name"] == "go-glm")
model["enabled"] = False
request = urllib.request.Request(
    base + "/models", method="PUT",
    headers={**headers, "If-Match": etag, "Content-Type": "application/json"},
    data=json.dumps({"models": models}).encode(),
)
with opener.open(request, timeout=30) as response:
    print(json.load(response)["revision"])
```

## Probes and failures

The live cutover exercised configuration staging, stale/missing preconditions,
selection, reload and rollback. Latest, selected and running revisions were then
aligned with the original settings and no pending test edit. Auth checks verified
operator/app denial and rejection of `x-api-key` on management. All 65 pre-cutover
usage entries remained, and three new OpenCode Go calls (Chat Completions,
Responses and Messages) persisted verified attribution. These are dated deployment
results, not a guarantee about a later configuration.

For recovery, retain bootstrap YAML, the Cerberus descriptor and the installed
artifact alongside a consistent SQLite backup made with SQLite's backup API.
Do not copy only the live `.db` file while WAL writes may be pending. Usage backups
remain a separate PostgreSQL concern. On this host, the private
`~/.config/station/gateway/management-cutover-backup` file points to the cutover
snapshot and verification records. Treat snapshots as sensitive: old revisions
can restore revoked identities, and their hashes and secret references are private.

A restart picks up the newly activated revision (keys do not need one). When the unit definition changes (the drop-in in
`deploy/llm-gateway.yaml`), the installer applies it. Check the actual management running revision after either operation.

`scripts/gateway-management-smoke.py --base-url BASE --token-file FILE` checks
the authenticated status/config/history reads, absence of the legacy `clients` field,
missing-token denial and pure configuration validation. It makes no edits,
activations or inference calls. The token needs management read and write
(validation) scopes. It prints only check names, status and pass/fail results.
Use `scripts/gateway-smoke.py` separately to verify inference after deployment.

| Status | Meaning |
|---|---|
| 400 | Malformed JSON, unknown fields, invalid ETag or query parameters |
| 401 / 403 | Missing/invalid bearer credential or insufficient scope |
| 404 | Requested stored revision does not exist |
| 412 / 428 | Stale / missing revision precondition |
| 413 / 415 | Body too large / unsupported content type |
| 422 | Invalid typed configuration, or a document that names clients |
| 503 | Store failure; no successful write is claimed |

Management handler errors use `{error: {code, message, request_id}}`. Authentication
failures may use the shared auth library's envelope. `X-Request-Id` is available
for correlation. Logs carry the verified principal, operation and revision IDs,
and SQLite retains the corresponding revision/selection actors. These are
management audit records, not an implementation of the portfolio activity bus.
