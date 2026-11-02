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

# ── junk classification: chat content is kept under junk_mode="mark" ───────
# T-20261102-ingestion-structure-junk-filter-silently-drops: the text parser emits
# every block as a "paragraph", and the OCR letter-ratio rule used to DELETE tables,
# terminal output and number/colour/receipt lists from chat sessions.
from structure import JUNK_MODE_MARK, StructureBuilder, KIND_TABLE
CHAT = """user: I compared the plans, here is the table.

| Plan | Price | GB |
|------|-------|----|
| A    | $10   | 5  |
| B    | $20   | 50 |

$ ls -la /tmp
-rw-r--r-- 1 me 4096 11-02 10:00 a.txt

1. 42
2. 17
3. 3.14

- Walmart: $120.00
- TJ's: $85.40
- 7-11: $12.99
"""
marked = parse_document("chat", CHAT, source_type="text", junk_mode=JUNK_MODE_MARK)
texts = [n.text for n in marked.leaves()]
check("mark mode drops nothing", marked.dropped_leaves == 0)
for needle in ("| Plan | Price", "$ ls -la", "3. 3.14", "Walmart: $120.00", "I compared the plans"):
    check(f"mark mode keeps {needle!r}", any(needle in t for t in texts))
reasons = {n.text.split(chr(10))[0]: n.meta.get("junk_reason") for n in marked.leaves()}
check("table classified (tagged, not dropped)", reasons.get("| Plan | Price | GB |") == "low_letter_ratio")
check("prose not classified", reasons.get("user: I compared the plans, here is the table.") is None)

legacy = parse_document("chat", CHAT, source_type="text")
check("legacy drop mode still drops (older kernels unchanged)", legacy.dropped_leaves >= 3)

b = StructureBuilder("pdf", "pdf", "pdf", "docling", JUNK_MODE_MARK)
b.add_leaf(KIND_PARAGRAPH, "<!-- image not available -->")
b.add_leaf(KIND_PARAGRAPH, "! ! !")
b.add_leaf(KIND_TABLE, "| 1 | 2 |")
b.add_leaf(KIND_PARAGRAPH, "   ")
lv = b.doc.leaves()
check("docling layout marker tagged", lv[0].meta.get("junk_reason") == "layout_marker")
check("OCR punctuation tagged", lv[1].meta.get("junk_reason") == "few_letters")
check("numeric table never tagged", "junk_reason" not in lv[2].meta)
check("empty leaf skipped, not counted", len(lv) == 3 and b.doc.dropped_leaves == 0)

print()
if FAILS:
    print(f"{len(FAILS)} FAILED: {FAILS}"); raise SystemExit(1)
print("ALL PASSED")
