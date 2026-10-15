# Security policy

## Supported versions

Station is a pre-release prototype with no tagged release. Security fixes are
made on `main`.

## Report a vulnerability

Do not include an exploit, token, API key, or other sensitive material in a
public issue.

Use GitHub's private vulnerability-reporting flow when the repository's Security
tab offers it. If it is unavailable, contact a repository maintainer privately
through a contact channel published on the Hollis Labs organization or
maintainer profile. Include:

- the affected commit and operating system
- the `-http-addr` in use and which logical servers `station.yaml` exposes
- reproduction steps and the security impact
- whether credentials or private records may have been exposed
- a safe way to contact you about coordination

Maintainers will acknowledge a private report, investigate it, and coordinate
disclosure; response times are best effort during the pre-release period.

## Deployment boundary

Station hosts MCP servers over stdio and HTTP. As shipped, the HTTP endpoints in
`station.yaml` are **unauthenticated** (`bearer_tokens: []`).

- `station serve` defaults to `-http-addr :8080` in the README examples, which
  binds **all interfaces**. Run `station -http-addr 127.0.0.1:8080` unless you
  have your own access control in front.
- Set `bearer_tokens` on any endpoint before serving beyond loopback.
- There is no built-in TLS. Use a trusted TLS-terminating proxy, VPN or SSH
  tunnel for remote access.
- A plugin's tools are only as private as the data behind them. For example the
  Atlas server (now in `station-plugins`) serves **`visibility:private`**
  records; `visibility` is a label, not access control, and Tesseract fences
  nothing under `project/*`. Read each plugin's README before exposing it.
- Station spawns the plugin processes `station.yaml` names, with the authority
  of the user running it. Treat the config file as executable configuration.

## Data at rest

Station keeps no database of its own; any data a plugin holds is described in
that plugin's README.

Credentials: prefer `keychain://…` references in `station.yaml` and environment
variables over literal values, and do not commit tokens.

## External data processors

Station makes no outbound calls of its own beyond the upstreams you configure:
any API a plugin such as `github-pilot` is pointed at.
Data sent there is governed by those services.

## Current security limitations

- shipped HTTP endpoints are unauthenticated by default
- README examples bind all interfaces
- no built-in TLS
- pre-release: interfaces and behavior change without notice

These are deployment constraints, not hidden roadmap promises. Operate within
them or place Station behind controls that provide the missing boundary.
