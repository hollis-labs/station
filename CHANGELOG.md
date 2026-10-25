# Changelog

All notable user-facing changes land here.

## Unreleased

## 0.1.0 - 2026-10-20

First tagged release (the baseline tag; later changes are listed under the next version).

### Security

- Go toolchain 1.26.10 (`go` directive and CI pin) for GO-2026-6809 (crypto/x509); no behavior change.

### Changed

- LLM gateway adopts `libs/llm/core v0.4.0` for bounded recurring catalog sync,
  source-bound caches and atomic price provenance. Old unbound caches require a
  fresh fetch on upgrade. Configure cycle timeout, response limit and retry bounds.
- Catalog status includes safe refresh and price-file health. Reload local price
  overrides independently with `POST /v1/catalog/prices/reload`; failed reloads
  retain the last good table. Warmup uses the configured refresh timeout.

- LLM gateway adopts `libs/llm/core v0.3.0`: catalog metadata preserves canonical
  identity, type/status, provider request descriptions, reasoning controls, input
  limits and pricing dimensions. Missing prices remain unknown; explicit zero
  remains free. Catalog prices carry currency/unit and capability availability.
- Usage totals count partial-cost calls in `unpriced_calls` across memory,
  SQLite and PostgreSQL, retaining known cost components as lower bounds.

- Moved to the `libs` monorepo: `mcp-host`, `go-mcp`, `plugin-sdk` and
  `api-projection` now come from `libs/mcp/plugin-mcp` v0.4.0. Inprocess
  logical servers need a trusted Init factory (protocol 2); `cmd/station`
  supplies one for the `clock` starter and fails closed for any other.
- `clock-plugin` acknowledges the capability contract.

### Added

- `platform/`: plugin platform with route, middleware, transport and stop
  hooks, built on `libs/app/app-host` and `libs/security/auth`.
- `plugins/llm-gateway`: transparent LLM relay for OpenAI Chat Completions and Anthropic Messages, with capability-aware targets, `llm.invoke` policy, failover and a usage ledger.
- `serve -api-config` and `validate -api-config`.
- `auth.jwt`: offline JWT verification against a cached JWKS (Tether-issued tokens).
- Per-route request body caps (`Route.MaxBodyBytes`).
- Redacting JSON logging (`security/redact`), request info shared with the access log
  (verified principal, route pattern), `llm.call` and `llm.refused` events.
- `station secret set|check <ref>` and `api_key_ref` for provider keys (`security/secrets`).
- llm-gateway durable ledger: `ledger.store: postgres` (own database and role, migrated
  `llm_calls`, idempotent entry IDs, async writer with retry and visible drops);
  `/v1/usage` filters (`principal`, `since`) and buffer stats.
- llm-gateway JEV: `kind: decision`, `jev` dialect, `POST /v1/systemone`; decision answers kept in the ledger.
- `App.Close` releases plugin resources for an app that was assembled but not served.
- llm-gateway: `POST /v1/embeddings`, model `kind`, `llm.invoke` rule field `operation`.

### Added

- Initial prototype: a thin binary (`cmd/station/main.go`) wrapping
  `mcphost.Run` from [`github.com/hollis-labs/mcp-host`](https://github.com/hollis-labs/mcp-host),
  plus Station's own config (`station.yaml`) and two starter virtual MCPs
  proving the library end to end:
  - `plugins/echo-server` — process-mode, a real standalone MCP server
    with one `echo` tool.
  - `plugins/clock-plugin` — inprocess-mode, a plugin-sdk-dialect
    subprocess with one `now` tool.
- `cmd/station/main_test.go` — builds the real binary and drives it with
  a real MCP client over real stdio.
- `cmd/station/smoke_test.go` — the two-virtual-MCPs acceptance test:
  both plugins running simultaneously, each independently reachable
  (stdio + HTTP), each killed and confirmed to recover via mcp-host's
  supervision.
- CLI subcommands (`serve`, `validate`, `list`, `version`) — bare
  invocation still defaults to `serve`. `validate`/`list` are config
  inspection only, no side effects; matches Tangent's convention that MCP
  is the only agent-facing interface, with the CLI staying an ops surface.

### Changed

- Moved the Atlas server (`plugins/atlas-server`, `internal/atlas/`) to
  [`station-plugins`](https://github.com/hollis-labs/station-plugins) as
  `atlas-server`; Station keeps the echo and clock starters and the GitHub pilot.
- Bumped to `mcp-host v0.2.0`: `inprocess` tool discovery is now
  manifest-driven (`station.yaml`'s `inprocess.tools` block), not a live
  RPC call. `plugins/clock-plugin` rewritten to use plugin-sdk's
  documented `subprocess.Serve` helper as a direct consequence.
