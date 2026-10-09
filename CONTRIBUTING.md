# Contributing to Station

Station is a pre-release prototype built on
[`mcp-host`](https://github.com/hollis-labs/libs/tree/main/plugin-mcp/mcp-host). Contributions are
welcome; the bar is correct, minimal, well-tested Go.

## Before you start

- `README.md` has the quickstart, config and package map; `AGENTS.md` is the
  fastest orientation to the code layout and boundaries.
- Station stays a thin wrapper: host logic (transports, supervision, config
  schema) belongs in `mcp-host`, not here.
- License: see [`LICENSE`](LICENSE) and [`TRADEMARK.md`](TRADEMARK.md). Code
  contributions are accepted under the repository's MIT license; the Station and
  Hollis Labs names remain protected marks.

## Workflow

1. **Open an issue or discussion first** for anything larger than a small fix.
2. **Branch from `main`** using `feat/<topic>`, `fix/<topic>` or `docs/<topic>`.
3. **Change one thing per branch** and keep commits small and coherent.
4. **Run the checks** before opening a pull request (also run by the Go workflow):

   ```bash
   gofmt -l .
   go vet ./...
   go test -race -count=1 ./...
   ```

   The tests build and spawn the real binaries, so expect a few seconds.
5. **Open a pull request.** Say what changed, what it deliberately leaves alone,
   and the commands you ran with their results. A maintainer will review it.

Conventional Commits (`feat(scope): …`, `fix: …`, `docs: …`) are welcome; use
the body to explain *why*.

## Security

Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md).
