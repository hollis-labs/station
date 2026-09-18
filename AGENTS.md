# Station

Station is a prototype built on `github.com/hollis-labs/mcp-host` (the
generic dual-transport MCP plugin host library, `libs/mcp-host` in this
portfolio). Station itself stays thin — see `README.md` for the package
map — and defers everything host-shaped to the library.

## Start Here

- `README.md` covers the quickstart, config, and package map.
- `cmd/station/main.go` is the entire binary — read it before adding
  anything to `main`, since it should stay a thin wrapper around
  `mcphost.Run`, not grow its own host logic.
- `libs/mcp-host/README.md` and `libs/mcp-host/AGENTS.md` (sibling repo,
  `../../libs/mcp-host` from here) own everything about the host itself:
  config schema, transport modes, known gaps. Read those before assuming
  a limitation is Station's to fix.

## Commands

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

There is no CI workflow and no Makefile in this repo, so these are the only
gate (same convention as `mcp-host`/`go-mcp`).

`go test ./...` builds the real `station` binary and both real plugin
binaries via `go build`, then drives them as subprocesses with a genuine
MCP client, killing and confirming recovery of each — expect it to take
several seconds and spawn/kill real processes; that's the point, not a
flake.

## Boundaries

`go.mod` requires the real, published `github.com/hollis-labs/mcp-host`
(`v0.1.0`) — no `replace` directive. This repo is also listed in
`~/dev/hollis-labs/go.work` alongside `libs/mcp-host` for convenience when
developing both together; that's a local override only, not something
`go.mod` itself depends on. Bumping the `mcp-host` version here should
follow a real tag on that repo, not a local, untagged change.

Station should not reimplement anything `libs/mcp-host` already owns —
config parsing, the transport interface, supervision, serving. If a change
here starts looking like host logic (a new transport mode, a new way to
expose a logical server, config schema changes), it almost certainly
belongs in the library instead, as a change that benefits every future
consumer, not just Station.

`plugins/echo-server` and `plugins/clock-plugin` are demonstration
starters, not Station's real product surface — don't read them as evidence
of what Station is "for." They exist to prove the library works
end-to-end, mirroring the library's own `examples/plugins/` (which Station
does not depend on; these are Station's own copies, since a real
consumer's product tools shouldn't live inside the library's example
code).
