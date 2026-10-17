# Adopt the Station HTTP LLM gateway

Station's local dogfood gateway serves API-powered agents and tools. MCP is not
involved. Application adoption and launch/evaluation configuration are tracked
separately in CW-20261017-0032; this guide describes the Station contract.

## Endpoint and access

- HTTPS API root: `https://llm.nanite.cloud/api/llm-gateway/v1`.
- OpenAI-compatible SDK base URL: that same root, including `/v1`.
- Anthropic SDK base URL: `https://llm.nanite.cloud/api/llm-gateway`;
  the SDK appends `/v1/messages`.
- Host-local diagnostic API root: `http://127.0.0.1:18082/api/llm-gateway/v1`.
- Host: agent-os. DNS points to its Tailscale IP; Caddy terminates wildcard TLS
  and forwards to the loopback-only listener. Clients on other machines need
  tailnet connectivity. TLS verification stays enabled.
- Health: `GET https://llm.nanite.cloud/healthz` (no credential; liveness only).
- Authentication is enforced on every gateway route, including loopback.

The HTTPS route was activated and verified on 2026-10-17. The complete live
matrix passed through Caddy: 12 model routes, three streaming API families,
three tool-call probes, auth/scope restrictions, and persisted attribution for
all 18 inference requests. TLS certificate validation was enabled. A generic
agent-os landing response is not gateway readiness: `/v1/models` must return 401
without a key, and a JSON model list with an authorized key.

## Provision one identity per app or evaluation launch

An operator on agent-os issues the credential. Adopting agents should request
one for their app, not copy the operator or smoke-test credential.

```bash
STATION=/home/chrispian/.cerberus/apps/station/station-llm-gateway/bin/station-llm-gateway
KEYS=/home/chrispian/.config/station/gateway/clients.yaml

"$STATION" client issue -keys-file "$KEYS" -id my-app-eval \
  -output /home/chrispian/.config/station/gateway/tokens/my-app-eval.token \
  -scopes llm-gateway.chat,llm-gateway.models.read
cerberus resource status station-llm-gateway
cerberus resource reload station-llm-gateway --ack
```

The output file is created exclusively with mode 0600. The registry contains
SHA-256 hashes, never raw tokens. Issue refuses an existing identity or output
file. Store/distribute the token through the adopting application's approved
secret mechanism; do not paste it into task descriptions, logs, committed
config, URLs, or command arguments. The `tokens` directory is owner-only.

Scopes:

| Scope | Permission |
|---|---|
| `llm-gateway.models.read` | Read models and catalog metadata |
| `llm-gateway.chat` | Chat Completions, Responses, and Messages |
| `llm-gateway.embed` | Embeddings |
| `llm-gateway.decide` | JEV System-1 decisions |
| `llm-gateway.usage.read` | Operator access to usage across principals |
| `llm-gateway.catalog.write` | Trigger metadata refresh |
| `llm-gateway.*` | All gateway permissions; reserve for operators |

Use `station client list -keys-file "$KEYS"` to inspect IDs/scopes without
printing credentials. Revocation:

```bash
"$STATION" client revoke -keys-file "$KEYS" -id my-app-eval
cerberus resource reload station-llm-gateway --ack
```

Reload is required for issuance and revocation. Existing in-flight work can
finish during graceful shutdown. Rotation currently means revoke, issue a new
credential file for the same identity, then reload; no overlapping-key rotation
or token-expiry service is configured. Operator credentials can read all usage;
ordinary app credentials cannot. The existing JWT support is not configured for
this local deployment.

## Identity and conversations

Send `Authorization: Bearer <app-token>`. Anthropic clients may instead send
`x-api-key: <app-token>`; this is a Station client token, not a vendor token.
Station resolves provider credentials itself and replaces upstream auth.

For every conversation or agent run, generate an opaque ID and send it as
`X-Station-Session`. Reuse it across turns, tools, retries, and related requests;
use a new ID for a genuinely new conversation. Allowed format: 1–128 ASCII
letters/digits, dots, underscores, colons, or hyphens, starting with a letter
or digit. Do not use prompts, email addresses, or secrets as labels.

