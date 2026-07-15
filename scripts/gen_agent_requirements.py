#!/usr/bin/env python3
"""PLAT-01 — auto-generate per-agent requirements.txt + the union lockfile.

Deterministic, so the committed files can be drift-checked in CI (regenerate →
`git diff --exit-code`). No pip-tools needed: dependencies and their pins are
read from the *installed* agent Python via ``importlib.metadata``.

For each agent (``agents/system/<name>/`` package, and each top-level
``agents/*_agent.py``):

  1. AST-parse every non-test ``.py`` and collect top-level imported modules.
  2. Drop stdlib and modules that resolve to a local/sibling file (an agent's own
     helper modules are not PyPI deps).
  3. Map the remaining import names to installed distributions and pin them.

Outputs:
  * ``agents/system/<name>/requirements.txt`` — that agent's direct deps, pinned.
  * ``agents/requirements.lock`` — the transitive closure of ALL agents' deps,
    each pinned ``==`` to the installed version (the reproducible union lock).

The SDK is emitted as ``cambrian-agent-sdk>=0.1,<0.2`` (a released range, not a
local pin) so a clean install resolves it from PyPI / a local wheel.

Usage:
  python scripts/gen_agent_requirements.py [--check]   # --check = fail on drift
"""
from __future__ import annotations

import argparse
import ast
import sys
from importlib import metadata as md
from pathlib import Path

CORE = Path(__file__).resolve().parents[1]
AGENTS = CORE / "agents"
SDK_IMPORT = "cambrian_agent_sdk"
SDK_REQ = "cambrian-agent-sdk>=0.1,<0.2"

# Modules that are import-visible but are the local source tree, never PyPI deps.
LOCAL_ROOTS = {"cambrian_agent_sdk"}  # handled specially (SDK), excluded from pins

_STDLIB = set(sys.stdlib_module_names) | {"__future__"}


def _agent_units() -> list[tuple[str, Path, list[Path]]]:
    """(name, out_dir, [py files]) for each agent — system packages + top-level."""
    units: list[tuple[str, Path, list[Path]]] = []
    sysdir = AGENTS / "system"
    if sysdir.is_dir():
        for d in sorted(p for p in sysdir.iterdir() if p.is_dir()):
            pys = [f for f in d.rglob("*.py") if not _is_test(f)]
            if any(f.name == "agent.py" for f in pys):
                units.append((d.name, d, pys))
    for f in sorted(AGENTS.glob("*_agent.py")):
        if not _is_test(f):
            units.append((f.stem, AGENTS, [f]))
    return units


def _is_test(f: Path) -> bool:
    return f.name.startswith("test_") or f.name.endswith("_test.py") or f.name == "conftest.py"


def _top_imports(py_files: list[Path]) -> set[str]:
    names: set[str] = set()
    for f in py_files:
        try:
            tree = ast.parse(f.read_text(encoding="utf-8"), filename=str(f))
        except (SyntaxError, UnicodeDecodeError):
            continue
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for a in node.names:
                    names.add(a.name.split(".", 1)[0])
            elif isinstance(node, ast.ImportFrom):
                if node.level == 0 and node.module:  # absolute import only
                    names.add(node.module.split(".", 1)[0])
    return names


def _local_module_roots(unit_dirs: list[Path]) -> set[str]:
    """Top-level module names that are local files/packages under the agents tree
    (an agent's sibling helpers), which must never be treated as PyPI deps."""
    roots: set[str] = set()
    for d in unit_dirs:
        for f in d.glob("*.py"):
            roots.add(f.stem)
        for sub in d.iterdir():
            if sub.is_dir() and (sub / "__init__.py").exists():
                roots.add(sub.name)
    return roots


def _import_to_dists() -> dict[str, list[str]]:
    return md.packages_distributions()


def _pin(dist: str) -> str | None:
    try:
        return f"{dist}=={md.version(dist)}"
    except md.PackageNotFoundError:
        return None


