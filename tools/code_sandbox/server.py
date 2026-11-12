"""Cambrian's code sandbox MCP server: llm-sandbox plus per-run input folders.

The upstream server (vndee/llm-sandbox 0.3.46, ``python -m llm_sandbox.mcp_server.server``)
runs each ``execute_code`` call in a fresh Docker container whose working directory is
``/sandbox``; no host file is visible there and the server has no setting to mount one
(personal run 2026-11-09, p06-expense-analysis#3: ``FileNotFoundError`` on
``Finance/bank-export.csv``). This wrapper is the upstream server with four additions.

1. **Per-run inputs (production; owner decision 2026-11-10, option C,
   decisions/code-sandbox-files-per-run-mount.md).** ``execute_code`` takes an ``inputs``
   argument (ledger ids or the person's file paths). The SERVER NEVER RESOLVES THEM: the
   Cambrian kernel reads each under the person's access, writes exactly those files into a
   fresh folder and sends its host path in the request's ``_meta``
   (``cambrian/inputs_dir``, plus ``cambrian/outputs_dir`` and the name map
   ``cambrian/inputs``). For that one execution the folder is bind-mounted READ-ONLY at
   ``/inputs`` and the outputs folder writable at ``/outputs``; Python starts in
   ``/inputs`` with ``INPUTS`` mapping each input to its path. After the call the kernel
   records every file in the outputs folder in its ledger and deletes both folders.
   ``inputs`` without the kernel's ``_meta`` is refused (the server cannot check access).

2. **A pre-built image (owner decision 2026-11-10, option a).** ``SANDBOX_IMAGE`` names the
   image Python runs in (``cambrian/code-sandbox:py311-1``, built from ``image/``, with
   pandas, numpy, matplotlib and openpyxl). The image must already be on the Docker host:
   a missing one is an error naming the build command, never a pull. llm-sandbox's per-call
   venv + ``pip install --upgrade pip`` (which needs network) is skipped, and a requested
   library is accepted only when the image's ``cambrian.sandbox.preinstalled`` label lists
   it; any other is refused, so nothing falls back to the network.

3. **A read-only root that works.** Under ``SANDBOX_READ_ONLY=true`` upstream cannot copy
   the code file into the container ("container rootfs is marked read-only"), so every call
   failed. The wrapper gives each call its own writable scratch folders for ``/sandbox`` and
   ``/tmp`` (deleted after the call); everything else stays read-only.

4. **``SANDBOX_MOUNTS`` (tests and local development only).** ``<host dir>=><container
   dir>`` entries separated by ``;``, bind-mounted read-only into every container, Python
   starting in the first. A fixed mount of a person's home on a shared sandbox lets other
   people's runs see it, so real deployments use (1) instead.

Launch it like the upstream module (the catalog does, from the kernel's directory):

    uv run --no-project --with llm-sandbox[mcp-docker]==0.3.46 python tools/code_sandbox/server.py
"""

from __future__ import annotations

import contextvars
import os
import sys
import shutil
import tempfile
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

MOUNTS_ENV = "SANDBOX_MOUNTS"
IMAGE_ENV = "SANDBOX_IMAGE"
READ_ONLY_ENV = "SANDBOX_READ_ONLY"
NETWORK_ENV = "SANDBOX_NETWORK_MODE"
BACKEND_ENV = "BACKEND"

#: request _meta keys the kernel sets (domain/staged_inputs.go)
META_INPUTS_DIR = "cambrian/inputs_dir"
META_OUTPUTS_DIR = "cambrian/outputs_dir"
META_FILES = "cambrian/inputs"

INPUTS_TARGET = "/inputs"
OUTPUTS_TARGET = "/outputs"
WORK_TARGET = "/sandbox"
TMP_TARGET = "/tmp"

PREINSTALLED_LABEL = "cambrian.sandbox.preinstalled"
BUILD_HINT = "build it with `make sandbox-image` in cambrian-core (tools/code_sandbox/image)"

_TRUE = {"1", "true", "yes", "on"}


try:  # the MCP SDK shows a ToolError's message to the caller; any other exception is masked
    from mcp.server.mcpserver.exceptions import ToolError as _ToolError  # mcp >= 2
except ImportError:  # pragma: no cover - mcp 1.x, or no mcp (unit tests)
    try:
        from mcp.server.fastmcp.exceptions import ToolError as _ToolError
    except ImportError:
        _ToolError = RuntimeError  # type: ignore[misc,assignment]


