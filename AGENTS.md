# Station

Station is Hollis Labs' MCP host and LLM gateway, built on the Substrate framework. One Go module builds three
binaries that run as four units:

| Binary | Units | What it is |
|---|---|---|
| `cmd/station` | avatar-station, design-station | The libs `mcp-host` engine inside a Substrate service: process-mode logical servers served over HTTP |
| `cmd/llm-gateway` | llm-gateway | OpenAI- and Anthropic-compatible relay with a ledger, budgets, policy and a management API |
| `cmd/usage-monitor` | usage-monitor | Collects and serves provider usage (pull collectors and authenticated push) |

## Where Station is

In daily use on the Hollis Labs fleet host, with real data (gateway revisions and usage history). The port to Substrate
is in review; the live units still run the previous build until the cutover in `docs/cutover-plan.md`, which the owner
approves. A change must not make that cutover riskier: data, credentials and anything that can silently lose work get
the real treatment.

## Start here

- `docs/architecture.md` explains the layout; `docs/conversion-notes.md` records what changed, the gaps found, and the
  evidence; `docs/cutover-plan.md` is the live move.
- `internal/appboot` is the shared boot path (database guard, module sets, offline command plan); each `cmd/*/main.go`
  is thin around it. `internal/llmgateway` and `internal/usagemonitor` hold the services; `internal/hostguard` is the
  Host-header and proxy-header guard (kept until the framework has one).
- Pins live in Substrate's BOM. Never edit versions in `go.mod` by hand: `go run github.com/hollis-labs/substrate/cmd/substrate pins check --app .`

## Commands

```bash
gofmt -l .
go vet ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 run ./...
go test -race -count=1 ./...
go run github.com/hollis-labs/substrate/cmd/substrate pins check --app .
go run github.com/hollis-labs/substrate/cmd/substrate scan-public --tree
```

The tests build and run the real binaries on throwaway data directories. Postgres ledger tests skip unless
`STATION_TEST_PG_DSN` names a **throwaway** database (they truncate `llm_calls`); with several packages sharing it use
`go test -p 1`. Never point it at the live ledger.

## Rules that are easy to break

- Every run of a binary uses an explicit data directory in tests (`STATION_DATA_DIR`, `LLM_GATEWAY_DATA_DIR`,
  `USAGE_MONITOR_DATA_DIR`); the default is the live one. Tests also run from an empty working directory and with HOME and
  XDG redirected. `secret set` against a real session bus writes to the real keyring: unset `DBUS_SESSION_BUS_ADDRESS`
  when trying commands by hand.
- All three binaries refuse a non-loopback `httphost.addr` (`hostguard.RequireLoopbackBind`) before listening, and serve other
  Host names only from a loopback peer with exactly one `X-Forwarded-Proto: https`.
- The database module's store has one write connection: reads through `Reader()`, writes through `WriteTx` (callbacks must be
  re-runnable), never a statement inside an open cursor.
- The relay prefix `/api/llm-gateway/v1/` is declared raw (`httphost.Options.RawPrefixes`): upstream error bodies reach clients
  untouched. Do not add framework-owned behaviour on that prefix without the error-shape test.
- MCP is the agent-facing interface for the MCP host; the CLI is the operator surface. Do not add MCP tools or HTTP
  management for operating Station itself. `serve.stdio` stays refused.
- The old gateway/usage database files are never written by the new build: `migrate-from` reads a copy.

Open a pull request for changes; see `CONTRIBUTING.md`.