OpenCode Go requires this header. Missing IDs return HTTP 400 `session_required`;
invalid/duplicate IDs return 400 `invalid_session`. Station hashes the label
with the verified app principal and forwards the stable result as
`x-opencode-session`. Identical labels from different apps produce different
provider sessions. This is correlation, not delegated authority.

`X-Request-Id` identifies one HTTP request and appears in response/log/ledger
records. Use a distinct value for each attempt, or let Station generate one.
The usage entry's `conversation_id` is the namespaced conversation; the existing
`row.session_id` field remains the authenticated principal for accounting
compatibility. Client body fields such as `caller_id` do not change attribution.

## Explicit model routes

Use the alias in `model`. Model discovery is disabled in this deployment;
these routes are deliberately assigned to native API operations.

| Alias | Endpoint suffix | Upstream model |
|---|---|---|
| `go-glm` | `/chat/completions` | Go `glm-5.3-flash` |
| `go-gpt` | `/responses` | Go `gpt-6-luna` |
| `go-haiku` | `/messages` | Go `claude-haiku-5-5` |
| `openai-mini` | `/chat/completions` | OpenAI `gpt-5-mini` |
| `anthropic-haiku` | `/messages` | Anthropic `claude-haiku-5-5` |
| `gemini-flash` | `/chat/completions` | Gemini `gemini-2.5-flash` |
| `openrouter-auto` | `/chat/completions` | OpenRouter `openrouter/auto` |
| `hf-llama` | `/chat/completions` | HF `meta-llama/Llama-3.1-8B-Instruct` |
| `local-light` | `/chat/completions` | Local `qwen3-4b-instruct-2507` |
| `embed-local` | `/embeddings` | Local `qwen3-embedding-0.6b` |
| `embed-openai` | `/embeddings` | OpenAI `text-embedding-3-small` |
| `jev` | `/systemone` | JEV `jev-1.13.0` |

`GET /models` gives the OpenAI-family list by default. Supply
`anthropic-version: 2023-06-01` for the Anthropic list. Decision models are not
included in those chat/embedding lists. Target metadata includes `operations`
when explicitly configured. Sending `go-gpt` to Chat Completions is rejected
locally with 400 `model_not_available`. There is no automatic conversion between
Chat Completions and Responses. Native request parameters still apply:
`max_completion_tokens` for the tested OpenAI mini request, `max_output_tokens`
for Responses, and `max_tokens` for Messages/Go GLM.

OpenCode Zen pay-as-you-go routes are not enabled here; Go access is enabled.
The configured workspace key and subscription are workspace-bound. Local
providers have short best-effort timeouts; Station does not start a model server
when called. There is no fallback to a different paid provider in these routes.

## Minimal request without putting the token in argv

Python's standard library is sufficient; use your app's normal secret injection
in production launch configuration.

```python
import json
from pathlib import Path
import urllib.request
import uuid

base = "https://llm.nanite.cloud/api/llm-gateway/v1"
token = Path("/private/path/my-app-eval.token").read_text().strip()
conversation = str(uuid.uuid4())  # retain for subsequent turns in this conversation
request = urllib.request.Request(
    base + "/chat/completions",
    data=json.dumps({
        "model": "go-glm",
        "messages": [{"role": "user", "content": "Reply OK."}],
        "max_tokens": 256,
    }).encode(),
    headers={
        "Authorization": "Bearer " + token,
        "Content-Type": "application/json",
        "X-Station-Session": conversation,
    },
)
with urllib.request.urlopen(request, timeout=90) as response:
    print(response.headers.get("X-Request-Id"))
    print(json.load(response))
```

For SDKs, use their custom/default headers option to attach
`X-Station-Session`. Never globally share one session ID among unrelated runs.

## Verification and evaluation