class SandboxConfigError(_ToolError):  # type: ignore[valid-type,misc]
    """A call this server must refuse: the message is shown to the caller (an MCP tool error)."""


def parse_mounts(raw: str) -> List[Tuple[str, str]]:
    """``"C:/u/home=>/home/person;D:/x=>/data"`` -> ``[("C:/u/home", "/home/person"), ...]``.

    An entry without ``=>`` or with a relative container path is refused (ValueError):
    a mount the operator asked for and did not get must not pass silently."""
    out: List[Tuple[str, str]] = []
    for part in (raw or "").split(";"):
        part = part.strip()
        if not part:
            continue
        host, sep, target = part.partition("=>")
        host, target = host.strip(), target.strip()
        if not sep or not host or not target.startswith("/"):
            raise ValueError(f"{MOUNTS_ENV} entry {part!r}: want '<host dir>=></absolute container dir>'")
        out.append((host, target))
    return out


def python_prelude(workdir: str, inputs: Optional[Dict[str, str]] = None) -> str:
    """The lines prepended to Python code: start in ``workdir``; ``INPUTS`` names each input."""
    lines = ["import os as _cambrian_os", f"_cambrian_os.chdir({workdir!r})", "del _cambrian_os"]
    if inputs is not None:
        lines.append(f"INPUTS = {inputs!r}")
    return "\n".join(lines) + "\n"


@dataclass
class Mount:
    host: str
    target: str
    read_only: bool


@dataclass
class CallPlan:
    """What one execute_code call gets: its mounts, where Python starts, its inputs."""

    mounts: List[Mount] = field(default_factory=list)
    workdir: Optional[str] = None
    inputs: Optional[Dict[str, str]] = None  # input as written -> container path
    scratch: List[str] = field(default_factory=list)  # server-made folders to delete

    def cleanup(self) -> None:
        for d in self.scratch:
            shutil.rmtree(d, ignore_errors=True)
        self.scratch = []


_current: contextvars.ContextVar[Optional[CallPlan]] = contextvars.ContextVar("cambrian_call_plan", default=None)


def meta_of(ctx: Any) -> Dict[str, Any]:
    """The request's _meta as a dict (mcp 2.x: a mapping; 1.x: a pydantic model)."""
    try:
        m = ctx.request_context.meta
    except Exception:  # noqa: BLE001 - no request context: no meta
        return {}
    if m is None:
        return {}
    if isinstance(m, Mapping):
        return dict(m)
    out: Dict[str, Any] = {}
    for attr in ("model_extra", "__pydantic_extra__"):
        extra = getattr(m, attr, None)
        if isinstance(extra, Mapping):
            out.update(extra)
    return out


def _host_dir(meta: Mapping[str, Any], key: str) -> Optional[str]:
    v = meta.get(key)
    if v in (None, ""):
        return None
    if not isinstance(v, str) or not os.path.isabs(v) or not os.path.isdir(v):
        raise SandboxConfigError(f"_meta {key} must be an existing absolute folder, got {v!r}")
    return v


def plan_call(meta: Mapping[str, Any], inputs: Optional[List[str]], static: List[Tuple[str, str]],
              read_only_root: bool) -> CallPlan:
    """The mounts of one call. ``inputs`` are only ever served from the kernel's folder."""
    plan = CallPlan(mounts=[Mount(h, t, True) for h, t in static])
    if static:
        plan.workdir = static[0][1]
    inputs_dir = _host_dir(meta, META_INPUTS_DIR)
    outputs_dir = _host_dir(meta, META_OUTPUTS_DIR)
    wanted = [s for s in (inputs or []) if str(s).strip()]
    if wanted and not inputs_dir:
        raise SandboxConfigError(
            "inputs must be staged by the Cambrian kernel (it reads each one under the person's access "
            "and passes the folder in the request's _meta); this call has none, so nothing was mounted")
    if inputs_dir:
        plan.mounts.append(Mount(inputs_dir, INPUTS_TARGET, True))
        plan.workdir = INPUTS_TARGET
        files = meta.get(META_FILES) or {}
        plan.inputs = {str(k): f"{INPUTS_TARGET}/{v}" for k, v in dict(files).items()} if isinstance(files, Mapping) else {}
    if outputs_dir:
        plan.mounts.append(Mount(outputs_dir, OUTPUTS_TARGET, False))
    if read_only_root:
        # Upstream copies the code file into /sandbox and reads plots back from /tmp; on a
        # read-only root both must be writable, and only for this call.
        for target in (WORK_TARGET, TMP_TARGET):
            d = tempfile.mkdtemp(prefix="cambrian-sbx-")
            plan.scratch.append(d)
            plan.mounts.append(Mount(d, target, False))
    return plan


