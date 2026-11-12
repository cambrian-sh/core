"""Tests for Cambrian's code-sandbox server (tools/code_sandbox/server.py).

Unit tests need neither Docker nor llm-sandbox. The tests marked ``docker`` need a running
Docker engine and the built image (``make sandbox-image``) and are skipped cleanly without
them; the end-to-end one also needs llm-sandbox, e.g.

    uv run --no-project --with "llm-sandbox[mcp-docker]==0.3.46" --with pytest \\
        python -m pytest tools/code_sandbox/server_test.py
"""
import os
import shutil
import subprocess
import sys
import types

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import server as wrapper  # noqa: E402

IMAGE = os.environ.get("SANDBOX_TEST_IMAGE", "cambrian/code-sandbox:py311-1")


def test_parse_mounts():
    assert wrapper.parse_mounts("") == []
    assert wrapper.parse_mounts("C:/u/home=>/home/person; D:/x =>/data") == [
        ("C:/u/home", "/home/person"), ("D:/x", "/data")]
    for bad in ("C:/u/home", "C:/u/home=>relative", "=>/x"):
        with pytest.raises(ValueError):
            wrapper.parse_mounts(bad)


def _staged(tmp_path):
    inputs, outputs = tmp_path / "inputs", tmp_path / "outputs"
    inputs.mkdir()
    outputs.mkdir()
    (inputs / "bank-export.csv").write_text("date,amount\n2026-11-01,12.5\n")
    meta = {wrapper.META_INPUTS_DIR: str(inputs), wrapper.META_OUTPUTS_DIR: str(outputs),
            wrapper.META_FILES: {"led_abc": "bank-export.csv"}}
    return meta, inputs, outputs


def test_plan_mounts_inputs_read_only_and_outputs_writable(tmp_path):
    meta, inputs, outputs = _staged(tmp_path)
    plan = wrapper.plan_call(meta, ["led_abc"], [], read_only_root=False)
    assert [(m.host, m.target, m.read_only) for m in plan.mounts] == [
        (str(inputs), "/inputs", True), (str(outputs), "/outputs", False)]
    assert plan.workdir == "/inputs"
    assert plan.inputs == {"led_abc": "/inputs/bank-export.csv"}


def test_inputs_without_the_kernels_folder_are_refused(tmp_path):
    with pytest.raises(wrapper.SandboxConfigError, match="staged by the Cambrian kernel"):
        wrapper.plan_call({}, ["Finance/bank-export.csv"], [], read_only_root=False)
    with pytest.raises(wrapper.SandboxConfigError):
        wrapper.plan_call({wrapper.META_INPUTS_DIR: "relative/dir"}, ["x"], [], read_only_root=False)
    # No inputs, no meta: an ordinary run with nothing of the person's visible.
    plan = wrapper.plan_call({}, None, [], read_only_root=False)
    assert plan.mounts == [] and plan.workdir is None


def test_read_only_root_gets_per_call_scratch_that_is_removed(tmp_path):
    plan = wrapper.plan_call({}, None, [], read_only_root=True)
    targets = {m.target: m for m in plan.mounts}
    assert set(targets) == {"/sandbox", "/tmp"} and not any(m.read_only for m in plan.mounts)
    dirs = list(plan.scratch)
    assert all(os.path.isdir(d) for d in dirs)
    plan.cleanup()
    assert not any(os.path.exists(d) for d in dirs)


class _Session:
    def __init__(self, **kw):
        self.kw = kw
        self.ran = None

    def run(self, code, **kw):
        self.ran = code
        return code


class _Server:
    def __init__(self):
        self.tools = {"execute_code": "upstream"}

    def remove_tool(self, name):
        del self.tools[name]

    def add_tool(self, fn, name=None, description=None, structured_output=None):
        self.tools[name] = types.SimpleNamespace(fn=fn, description=description)


def _srv(seen):
    srv = types.SimpleNamespace(
        _get_runtime_configs=lambda: {"network_mode": "none"},
        ArtifactSandboxSession=_Session, SandboxSession=_Session, mcp=_Server())

    def upstream(code, language="python", libraries=None, timeout=30):
        cfg = srv._get_runtime_configs()
        session = (srv.ArtifactSandboxSession if language == "python" else srv.SandboxSession)(lang=language)
        seen.append({"cfg": cfg, "session": session, "ran": session.run(code), "libraries": libraries})
        return ["ok"]

    srv.execute_code = upstream
    return srv


def _ctx(meta):
    return types.SimpleNamespace(request_context=types.SimpleNamespace(meta=meta))


class _Preinstalled:
    def preinstalled(self):
        return {"pandas", "numpy", "matplotlib", "openpyxl"}


def test_install_mounts_the_call_folder_for_that_call_only(tmp_path):
    seen = []
    srv = _srv(seen)
    wrapper.install(srv, [], lambda **kw: kw, image=IMAGE, image_check=_Preinstalled())
    meta, inputs, outputs = _staged(tmp_path)
    tool = srv.mcp.tools["execute_code"]
    assert "/inputs" in tool.description and "/outputs" in tool.description and "pandas" in tool.description
    tool.fn(code="import pandas as pd\npd.read_csv('bank-export.csv')", inputs=["led_abc"],
            libraries=["pandas>=2"], ctx=_ctx(meta))
    call = seen[-1]
    assert call["cfg"]["mounts"] == [
        {"target": "/inputs", "source": str(inputs), "type": "bind", "read_only": True},
        {"target": "/outputs", "source": str(outputs), "type": "bind", "read_only": False}]
    assert call["session"].kw["image"] == IMAGE and call["session"].kw["skip_environment_setup"] is True
    assert call["ran"].startswith(wrapper.python_prelude("/inputs", {"led_abc": "/inputs/bank-export.csv"}))
    assert call["libraries"] == []  # preinstalled: never handed to pip
    # The next call without inputs sees none of it.
    tool.fn(code="print(1)")
    assert "mounts" not in seen[-1]["cfg"] and seen[-1]["ran"] == "print(1)"


