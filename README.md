# Station

Station is Hollis Labs' MCP host and LLM gateway, built on the
[Substrate](https://github.com/hollis-labs/substrate) framework. One Go module builds three binaries that run as four units:

| Binary | Units | What it does |
|---|---|---|
| `station` (`cmd/station`) | avatar-station, design-station | Hosts MCP logical servers (the libs [`mcp-host`](https://github.com/hollis-labs/libs/tree/main/mcp/plugin-mcp/mcp-host) engine) and serves each over HTTP under its own path |
| `llm-gateway` | llm-gateway | OpenAI- and Anthropic-compatible relay with a ledger, budgets, policy, a catalog and a management API |
| `usage-monitor` | usage-monitor | Collects provider usage (pull collectors, authenticated push) and serves it |

> Station runs in Hollis Labs' own fleet and has no outside consumers. The code and docs describe what exists today; interfaces
> change without notice.

## How a unit works

- Every unit is its own identity provider: **identity API keys** (created offline with `<unit> identity key create`) carry scopes,
  for example `llm-gateway.chat`, `usage-monitor.read`, `station.<server id>`. There are no shared bearer tokens or client-key files.
- Units listen on loopback only and refuse any other `httphost.addr`. A local TLS proxy's public name is served when it is listed in
  the unit's `allowed_hosts` and the request comes from a loopback peer with exactly one `X-Forwarded-Proto: https`.
- State lives in one data directory per unit (`STATION_DATA_DIR`, `LLM_GATEWAY_DATA_DIR`, `USAGE_MONITOR_DATA_DIR`; the default is the
  XDG data home of the app name). Read-only commands (`validate`, `list`, `--help`, `secret check`) create nothing.
- Credentials are references (`keychain://ns/key`, `file://...`), never values. `<unit> secret set <ref>` reads the value from stdin;
  `secret check <ref>` says where it resolves from and how long it is.
- Configuration is a YAML override file named by `SUBSTRATE_CONFIG` (see `examples/`). The gateway's own configuration (providers,
  models, ledger, policy) is a revision in its database: `llm-gateway gateway-store stage|activate|show|status|history`.

## Quickstart (throwaway data directory)

```bash
go build -o bin/ ./cmd/...

export SUBSTRATE_CONFIG=$PWD/examples/usage-monitor.yaml
export USAGE_MONITOR_DATA_DIR=$(mktemp -d)/usage     # an explicit directory: the default is the live one
printf '%s\n' "a long passphrase" | bin/usage-monitor identity setup --username owner
bin/usage-monitor identity key create --name reader --scope usage-monitor.read --out /tmp/reader.key
bin/usage-monitor init          # a fresh unit with no history; an existing unit uses migrate-from instead, and serve needs one of the two
bin/usage-monitor validate
bin/usage-monitor serve
```

`scripts/first-run UNIT` does the same setup non-interactively for a new unit (the owner, one 0600 key file per client, and `init` for the usage monitor); it is not the cutover procedure, which migrates first (`docs/cutover-plan.md`). Each binary
has `--help`; `<unit> version` prints the build.

## Gateway

```bash
export SUBSTRATE_CONFIG=$PWD/examples/llm-gateway.yaml LLM_GATEWAY_DATA_DIR=$(mktemp -d)/gw
printf '%s' "$KEY" | bin/llm-gateway secret set keychain://openai/work
bin/llm-gateway gateway-store stage --input examples/gateway.json --expected 0 --actor me
bin/llm-gateway gateway-store activate --revision 1 --expected 0 --actor me
bin/llm-gateway validate && bin/llm-gateway serve
```

Relay routes are under `/api/llm-gateway/v1/` (clients keep their base URL); the management API is under
`/api/llm-gateway/v1/management/`. The usage ledger is SQLite in the service database, or PostgreSQL with separate runtime and
migration logins (`llm-gateway ledger-migrate`; `docs/conversion-notes.md` has the role layout and the proof).

## Documents

- `docs/architecture.md`: layout, scopes, the bind rule, the gateway and usage services.
- `docs/conversion-notes.md`: the port from the previous build: gaps, old-to-new map, rehearsals and proofs.
- `docs/cutover-plan.md`: the live move, step by step, with rollback.
- `docs/llm-gateway-adoption.md`, `docs/llm-gateway-management.md`, `docs/llm-gateway-standards.md`: using and operating the gateway.
- `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`.

## Development

```bash
go vet ./... && go test -race -count=1 ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 run ./...
go run github.com/hollis-labs/substrate/cmd/substrate pins check --app .
```

See `AGENTS.md` for the rules that are easy to break (explicit data directories in tests, the single write connection, the raw relay prefix).

## License

MIT License — see [`LICENSE`](./LICENSE). © 2026 Chrispian Burks / Hollis Labs.