def normalise_library(spec: str) -> str:
    """``"Pandas>=2"`` -> ``"pandas"``."""
    s = spec.strip().lower()
    for sep in ("[", "=", "<", ">", "!", "~", " ", ";"):
        s = s.split(sep, 1)[0]
    return s.replace("_", "-")


def check_libraries(libraries: Optional[List[str]], preinstalled: Optional[set]) -> List[str]:
    """Libraries to hand upstream: none when an image is in use (they are in it or refused)."""
    if preinstalled is None:
        return list(libraries or [])
    missing = [lib for lib in (libraries or []) if normalise_library(lib) not in preinstalled]
    if missing:
        raise SandboxConfigError(
            f"libraries {missing} are not in the sandbox image and cannot be installed (the sandbox has no "
            f"network); available: {', '.join(sorted(preinstalled)) or 'none'}")
    return []


def _truthy(v: Optional[str]) -> bool:
    return (v or "").strip().lower() in _TRUE


class ImageCheck:
    """Looks up SANDBOX_IMAGE on the Docker host: present (with its preinstalled label) or a
    clear error. Never pulls."""

    def __init__(self, image: str, client_factory: Optional[Callable[[], Any]] = None) -> None:
        self.image = image
        self._factory = client_factory

    def preinstalled(self) -> set:
        try:
            import docker  # noqa: PLC0415 - the upstream server's own dependency
            from docker.errors import ImageNotFound  # noqa: PLC0415
        except ImportError as e:  # pragma: no cover - llm-sandbox[mcp-docker] brings it
            raise SandboxConfigError(f"docker client unavailable: {e}") from e
        client = self._factory() if self._factory else docker.from_env()
        try:
            img = client.images.get(self.image)
        except ImageNotFound as e:
            raise SandboxConfigError(f"sandbox image {self.image!r} is not on this Docker host; {BUILD_HINT}") from e
        labels = (getattr(img, "labels", None) or {})
        return {normalise_library(x) for x in str(labels.get(PREINSTALLED_LABEL, "")).split(",") if x.strip()}


def _wrap_sessions(srv: Any, image: Optional[str]) -> None:
    """Python sessions: the configured image, no network-bound env setup, the call's prelude."""

    def wrap(factory: Callable[..., Any]) -> Callable[..., Any]:
        def make(**kw: Any) -> Any:
            python = str(kw.get("lang", "python")).lower() == "python"
            if python and image:
                kw.setdefault("image", image)
                kw["skip_environment_setup"] = True
            session = factory(**kw)
            plan = _current.get()
            if not python or plan is None or not plan.workdir:
                return session
            run = session.run

            def run_in_workdir(code: str, *a: Any, **k: Any) -> Any:
                return run(python_prelude(plan.workdir, plan.inputs) + code, *a, **k)

            session.run = run_in_workdir
            return session

        return make

    srv.ArtifactSandboxSession = wrap(srv.ArtifactSandboxSession)
    srv.SandboxSession = wrap(srv.SandboxSession)


def describe(static: List[Tuple[str, str]], image: Optional[str]) -> str:
    """The execute_code tool description the model reads."""
    text = (
        "Execute code in a secure, throwaway sandbox (no network) and capture plots as images.\n\n"
        "Args:\n"
        "    code: The code to execute.\n"
        "    language: python (default), javascript, java, cpp, go, r or ruby.\n"
        "    libraries: Packages the code needs. The sandbox cannot install anything; "
        + ("Python already has pandas, numpy, matplotlib and openpyxl.\n" if image else
           "use only what the runtime has.\n")
        + "    timeout: Seconds before the run is stopped (default 30).\n"
        "    inputs: Files the code reads: ledger ids (led_...) of earlier results, or paths of the "
        "person's files as the file tools show them. Cambrian reads each under the person's access "
        f"and mounts them read-only at {INPUTS_TARGET} for this run only; Python starts in {INPUTS_TARGET}, "
        "so open a file by its name (a path keeps its file name: 'Finance/bank.csv' is 'bank.csv'); "
        "the dict INPUTS maps each input as you wrote it to its full path. Nothing else of the "
        "person's is visible.\n\n"
        f"Write output files to {OUTPUTS_TARGET}: Cambrian records each one and returns its ledger id."
    )
    if static:
        text += ("\n\nAlso mounted read-only on every run (local development): "
                 + ", ".join(t for _, t in static) + ".")
    return text


