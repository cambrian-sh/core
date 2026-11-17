# Agent profiles

A profile is one specialist agent, written as data. One runtime,
[`../worker_agent.py`](../worker_agent.py), runs any profile:

```sh
python agents/worker_agent.py --profile data-analyst
```

Owner directive: [`decisions/agent-stack-v2-mcp-tools-and-profiles.md`](../../../cambrian-knowledge/decisions/agent-stack-v2-mcp-tools-and-profiles.md)
(D3: specialists are declarative profiles over one `worker_agent` runtime). Related:
ADR-0139 C2 (agent card), C3 (skill format), D3 (pinned skill bodies), D8 (trust tiers).

A profile names its role; its **method** lives in its skills (`skills/<name>/SKILL.md`),
and its **tools** come from MCP servers (`mcp:<server id>/<tool>`). To add a specialist
(a network analyst, a support triager), write a profile and, if no existing skill
covers its procedure, a skill. Do not write an agent.

## Starter profiles

| Profile | For | Skills | Tools |
| --- | --- | --- | --- |
| [`assistant`](assistant.yaml) | General help: read files and documents, summarise, write checked reports, organise files (create folders, move and rename) | `file-report`, `summarise`, `organise-files` | filesystem reads + `write_file`, `create_directory`, `move_file`, pdf-reader, markitdown, time, calculator |
| [`researcher`](researcher.yaml) | Web research with citations | `web-research`, `summarise` | web-search, fetch, pdf-reader, markitdown, filesystem reads, time |
| [`data-analyst`](data-analyst.yaml) | Data analysis and charts in a sandbox | `exploratory-analysis`, `chart-report` | code-sandbox, filesystem reads + `write_file`, sqlite |

File organising is part of `assistant`, not a separate agent (decision 2026-11-06,
`T-20261106-no-shipped-agent-profile-can-organise-files-or`): under the agent-stack-v2
rule an agent exists only if it brings something a skill cannot, and a file organiser
would bring no code, data access, trust tier or model of its own — `assistant` already
writes files under the same first-party tier, and every write tool still needs the
user's approval. So the procedure is the `organise-files` skill and the reach is two
more allowlist entries. No starter profile reaches `shell` or `terminal`: a deployment
that wants terminal commands writes its own profile (the use-case suite's
`uc-personal-assistant` is an example).

`assistant` is also what a bare `python worker_agent.py` runs. The kernel never launches
it bare: it registers one agent per profile (see "How the kernel runs profiles").

## Format

