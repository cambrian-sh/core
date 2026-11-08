# System skills

Authored procedures the kernel serves to agents (ADR-0046, format ADR-0139 C3).
Each skill is a directory holding one `SKILL.md`: YAML frontmatter followed by a
Markdown body.

```
skills/
  file-report/            structured report from files, every claim checked
    SKILL.md
  summarise/              faithful summary; refuses when there is no source
  organise-files/         create folders, move and rename files; never rewrite to move
  web-research/           search, read, cross-check, cite
  exploratory-analysis/   load, profile, clean, stats, findings (code sandbox)
  chart-report/           charts in the sandbox plus a report that references them
```

The agent profiles in `agents/profiles/` name which skills each specialist loads; every
tool a skill lists must fall inside the allowlist of each profile that uses it
(`agents/profiles/README.md`).

## Frontmatter

| Key | Meaning |
| --- | --- |
| `name` | The identity agents load the skill by (`use_skill`). Required. |
| `description` | What the skill is for. The menu line is derived from its first sentence, capped at 120 characters (ADR-0045 D3), so put the purpose first. |
| `tools` | Tools the skill grants for the rest of the run when it is loaded (ADR-0046 D6). The grant is intersected with what policy allows the agent (ADR-0085 D4). A tool the registry does not know, or that policy refuses, is dropped with a `skill_grant_clipped` log line and the skill still loads. Dangerous tools still need operator approval. |
| `scope` | Classification tags (ADR-0046 D9). `[]` means unscoped: visible to every agent whose read scope requires no tags. |
| `version`, `trust` | ADR-0139 C3 metadata, parsed leniently. `version` becomes the shipped skill's lifecycle version (else `sha-<12 hex>` of the file). For an agent-proposed skill (ADR-0139 D13) the kernel ignores both: trust is always `agent-proposed` and the version is the ledger's. |

MCP tool names have the form `mcp:<server id>/<tool>`, where `<server id>` is the
`id` in `configs/mcp.json`. `file-report` names `mcp:filesystem/...`, which is the
`@modelcontextprotocol/server-filesystem` server under the id `filesystem` (as in
`configs/mcp.json.example`). A deployment that uses a different id gets those grants
clipped; copy the skill and change the prefix.

## How the kernel finds this directory

At boot the kernel reads `skills/` **relative to its working directory**
(`app/app.go`, `skilldiscovery.LoadRegistry("skills", ...)`), the same way it reads
`tools/`. There is no config key for the path.

- Running from `cambrian-core/` (`go run ./cmd/orchestrator`, `bin/` scripts): found.
- Any other deployment: start the kernel with its working directory set to a folder
  that contains `skills/`, or copy or link this directory next to the binary's
  working directory.
- Docker: the image's working directory is `/app`, so the image needs
  `COPY cambrian-core/skills/ /app/skills/`. The Dockerfile does not do this yet
  (tracked as a ticket).

A missing directory is not an error. The kernel boots with no system skills. Skills
are re-read and re-indexed on every boot, and a skill whose directory was deleted is
removed from the index.
