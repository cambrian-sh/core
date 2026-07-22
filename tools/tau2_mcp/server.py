#!/usr/bin/env python3
"""Minimal MCP stdio server exposing the τ²-bench **mock** domain tools to the Cambrian
kernel, so an agent can actually accomplish the mock-domain tasks
(``datasets/tau2/mock.jsonl``) instead of improvising with the filesystem.

Faithful port of ``tau2.domains.mock.tools.MockTools``
(``sierra-research/tau2-bench``):

- ``create_task(user_id, title, description?)`` → the created task (WRITE)
- ``get_users()`` → all users (READ)
- ``update_task_status(task_id, status)`` → the updated task (WRITE)
- ``transfer_to_human_agents(summary)`` → confirmation (GENERIC)

State is seeded from the real mock ``db.json`` and kept in memory. On every mutation
the current DB is written to ``$TAU2_MOCK_DB_OUT`` (when set) so a benchmark can do a
true post-run state check instead of the answer-text proxy.

Pure stdlib — implements just the MCP subset the Cambrian client uses (``initialize`` →
``tools/list`` → ``tools/call``) over newline-delimited JSON-RPC 2.0 on stdio. No
dependency on the ``mcp`` package (avoids version coupling). Registered in
``configs/mcp.json`` as a stdio server: ``python tools/tau2_mcp/server.py``.
"""
from __future__ import annotations

import copy
import json
import os
import sys
from typing import Any

# Seed DB — the real mock-domain db.json (data/tau2/domains/mock/db.json).
_SEED: dict[str, Any] = {
    "tasks": {
        "task_1": {
            "task_id": "task_1",
            "title": "Test task",
            "description": "A test task",
            "status": "pending",
        }
    },
    "users": {
        "user_1": {"user_id": "user_1", "name": "Test User", "tasks": ["task_1"]},
    },
}

_DB: dict[str, Any] = copy.deepcopy(_SEED)
_VALID_STATUS = {"pending", "in_progress", "completed", "cancelled"}


def _persist() -> None:
    out = os.environ.get("TAU2_MOCK_DB_OUT", "").strip()
    if not out:
        return
    try:
        with open(out, "w", encoding="utf-8") as f:
            json.dump(_DB, f, indent=2, sort_keys=True)
    except OSError:
        pass  # persistence is best-effort; never break a tool call


# ── tool implementations ─────────────────────────────────────────────────────

def create_task(user_id: str, title: str, description: str | None = None) -> dict[str, Any]:
    if user_id not in _DB["users"]:
        raise ValueError(f"User {user_id} not found")
    task_id = f"task_{len(_DB['tasks']) + 1}"
    task = {"task_id": task_id, "title": title, "description": description, "status": "pending"}
    _DB["tasks"][task_id] = task
    _DB["users"][user_id]["tasks"].append(task_id)
    _persist()
    return task


def get_users() -> list[dict[str, Any]]:
    return list(_DB["users"].values())


def update_task_status(task_id: str, status: str) -> dict[str, Any]:
    if task_id not in _DB["tasks"]:
        raise ValueError(f"Task {task_id} not found")
    if status not in _VALID_STATUS:
        raise ValueError(f"Invalid status {status!r}; valid: {sorted(_VALID_STATUS)}")
    _DB["tasks"][task_id]["status"] = status
    _persist()
    return _DB["tasks"][task_id]


def transfer_to_human_agents(summary: str) -> str:
    del summary
    return "Transfer successful"


_TOOLS: dict[str, dict[str, Any]] = {
    "create_task": {
        "fn": create_task,
        "description": "Create a new task for a user in the mock task tracker. Returns the created task.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "user_id": {"type": "string", "description": "ID of the user (e.g. user_1)"},
                "title": {"type": "string", "description": "Title of the task"},
                "description": {"type": "string", "description": "Optional description"},
            },
            "required": ["user_id", "title"],
        },
    },
    "get_users": {
        "fn": get_users,
        "description": "List all users in the mock task tracker (with their task ids).",
        "inputSchema": {"type": "object", "properties": {}},
    },
    "update_task_status": {
        "fn": update_task_status,
        "description": "Update a task's status (pending/in_progress/completed/cancelled). Returns the updated task.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "task_id": {"type": "string"},
                "status": {"type": "string", "enum": sorted(_VALID_STATUS)},
            },
            "required": ["task_id", "status"],
        },
    },
    "transfer_to_human_agents": {
        "fn": transfer_to_human_agents,
        "description": "Transfer the user to a human agent with a summary. Use only if the task cannot be solved with the available tools.",
        "inputSchema": {
            "type": "object",
            "properties": {"summary": {"type": "string"}},
            "required": ["summary"],
        },
    },
}


# ── MCP JSON-RPC plumbing ─────────────────────────────────────────────────────

def _tools_list() -> dict[str, Any]:
    return {
        "tools": [
            {"name": name, "description": t["description"], "inputSchema": t["inputSchema"]}
            for name, t in _TOOLS.items()
        ]
    }


def _tools_call(params: dict[str, Any]) -> dict[str, Any]:
    name = params.get("name", "")
    args = params.get("arguments", {}) or {}
    tool = _TOOLS.get(name)
    if tool is None:
        return {"content": [{"type": "text", "text": f"unknown tool {name!r}"}], "isError": True}
    try:
        result = tool["fn"](**args)
    except (ValueError, TypeError) as exc:
        return {"content": [{"type": "text", "text": f"error: {exc}"}], "isError": True}
    text = result if isinstance(result, str) else json.dumps(result)
    return {"content": [{"type": "text", "text": text}], "isError": False}


def _handle(msg: dict[str, Any]) -> dict[str, Any] | None:
    method = msg.get("method")
    mid = msg.get("id")
    if method == "initialize":
        params = msg.get("params") or {}
        proto = params.get("protocolVersion") or "2025-06-18"
        return _ok(mid, {
            "protocolVersion": proto,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "tau2-mock", "version": "0.1.0"},
        })
    if method in ("notifications/initialized", "initialized"):
        return None  # notification: no response
    if method == "tools/list":
        return _ok(mid, _tools_list())
    if method == "tools/call":
        return _ok(mid, _tools_call(msg.get("params") or {}))
    if method == "ping":
        return _ok(mid, {})
    if mid is not None:
        return {"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": f"method not found: {method}"}}
    return None


def _ok(mid: Any, result: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": mid, "result": result}


def main() -> None:
    _persist()  # write the seed once at startup
    stdout = sys.stdout
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
            stdout.write(json.dumps(resp) + "\n")
            stdout.flush()


if __name__ == "__main__":
    main()