One file per profile: `agents/profiles/<name>.yaml`. The file name (without `.yaml`) must
equal `name`.

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | The agent id. `^[a-z][a-z0-9_-]{0,62}$`. |
| `description` | yes | Routing text: what the agent is for, first sentence first. One line, no quote characters (the kernel's `AGENT_DESCRIPTION` regex stops at a quote). |
| `role` | yes | A short persona for the prompt's `<Role>`. One or two sentences; the procedure belongs in skills. |
| `capabilities` | yes | Capability tags in the fleet's declared spelling, `lower_snake_case`. See "Capabilities". |
| `skills` | yes | Skill names. Each must exist under `cambrian-core/skills/` (by its frontmatter `name`). This list is the agent's whole skill menu. |
| `tools` | yes | Allowlist globs over kernel tool names. See "Tool globs". |
| `trust_tier` | yes | `system`, `first-party` or `third-party` (ADR-0139 D8). |
| `model` | no | Model preference: a generator id from `configs/providers.json`, or `role:<planner\|verifier\|router\|memory\|facts>` (the generator assigned to that role, resolved per call). The kernel puts it first on the agent's model chain; an unknown or unhealthy id falls back to the step allocation and the default, with a boot WARN (`resolved=false`). |
| `max_tool_rounds` | no | Cap on tool rounds per task, 1..64. Default: the SDK's. |
| `max_tokens` | no | Generation cap per turn, 256..131072. Default: the SDK's (4096). |
| `version` | no | `MAJOR.MINOR.PATCH`, default `1.0.0`. Becomes the manifest version. |
| `examples` | no | Up to 5 example tasks (one line each, at most 300 characters, no duplicates), ADR-0139 C2. Carried on the manifest and published on the agent's A2A card skills (`internal/agentcard`). |
| `streams_output` | no | `true` or `false` (default `false`). `true` declares that the agent pushes its own live step output over the agent-plane `EmitStepOutput` RPC (an adapter around an agent that calls its own model), so the kernel stops forwarding gateway deltas for its steps and one step never streams twice (ADR-0139 amendment 2026-11-06). Carried on the manifest and on the A2A card's Cambrian extension. A profile run by `worker_agent.py` generates through the kernel and leaves it unset. |
| `sandbox_paths` | no | Paths the agent needs beyond its code and runtime when it runs confined (ADR-0157 D1.2), e.g. a model cache or a data directory: a one-line JSON list of `{"path": ..., "write": ...}` objects, `sandbox_paths: [{"path": "/srv/models"}, {"path": "~/cache/hf", "write": true}]`. `path` is absolute (`/…`, `C:\\…` escaped in JSON, `D:/…`) or starts with `~/`; `write` is `true`/`false` (default read-only); no other key, no path twice. Carried on the manifest's `sandbox_paths`; at boot (with `execution.agents.sandbox.enabled`) each path must sit inside the operator's `execution.agents.sandbox.allowlist_cap` and outside the kernel's own state, exactly like an `AGENT_MANIFEST`'s, or the agent is not started and the error names the path. The starter profiles need none: the kernel shows every agent `agents/`, `prompts/` and `skills/`. |

Any other field is an error.

### YAML subset

PyYAML is not an SDK dependency and a profile is flat, so a profile uses the same strict
YAML subset as prompt frontmatter ([`prompts/SPEC.md`](../../prompts/SPEC.md) §2.2),
parsed by the SDK's prompt-library parser:

- `key: value` scalars; quote a value that contains `": "` or `" #"`, or starts with one of `&*!|>%@` or a backquote;
- block lists (`key:` then `  - item` lines) and flow lists (`[a, b]`);
- an inline list of objects as one-line JSON (`[{"path": "/srv/models"}]`, for `sandbox_paths`);
- full-line `#` comments; integers.

No multi-line strings, anchors or nested maps (JSON is YAML flow syntax, so the inline list of objects reads the same in a full parser). Every file in the subset is valid YAML that a
full parser reads the same way. The kernel does NOT use a full parser: it ports this subset
and the validator (`internal/agentprofile`), because a full parser accepts files the
runtime refuses, and a profile the kernel registered but the runtime refused at boot would
be routed to and never answer. The two ports are held together by a Go-written fixture,
`internal/agentprofile/testdata/profile_cases.json`, which `sdk/tests/test_profile_parity.py`
checks (regenerate it with `go test ./internal/agentprofile -run TestProfileCasesFixture
-update-profile-cases` after changing a rule in both). (Avoid bare `yes`/`no`/`on`/`off`
values: YAML 1.1 parsers read them as booleans.)

### Capabilities

The kernel has no closed capability list: its vocabulary is the union of what registered
agents declare (`domain/capability_resolve.go`, ADR-0100), and the L1 gate compares
spellings verbatim unless `execution.canonical_vocab` is on. So use the fleet's spelling.
The SDK's `CAPABILITY_VOCABULARY` (and its kernel port) is the current fleet vocabulary,
which since part B is what the starter profiles declare:

`general_purpose`, `analysis`, `data_analysis`, `code_execution`, `research`,
`web_research`, `file_read`, `file_write`, `file_organisation`, `summarisation`

A tag is a routing contract, so declare one only when the profile's tools can do it:
`file_organisation` means the allowlist holds `move_file` and `create_directory`, not
just `write_file`. The planner sees each agent's tags AND its allowlist (next
section), and is told to choose tags whose agents hold a tool for the step's action.

(The tags only the deleted thin agents declared, such as `planning`, `calculation` and
`shell_execution`, left with them.)

A tag outside it is accepted with a warning (a new tag routes only when a planner step
asks for it). `conversation` and `chat` are reserved for the chat front desk and are
refused. `general_purpose` makes an agent a generalist fallback (the last rung of the
resolution ladder); declare it on at most one or two profiles.

### What the planner sees

Under `execution.capability_contract` the planner's `<capability_clusters>` lists each
tag with the agents that declare it, then one line per profile agent with its
allowlist grouped by server (`- assistant can call: filesystem (read_*, ...,
move_file); pdf-reader (all tools); ...`; `internal/awareness/planner.go`
`agentToolLine`). The allowlist is the ceiling the kernel enforces, so the line is
exactly what the agent could ever call. `planner_cap` / `replan_cap` v2 tell the
planner to pick, for a step that acts on something, a tag only agents with a tool
for that action declare. The step's tags then travel with it into the auction,
including into every fan-out child (`domain.WithPlanStep`), where L1 Declaration
admits only agents that declare them. An agent that still lacks the tool for its step
yields it (`yield_subgoal` with a capability hint) or says so; the SDK's
`react-anti-patterns` v2 forbids imitating the action with a destructive substitute.

### Tool globs

- `mcp:<server id>/<tool glob>`: the server id is literal; the tool part may use `*` and
  `?` (`mcp:filesystem/read_*`, `mcp:fetch/*`). `mcp:*/...` is refused: an allowlist names
  the servers it reaches, so a server added later is not silently included.