The repository's `scripts/gateway-smoke.py` uses the configured dogfood aliases.
It reads credentials from files and prints only status, request IDs and timing.
Without `--live`, it checks auth, scope denial, model listing, missing session,
and invalid routing. `--live` makes bounded, potentially billable calls.

```bash
python3 scripts/gateway-smoke.py \
  --base-url https://llm.nanite.cloud/api/llm-gateway/v1 \
  --token-file /private/path/smoke.token \
  --operator-token-file /private/path/operator.token \
  --output /private/path/results.json --live
```

The complete smoke matrix needs chat, model-read, embed and decide scopes.
Only the operator running the test supplies the optional operator token for
ledger checks. An adopter with narrower scopes should run the checks relevant
to its launch; do not widen access just to satisfy the full smoke matrix.

The matrix exercises basic inference on all configured aliases, streaming for
the three Go protocol families, and native tool-call generation. Tools are not
executed by the probe. It is connectivity/conformance evidence, not a model
quality evaluation, complete feature certification, or a load test. Reuse the
existing Go mock tests for cancellation, failover, redaction and policy behavior.

Record app identity, model alias, operation, conversation label, request IDs,
stream/tool behavior, latency and outcome in evaluations. Check that completion
and tool semantics survive your app's own SDK and launch configuration.

## Operations and durable state

- Cerberus resource: `station-llm-gateway`, artifact-backed systemd user service,
  enabled at boot with automatic restart; it runs `serve -api-only`.
- Dedicated PostgreSQL resource: `station-gateway-ledger`, Docker Compose,
  loopback `127.0.0.1:15438`, persistent named volume, restart unless stopped.
- Runtime configuration: `~/.config/station/gateway/api.yaml`.
- Resource descriptor: `~/.config/station/gateway/station.cerberus.yaml`.
- Client registry: `~/.config/station/gateway/clients.yaml`.
- Provider and ledger credentials: secret references resolved by Station;
  local OS keychain access must remain available to the service.
- Logs: `~/.cerberus/apps/station/station-llm-gateway/logs/`.
- The old `station-gateway-verification` service on 18081 is stopped.

After source changes, use `cerberus resource deploy station-llm-gateway --ack`;
a bare build does not replace the installed artifact. For config/key changes,
validate with the installed binary, then reload. Validation does not listen,
but it resolves secrets, connects/migrates PostgreSQL, and fetches metadata when
`catalog.warmup: true`. After restart, wait for health and authenticated model
listing before sending inference; initial metadata fetch can take up to 10s.
A failed warmup fails startup rather than exposing a cold metadata catalog.

```bash
cerberus resource status station-llm-gateway
cerberus resource inspect station-llm-gateway
cerberus resource reload station-llm-gateway --ack
```

`GET /v1/usage` requires the usage-read scope. It returns entries, totals, and
buffer health. Records persist across normal restarts; the asynchronous writer
is still bounded and can lose queued records on abrupt crash/overflow. Monitor
`queued`, `dropped`, and `write_failures`. This is not a lossless financial ledger.
Payload capture is off; prompts and completions are not stored by the configured
capture feature. JEV's bounded decision answers remain part of its usage detail.

No budget limits are configured for this dogfood service. Policy and auth are
enforced, but an authorized inference client can spend against configured
providers. Cost figures are API-rate estimates when known, not subscription
charges; unknown cost is not zero. Hard budget reservations, comprehensive
telemetry, and portfolio activity-event integration remain separate work.

A local ledger backup is kept under `~/.local/state/station/backups/`; the
initial dump was verified with `pg_restore --list`. This is not a scheduled or
off-host backup. Operator backup command (run with a private output directory):

```bash
umask 077
docker exec station-gateway-ledger pg_dump -U postgres -Fc station_ledger \
  > "$HOME/.local/state/station/backups/ledger-$(date -u +%Y%m%dT%H%M%SZ).dump"
```

Do not run the PostgreSQL integration tests against the live ledger: they
truncate tables. Use the separate `station_ledger_test` database via
`STATION_TEST_PG_DSN`. Never remove the Compose volume as routine cleanup.
