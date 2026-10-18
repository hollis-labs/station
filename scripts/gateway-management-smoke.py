#!/usr/bin/env python3
"""Read-only management checks plus pure validation; no edits or inference."""
import argparse
import ipaddress
import json
import os
import stat
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def private_token(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "r", encoding="ascii") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError("token file must be regular and mode 0600")
        token = stream.read(4097).strip()
        if not token or len(token) > 4096 or any(c.isspace() for c in token):
            raise ValueError("invalid token file")
        return token


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True, help="API root ending in /v1/management")
    parser.add_argument("--token-file", required=True, help="regular mode-0600 file; no symlink")
    args = parser.parse_args()
    try:
        url = urllib.parse.urlsplit(args.base_url)
        try:
            loopback = ipaddress.ip_address(url.hostname).is_loopback
        except ValueError:
            loopback = False
        if (not url.hostname or url.username is not None or url.password is not None
                or url.query or url.fragment or not url.path.rstrip("/").endswith("/v1/management")
                or not (url.scheme == "https" or (url.scheme == "http" and loopback))):
            raise ValueError("use an HTTPS management URL or explicit loopback HTTP")
        token = private_token(args.token_file)
    except (OSError, ValueError, UnicodeError) as error:
        # Do not include file contents, URLs or exception text in diagnostics.
        print(json.dumps({"check": "inputs", "ok": False, "error_class": type(error).__name__}))
        return 1

    opener = urllib.request.build_opener(NoRedirect)
    results = []

    def call(name, path, key=token, expected=200, body=None, check=lambda value: True):
        headers = {"Accept": "application/json"}
        if key is not None:
            headers["Authorization"] = "Bearer " + key
        if body is not None:
            headers["Content-Type"] = "application/json"
        result = {"check": name, "ok": False}
        value = None
        try:
            request = urllib.request.Request(
                args.base_url.rstrip("/") + path, headers=headers,
                data=json.dumps(body).encode() if body is not None else None,
            )
            try:
                response = opener.open(request, timeout=30)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                result["status"] = response.code
                raw = response.read(8 * 1024 * 1024 + 1)
                if len(raw) > 8 * 1024 * 1024:
                    raise ValueError("response exceeded probe cap")
                value = json.loads(raw)
                result["ok"] = (response.code == expected
                                and response.headers.get("Cache-Control") == "no-store"
                                and isinstance(value, dict) and bool(check(value)))
        except Exception as error:
            result["error_class"] = type(error).__name__
        results.append(result)
        print(json.dumps(result), flush=True)
        return value if result["ok"] else None

    call("missing credential", "/status", key=None, expected=401)
    call("invalid credential", "/status", key="invalid-probe-credential", expected=401)
    call("status", "/status", check=lambda v: v.get("apply_mode") == "service_reload"
         and all(isinstance(v.get(k), int) and v[k] > 0
                 for k in ("latest_revision", "selected_revision", "running_revision")))
    config = call("configuration", "/config", check=lambda v: v.get("schema_version") == 1
                  and isinstance(v.get("gateway"), dict) and "clients" not in v)
    call("client metadata", "/clients", check=lambda v: isinstance(v.get("clients"), list)
         and all(set(c) == {"id", "scopes"} for c in v["clients"]))
    for resource in ("providers", "models", "policy", "budgets"):
        call(resource, "/" + resource, check=lambda v, key=resource: key in v and "revision" in v)
    for resource in ("revisions", "activations"):
        call(resource, "/" + resource, check=lambda v: isinstance(v.get("items"), list)
             and all("document" not in item and "clients" not in item for item in v["items"]))
    if config is not None:
        call("pure validation", "/validate", body={"schema_version": config["schema_version"],
                                                   "gateway": config["gateway"]},
             check=lambda v: v.get("valid") is True and v.get("runtime_checked") is False
             and v.get("validation") == "configuration_only")
    return 0 if all(result["ok"] for result in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
