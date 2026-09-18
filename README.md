# Station

Station is a prototype built on
[`github.com/hollis-labs/mcp-host`](../../libs/mcp-host): the generic
dual-transport MCP plugin host library. Station itself is thin — flag
parsing, its own config, and two starter virtual MCPs — proving the
library is feature-complete enough to build a real product on top of.

## Status

Pre-v1, no consumers yet, built on `mcp-host v0.1.0`. Two working virtual
MCPs (`echo`, process-mode; `clock`, inprocess-mode) run simultaneously,
each independently reachable, each recovering from a real process kill —
see `cmd/station/smoke_test.go`.

## Quickstart

```bash
go build -o bin/echo-server ./plugins/echo-server
go build -o bin/clock-plugin ./plugins/clock-plugin
go build -o station ./cmd/station
./station -config station.yaml -http-addr :8080
```

This serves `echo` over stdio and `clock` over HTTP at `/clock`
simultaneously. Point an MCP client at station's stdio for `echo`'s tools,
or at `http://localhost:8080/clock` for `clock`'s.

## Depending on mcp-host

Station depends on the real, published
[`github.com/hollis-labs/mcp-host`](https://github.com/hollis-labs/mcp-host)
(`v0.1.0` as of this writing) — no `replace` directive needed. Both repos
are also listed in `~/dev/hollis-labs/go.work` for convenience when
developing them together locally; that workspace entry overrides
resolution to the local checkout, but `go.mod`'s own `require` line is a
real version, so a build outside this workspace (`GOWORK=off`, or any
other machine) fetches it from GitHub like any other dependency.

## Config

Station's own `station.yaml` configures its two starter virtual MCPs. See
`libs/mcp-host/README.md` for the full config schema, the process/inprocess
transport split, and how to add a new logical server — Station's config is
just an instance of that schema, nothing Station-specific about the format
itself.

## Package map

- `cmd/station/main.go` — the whole binary: flag parsing, config loading,
  then a single call into `mcphost.Run`. Everything else lives in the
  library.
- `plugins/echo-server` — process-mode starter virtual MCP: a real,
  standalone MCP server with one `echo` tool.
- `plugins/clock-plugin` — inprocess-mode starter virtual MCP: a
  plugin-sdk-dialect subprocess with one `now` tool.
- `cmd/station/main_test.go` — builds the real binary and drives it with a
  real MCP client over real stdio.
- `cmd/station/smoke_test.go` — the two-virtual-MCPs acceptance test:
  both plugins running simultaneously under one station process, each
  independently reachable, each killed and confirmed to recover.

Replace either starter plugin with Station's actual first real capability
whenever that's decided; nothing about the wiring changes.

## Commands

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

No CI, no Makefile — these three are the only gate. `go test ./...` builds
real binaries and spawns/kills real processes, so expect it to take several
seconds — that's the point, not a flake.

## Known gaps

Everything in `libs/mcp-host/README.md`'s "Known gaps and boundaries"
applies here too — Station doesn't work around any of them, it just
consumes the library as-is. Station-specific: the two starter plugins are
demonstration tools, not a real product surface yet; swapping them for
Station's actual first capability is the next real step, not blocked on
anything in the library today.
