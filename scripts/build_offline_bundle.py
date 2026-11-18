#!/usr/bin/env python3
"""ADR-0157 D7.3 — build the offline bundle for the kernel-managed sandbox Python env.

An air-gapped install (public sector) builds the env with no network: the pinned
interpreter and installer archives must already be in ``<env root>/downloads`` (or
behind a ``file://`` mirror) and every wheel of the base lock in a local wheelhouse.
This script produces exactly that, per platform, on a machine that has network:

  <out>/downloads/   the pinned python-build-standalone + uv archives, SHA-256 verified
                     against internal/pyenv/pins.json
  <out>/wheelhouse/  one wheel per base-lock requirement that applies to the platform,
                     downloaded in hash-checking mode (pip --require-hashes)

The operator copies <out> to the target and sets, in configs/config.json:

  "execution": {"agents": {"python": {
      "offline": true,
      "python_mirror_url": "file:///<out>/downloads/",
      "installer_mirror_url": "file:///<out>/downloads/",
      "wheelhouse": ["<out>/wheelhouse"]}}}

then runs ``cambrian-orchestrator python-env build`` (or setup). It is release
tooling, not a kernel runtime feature (D7.3). Run it with any Python >= 3.9 that has
pip — the kernel-managed interpreter itself works (``<gen>/python.exe -m pip`` ships
with the standalone build); it never needs the host's site-packages.

Usage:
  python scripts/build_offline_bundle.py --platform linux-x86_64-gnu --out DIR [--lock agents/base.lock]
"""
from __future__ import annotations

import argparse
import hashlib
import json
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

CORE = Path(__file__).resolve().parents[1]
PINS = CORE / "internal" / "pyenv" / "pins.json"

# Target environments for marker evaluation and pip's wheel tag selection. pip
# download evaluates environment markers against the RUNNING interpreter even with
# --platform, so markers are evaluated here for the target and stripped.
TARGETS = {
    "windows-x86_64": {
        "env": {"sys_platform": "win32", "platform_system": "Windows", "os_name": "nt", "platform_machine": "AMD64"},
        "platforms": ["win_amd64"],
    },
    "linux-x86_64-gnu": {
        "env": {"sys_platform": "linux", "platform_system": "Linux", "os_name": "posix", "platform_machine": "x86_64"},
        "platforms": ["manylinux_2_28_x86_64", "manylinux_2_27_x86_64", "manylinux_2_24_x86_64",
                      "manylinux_2_17_x86_64", "manylinux2014_x86_64", "manylinux2010_x86_64",
                      "manylinux1_x86_64", "linux_x86_64"],
    },
    "linux-aarch64-gnu": {
        "env": {"sys_platform": "linux", "platform_system": "Linux", "os_name": "posix", "platform_machine": "aarch64"},
        "platforms": ["manylinux_2_28_aarch64", "manylinux_2_27_aarch64", "manylinux_2_24_aarch64",
                      "manylinux_2_17_aarch64", "manylinux2014_aarch64", "linux_aarch64"],
    },
    "macos-x86_64": {
        "env": {"sys_platform": "darwin", "platform_system": "Darwin", "os_name": "posix", "platform_machine": "x86_64"},
        "platforms": [f"macosx_{v}_x86_64" for v in ("10_9", "10_12", "10_13", "10_14", "10_15", "11_0", "12_0", "13_0")]
                     + ["macosx_10_9_universal2", "macosx_11_0_universal2"],
    },
    "macos-aarch64": {
        "env": {"sys_platform": "darwin", "platform_system": "Darwin", "os_name": "posix", "platform_machine": "arm64"},
        "platforms": [f"macosx_{v}_arm64" for v in ("11_0", "12_0", "13_0", "14_0")]
                     + ["macosx_10_9_universal2", "macosx_11_0_universal2"],
    },
}
PY_ENV = {"python_version": "3.12", "python_full_version": "3.12.16", "implementation_name": "cpython",
          "platform_python_implementation": "CPython", "extra": ""}


def _marker_cls():
    try:
        from packaging.markers import Marker  # type: ignore
    except ImportError:  # pip vendors packaging
        from pip._vendor.packaging.markers import Marker  # type: ignore
    return Marker


def filter_lock(lock_text: str, platform: str) -> str:
    """The lock's requirements that apply to platform, markers evaluated and removed."""
    Marker = _marker_cls()
    env = dict(PY_ENV, **TARGETS[platform]["env"])
    joined = lock_text.replace("\r\n", "\n").replace("\\\n", " ")
    out = []
    for line in joined.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        spec, _, opts = line.partition(" --")
        opts = ("--" + opts) if opts else ""
        pin, _, marker = spec.partition(";")
        if marker.strip() and not Marker(marker.strip()).evaluate(env):
            continue
        out.append(pin.strip() + " " + " ".join(opts.split()))
    return "\n".join(out) + "\n"


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch_archive(base_url: str, asset: dict, dest_dir: Path) -> Path:
    dest = dest_dir / asset["name"]
    if dest.exists() and sha256(dest) == asset["sha256"]:
        return dest
    url = base_url.rstrip("/") + "/" + asset["name"].replace("+", "%2B")
    tmp = dest.with_suffix(dest.suffix + ".part")
    with urllib.request.urlopen(url) as r, tmp.open("wb") as f:
        while chunk := r.read(1 << 20):
            f.write(chunk)
    got = sha256(tmp)
    if got != asset["sha256"]:
        tmp.unlink()
        raise SystemExit(f"sha256 mismatch for {asset['name']}: pinned {asset['sha256']}, got {got}")
    tmp.replace(dest)
    return dest


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    ap.add_argument("--platform", required=True, choices=sorted(TARGETS))
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--lock", type=Path, default=CORE / "agents" / "base.lock")
    args = ap.parse_args(argv)

    pins = json.loads(PINS.read_text(encoding="utf-8"))
    downloads = args.out / "downloads"
    wheelhouse = args.out / "wheelhouse"
    downloads.mkdir(parents=True, exist_ok=True)
    wheelhouse.mkdir(parents=True, exist_ok=True)

    for kind in ("python", "installer"):
        rel = pins[kind]
        asset = rel["assets"][args.platform]
        p = fetch_archive(rel["base_url"], asset, downloads)
        print(f"ok  {kind:9s} {p.name}")

    filtered = filter_lock(args.lock.read_text(encoding="utf-8"), args.platform)
    with tempfile.TemporaryDirectory() as td:
        req = Path(td) / "lock.txt"
        req.write_text(filtered, encoding="utf-8")
        cmd = [sys.executable, "-m", "pip", "download", "--disable-pip-version-check", "--no-deps",
               "--require-hashes", "--only-binary", ":all:", "--python-version", "3.12",
               "--implementation", "cp", "--abi", "cp312", "--abi", "abi3", "--abi", "none",
               "-d", str(wheelhouse), "-r", str(req)]
        for plat in TARGETS[args.platform]["platforms"]:
            cmd += ["--platform", plat]
        subprocess.run(cmd, check=True)
    n = len(list(wheelhouse.glob("*.whl")))
    print(f"ok  wheelhouse {n} wheels for {filtered.count(chr(10))} requirements -> {wheelhouse}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
