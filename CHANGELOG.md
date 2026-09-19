# Changelog

All notable user-facing changes land here.

## Unreleased

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
- `plugins/atlas-server` and `internal/atlas/` — Station's first real
  capability: a process-mode MCP server for the Portfolio Atlas, read slice
  only. Eight tools scoped to the Atlas namespace in Tesseract, none taking a
  namespace and all annotated read-only: `atlas_guide`, `atlas_get`,
  `atlas_search`, `atlas_list`, `atlas_backlinks`, `atlas_review_queue`,
  `atlas_audit` and `atlas_tags`. Records are shown as stored; the audit reports
  where they disagree with the (draft) contract, each finding with its basis,
  and repairs nothing. Registered in `station.yaml` as the `atlas` logical
  server on `/atlas`. Tests run against a fake Tesseract and drive the real
  binaries through Station over stdio and HTTP.

### Changed

- Bumped to `mcp-host v0.2.0`: `inprocess` tool discovery is now
  manifest-driven (`station.yaml`'s `inprocess.tools` block), not a live
  RPC call. `plugins/clock-plugin` rewritten to use plugin-sdk's
  documented `subprocess.Serve` helper as a direct consequence.
