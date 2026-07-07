"""Standalone tests for the structure schema + dependency-free parser."""
from __future__ import annotations
from structure import parse_document, KIND_SECTION, KIND_PARAGRAPH, KIND_CODE

FAILS = []
def check(name, cond):
    print(("PASS" if cond else "FAIL"), "-", name)
    if not cond: FAILS.append(name)

def by_title(doc, t):
    return next((n for n in doc.nodes if n.kind == KIND_SECTION and n.title == t), None)

# ── Markdown textbook ───────────────────────────────────────────────────────
MD = """# Biology 101

Intro paragraph about the course.

## Chapter 3: Cells

Cells are the basic unit of life.

### Section 3.2 Photosynthesis

Photosynthesis converts light to energy.

The Calvin cycle fixes carbon.

```python
def atp():
    return "energy"
```

### Section 3.3 Respiration

Respiration releases energy.

## Chapter 4: Genetics

DNA carries heredity.
"""
doc = parse_document("bio101", MD, source_type="markdown", title="Biology 101")
check("backend is markdown", doc.backend == "markdown")
secs = {n.title: n for n in doc.sections()}
check("chapter 3 section present", "Chapter 3: Cells" in secs)
check("section 3.2 present", "Section 3.2 Photosynthesis" in secs)
check("section 3.2 level=3 (### )", secs["Section 3.2 Photosynthesis"].level == 3)
check("chapter 3 level=2 (## )", secs["Chapter 3: Cells"].level == 2)

# hierarchy: 3.2 is a child of Chapter 3
ch3 = secs["Chapter 3: Cells"]; s32 = secs["Section 3.2 Photosynthesis"]
check("3.2 parent is chapter 3", s32.parent_id == ch3.id)
check("3.2 breadcrumb inherits chapter", s32.section_path == "Biology 101 › Chapter 3: Cells › Section 3.2 Photosynthesis")

# leaves under 3.2 carry the section breadcrumb (inherited path onto every leaf)
leaves_32 = [n for n in doc.leaves() if n.parent_id == s32.id]
check("3.2 has content leaves", len(leaves_32) >= 2)
check("calvin leaf inherits 3.2 path",
      any("Calvin" in n.text and n.section_path == "Biology 101 › Chapter 3: Cells › Section 3.2 Photosynthesis"
          for n in leaves_32))
check("code fence kept as one code leaf",
      any(n.kind == KIND_CODE and "def atp" in n.text for n in doc.leaves()))

# ordinal path is ltree-safe (labels start with a letter)
import re
check("paths are ltree-safe", all(re.fullmatch(r"(n\d+)(\.n\d+)*", n.path) for n in doc.nodes if n.path))

# sibling order: Chapter 4 comes after Chapter 3 under the doc root
ch4 = secs["Chapter 4: Genetics"]
check("chapter 4 order > chapter 3 order", ch4.order > ch3.order and ch4.parent_id == ch3.parent_id)

# ── Plain-text "Chapter/Section" (no markdown #) ─────────────────────────────
TXT = """Chapter 1
This is the first chapter body paragraph.

Section 1.2 Overview
Overview content here.

Chapter 2
Second chapter starts. In chapter 1 we saw something (this line is prose, not a heading).
"""
doc2 = parse_document("txtbook", TXT, source_type="text")
check("plain-text backend is text", doc2.backend == "text")
t2 = {n.title: n for n in doc2.sections()}
check("plain-text Chapter 1 detected", any(t.startswith("Chapter 1") for t in t2))
check("plain-text Chapter 2 detected", any(t.startswith("Chapter 2") for t in t2))
check("prose 'in chapter 1 we saw' NOT a heading",
      not any("we saw" in (n.title or "") for n in doc2.sections()))

print()
if FAILS:
    print(f"{len(FAILS)} FAILED: {FAILS}"); raise SystemExit(1)
print("ALL PASSED")