def _direct_reqs(imports: set[str], local_roots: set[str], i2d: dict[str, list[str]]) -> tuple[list[str], set[str]]:
    """Return (sorted requirement lines, set of distribution names) for one agent."""
    reqs: set[str] = set()
    dists: set[str] = set()
    for name in sorted(imports):
        if name in _STDLIB or name in local_roots:
            continue
        if name == SDK_IMPORT:
            reqs.add(SDK_REQ)
            continue
        for dist in i2d.get(name, []):
            if dist.replace("-", "_") in LOCAL_ROOTS:
                continue
            pin = _pin(dist)
            if pin:
                reqs.add(pin)
                dists.add(dist)
    return sorted(reqs, key=str.lower), dists


def _closure(seed: set[str]) -> set[str]:
    """Transitive dependency closure over installed metadata (dist names)."""
    seen: set[str] = set()
    stack = list(seed)
    while stack:
        d = stack.pop()
        key = md.metadata(d)["Name"] if _exists(d) else d
        if key in seen:
            continue
        seen.add(key)
        for r in md.requires(d) or []:
            dep = _req_name(r)
            if dep and _exists(dep):
                stack.append(dep)
    return seen


def _exists(dist: str) -> bool:
    try:
        md.version(dist)
        return True
    except md.PackageNotFoundError:
        return False


def _req_name(req: str) -> str | None:
    """Extract the distribution name from a requirement string, skipping
    optional (extra-gated) deps so the lock stays to what agents actually pull."""
    if "extra ==" in req:  # optional dependency behind an extra — skip
        return None
    name = ""
    for ch in req.strip():
        if ch.isalnum() or ch in "-_.":
            name += ch
        else:
            break
    return name or None


HEADER = "# AUTO-GENERATED by scripts/gen_agent_requirements.py — do not edit by hand.\n"


def generate() -> dict[Path, str]:
    units = _agent_units()
    i2d = _import_to_dists()
    all_dists: set[str] = set()
    files: dict[Path, str] = {}

    for name, out_dir, pys in units:
        local_roots = _local_module_roots([out_dir]) | {name}
        imports = _top_imports(pys)
        reqs, dists = _direct_reqs(imports, local_roots, i2d)
        all_dists |= dists
        # Only system-agent packages get their own requirements.txt (spec).
        if out_dir.parent.name == "system":
            body = HEADER + f"# {name} — direct dependencies\n"
            body += "".join(r + "\n" for r in reqs) or SDK_REQ + "\n"
            files[out_dir / "requirements.txt"] = body

    locked = sorted(_closure(all_dists), key=str.lower)
    lock_lines = []
    for dist in locked:
        if dist.replace("-", "_") in LOCAL_ROOTS:
            continue
        pin = _pin(dist)
        if pin:
            lock_lines.append(pin)
    lock = HEADER + "# Union lockfile: transitive closure of every agent's deps.\n"
    lock += SDK_REQ + "\n"
    lock += "".join(l + "\n" for l in sorted(lock_lines, key=str.lower))
    files[AGENTS / "requirements.lock"] = lock
    return files


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true", help="fail if committed files are stale")
    args = ap.parse_args(argv)

    files = generate()
    drift = []
    for path, content in files.items():
        existing = path.read_text(encoding="utf-8") if path.exists() else None
        if existing != content:
            drift.append(path)
            if not args.check:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(content, encoding="utf-8", newline="\n")

    rel = lambda p: p.relative_to(CORE).as_posix()
    if args.check:
        if drift:
            print("STALE requirements (run scripts/gen_agent_requirements.py):", file=sys.stderr)
            for p in drift:
                print("  " + rel(p), file=sys.stderr)
            return 1
        print("requirements up to date")
        return 0
    for p in sorted(files, key=lambda p: p.as_posix()):
        print(("wrote " if p in drift else "ok    ") + rel(p))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
