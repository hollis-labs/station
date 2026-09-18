# Station

Station is a prototype built on `github.com/hollis-labs/mcp-host` (the
generic dual-transport MCP plugin host library, `libs/mcp-host` in this
portfolio). Station itself stays thin — see `README.md` for the package
map — and defers everything host-shaped to the library.

## Where Station is

Not released, not deployed, no consumers. Public repo from the start —
built in the open as a prototype proving `mcp-host` is feature-complete
enough to build on, not yet a product with users. Chrispian decides when
that changes — there are no criteria to meet and no date.

So **release readiness is a direction, not a phase.** Security, testing and
release prep are ordinary work competing on merit with features, bug fixes
and everything else, sequenced by Chrispian's direction each session. A
`public-release` tag names the subject, never the urgency, and a board
sorted by it is not a plan.

The reasoning is `~/dev/projects/agent-setup/docs/what-a-check-may-assert.md`,
*Tighten at the first real consumer*: until someone outside the project can
be broken by a regression, the cost of a regression is one session noticing.

**Where this stops.** This is not licence to skip verification. Data
integrity, security boundaries, and anything that can silently lose work
still get the real treatment — what changes is what gets *scheduled*, not
how carefully it is done once it is.

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
(`v0.2.0`) — no `replace` directive. This repo is also listed in
`~/dev/hollis-labs/go.work` alongside `libs/mcp-host` for convenience when
developing both together; that's a local override only, not something
`go.mod` itself depends on. Bumping the `mcp-host` version here should
follow a real tag on that repo, not a local, untagged change.

MCP is the only agent-facing interface — verified against Tangent's own
source and docs, which state this explicitly for itself. `cmd/station`'s
`validate`/`list`/`version` subcommands are an ops/inspection surface, not
a second agent API; don't add MCP tools (or an HTTP management API) for
operating Station itself (listing/adding/restarting logical servers). If
that need ever becomes real, look at how Tangent's `tangent plugin
{install,remove,list,dir}` stays CLI-only before reaching for an MCP tool
instead.

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