- A bare native tool name (`web_search`), optionally with `*`/`?`.
- Refused: whitespace, `[...]` classes, `**`, a bare `*`, any other namespace.

Every tool a listed skill grants (its SKILL.md `tools`) must match the allowlist;
otherwise loading fails with an error naming the tool. The kernel would clip such a grant,
and the skill's procedure would break at that step.

## The runtime (`worker_agent.py`)

- Profile: `--profile <name>`, then `$CAMBRIAN_AGENT_PROFILE` (manual runs only; the
  kernel's agent environment does not pass `CAMBRIAN_*`), then `assistant`.
  `$CAMBRIAN_PROFILES_DIR` and `$CAMBRIAN_SKILLS_DIR` override the directories (defaults:
  this directory, and the nearest ancestor `skills/`).
- Agent id: `--agent-id`, then `$CAMBRIAN_AGENT_ID` (both set by the kernel), then `name`.
- An invalid profile exits with status 2 at boot, before serving.
- Role, `max_tool_rounds` and `max_tokens` come from the profile.
- Skills: the skill menu is exactly the profile's skills, as Tier-1 index lines served by
  the kernel (`ListSkills` with `x-skill-names`). A body loads on demand with `use_skill` and
  stays pinned for the run (ADR-0139 D3, Phase 0). A skill outside the profile cannot be loaded.
- Tools: the menu is the full granted set (`seed_tools_full`) filtered by the allowlist.
  `find_tools`, `describe_tool` and skill-conferred grants go through the same filter, and a
  call outside the allowlist is refused locally with a `denied` result. That filter keeps
  the menu small; the kernel is the enforcing control (next section).

## How the kernel runs profiles (part B, done)

Ticket `T-20261105-agent-stack-v2-worker-agent-runtime-plus`, part B.

1. **Discovery: one agent per profile** (`internal/storage/profile_discovery.go`). The
   kernel's agent scan reads `agents/profiles/*.yaml` with the ported validator, against
   the skills it serves (`skills/`). For each valid profile it registers an agent with
   `ID = name`, `Description = description`, `Runtime = python`, `ExecPath =
   worker_agent.py`, and per-agent launch arguments `--profile <name> --agent-id <id>`
   (`AgentDefinition.LaunchArgs`, appended by `buildAgentCmd`). `SourceHash` covers both
   `worker_agent.py` and the profile file, so editing either re-marks the agent
   provisional. The manifest carries `capabilities`, `version`, `tool_allowlist` and
   `skills`; its legacy `tools` stays empty. The definition carries `profile`,
   `trust_tier` and `model`.
   - `worker_agent.py` itself is never registered (it carries no `AGENT_DESCRIPTION` or
     `AGENT_MANIFEST` any more), and the boot reconcile prunes profile-runtime records not
     discovered this boot: a removed or now-invalid profile, and the pre-part-B
     `worker_agent` registration.
   - An invalid profile is a boot `ERROR` (`agent profile INVALID`, naming the file and
     the rule) and is skipped; the kernel keeps serving. So is a profile whose name is
     already a file agent's id.
   - `System` is never conferred by a profile: `trust_tier: system` without the
     composition root's grant registers a regular agent, with a WARN.
2. **Tools are enforced as grants plus a ceiling** (`app/profile_agents.go`,
   `domain/tool_ceiling.go`).
   - Grants: each profile's globs are expanded against the tool registry into its
     ToolGrants at boot and re-expanded whenever an MCP server's tools change (the MCP
     tool sink re-syncs on every connect, reconnect and drop). A glob matching nothing is
     a boot WARN. An operator's own grant on a profile agent survives a re-sync.
   - Ceiling: the same globs are a hard ceiling (`ToolExecutor.Ceiling`) checked wherever
     the ADR-0051 `RestrictedTools` ceiling is: before the `tools_unrestricted` bypass,
     before a skill's run-scoped grants, and on the menu and describe paths. So **a
     profile agent stays inside its allowlist under `tools_unrestricted=true`**, a skill
     cannot widen it, and an operator grant outside it is ineffective (widen a profile
     by editing it).
   - Dangerous tools stay approval-gated and effect classes still apply; a grant never
     bypasses them.
   - Contributed tools (ADR-0127, since 2026-11-15): a profile may list `local:*` or
     `local:<machine id or *>/<tool glob>`. That only lets its CEILING admit those tools —
     contributed tools are not in the registry, so the glob expands to no grant; the
     agent reaches them only with the operator's explicit `local:*` fleet grant
     (decisions/local-fleet-grant-holders.md: only agents whose input is the owner's own
     words), and the fleet is always the run beneficiary's own machines.
