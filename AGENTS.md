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

## Atlas server

`plugins/atlas-server` (a thin `main`) and `internal/atlas/` are Station's
first real capability: an MCP server for the Portfolio Atlas, the curated
record layer in Tesseract under `project/atlas/knowledge`. It is a product
plugin, so it lives here; host logic still belongs in `libs/mcp-host`.

- **The contract table is the one source.** `internal/atlas/contract` holds the
  classes, key scheme and vocabularies as data; the audit, the `atlas_guide`
  tool and the tool schemas all read it, so don't restate a vocabulary
  anywhere else in the code. Each term says whether it is in the contract,
  proposed, or a convention, because the contract (the `ATLAS-META-*` records
  in Tesseract) is still a draft. An audit finding says what it is measured
  against and never repairs anything: whether the record or the contract is
  wrong is Chrispian's call.
- **Atlas rules stay here; Tesseract stays agnostic.** Don't ask Tesseract for
  Atlas-specific behavior. Its generic gaps (a duplicate key and a stale
  `supersedes` are both accepted silently, and nothing under `project/*` is
  write-fenced) are why only the read slice exists. The reasoning and the
  asks are in Tesseract: `project/atlas/memory/decisions/atlas_mcp_target_architecture`
  and `project/tesseract/workspace/handoff/atlas_write_path_needs_2026_09_19`.
  The write slice and a read-only projection instance are separate Torque
  tasks tagged `atlas-mcp`.
- **Every tool is read-only and takes no namespace.** Their annotations say so
  and a test enforces both. A tool that writes is the write slice, and is gated
  as above.
- **Tests use the fake Tesseract** (`internal/atlas/atlastest`), which speaks
  the HTTP door's verified shapes, so no test needs a daemon. Don't assert
  counts of the live register, or that the code agrees with agent-setup's
  atlas skill files: both drift by design and reconcile at a release.
- **Exposure.** `/atlas` in `station.yaml` is unauthenticated and Atlas holds
  `visibility:private` records; see the README before serving beyond loopback.
