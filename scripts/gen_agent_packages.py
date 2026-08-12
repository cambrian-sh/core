#!/usr/bin/env python3
"""PLAT-01 twin for JS agents (ADR-0125 D6): keep each JS agent unit's
package.json honest against its actual imports, and maintain the union
workspace manifest (agents/package.json) whose single `bun install` at the
agents root is the union-lockfile analog (bun.lock) of requirements.lock.

Units mirror gen_agent_requirements.py's shapes, JS-flavoured:
  - a directory under agents/ shipping package.json + agent.ts|agent.js
    (owns its own dependencies);
  - a top-level agents/*agent.ts|js single file (its external imports belong
    to the ROOT agents/package.json, the way top-level Python agents share the
    union lock).

Modes:
  generate (default) — add missing externals to the owning package.json with
    "*" (never clobbering an existing semver pin), maintain the root
    workspaces array, and warn on declared-but-unused deps (types/runtime
    plugins make removal unsafe, so never auto-remove).
  --check — fail (exit 1) when an import is undeclared or the root manifest
    drifts from a fresh generation. With zero JS units both modes are no-ops
    and no root package.json is ever created.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AGENTS = ROOT / "agents"

IMPORT_RE = re.compile(
    r"""(?:^|\s)import\s+(?:[^'"]*?\bfrom\s+)?['"]([^'"]+)['"]"""
    r"""|require\(\s*['"]([^'"]+)['"]\s*\)"""
    r"""|import\(\s*['"]([^'"]+)['"]\s*\)""",
    re.MULTILINE,
)

# Node builtins importable without the node: prefix (bun implements the same
# surface). Anything prefixed node:/bun: is excluded before this list matters.
NODE_BUILTINS = {
    "assert", "async_hooks", "buffer", "child_process", "cluster", "console",
    "constants", "crypto", "dgram", "diagnostics_channel", "dns", "domain",
    "events", "fs", "http", "http2", "https", "inspector", "module", "net",
    "os", "path", "perf_hooks", "process", "punycode", "querystring",
    "readline", "repl", "stream", "string_decoder", "sys", "timers", "tls",
    "trace_events", "tty", "url", "util", "v8", "vm", "wasi",
    "worker_threads", "zlib", "test",
}

SOURCE_EXTS = {".ts", ".tsx", ".js", ".mjs", ".cjs"}


def _package_name(spec: str) -> str | None:
    """Bare import spec -> package name, or None for non-external imports."""
    if spec.startswith((".", "/", "node:", "bun:")):
        return None
    parts = spec.split("/")
    name = "/".join(parts[:2]) if spec.startswith("@") and len(parts) >= 2 else parts[0]
    if name in NODE_BUILTINS or name == "bun":
        return None
    return name


def _is_test(p: Path) -> bool:
    stem = p.name
    return ".test." in stem or ".spec." in stem or stem.startswith("test_")


def _imports_of(files: list[Path]) -> set[str]:
    out: set[str] = set()
    for f in files:
        try:
            text = f.read_text(encoding="utf-8")
        except OSError:
            continue
        for m in IMPORT_RE.finditer(text):
            spec = next(g for g in m.groups() if g)
            if (name := _package_name(spec)) is not None:
                out.add(name)
    return out


def _unit_sources(unit_dir: Path) -> list[Path]:
    files = []
    for p in unit_dir.rglob("*"):
        if "node_modules" in p.parts:
            continue
        if p.is_file() and p.suffix in SOURCE_EXTS and not _is_test(p):
            files.append(p)
    return files


def _declared(pkg: dict) -> set[str]:
    out: set[str] = set()
    for key in ("dependencies", "devDependencies", "peerDependencies", "optionalDependencies"):
        out.update(pkg.get(key, {}) or {})
    return out


def _read_json(p: Path) -> dict:
    return json.loads(p.read_text(encoding="utf-8"))


def _write_json(p: Path, data: dict) -> None:
    p.write_text(json.dumps(data, indent=2, sort_keys=False) + "\n", encoding="utf-8")


def discover_units() -> tuple[list[Path], list[Path]]:
    """Returns (package_unit_dirs, top_level_single_files)."""
    pkg_units: list[Path] = []
    singles: list[Path] = []
    if not AGENTS.is_dir():
        return pkg_units, singles
    for p in sorted(AGENTS.rglob("package.json")):
        if "node_modules" in p.parts or p.parent == AGENTS:
            continue
        d = p.parent
        if (d / "agent.ts").is_file() or (d / "agent.js").is_file():
            pkg_units.append(d)
    for p in sorted(AGENTS.iterdir()):
        if p.is_file() and p.suffix in (".ts", ".js") and p.stem.endswith("agent") and not _is_test(p):
            singles.append(p)
    return pkg_units, singles


def run(check: bool) -> int:
    pkg_units, singles = discover_units()
    if not pkg_units and not singles:
        if not check:
            print("gen_agent_packages: no JS agent units found - nothing to do")
        return 0

    failed = False

    # Per-package units: imports must be declared in the unit's own package.json.
    for unit in pkg_units:
        pkg_path = unit / "package.json"
        pkg = _read_json(pkg_path)
        imports = _imports_of(_unit_sources(unit))
        declared = _declared(pkg)
        missing = sorted(imports - declared)
        unused = sorted(declared - imports)
        rel = unit.relative_to(ROOT)
        if missing:
            if check:
                print(f"DRIFT {rel}: imports not declared in package.json: {', '.join(missing)}")
                failed = True
            else:
                deps = pkg.setdefault("dependencies", {})
                for name in missing:
                    deps[name] = "*"
                pkg["dependencies"] = dict(sorted(deps.items()))
                _write_json(pkg_path, pkg)
                print(f"updated {rel}/package.json: +{', '.join(missing)}")
        if unused:
            print(f"note {rel}: declared but not imported (left alone): {', '.join(unused)}")

    # Top-level single files share the ROOT manifest, like Python's union lock.
    single_imports = _imports_of(singles)
    root_path = AGENTS / "package.json"
    workspaces = sorted(str(u.relative_to(AGENTS)).replace("\\", "/") for u in pkg_units)
    need_root = bool(single_imports) or bool(workspaces) or root_path.is_file()

    if need_root:
        root = _read_json(root_path) if root_path.is_file() else {
            "name": "cambrian-agents",
            "private": True,
        }
        expected = dict(root)
        if workspaces:
            expected["workspaces"] = workspaces
        else:
            expected.pop("workspaces", None)
        deps = dict(expected.get("dependencies", {}) or {})
        for name in sorted(single_imports - set(deps)):
            deps[name] = "*"
        if deps:
            expected["dependencies"] = dict(sorted(deps.items()))

        if expected != root or not root_path.is_file():
            if check:
                print("DRIFT agents/package.json: stale - run `make agent-packages`")
                failed = True
            else:
                _write_json(root_path, expected)
                print("updated agents/package.json")

    if check and not failed:
        print("agent-packages-check: OK")
    return 1 if failed else 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--check", action="store_true", help="fail on drift instead of writing")
    args = ap.parse_args()
    return run(check=args.check)


if __name__ == "__main__":
    sys.exit(main())
