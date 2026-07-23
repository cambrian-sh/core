#!/usr/bin/env python3
"""MCP stdio proxy exposing the τ²-bench **airline** tools to the Cambrian kernel,
forwarding every tool call to the airline HUB (``datasets/airline/airline_hub.py``) so
mutations land in the SAME env tau2's evaluator scores (Option A shared env).

Pure stdlib: MCP stdio (newline-delimited JSON-RPC: initialize/tools/list/tools/call) on
the kernel side; plain HTTP (urllib) to the hub on the other. Tool schemas are fetched
live from the hub's ``GET /tools``; ``tools/call`` forwards to the hub's ``POST /tool``.
The hub URL comes from ``AIRLINE_HUB_URL`` (default http://127.0.0.1:8899).

Registered in ``configs/mcp.json`` as a stdio server: ``python .../airline_proxy.py``.
"""
from __future__ import annotations

import json
import os
import sys
import urllib.request

# Generic across τ²-bench conversational domains: TAU2_HUB_URL + TAU2_MCP_NAME select the
# hub + the advertised MCP server name; AIRLINE_HUB_URL is kept as the back-compat default.
_HUB = (os.environ.get("TAU2_HUB_URL") or os.environ.get("AIRLINE_HUB_URL", "http://127.0.0.1:8899")).rstrip("/")
_MCP_NAME = os.environ.get("TAU2_MCP_NAME", "tau2-airline")


def _hub(path: str, payload: dict | None = None, method: str = "POST") -> dict:
    url = f"{_HUB}{path}"
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.loads(r.read() or b"{}")


def _tools_list() -> dict:
    try:
        return {"tools": _hub("/tools", method="GET").get("tools", [])}
    except Exception as exc:  # noqa: BLE001 - degrade to empty menu
        sys.stderr.write(f"airline_proxy: /tools failed: {exc}\n")
        return {"tools": []}


def _tools_call(params: dict) -> dict:
    name = params.get("name", "")
    args = params.get("arguments", {}) or {}
    try:
        resp = _hub("/tool", {"name": name, "args": args})
    except Exception as exc:  # noqa: BLE001
        return {"content": [{"type": "text", "text": f"hub error: {exc}"}], "isError": True}
    text = resp.get("result", "")
    if not isinstance(text, str):
        text = json.dumps(text)
    return {"content": [{"type": "text", "text": text}], "isError": bool(resp.get("error"))}


def _ok(mid, result):
    return {"jsonrpc": "2.0", "id": mid, "result": result}


def _handle(msg: dict) -> dict | None:
    method, mid = msg.get("method"), msg.get("id")
    if method == "initialize":
        proto = (msg.get("params") or {}).get("protocolVersion") or "2025-06-18"
        return _ok(mid, {"protocolVersion": proto, "capabilities": {"tools": {}},
                         "serverInfo": {"name": _MCP_NAME, "version": "0.1.0"}})
    if method in ("notifications/initialized", "initialized"):
        return None
    if method == "tools/list":
        return _ok(mid, _tools_list())
    if method == "tools/call":
        return _ok(mid, _tools_call(msg.get("params") or {}))
    if method == "ping":
        return _ok(mid, {})
    if mid is not None:
        return {"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": f"method not found: {method}"}}
    return None


def main() -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        resp = _handle(msg)
        if resp is not None:
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()


if __name__ == "__main__":
    main()
