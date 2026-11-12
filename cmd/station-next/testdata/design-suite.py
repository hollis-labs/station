#!/usr/bin/env python3
"""Stand-in for the design-suite entry point: a Python stdio MCP server (newline-delimited
JSON-RPC, standard library only) with one tool, probe, that reports facts about its environment
and never a value: set, length and the first eight hex digits of the SHA-256."""
import hashlib
import json
import os
import sys


def probe(args):
    out = {}
    for name in (args.get("names") or "").split(","):
        name = name.strip()
        if not name:
            continue
        v = os.environ.get(name)
        out[name] = {"set": bool(v), "len": len(v or ""), "sha8": hashlib.sha256((v or "").encode()).hexdigest()[:8]}
    return {"env": out, "args": sys.argv[1:], "python": True}


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    method, mid = msg.get("method"), msg.get("id")
    if mid is None:
        continue  # notifications
    if method == "initialize":
        result = {"protocolVersion": msg["params"].get("protocolVersion", "2025-03-26"),
                  "capabilities": {"tools": {"listChanged": False}},
                  "serverInfo": {"name": "design-suite-standin", "version": "0.1.0"}}
    elif method == "tools/list":
        result = {"tools": [{"name": "probe", "description": "env facts, never values",
                             "inputSchema": {"type": "object", "properties": {"names": {"type": "string"}}},
                             "annotations": {"readOnlyHint": True}}]}
    elif method == "tools/call":
        result = {"content": [{"type": "text", "text": json.dumps(probe(msg["params"].get("arguments") or {}))}]}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "no such method"}}), flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": mid, "result": result}), flush=True)