def test_library_not_in_the_image_is_refused_not_installed():
    seen = []
    srv = _srv(seen)
    wrapper.install(srv, [], lambda **kw: kw, image=IMAGE, image_check=_Preinstalled())
    with pytest.raises(wrapper.SandboxConfigError, match="no network"):
        srv.mcp.tools["execute_code"].fn(code="import scipy", libraries=["scipy"])
    assert seen == []


def test_static_mounts_stay_for_local_development():
    seen = []
    srv = _srv(seen)
    wrapper.install(srv, wrapper.parse_mounts("C:/jobs/home=>/home/person"), lambda **kw: kw)
    srv.mcp.tools["execute_code"].fn(code="open('Finance/x.csv')")
    assert seen[-1]["cfg"]["mounts"] == [
        {"target": "/home/person", "source": "C:/jobs/home", "type": "bind", "read_only": True}]
    assert seen[-1]["ran"].startswith(wrapper.python_prelude("/home/person"))
    js = srv.SandboxSession(lang="javascript")
    assert js.run("console.log(1)") == "console.log(1)"


def test_missing_image_is_a_clear_error():
    docker = pytest.importorskip("docker")

    class _Images:
        def get(self, name):
            raise docker.errors.ImageNotFound(name)

    check = wrapper.ImageCheck("cambrian/nope:0", client_factory=lambda: types.SimpleNamespace(images=_Images()))
    with pytest.raises(wrapper.SandboxConfigError, match="make sandbox-image"):
        check.preinstalled()


# --- Docker-backed tests -------------------------------------------------------------------


def _docker_image_ready() -> str:
    if shutil.which("docker") is None:
        return "docker CLI not found"
    try:
        r = subprocess.run(["docker", "image", "inspect", IMAGE], capture_output=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as e:
        return f"docker unavailable: {e}"
    return "" if r.returncode == 0 else f"image {IMAGE} not built (make sandbox-image)"


def _docker_run(*args):
    env = dict(os.environ, MSYS_NO_PATHCONV="1")  # Git Bash would rewrite /inputs
    return subprocess.run(["docker", "run", "--rm", *args], capture_output=True, text=True, timeout=180, env=env)


@pytest.mark.docker
def test_image_imports_the_data_libraries_with_no_network():
    why = _docker_image_ready()
    if why:
        pytest.skip(why)
    # /tmp writable as the server makes it (a per-call scratch folder) on a read-only root.
    r = _docker_run("--network", "none", "--read-only", "--tmpfs", "/tmp", "--cap-drop", "ALL",
                    "--security-opt", "no-new-privileges:true", IMAGE,
                    "python", "-c", "import pandas, numpy, matplotlib, openpyxl; print('ok')")
    assert r.returncode == 0 and r.stdout.strip() == "ok", r.stderr


@pytest.mark.docker
def test_inputs_mount_is_read_only_in_the_container(tmp_path):
    why = _docker_image_ready()
    if why:
        pytest.skip(why)
    (tmp_path / "a.csv").write_text("x\n1\n")
    r = _docker_run("--network", "none", "-v", f"{tmp_path}:/inputs:ro", IMAGE, "python", "-c",
                    "print(open('/inputs/a.csv').read().strip());\n"
                    "try:\n open('/inputs/b.txt','w').write('x'); print('WROTE')\n"
                    "except OSError as e: print('refused', e.errno)")
    assert r.returncode == 0, r.stderr
    assert "x\n1" in r.stdout and "refused" in r.stdout and "WROTE" not in r.stdout
    assert not (tmp_path / "b.txt").exists()


@pytest.mark.docker
def test_end_to_end_through_llm_sandbox(tmp_path):
    """execute_code as the kernel calls it: hardened, no network, the image, a staged input,
    an output file written to /outputs."""
    why = _docker_image_ready()
    if why:
        pytest.skip(why)
    srv = pytest.importorskip("llm_sandbox.mcp_server.server")
    from docker.types import Mount

    os.environ.update({"SANDBOX_NETWORK_MODE": "none", "SANDBOX_READ_ONLY": "true", "SANDBOX_CAP_DROP": "ALL",
                       "SANDBOX_SECURITY_OPT": "no-new-privileges:true", "BACKEND": "docker"})
    wrapper.install(srv, [], Mount, image=IMAGE, image_check=wrapper.ImageCheck(IMAGE))
    meta, inputs, outputs = _staged(tmp_path)
    code = ("import pandas as pd, matplotlib.pyplot as plt\n"
            "df = pd.read_csv(INPUTS['led_abc'])\n"
            "df.to_csv('/outputs/copy.csv', index=False)\n"
            "plt.plot(df['amount']); plt.show()\n"
            "print('sum', df['amount'].sum())\n")
    out = srv.execute_code(code=code, inputs=["led_abc"], timeout=120, ctx=_ctx(meta))
    texts = [getattr(c, "text", "") for c in out]
    assert any('"exit_code": 0' in t or '"exit_code":0' in t for t in texts), texts
    assert any("sum 12.5" in t for t in texts), texts
    assert any(getattr(c, "type", "") == "image" for c in out), "plot not captured"
    assert (outputs / "copy.csv").read_text().startswith("date,amount")
    assert sorted(p.name for p in inputs.iterdir()) == ["bank-export.csv"]  # nothing written into inputs
