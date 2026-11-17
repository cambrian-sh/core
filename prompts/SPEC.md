# Prompt file format (ADR-0139 C4 / D2)

This is the one specification for every model-facing prompt Cambrian ships. It
has two homes and one format (owner decision Q6):

| Home | Holds | Loader |
| --- | --- | --- |
| `cambrian-core/prompts/` | kernel organs and system agents | Go, `github.com/cambrian-sh/core/prompts` (`go:embed`) |
| `sdk/cambrian_agent_sdk/prompts/` | SDK ReAct and helper prompts | Python, `cambrian_agent_sdk.prompt_library` (`importlib.resources`) |

Third-party agents never depend on core files: the SDK ships its own prompts as
package data. Benchmark-shaped variants live in `cambrian-benchmarks`, never here.

The two loaders implement the same rules, render the same text and compute the
same hash. That is held by a fixture the Go tests write
(`prompts/testdata/hash_cases.json`) and the SDK tests read
(`sdk/tests/test_prompt_hash_parity.py`).

## 1. Layout

```
<root>/<id>/v<N>.md            one file per prompt version
<root>/<id>/<name>.schema.json optional response schema, a sibling of the version file
```

- `<id>` matches `[a-z][a-z0-9_-]*`.
- `<N>` is a positive integer without leading zeros. Versions of one id are
  contiguous from 1: `v1.md`, `v2.md`, ... A gap is a lint failure.
- A new version is a new file. An existing version file is never edited once it
  has been sent in a recorded benchmark run (D2: a version bump requires a run);
  fixing a typo is a new version.
- `Get(id)` returns the highest version; `GetVersion(id, n)` a specific one.

## 2. File structure

A file is YAML frontmatter between two `---` lines, then the body:

```
---
id: clarifier
version: 1
tier: stable
description: One sentence saying what the prompt asks of the model.
inputs: [classification_tags, goal]
response_schema: clarification.schema.json
model_notes: Optional notes for whoever tunes this prompt for a model.
---
The body, in markdown, with named slots such as {{goal}}.
```

Line endings are normalised to LF before anything else happens, so a Windows
checkout renders and hashes the same bytes as a Linux one.

### 2.1 Frontmatter fields

| Field | Required | Type | Meaning |
| --- | --- | --- | --- |
| `id` | yes | string | Equals the directory name. |
| `version` | yes | int | Equals `<N>` in the file name. |
| `tier` | yes | `stable` \| `context` \| `volatile` | Where the prompt sits in the D2 three-tier assembly (below). |
| `description` | yes | string | What the prompt asks of the model. Non-empty. |
| `inputs` | yes | list of slot names | Exactly the slots the body uses. `[]` when there are none. |
| `response_schema` | no | file name or inline JSON object | The JSON Schema the reply must satisfy. A file name must be a sibling of the version file and end in `.schema.json`. |
| `model_notes` | no | string | Tuning notes; never sent to the model. |

Unknown fields are an error: a misspelt `response_shema` must not be silently
ignored.

### 2.2 The frontmatter YAML subset

The frontmatter is YAML, restricted to a subset both loaders parse identically
(the SDK has no YAML dependency):

- one `key: value` per line, no nesting except the forms below;
- a value is a plain or quoted (`"..."` or `'...'`) scalar on one line, an
  integer, a flow list `[a, b]`, or a block list (`key:` followed by `  - item`
  lines);
- `response_schema` may instead be an inline JSON object on one line
  (`{"type": "object"}`); prefer a schema file;
- a flow list whose first item starts with `{` is an inline JSON list of
  objects on one line (an agent profile's `sandbox_paths`,
  `agents/profiles/README.md`): `[{"path": "/srv/models"}]`;
- full-line `#` comments are allowed; no anchors, tags, or multi-line scalars.

### 2.3 Tiers (D2)

Every assembled prompt is built in three tiers, in this order, so the provider
cache can hold the stable prefix:

1. `stable`: identity, rules, tool policy, the skill index. The same for every
   call of a run.
2. `context`: project or principal instructions (AGENTS.md-style).
3. `volatile`: memory snapshot, time, step state.

The system prompt does not change mid-run; budget or limit notices go into the
message stream. A single-shot organ prompt (one file, one call) is `stable`: its
slots carry the per-call data, and the hash covers its static text.

## 3. Slots and rendering

- A slot is `{{name}}` with `name` matching `[a-z][a-z0-9_]*`, no spaces inside
  the braces. Any other `{{` in the body is a lint failure.