3. **Model preference** is bound at the LLM gateway: the generator is the first rung of
   the model chain of every lease bound to the agent (resolved per call, so a role
   reassignment is live). A dangling id or an unassigned role is a boot WARN with
   `resolved=false`, and the agent runs on the step allocation and the default.
4. **Trust tier** is stored on the definition for the agent card and the console, and read
   at launch (ADR-0139 D8 stage 1, `execution.agents.sandbox`): first-party agents launch
   as before (env allowlist + SEC-01 cap); a `third-party` profile is refused unless
   `execution.agents.sandbox.third_party.on_unavailable` is `degrade`. With
   `execution.agents.sandbox.enabled` (ADR-0157 S1, default off) every profile agent, of
   every tier, runs through the kernel's sandbox without sight of the kernel's own state
   (config, key file, data directories) and with a private scratch directory. On Windows
   that is an AppContainer (ADR-0157 S5): the interpreter must be one the kernel can grant
   (a user-owned install or venv; an Administrators-owned `C:\PythonXY` falls back to low
   integrity, where reads are not hidden, and the boot log says so and names the fix). The
   starter profiles need no `sandbox_paths`: `worker_agent.py` needs only the agents,
   prompts and skills directories, which the kernel shows every agent.

Only profiles get allowlist grants. A `tool_allowlist` in an ordinary agent's own
`AGENT_MANIFEST` is not turned into grants: that would be a self-asserted privilege.

## Composed workers (ADR-0145 D12, `execution.workers.composed`, default off)

With the flag on, a profile is a **profession**, not the agent that runs a plan step. The
gatekeeper still decides which professions cover a step (capabilities and effects); the
kernel then composes ONE worker for the step on the runtime `worker_agent.py --composed`
(agent id `composed_worker`): the professions' skills (profile + grants, served by the
kernel, sorted, capped by `max_skills`), the tools inside their allowlists that the person's
scope also allows (a step's declared effects do not narrow them, ADR-0145 D14), the step's brief and its input ledger
references. The composition is bound to the step's lease and enforced there (tool ceiling,
skill index, no operator skill request), recorded in the run ledger, and sent to the
runtime as data (`_composition`). The runtime's role and rules are developer-written
(`prompts/composed_worker`); a profile's `role` is not used for composed steps. A step
whose selected agent is not a profile (a code or service agent) is dispatched to it as
before. Code: `internal/metabolism/compose`, `app/composed_workers.go`,
`sdk/cambrian_agent_sdk/composition.py`.

## Tool ids (reconciled)

The profiles and skills use the server ids and tool names of the curated set in
`configs/mcp.json.example` (fetch, web-search, filesystem, code-sandbox, shell, git,
sqlite, time, calculator, pdf-reader, markitdown, sequential-thinking, terminal). Reconciled against it
in part B: every id below is a configured server, every glob matches a tool its server
lists, and every tool a skill names is listed. `TestShippedProfiles_ToolIDsMatchTheCuratedMCPSet`
(`internal/agentprofile`) keeps it that way, and refuses a starter profile that reaches
`shell` or `terminal` (Desktop Commander). `sdk/tests/test_profiles.py` and
`worker_agent_test.py` catch a skill/allowlist mismatch.

| Server id | Tool names used | Used by |
| --- | --- | --- |
| `filesystem` | `read_*`, `list_*`, `directory_tree`, `search_files`, `get_file_info`, `write_file`, `create_directory` and `move_file` (assistant); skills name `read_text_file`, `read_file`, `read_multiple_files`, `list_directory`, `directory_tree`, `search_files`, `get_file_info`, `write_file`, `create_directory`, `move_file` | assistant, researcher (reads), data-analyst; file-report, summarise, organise-files, exploratory-analysis, chart-report |
| `fetch` | `*`; skill names `fetch` | researcher; web-research |
| `web-search` | `*`; skills name `search`, `fetch_content` | researcher; web-research |
| `pdf-reader` | `*`; skills name `read` | assistant, researcher; summarise, organise-files, web-research |
| `markitdown` | `*`; skills name `convert_to_markdown` | assistant, researcher; summarise |
| `code-sandbox` | `*`; skills name `execute_code` | data-analyst; exploratory-analysis, chart-report |
| `sqlite` | `*`; skills name `list_tables`, `execute_sql` | data-analyst; exploratory-analysis |
| `time` | `*` | assistant, researcher |
| `calculator` | `calculate` | assistant (its role says to use it for any non-trivial arithmetic) |

Not used by any starter profile: `shell`, `git`, `sequential-thinking`, `terminal`. A
deployment's own `configs/mcp.json` may use other ids (`firecrawl`, `fast-filesystem`,
`super-shell`); a profile cannot reach a server whose id it does not name, and the kernel
warns at boot about each glob that matches no registered tool.
