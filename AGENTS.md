# Station

Station is a prototype built on `github.com/hollis-labs/libs/plugin-mcp/mcp-host` (the
generic dual-transport MCP plugin host library). Station itself stays thin — see `README.md` for the package
map — and defers everything host-shaped to the library.

## Where Station is

Not released, not deployed, no consumers. It is a public prototype, built in
the open to prove `mcp-host` is feature-complete enough to build on. Interfaces
can change without notice.

Being pre-release does not license skipping verification: data integrity,
security boundaries and anything that can silently lose work still get the real
treatment.

## Start Here

- `README.md` covers the quickstart, config, and package map.
- `cmd/station/main.go` is the entire binary — read it before adding
  anything to `main`, since it should stay a thin wrapper around
  `mcphost.Run`, not grow its own host logic.
- The `mcp-host` repository's `README.md` and `AGENTS.md` (github.com/hollis-labs/libs/plugin-mcp/mcp-host)
  own everything about the host itself:
  config schema, transport modes, known gaps. Read those before assuming
  a limitation is Station's to fix.

## Commands

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

The Go workflow runs these checks and the real-child tests on Linux. There is
no Makefile.

`go test ./...` builds the real `station` binary and both real plugin
binaries via `go build`, then drives them as subprocesses with a genuine
MCP client, killing and confirming recovery of each — expect it to take
several seconds and spawn/kill real processes; that's the point, not a
flake.

## Boundaries

`go.mod` requires the published `github.com/hollis-labs/libs/plugin-mcp`
module. All SDK/MCP/host imports use its package roots. No committed `replace`
or `go.work`; final consumer gates use the actual published release. Clock's
explicit owner policy is in `cmd/station/plugin_policy.go`; plugin-host owns
inprocess subprocess lifecycle, protocol and grants validation. Do not duplicate
its supervisor or wire codec here. `-plugin-state-dir` is required for clock.

MCP is the only agent-facing interface. `cmd/station`'s
`validate`/`list`/`version` subcommands are an ops/inspection surface, not
a second agent API; don't add MCP tools (or an HTTP management API) for
operating Station itself (listing/adding/restarting logical servers). If
that need ever becomes real, prefer extending the CLI over adding an MCP tool.

Station should not reimplement anything `mcp-host` already owns —
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
first real capability: an MCP server for the Portfolio Atlas, a curated record
layer stored in a Tesseract instance under the `project/atlas/knowledge`
namespace. It is a product plugin, so it lives here; host logic still belongs
in `mcp-host`.

- **The contract table is the one source.** `internal/atlas/contract` holds the
  classes, key scheme and vocabularies as data; the audit, the `atlas_guide`
  tool and the tool schemas all read it, so don't restate a vocabulary
  anywhere else in the code. Each term says whether it is in the contract,
  proposed, or a convention, because the contract is still a draft. An audit
  finding says what it is measured against and never repairs anything.
- **Atlas rules stay here; Tesseract stays agnostic.** Don't ask Tesseract for
  Atlas-specific behavior. Its generic gaps (a duplicate key and a stale
  `supersedes` are both accepted silently, and nothing under `project/*` is
  write-fenced) are why only the read slice exists.
- **Every tool is read-only and takes no namespace.** Their annotations say so
  and a test enforces both. A tool that writes belongs to a future write slice.
- **Tests use the fake Tesseract** (`internal/atlas/atlastest`), which speaks
  the HTTP API's verified shapes, so no test needs a daemon. Don't assert
  counts of a live register: they drift by design.
- **Exposure.** `/atlas` in `station.yaml` is unauthenticated and Atlas holds
  `visibility:private` records; see the README and `SECURITY.md` before
  serving beyond loopback.

Open a pull request for changes; a maintainer will review it. See
`CONTRIBUTING.md`.
