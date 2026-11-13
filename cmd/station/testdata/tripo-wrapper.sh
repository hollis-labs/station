#!/usr/bin/env bash
# Stand-in for the tripo shell wrapper: records that it ran, then execs the real server with the
# arguments it was given (production passes: mcp --toolset full).
exec "$FAKECHILD" "$@"
