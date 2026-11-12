# Security policy

## Supported versions

Station is a pre-1.0 prototype (tagged `0.x`). Security fixes are made on
`main` and released as a new patch version. There are no backports.

## Report a vulnerability

Do not include an exploit, token, API key, or other sensitive material in a
public issue.

Use GitHub's private vulnerability-reporting flow when the repository's Security
tab offers it. If it is unavailable, contact a repository maintainer privately
through a contact channel published on the Hollis Labs organization or
maintainer profile. Include:

- the affected commit and operating system
- the unit and its `httphost.addr`, and which logical servers its configuration exposes
- reproduction steps and the security impact
- whether credentials or private records may have been exposed
- a safe way to contact you about coordination

Maintainers will acknowledge a private report, investigate it, and coordinate
disclosure; response times are best effort during the pre-release period.

## Deployment boundary

Station is three binaries (the MCP host, the LLM gateway, the usage monitor). Each is its own identity provider and runs with
authentication enforced.

- Every route except `/healthz` needs an identity API key whose scopes cover the route; a route no scope maps is refused. There are
  no shared bearer tokens and no unauthenticated mounts.
- Each unit refuses to listen on anything but a loopback address. A local TLS proxy's public name is served only when it is listed in
  the unit's `allowed_hosts`, the peer is loopback and the request carries exactly one `X-Forwarded-Proto: https`; other forwarding
  headers are dropped. There is no built-in TLS: terminate it in a trusted proxy.
- Owner setup is offline only (`<unit> identity setup`); `POST /api/setup` is refused.
- A logical server's tools are only as private as the data behind them. `visibility` is a label, not access control. Station
  spawns the processes its configuration names with the authority of the user running it: treat the configuration as executable.
- The relay passes an upstream provider's error bodies to the caller unchanged; it does not forward the caller's key upstream.

## Data at rest

Each unit keeps one data directory (database, signing key, secrets state), private to the user (mode 0600/0700). The gateway
stores revisions that hold secret *references* (never values) and key hashes; the usage monitor stores observations. The
gateway ledger is SQLite in that database or PostgreSQL with separate runtime and migration logins.

Credentials are references (`keychain://...`, `file://...`) resolved when needed and registered with the log redactor. Do not
commit values or paste them into arguments; `<unit> secret set` reads stdin.

## External data processors

Station makes no outbound calls beyond the upstreams you configure: the LLM providers a gateway revision names, the provider usage
endpoints the usage monitor's collectors call, and the MCP servers' own dependencies. Data sent there is governed by those services.

## Current security limitations

- no built-in TLS
- a process running as the unit's user can read the unit's data directory and its keyring entries
- pre-release: interfaces and behavior change without notice

These are deployment constraints. Operate within them or place Station behind controls that provide the missing boundary.