def install(srv: Any, mounts: List[Tuple[str, str]], mount_type: Callable[..., Any],
            image: Optional[str] = None, image_check: Optional[ImageCheck] = None,
            context_type: Any = None) -> None:
    """Patch the upstream server module: per-call mounts in every container's runtime configs,
    the image and prelude in Python sessions, and execute_code re-registered with ``inputs``."""
    base = srv._get_runtime_configs

    def runtime_configs() -> dict:
        cfg = dict(base())
        plan = _current.get()
        extra = plan.mounts if plan is not None else [Mount(h, t, True) for h, t in mounts]
        if extra:
            cfg["mounts"] = list(cfg.get("mounts") or []) + [
                mount_type(target=m.target, source=m.host, type="bind", read_only=m.read_only) for m in extra]
        return cfg

    srv._get_runtime_configs = runtime_configs
    _wrap_sessions(srv, image)

    upstream = srv.execute_code
    read_only_root = _truthy(os.environ.get(READ_ONLY_ENV))

    def execute_code(code: str, language: str = "python", libraries: Optional[List[str]] = None,
                     timeout: int = 30, inputs: Optional[List[str]] = None, ctx: Any = None) -> Any:
        plan = plan_call(meta_of(ctx), inputs, mounts, read_only_root)
        try:
            if str(language).lower() == "python" and image_check is not None:
                libraries = check_libraries(libraries, image_check.preinstalled())
            token = _current.set(plan)
            try:
                return upstream(code=code, language=language, libraries=libraries, timeout=timeout)
            finally:
                _current.reset(token)
        finally:
            plan.cleanup()

    if context_type is not None:
        execute_code.__annotations__["ctx"] = context_type
    ret = getattr(upstream, "__annotations__", {}).get("return")
    if ret is not None:
        execute_code.__annotations__["return"] = ret
    execute_code.__doc__ = describe(mounts, image)
    srv.execute_code = execute_code
    _register(srv.mcp, execute_code)


def _register(server: Any, fn: Callable[..., Any]) -> None:
    """Replace the upstream execute_code tool with ``fn`` (mcp 1.x FastMCP or 2.x MCPServer)."""
    if server is None:
        return
    try:
        server.remove_tool("execute_code")
    except Exception:  # noqa: BLE001 - older mcp: drop it from the manager directly
        tools = getattr(getattr(server, "_tool_manager", None), "_tools", None)
        if isinstance(tools, dict):
            tools.pop("execute_code", None)
    try:
        server.add_tool(fn, name="execute_code", description=fn.__doc__, structured_output=False)
    except TypeError:  # an mcp without structured_output
        server.add_tool(fn, name="execute_code", description=fn.__doc__)


def _context_type() -> Any:
    try:
        from mcp.server.mcpserver import Context  # noqa: PLC0415 - mcp >= 2
    except ImportError:  # pragma: no cover - mcp 1.x
        from mcp.server.fastmcp import Context  # noqa: PLC0415
    return Context


def main() -> None:
    from docker.types import Mount as DockerMount  # noqa: PLC0415 - the upstream server's own dependency
    from llm_sandbox.mcp_server import server as srv  # noqa: PLC0415

    image = (os.environ.get(IMAGE_ENV) or "").strip() or None
    docker_backend = (os.environ.get(BACKEND_ENV) or "docker").strip().lower() == "docker"
    check = ImageCheck(image) if image and docker_backend else None
    if check is not None:
        try:
            check.preinstalled()
        except SandboxConfigError as e:
            # Said at start (the kernel's health log) and again on every call, which fails
            # with the same message; the server stays up so the tool can say why.
            print(f"code-sandbox: {e}", file=sys.stderr)
    install(srv, parse_mounts(os.environ.get(MOUNTS_ENV, "")), DockerMount, image=image,
            image_check=check, context_type=_context_type())
    srv.main()


if __name__ == "__main__":
    main()