- The set of slot names in the body equals the set in `inputs`. A slot may
  appear more than once.
- `Render(values)` fails when a declared input has no value (missing) or a value
  is given for a name that is not an input (extra). An empty string is a value.
- Substitution is a single pass: a value that itself contains `{{x}}` is
  inserted literally and is never expanded.
- The body is normalised before rendering and hashing: CRLF becomes LF, leading
  newlines are removed and trailing spaces, tabs and newlines are removed. No
  trailing newline is added.

### 3.1 Untrusted content

Untrusted content (a user goal, a transcript, a retrieved document, a tool
result) always sits inside a named tag (`<goal>`, `<documents>`, ...) with a
sentence telling the model the tag holds data, not instructions. The task comes
last. The caller neutralises a closing tag inside the value before rendering.

### 3.2 Fragment sets (SDK)

Some prompts are not sent whole: the SDK's ReAct loop assembles its system prompt
from pieces chosen by protocol (text or native tool-calling), budget and menu
(`sdk/cambrian_agent_sdk/prompts/react-*/`). Such a file is a **fragment set**: its
body is a sequence of named sections, each introduced by a line of exactly

```
<!-- section: <name> -->
```

(`<name>` matches `[a-z][a-z0-9_-]*`). The file is still ONE prompt under this spec:
one `id`, one `version`, one `inputs` list (the union of every section's slots), one
static hash over the whole body (§4), and the same lint (§5) — the delimiter line is
ordinary body text to every rule here. Rendering is per section
(`prompt_library.fragment(id, section).render(values)`; `prompt_library.sections`):

- a section's text is the lines between its delimiter and the next, with leading
  and trailing blank lines dropped; the delimiter line itself is never sent;
- a section requires exactly its own slots (missing or extra is an error);
- text before the first delimiter, a repeated section name, or a file with no
  section are load errors.

The Go loader has no fragment API: no core prompt is a fragment set. A core prompt
that needs one gets a Go `Sections` helper and a parity case against the SDK first.

## 4. The static hash

`StaticHash` certifies exactly the static text the model is sent:

```
static = normalised body with every {{slot}} replaced by the token {{}}
StaticHash = lowercase hex sha256(UTF-8 bytes of static)
```

`{{}}` is an empty slot name, so it can never collide with a real slot. The hash
is the same for every render of a version and changes whenever any sent static
byte changes. It replaces hand-maintained "prompt version" hashes over text that
was assembled separately from what was sent (the drift class in
`investigations/2026-11-04-agent-context-interview-prompts.md` §3, where the
planner hashed pin rules it never sent).

## 5. Lint

One rule set, two runners: `cambrian-core/prompts/lint_test.go` over the core
prompts and `sdk/tests/test_prompt_lint.py` over the SDK prompts.

**Failures**

| Rule | Check |
| --- | --- |
| L1 | Frontmatter parses (the §2.2 subset); `id`, `version`, `tier`, `description`, `inputs` present; no unknown field. |
| L2 | `id` equals the directory; `version` equals the file name; versions are contiguous from 1. |
| L3 | `tier` is `stable`, `context` or `volatile`. |
| L4 | Slot names are valid, `inputs` has no duplicates, the body's slot set equals `inputs`, and the body has no malformed `{{`. |
| L5 | `response_schema`, when present, is a sibling `*.schema.json` file (or inline object) that parses as a JSON object. |
| L6 | No ADR or decision numbers in the body (`ADR-0139`, `ADR 139`, case-insensitive). The model gains nothing from them and they go stale. |
| L7 | No shouting: a run of 3 or more consecutive all-caps words of which at least 2 are not acronyms (for example `OUTPUT EXACTLY ONE JSON`). |

**Warnings** (reported, not failed): a single all-caps word of 3 or more letters
that is not an acronym (`NOT`, `ONLY`, `NEVER`). Emphasis by capitals was an
Ollama-era habit; modern models follow plain instructions, and repeated
emphasis dilutes itself. Prefer stating the rule once, plainly.

**Acronym allowlist** (shared by both runners and checked by the parity test):
`API CLI CSV DAG HTML HTTP HTTPS ID IDS ISO JSON LLM MCP OK PDF SDK SQL URI URL
UTC UUID XML YAML`.

A word is a run of `[A-Za-z0-9_']`; it is all-caps when it has at least two
letters and no lowercase letter.
