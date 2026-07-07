"""Test the DoclingAgent run() contract (routing + JSON output)."""
from __future__ import annotations
import base64, json, types
from agent import DoclingAgent

FAILS = []
def check(name, cond):
    print(("PASS" if cond else "FAIL"), "-", name)
    if not cond: FAILS.append(name)

def task(obj):
    return types.SimpleNamespace(data=json.dumps(obj).encode("utf-8"), text="")

a = DoclingAgent(agent_id="docling_test")

# 1) Markdown document → structured tree via the text backend
md = "# Book\n\n## Chapter 1\n\nHello world.\n\n### 1.1 Intro\n\nIntro text.\n"
out = json.loads(a.run(task({"doc_id": "b1", "title": "Book", "source_type": "markdown", "text": md})))
check("ok", out.get("ok") is True)
check("backend markdown", out.get("backend") == "markdown")
secs = [n for n in out["nodes"] if n["kind"] == "section"]
check("has chapter 1 + 1.1 sections", any(n["title"] == "Chapter 1" for n in secs) and any("1.1" in n["title"] for n in secs))
leaf = next((n for n in out["nodes"] if n["kind"] == "paragraph" and "Intro text" in n["text"]), None)
check("intro leaf carries section breadcrumb", leaf is not None and leaf["section_path"].endswith("1.1 Intro"))
check("nodes have ltree-safe paths", all(n["path"] == "" or n["path"].startswith("n") for n in out["nodes"]))

# 2) Binary doc (pdf) with bytes but no docling installed → graceful (no crash)
fake_pdf = base64.b64encode(b"%PDF-1.4 fake").decode()
out2 = json.loads(a.run(task({"doc_id": "p1", "source_type": "pdf", "data_b64": fake_pdf})))
check("binary path does not crash", isinstance(out2.get("nodes"), list))
check("binary path reports backend", out2.get("backend") in ("docling", "text", "none"))

# 3) Malformed request → does not crash, returns a document root
out3 = json.loads(a.run(types.SimpleNamespace(data=b"not json", text="")))
check("malformed request handled", isinstance(out3.get("nodes"), list))

print()
if FAILS:
    print(f"{len(FAILS)} FAILED: {FAILS}"); raise SystemExit(1)
print("ALL PASSED")
