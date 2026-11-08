# MCP servers Cambrian ships

Cambrian gets its tools only from MCP servers. This is an owner directive,
[`decisions/agent-stack-v2-mcp-tools-and-profiles.md`](../../cambrian-knowledge/decisions/agent-stack-v2-mcp-tools-and-profiles.md).
Every MCP tool still goes through `ToolExecutor`, which applies grants, effect classes,
approvals and provenance (ADR-0043, ADR-0086, ADR-0139 D8).

**From the console (preferred).** The operator console's Integrations page installs any
entry of the curated catalog, collects its credentials, starts and stops it live, shows its
health and tests the connection. Nothing below needs editing for that path. The catalog is
[`internal/integrations/catalog.json`](../internal/integrations/catalog.json), embedded in the
kernel; see "Integrations from the console" below.

**By file (still supported).** The server set lives in two files:

- [`mcp.json.example`](mcp.json.example) is the **keyless core set**: fetch, keyless search,
  filesystem, code sandbox, shell, git, SQLite, time, documents and sequential-thinking.
  Copy it to `mcp.json` (gitignored).
- [`mcp.integrations.json.example`](mcp.integrations.json.example) is a **catalog of keyed
  integrations**: GitHub, Brave, Tavily, Exa, SearXNG, Google Workspace, Microsoft 365, Slack,
  Postgres, Notion, Atlassian and Linear, plus the optional Playwright, Excel and Word
  servers. The kernel does not load this file. Copy entries from it into `mcp.json` one at
  a time.

> **The live handshake is not yet verified. It is pending owner approval.**
> Every version below exists on its registry; each was checked on 2026-11-05 for its latest
> stable version, release date, maintainer and license. No server has yet been installed
> or dialled by an MCP handshake on a Cambrian host. Tool names come from each server's
> docs or source, not from a `tools/list`, so an advertised name may differ.
>
> If a name differs, the fail-closed default catches it: the unlisted tool registers as
> dangerous. Verification is tracked in ticket `T-20261105-curate-verify-and-pin-the-mcp-server-set`,
> with the live run as `T-20261105-live-install-and-handshake-verification-of-the`.
> An approved handshake attempt on 2026-11-05 was refused by the agent's permission
> layer, so nothing ran. After a live run, record each
> row's result in the "Handshake" column.

## Catalog

Legend:

- **Policy** gives the default policy, then the exceptions:
  - **R**: `effects: [read]`, not dangerous.
  - **R+E**: `[read, egress]`, not dangerous. There is no approval prompt, but egress is
    visible to grants and to the egress auditor.
  - **D**: dangerous, so operator approval is required.
  - **FC**: fail-closed. There is no default policy, so every unlisted tool is D with
    read+write+egress.
- **Reg.** is registry-verified (Y means checked on npm, PyPI or GitHub on 2026-11-05).
- **HS** is the live handshake.

### Core (keyless), in `mcp.json.example`

| Category | id | Package / version | Runtime | Credentials | Policy | Reg. | HS | Tools (per docs) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Web fetch | `fetch` | PyPI `mcp-server-fetch==2026.11.4` (2026-11-04), MIT, MCP steering group (official reference) | uvx | none | `fetch` R+E | Y | not verified | `fetch` |
| Web search, keyless | `web-search` | PyPI `duckduckgo-mcp-server==0.7.0` (2026-09-04), MIT, Nick Clyde (community) | uvx | none | R+E per tool; FC otherwise | Y | not verified | `search`, `fetch_content` |
| Filesystem | `filesystem` | npm `@modelcontextprotocol/server-filesystem@1.0.0` (2026-10-11), official. The latest is 2.2.0 (2026-11-04), which needs node>=24 | npx (node 22+) | none; roots are given as args | reads R; write/edit/append/create/move/copy D; FC | Y | not verified | `read_text_file`, `read_file` (deprecated alias), `read_media_file`, `read_multiple_files`, `list_directory`, `list_directory_with_sizes`, `directory_tree`, `search_files`, `get_file_info`, `list_allowed_directories`, `write_file`, `edit_file`, `append_file`\*, `create_directory`, `move_file`, `copy_file`\* |
| Code sandbox | `code-sandbox` | PyPI `llm-sandbox[mcp-docker]==0.3.46` (2026-10-16), MIT, Duy Huynh (vndee, community, 1.1k stars, active) | uv + **Docker** | none; `SANDBOX_*` hardening variables | `execute_code` D (read+write+egress); language info R; FC | Y | not verified | `execute_code` (captures plots as images), `get_supported_languages`, `get_language_details` |
| Shell | `shell` | PyPI `mcp-shell-server==1.1.14` (2026-10-19), MIT, tumf (community) | uvx (Python 3.11+) | none; `ALLOW_COMMANDS` | `shell_execute` D (read+write+egress) | Y | not verified | `shell_execute` |
| Git | `git` | PyPI `mcp-server-git==2026.11.4` (2026-11-04), MIT, official | uvx | none; `--repository` | history/diff R; add/commit/reset/branch/checkout/merge D; FC | Y | not verified | `git_status`, `git_diff_unstaged`, `git_diff_staged`, `git_diff`, `git_log`, `git_show`, `git_branch`, `git_remote`, `git_add`, `git_commit`, `git_reset`, `git_create_branch`, `git_checkout`, `git_merge` |
| SQLite | `sqlite` | npm `@toolbox-sdk/server@1.15.0` (2026-10-23), Apache-2.0, Google (googleapis/mcp-toolbox) | npx | `SQLITE_DATABASE` (a path, not a secret) | `list_tables` R; `execute_sql` D | Y | not verified | `list_tables`, `execute_sql` |
| Time | `time` | PyPI `mcp-server-time==2026.11.4` (2026-11-04), MIT, official | uvx | none | R (server default) | Y | not verified | `get_current_time`, `convert_time`, `get_time_ago`\* |
| Calculator (exact arithmetic) | `calculator` | PyPI `mcp-server-calculator==0.2.1` (2026-08-08), MIT, He Jie (githejie, community, about 160 stars) | uvx (Python 3.10+) | none | `calculate` R; FC | Y | not verified | `calculate(expression)`: an `ast` walker over numbers, `+ - * / // % **` and the `math` module; no `eval`, no I/O |
| Documents (PDF/Office read) | `pdf-reader` | npm `@sylphx/pdf-reader-mcp@8.5.9` (2026-11-03), MIT, SylphxAI (repo now `SylphxAI/anymd`) | npx | none | R+E per tool; FC | Y | not verified | `read`, `outline`, `search`, `inspect` |
| Documents (Office to Markdown) | `markitdown` | PyPI `markitdown-mcp==0.0.1a8` (2026-10-21, pre-release), MIT, Microsoft | uvx | none | R+E | Y | not verified | `convert_to_markdown(uri)` (http, https, file or data URIs) |
| Reasoning scratchpad | `sequential-thinking` | npm `@modelcontextprotocol/server-sequential-thinking@1.0.0` (2026-10-11), official. 2.x needs node>=24 | npx | none | R (server default) | Y | not verified | `sequentialthinking` |

\* The tool appears in the current upstream docs but may postdate the pinned version. If the
pinned version does not advertise it, the policy entry is harmless.

### Integrations (keyed or optional), in `mcp.integrations.json.example`

| Category | id | Package / version / endpoint | Runtime | Credentials (env) | Policy | Reg. | HS | Tools (per docs) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| GitHub, read | `github` | remote `https://api.githubcopilot.com/mcp/readonly`, official (github/github-mcp-server v2.1.0, 2026-11-03, MIT) | http | `GITHUB_MCP_PAT` (bearer) | R+E (read-only endpoint) | Y | not verified | repos, issues, pull requests, actions, code security, … (read tools of the default toolset) |
| GitHub, write | `github-write` | remote `https://api.githubcopilot.com/mcp/` | http | `GITHUB_MCP_PAT` | FC | Y | not verified | default toolset, including create/update tools |
| Web search, keyed | `brave-search` | npm `@brave/brave-search-mcp-server@2.1.4` (2026-09-17), MIT, Brave (official) | npx | `BRAVE_API_KEY` | R+E | Y | not verified | web, local, image, video, news, summarizer search |
| Web search, keyed | `tavily` | npm `tavily-mcp@0.2.22` (2026-08-05), MIT, Tavily (official) | npx | `TAVILY_API_KEY` | R+E | Y | not verified | search, extract, crawl, map |
| Web search, keyed | `exa` | npm `exa-mcp-server@3.4.3` (2026-11-03), MIT, Exa (official) | npx | `EXA_API_KEY` | R+E | Y | not verified | web search, code context, crawling, … |
| Web search, self-hosted | `searxng` | npm `mcp-searxng@2.10.0` (2026-11-03), MIT, ihor-sokoliuk (community) | npx (node 22+) | `SEARXNG_URL` (not a secret) | R+E per tool; FC | Y | not verified | `searxng_web_search`, `web_url_read` |
| Google Workspace, one shared account | `google-workspace` | PyPI `workspace-mcp==2.3.0` (2026-10-30), MIT, Taylor Wilsdon (community) | uvx | `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `USER_GOOGLE_EMAIL`, plus a browser consent | R+E with `--read-only` | Y | not verified | Gmail, Drive and Calendar search/read (core tier) |
| Google Workspace, per person (read) | `google-workspace-personal` | PyPI `workspace-mcp==2.3.0`, same server, as an operator-run loopback HTTP sidecar in external-OAuth mode | http `127.0.0.1:8000/mcp` | per user: the person's Google token as `Authorization: Bearer` (ADR-0140/0141) | R+E; write-named tools D | Y | not verified | Gmail, Drive, Calendar read incl. `query_freebusy` (`--read-only`, extended tier) |
| Google Workspace, per person (write) | `google-workspace-personal-write` | same, second sidecar | http `127.0.0.1:8001/mcp` | per user, separate grant with send scopes | FC; reads R+E; `send_gmail_message`, `draft_gmail_message`, `manage_event` D | Y | not verified | Gmail send/draft, Calendar create/update/delete |
| Microsoft 365 | `microsoft-365` | npm `@softeria/ms-365-mcp-server@0.179.1` (2026-11-04), MIT, Softeria (community) | npx | `MS365_MCP_CLIENT_ID`, `MS365_MCP_TENANT_ID` (optional), plus a device-code login | R+E with `--read-only` | Y | not verified | mail, calendar, OneDrive, contacts, to-do; Teams and SharePoint with `--org-mode` |
| Slack | `slack` | npm `slack-mcp-server@1.3.0` (2026-05-14), MIT, korotovsky (community) | npx | `SLACK_MCP_XOXP_TOKEN` or `SLACK_MCP_XOXB_TOKEN`; `SLACK_MCP_ADD_MESSAGE_TOOL` | reads R+E; posting, reactions and usergroup changes D; FC | Y | not verified | `channels_list`, `conversations_history`, `conversations_replies`, `conversations_search_messages`, `attachment_get_data`, `usergroups_*`, `conversations_add_message`, `reactions_add`, `reactions_remove` |
| PostgreSQL | `postgres` | npm `@toolbox-sdk/server@1.15.0`, Google | npx | `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DATABASE`, `POSTGRES_USER`, `POSTGRES_PASSWORD` | `execute_sql`, `get_query_plan` D; 27 catalog/stat tools R; FC | Y | not verified | `execute_sql`, `list_tables`, `list_schemas`, `list_indexes`, `database_overview`, … |
| Notion | `notion` | npm `@notionhq/notion-mcp-server@2.6.1` (2026-10-29), MIT, Notion (official, local) | npx | `NOTION_TOKEN` | FC (tool names are generated from Notion's OpenAPI spec) | Y | not verified | `API-*` operations: search, pages, blocks, databases, comments |
| Atlassian | `atlassian` | remote `https://mcp.atlassian.com/v2/mcp`, official (Rovo MCP) | http | `ATLASSIAN_MCP_API_KEY` (service-account bearer; needs an admin to enable token auth) | FC | Y (docs) | not verified | Jira, Confluence, JSM, Bitbucket, Compass |
| Linear | `linear` | remote `https://mcp.linear.app/mcp/readonly`, official | http | `LINEAR_API_KEY` (bearer) | R+E (read-only endpoint) | Y (docs) | not verified | issues, projects, comments (find) |
| Browser (optional) | `playwright` | npm `@playwright/mcp@0.0.84` (2026-10-31), Apache-2.0, Microsoft | npx, plus a browser download | none | FC | Y | not verified | `browser_navigate`, `browser_click`, `browser_type`, `browser_snapshot`, … |
| Office authoring (optional) | `excel` | PyPI `excel-mcp-server==2.2.6` (2026-10-31), MIT, Haris Musa (community) | uvx | `EXCEL_FILES_PATH` (not a secret) | FC | Y | not verified | workbook, sheet, cell, formula and chart operations |
| Office authoring (optional) | `word` | PyPI `office-word-mcp-server==1.1.11` (2025-12-31), MIT, GongRzhe (community) | uvx | none | FC | Y | not verified | create/edit `.docx` |

### Considered and rejected

| Candidate | Why not |
| --- | --- |
| `@cfdude/super-shell-mcp` | It no longer exists on npm (404 on the registry, 2026-11-05). It is replaced by `shell`. |
| Pydantic `mcp-run-python` (PyPI 0.0.22) | **The repo was archived on 2026-01-30.** Pydantic's notice says there is "no safe way to run Python within pyodide safely". Python running in Pyodide can execute arbitrary JavaScript in the Deno runtime, and Deno cannot cap memory. Its successor, `pydantic/monty`, ships no MCP server. It also needs Deno, which is not installed on the dev host. |
| E2B MCP (`@e2b/mcp-server`, PyPI `e2b-mcp-server`) | The repo is archived and the npm package is deprecated. E2B's hosted sandboxes remain usable through its SDK, but nothing we can pin as an MCP server. |
| `@modelcontextprotocol/server-postgres`, `server-slack`, PyPI `mcp-server-sqlite` | These were moved to `modelcontextprotocol/servers-archived`, and npm marks them unsupported. |
| `crystaldba/postgres-mcp` (PyPI 0.3.0) | Its last release was 2025-05-16. Toolbox is vendor-maintained and covers both SQLite and Postgres. |
| PyPI `github-mcp-server` | **It is not GitHub's server.** It is a third-party, AGPL package under a confusable name. Use only `github/github-mcp-server`: the remote endpoint, the GHCR image or the release binaries. |
| Desktop Commander (`@wonderwhy-er/desktop-commander@0.3.1`) | It is well maintained (MIT, about 10k stars) but far broader than a shell: it adds file editing, process control and search, and enables telemetry by default. Its tools would also overlap with `filesystem`. Reconsider it if `mcp-shell-server` fails on Windows. |
| Vendor remote servers: Google Workspace MCP (Developer Preview, enrollment-gated; see "Google Workspace per person"), Slack `mcp.slack.com`, Microsoft Agent 365 / Work IQ, Notion `mcp.notion.com` | These are the preferred long-term targets, but all of them require **per-user OAuth 2.1**. Per-user credentials now exist (ADR-0140, below) with operator-registered clients; these servers still need dynamic client registration or a catalog entry each. |

## Prerequisites

| Runtime | Needed by | Note |
| --- | --- | --- |
| Node.js **22+** with `npx` | filesystem, sequential-thinking, pdf-reader, sqlite/postgres (Toolbox), searxng, Brave, Tavily, Exa, Notion, Microsoft 365, Slack, Playwright | The dev host has 22.14. The official 2.x reference servers need node **24**: upgrade node before you move those pins. `npx -y pkg@x.y.z` downloads on first run, so the first boot is slow. |
| `uv` / `uvx` | fetch, web-search, shell, git, time, calculator, markitdown, code-sandbox, Google Workspace, Excel, Word | `uvx pkg==x.y.z` resolves the pinned version in an isolated, cached environment. |
| **Docker** (daemon running) | code-sandbox | llm-sandbox starts one container per execution; the first call pulls its language image. Podman or Kubernetes backends also work (`BACKEND`, plus the `llm-sandbox[mcp-podman]` or `llm-sandbox[mcp-k8s]` extra). |
| Deno | nothing shipped | Only the rejected `mcp-run-python` needed it. |
| A browser build | playwright | It is downloaded on first use. |

## Integrations from the console

The kernel side of "connect your tools without editing JSON"
(`T-20261106-integrations-manage-mcp-servers-from-the`). Operator RPCs, capability
`integrations`: `ListIntegrationCatalog`, `ListIntegrations`, `InstallIntegration`,
`UpdateIntegration`, `SetIntegrationSecret`, `ClearIntegrationSecret`, `SetIntegrationEnabled`,
`TestIntegration`, `RemoveIntegration`, `RevertIntegrationToFile`, plus `integration_health` events on the
operator feed.

- **Catalog.** `internal/integrations/catalog.json` (embedded) holds every server in the tables
  above with its pinned launch spec, credential and setting fields (name, label, secret or not,
  help), default tool policy, trust tier, runtimes, platforms and an optional read-only probe
  tool. A deployment adds or replaces entries with `mcp.catalog_file` (same shape). The loader
  refuses an entry that declares no policy and does not say `fail_closed: true`, a non-dangerous
  default policy that writes, a credential-shaped field that is not secret, and a probe tool
  that is not read-only. **Edit the catalog, not just this file, when a pin or policy changes.**
- **State.** An installed integration is an ordinary MCP server entry stored **per id** in the
  ADR-0101 config store (`mcp.server_overrides.<id>`), with `enabled`, `env_values` (literal,
  non-secret settings), `secret_env` (names of secrets), `catalog_id` and `catalog_version`.
  The console never rewrites `mcp.json`.
- **Precedence**, per server id, lowest to highest: the file layers (`config.json` … `mcp.json`)
  < the store's per-id entry (it replaces the file entry with the same id; removing a
  file-declared server stores a tombstone) < an env var pinning the whole `mcp.servers` list.
  File-only servers stay visible next to installed ones. A store written by contract 0097 (one
  whole `mcp.servers` list) is migrated to per-id entries at boot.
- **Secrets.** Encrypted at rest (`mcp:<id>:env:<NAME>` for a stdio variable,
  `mcp:<id>:token` for an http token), injected at launch into **that server's process only**
  (`procenv.ServerEnv`), never logged, never returned: reads say set / source / last updated.
  ADR-0101 D5 holds: a kernel env var outranks the stored value when the server passes that name
  through (`env`), or for a token when it is `auth.token_env`. Errors shown as health are
  redacted of every injected value.
- **Live lifecycle.** Install, enable, disable, reconfigure and secret changes start, stop or
  restart that one server now and add or withdraw its tools (with their policies) from the
  registry and the tool index. A response says `restart_required` only when the kernel has no
  live connector. Enabling is refused while a required field is missing.
- **Health.** Per server: `stopped`, `starting`, `ready` or `error`, last handshake, tool count,
  redacted last error, restart count; every change is pushed on the feed. Liveness is checked
  every 30 s: a server is alive when it ANSWERS a ping, even with a JSON-RPC error (servers on
  the stateless 2026-07-28 protocol have no `ping` method; the ping carries their required
  `_meta`). A closed connection is an `error` at once; an unanswered ping is tolerated once (a
  serial server busy with a long call) and the second in a row drops it
  (`T-20261104-mcp-health-ping-drops-pdf-reader-and-fetch`).
- **Revert to file.** For a server `mcp.json` declares, `RevertIntegrationToFile` drops the
  console's override (`overrides_file: true`) or its removal tombstone, so the file entry applies
  again, and restarts the server on it now. Stored secrets are kept.
- **Test.** Uses the live session when the server is ready, else launches it once with the
  stored config; lists each advertised tool with the policy it would get (`tool`, `default` or
  `fail_closed`); calls the catalog's probe tool only if its policy is non-dangerous and
  read/egress-only.

## Enabling an integration by file

1. Pick its entry in `mcp.integrations.json.example`, read its `_note`, and copy the object into
   the `mcp.servers` array of `configs/mcp.json`. Unknown keys such as `_note` are ignored by the
   loader.
2. Provide the credential.
   - **stdio servers**: set each variable named in the entry's `env` list in the **kernel's**
     environment. `env` is a passthrough list of names, never values. A credential-shaped name
     reaches the child only because it is listed (`procenv.AllowlistedNamedSecrets`), and
     `CAMBRIAN_*`/`LANGFUSE_*` never pass.
   - **http servers**: set `auth.token_env`, or save the token from the operator console. The
     console stores it encrypted under `mcp:<id>:token` (ADR-0101) — or, on a premium kernel, in
     the org account bound to the server (ADR-0141, see "Shared credentials as org accounts"
     below) — and a set environment variable wins over the stored token.
3. Restart the kernel, or save the server from the console (contract 0097). The console's
   **Probe** connects once and lists the advertised tool names.
4. Compare the probed names with the entry's `tools` list:
   - For a server that can write, list its read tools as non-dangerous. Leave writes dangerous.
   - Never add a `default_tool_policy` to a server that has write tools. That would make
     every tool a future version adds safe by default.
5. Grant the tools to a profile or skill by their full name, `mcp:<id>/<tool>`.

**Do not paste the whole integrations file into `mcp.json` enabled.** The kernel dials every
enabled server at boot, concurrently (`Connector.ConnectAll`). A server with no credentials only
fails, gets skipped and is retried, but it still costs a process spawn and a reconnect loop. Give
an entry `"enabled": false` to keep it configured but not started.

Per-server settings no longer have to be kernel-wide: `"env_values": {"SANDBOX_NETWORK_MODE":
"none"}` gives a literal value to that one stdio server (credential-shaped names are refused —
use `secret_env` plus the console), and an `args` element `"${NAME}"` is filled from
`env_values[NAME]` (dropped when unset).

## Trust and sandbox notes

- **The policy is ours, not the server's.** Tool annotations a server advertises
  (`readOnlyHint`, `destructiveHint`) are descriptive only (A1.5). The `tools` and
  `default_tool_policy` entries in config are what the kernel enforces.
- **stdio servers are contained by trust tier** (ADR-0139 D8, `internal/sandbox`, 2026-11-06).
  See "Sandboxing by trust tier" below. Containment is not a reason to relax tool policy:
  effect classes and approvals apply on every runner.

## Sandboxing by trust tier

Every stdio server launch (boot, console save, Test, per-user instance, `register_mcp`) goes
through `internal/sandbox`. The launch env is still the `procenv` allowlist; the sandbox adds
process containment.

**Tier** (first match wins): the server's own `trust_tier` (an operator decision) → its catalog
entry's `trust_tier` through `mcp.sandbox.catalog_tiers` (`official` → `vetted`, `curated` →
`vetted`, `community` → `third-party`; `curated` = a community server Cambrian reviewed and
pins, e.g. `code-sandbox`, owner decision 2026-11-06) → `first-party` for a plugin's server → `mcp.sandbox.default_tier`
(`third-party`). `system` is refused: no launched process holds it.

**What each tier wants** (defaults; every value is `mcp.sandbox.tiers.<tier>.*`):

| | first-party | vetted | third-party |
|---|---|---|---|
| env allowlist, own workdir (`<work_root>/<tier>/<id>`, own TEMP), kill tree | yes | yes | yes |
| memory / processes / CPU time | 4096 MB / 128 / off | 4096 MB / 128 / off | 2048 MB / 64 / 3600 s |
| stall kill (request unanswered) / one-message cap | 900 s / 32 MiB | 900 s / 32 MiB | 600 s / 16 MiB |
| writes confined to the workdir, no network unless declared, low-privilege identity | no | no | yes |
| a wanted control unavailable here (`on_unavailable`) | degrade (start, report) | degrade | **refuse** |

A server declares its needs in `sandbox`: `network` (`none` default | `egress`), `hosts`
(recorded, **not filtered** — no egress proxy yet), `root`, `runner` (`native` | `docker`),
`image`, `memory_mb`, `max_processes`. Declared egress is a grant, not a missing control.
Every stdio catalog entry declares egress, because `npx`/`uvx` fetch the pinned package at
launch; a third-party server with no network needs a pre-baked image on the docker runner.

**What each platform can enforce** (the per-platform capability matrix):

| Control | Windows native | Linux native | macOS native | Docker runner (any host) |
|---|---|---|---|---|
| env, workdir, output cap, stall kill | yes | yes | yes | yes (workdir at `/work`) |
| kill tree | Job Object (kill-on-close) | process group; cgroup.kill with `cgroup_parent`; bwrap `--die-with-parent` | process group | `docker rm -f` |
| memory | Job Object, killed at the limit (completion-port watcher) | `ulimit -d` per process; tree total with `cgroup_parent` | `ulimit -d` per process | `--memory` |
| processes | Job Object active-process limit | `cgroup_parent` pids.max, or `ulimit -u` inside bwrap's user namespace; else **missing** | **missing** | `--pids-limit` |
| CPU time | Job Object per-job user time | `ulimit -t` per process | `ulimit -t` | `--ulimit cpu` |
| fs confinement | low-integrity token + low-labelled workdir: **writes** confined, reads not | bubblewrap: ro root, rw workdir (seen at `/tmp/work`), private /tmp, `hide_paths` masked; else **missing** | **missing** | host fs invisible, `--read-only` |
| network deny | **missing** (AppContainer/WFP not built) | bubblewrap `--unshare-net`; else **missing** | **missing** | `--network none` |
| low privilege | low integrity level (same user SID) | uid 65534 only for a root kernel with a traversable `work_root`; else **missing** | uid 65534 for a root kernel | `--user 65534:65534 --cap-drop ALL no-new-privileges` |

Consequences: on Windows native a third-party server that declares no egress is refused (use
`runner: docker`); on Linux native, third-party needs bubblewrap and a root kernel, else it is
refused; a degrade tier whose docker runner is unavailable falls back to native and says so.

**Status:** the integration's `policy_summary` ends with the effective containment, e.g.
`containment third-party/native: job_object+low_integrity [env workdir kill_tree … fs_confine
low_privilege]; notes: …`, and a refusal shows in the health `last_error`
(`sandbox refused to start … missing network: …`).

**Known caveats:** under low integrity, `npx`/`uvx` caches move into the workdir
(`mcp.sandbox` `ConfinedEnv`: `npm_config_cache`, `UV_CACHE_DIR`, `UV_TOOL_DIR`, …), so the first
start re-downloads; `code-sandbox` needs the Docker engine pipe, which a low-integrity process
likely cannot open (give it `trust_tier: vetted` by operator decision, or run it elsewhere).
- **`code-sandbox` is the only entry with real isolation**, and only as configured:
  - Code runs in a fresh Docker container.
  - Harden it by setting these in the kernel environment:
    `BACKEND=docker`, `SANDBOX_NETWORK_MODE=none`, `SANDBOX_READ_ONLY=true`,
    `SANDBOX_CAP_DROP=ALL`, `SANDBOX_SECURITY_OPT=no-new-privileges:true`,
    `SANDBOX_MEMORY=2g`, `SANDBOX_CPU_COUNT=1`.
  - With `SANDBOX_NETWORK_MODE=none`, on-demand `pip install` of pandas, numpy or matplotlib
    fails. Use an image with them pre-installed, or accept network access and keep egress
    declared.
  - Plots come back as images (base64 PNG content).
  - `execute_code` stays dangerous with write+egress until a live run proves the hardening took.
  - Windows-native backends (`windows-mxc`, `wsl2-bubblewrap`) exist in the llm-sandbox
    library. The MCP server documents only docker, podman and kubernetes.
- **`shell` is not a sandbox.** `ALLOW_COMMANDS` limits only which executables the server
  launches directly. An allowed program keeps the kernel user's files, network and credentials,
  and can spawn children; the upstream README says so explicitly. Keep the list minimal; never
  include an interpreter, `curl` or a package manager. Execution on Windows is unverified.
- **`calculator` is pure computation** (no files, no network), so `calculate` is R. It is in
  the set because a model multiplying in its head is wrong often enough to matter (E2E run
  20261105T000624Z: `724*37` answered 26888, correct 26788). The assistant profile's role tells
  it to call the tool for any non-trivial arithmetic. Caveat: the evaluator does not cap
  exponents, so `9**9**9` keeps the server busy until `call_timeout_ms`; the profile reaches only
  `calculate`, and the worst case is a stuck call, not a breach. For anything beyond a single
  expression (tables, statistics, code) use `code-sandbox`.
- **`fetch` and the document readers reach any URL, including localhost and intranet
  addresses.** That makes them a server-side request forgery (SSRF) path into local services
  such as the Postgres admin port or the Firecrawl API. Run the kernel where that matters, or
  block it in a profile.
- **SQL servers:** `execute_sql` takes raw SQL. Connect with a read-only database role or a
  copy of the file. The dangerous flag is a prompt, not a database permission.
- **Prompt injection across integrations:** Google warns against connecting Gmail, Drive and
  Chat to one agent at the same time. A mail or page the agent reads can instruct it to send
  data elsewhere. Keep write and send tools dangerous, and give read-only integrations to
  profiles that have no egress-capable write tools.
- **Pins:** an `npx -y`/`uvx` entry pinned to an exact version still downloads from the
  registry on a cold cache. Integrity rests on the registry and lock-free resolution of
  transitive dependencies. For production, mirror the packages or use digest-pinned images.

## Per-user credentials (ADR-0140, proposed; accounts moved to premium by ADR-0141)

The credential MODEL of a server and per-call selection are core (below). The accounts
themselves — sign-in, storage, refresh, the console RPCs — are the premium **accounts** plugin
(`cambrian-premium/plugins/accounts`, AccountsAdmin plane, capability `accounts`). An OSS kernel
has no accounts: a `per_user` server reports `awaiting_user` and every call fails "not
connected"; `shared` servers work exactly as before.

A server declares its credential model in `user_credential` (copied from the catalog entry at
install; override it on the server):

```json
"user_credential": {"scope": "per_user", "provider": "microsoft",
  "scopes": ["openid", "email", "offline_access", "User.Read", "Mail.Read"],
  "inject": "env", "name": "MS365_MCP_OAUTH_TOKEN"}
```

- `scope`: `shared` (default, the old model: one credential for the server), `per_user` (every
  call uses the requesting person's own credential, never another's and never the shared one) or
  `either` (the person's own when connected; the shared one only with `shared_fallback: true`,
  and never when the person's grant is dead).
- `inject`: `env` (stdio: the token goes into variable `name` of a process run for that person
  only, which gets the server's literal settings but NOT its shared `secret_env`), `bearer` or
  `header` (http/sse: a session per person whose every request carries that person's current
  token, as `Authorization: Bearer` or header `name`).
- Built-in per-user entries: `microsoft-365`, `slack`, `github`, `github-write`,
  `google-workspace-personal`, `google-workspace-personal-write`. `google-workspace` (stdio) stays
  shared: that mode has no injected-token variable.

### Google Workspace per person (ADR-0141 owner position 5)

Investigation and comparison: `cambrian-knowledge/investigations/2026-11-06-google-workspace-per-user-mcp.md`.
Not verified live (`T-20261106-per-user-google-workspace-curated-server-cannot`).

- **Server.** `workspace-mcp==2.3.0` (MIT, community) in **external-OAuth mode**: with
  `MCP_ENABLE_OAUTH21=true EXTERNAL_OAUTH21_PROVIDER=true`, it takes a Google access token in
  `Authorization: Bearer` on every request. It checks the token against Google userinfo (so the
  grant includes `openid email`) and calls Google with it. It never stores or refreshes the token;
  the premium accounts plugin refreshes it. With `WORKSPACE_MCP_STATELESS_MODE=true` it makes no
  disk writes. That mode is HTTP only, and the kernel does not launch HTTP servers, so **the
  operator runs it as a sidecar**. ADR-0139 D8 containment does not apply; use a container where
  isolation matters.
- **Two entries, two sidecars**, because a grant is per server and person:
  - `google-workspace-personal` (port 8000) is read-only. Scopes: `gmail.readonly`,
    `calendar.readonly`, `drive.readonly`.
  - `google-workspace-personal-write` (port 8001) has Gmail and Calendar only. Scopes add
    `gmail.compose`, `gmail.send` and `calendar.events`. It is fail-closed, and send, draft and
    event changes are dangerous (they need approval).
  - A person who needs both signs in twice.
- **Run (each sidecar).** `WORKSPACE_MCP_HOST=127.0.0.1` is **required**, because OAuth 2.1 mode
  binds `0.0.0.0` by default and the server accepts any valid Google token (it does no audience
  check).
  ```
  MCP_ENABLE_OAUTH21=true EXTERNAL_OAUTH21_PROVIDER=true WORKSPACE_MCP_STATELESS_MODE=true   WORKSPACE_MCP_HOST=127.0.0.1 PORT=8000 GOOGLE_OAUTH_CLIENT_ID=<oauth:google:client_id>     uvx workspace-mcp==2.3.0 --transport streamable-http --tools gmail drive calendar --tool-tier extended --read-only
  # write: PORT=8001, --tools gmail calendar --tool-tier extended --disabled-tools manage_gmail_label
  #   manage_gmail_filter list_gmail_filters modify_gmail_message_labels create_calendar
  #   manage_out_of_office manage_focus_time
  ```
- **Google side.** Create an OAuth client (Web application) with redirect URI
  `<CAMBRIAN_ACCOUNTS_PUBLIC_BASE_URL>/oauth/callback`, and enable the Gmail, Calendar and Drive
  APIs. `gmail.readonly` and `drive.readonly` are *restricted* scopes: an External-audience app
  needs Google verification, and an Internal (Workspace-only) app does not.
- **Google's own remote servers** (`gmailmcp`, `calendarmcp` and `drivemcp.googleapis.com/mcp/v1`)
  also take a bearer token. They are the long-term target, but they are Developer Preview behind
  enrollment, Gmail has no send tool, and they are three servers (three sign-ins per person).

**Who the call is for.** The kernel resolves the person from the agent's lease (conversation owner,
then task beneficiary, then `execution.memory_notes.default_principal`), never from the model or
a request field. No person, no connection, or a revoked grant is a clean tool error telling the
person to connect the integration in the console.

**Turning it on (premium).** Premium runtime environment (not the OSS config schema, ADR-0057 D5):

```
CAMBRIAN_ACCOUNTS_ENABLED=true
CAMBRIAN_ACCOUNTS_PORT=8767                                  # OAuth listener (one listener policy)
CAMBRIAN_ACCOUNTS_PUBLIC_BASE_URL=http://127.0.0.1:8767      # origin the browser reaches it at
CAMBRIAN_ACCOUNTS_REDIRECT_ALLOWLIST=http://127.0.0.1:8767/oauth/callback
CAMBRIAN_ACCOUNTS_PROVIDERS_FILE=                            # optional JSON {id: provider} overrides
CAMBRIAN_ACCOUNTS_SYSTEMS_FILE=                              # optional JSON {id: system} overrides
```

1. Register an OAuth client with each provider (Entra app, Slack app with user scopes, GitHub App,
   Google Cloud OAuth client). Redirect URI: `<public base url>/oauth/callback`. A server
   deployment uses its https origin; plain http is accepted only on loopback.
2. Store the client with AccountsAdmin `SetProviderClient{provider, client_id, client_secret}`
   (Operator; ADR-0141 phase 2). It writes the ADR-0101 secrets `oauth:<provider>:client_id` and
   `oauth:<provider>:client_secret` (provider ids: `google`, `microsoft`, `slack`, `github`); the
   secret is write-only, and `ListProviderClients` reports configured / last four / updated-at.
3. Each person connects their own account (AccountsAdmin `StartAccountConnect`; console page:
   ADR-0141 phase 4). The kernel opens a sign-in in the system browser (authorization code +
   PKCE, state, nonce, browser-bound), stores the account sealed in the config store
   (`sealed_records`, namespace `accounts`; ADR-0140 records are migrated there on open),
   refreshes it automatically, and marks it `reconnect_needed` when the provider refuses a refresh.
4. **Phishing defence** (ADR-0141 owner position 4). When the person has a known external
   identity (an identity binding or party identity in the access-policy plane: an email, or
   `<provider>:<subject>`), the signed-in account must match it, else the sign-in is refused, the
   tokens are revoked upstream and nothing is stored. With none known, the account is stored
   `pending_confirmation` — unusable, calls fail "waiting for your confirmation" — until the
   person confirms the shown account identity (AccountsAdmin `ConfirmMyAccount`); reject it with
   `DisconnectMyAccount`.

Per-person sessions (core) start on first use, close after `mcp.user_sessions.idle_seconds`
(600) and are capped at `mcp.user_sessions.max` (32). Operators can list and revoke any person's
account; every connect, disconnect, revoke and dead grant is audited (no secret in any entry).
Not built: device code sign-in, MCP 2025-11-25 authorization discovery / dynamic client
registration.

## Shared credentials as org accounts (premium, ADR-0141 phases 2 and 5)

On a premium kernel the accounts plugin owns the organisation's SHARED credentials too (org
accounts are always on; `CAMBRIAN_ACCOUNTS_ENABLED` gates only personal sign-in):

- **Boot migration (owner position 1).** Before any server connects, every stored
  `mcp:<id>:token` and `mcp:<id>:env:<NAME>` becomes an org account
  `static_token:org:mcp-<id>-<slot>` bound to (server, slot). Lossless (same sealed value),
  idempotent (a slot already bound is left alone, so an operator's later change always wins),
  audited as `migrate_org_account` naming the legacy secret and the account, never the value. A
  migration error stops the provider from installing, so the kernel keeps reading the legacy
  names (degraded, not broken).
- **Reads.** A shared credential resolves env first (unchanged), then the bound org account. The
  consumer-named store entries are no longer read (phase 5). A revoked account fails the server's
  credential closed; nothing falls back.
- **Writes.** `SetMCPServerToken`, `SetIntegrationSecret` and `InstallIntegration`'s `secrets`
  rotate the bound org account (creating and binding one when none is). `ClearMCPServerToken` /
  `ClearIntegrationSecret` unbind the slot and delete the legacy copy. The Integrations console
  reports such a secret with source `account`. Org accounts are managed directly with
  AccountsAdmin `ListOrgAccounts` / `CreateOrgAccount` / `RotateOrgAccount` /
  `RevokeOrgAccount` / `BindOrgAccount` / `UnbindOrgAccount`; one account may serve several
  consumers (an MCP server and an ingress poller share one Slack bot token).
- **Rollback.** The legacy `mcp:<id>:*` secrets are kept for one release, so a binary rollback
  reads every credential as it was at the migration. Rotations made after the upgrade live only
  in the org account: re-enter them after a rollback. Deleting the legacy names is
  `T-20261106-accounts-delete-legacy-consumer-secrets-after`.
- **OSS** (no premium): unchanged — `mcp:<id>:token` / `mcp:<id>:env:<NAME>` in the ADR-0101
  store are the shared credential.

## Org OAuth install (premium, ADR-0141 D1)

An org account can also be INSTALLED by an operator through OAuth instead of pasted — the
provider's organisation install, declared as data in the provider table (`org_install` in
`cambrian-premium/accounts/providers.json`, overridable with `CAMBRIAN_ACCOUNTS_PROVIDERS_FILE`).
AccountsAdmin `StartOrgAccountInstall{provider, consumers[], account_id?}` (Operator) returns the
same single-use kernel start URL as a personal sign-in (state, browser-binding cookie, PKCE where
the install returns a code); the listener therefore needs `CAMBRIAN_ACCOUNTS_ENABLED=true`. The
tokens become an org account `<provider>:org:<team|tenant>` owned by the organisation, refreshed
automatically, and the given consumers are bound to it.

| Provider | Org install | Account |
| --- | --- | --- |
| `slack` | OAuth v2 app install (authorization code; BOT token, `scope` = bot scopes; refresh when the app rotates tokens) | `slack:org:<team id>` |
| `microsoft` | tenant admin consent (`/organizations/v2.0/adminconsent`), then app-only tokens by client credentials (`https://graph.microsoft.com/.default`), re-acquired before expiry; granted scopes = the token's `roles` | `microsoft:org:<tenant id>` |
| `google` | none: org-wide Google access is a service account with domain-wide delegation, i.e. a pasted JSON key (`CreateOrgAccount`, provider `google_service_account`) | — |
| `github` | none: a GitHub App installation token is minted from the App's private key, not an OAuth code flow; paste a token | — |

**Least privilege.** The install asks for exactly the union of the scopes its consumers declare:
an MCP server's `user_credential.scopes`, plus a grant's own `scopes` (an ingress binding's
`scopes`). Widening an installed account (`account_id`) asks for everything it already has plus
what every bound consumer declares (incremental consent), and must authorise the SAME team /
tenant or it is refused.

## Grant scope enforcement (premium, ADR-0141)

A consumer may use an account only when the account's GRANTED scopes cover the scopes the
consumer declares — personal and org accounts alike:

- MCP: the server's `user_credential.scopes`; ingress: the binding's `scopes` (Ingress Studio
  transport spec, outbound bindings only).
- Otherwise resolution fails closed with `ErrCredentialInsufficientScope` ("needs more
  permission: missing …"); an MCP call returns "integration X needs more permission than your
  connected account was granted … Reconnect it". Never a fallback, also not for `either` +
  `shared_fallback`.
- Scope spelling is provider data (`scope_rules`: `implicit` scopes such as `openid`, `aliases`
  such as Google's `email`, `strip_prefixes` such as `https://graph.microsoft.com/`, `implies`
  such as `gmail.modify` ⇒ `gmail.readonly`).
- A pasted key's scopes cannot be known: with none recorded (`CreateOrgAccount.scopes`) its
  consumers are `unverified`, not refused; recorded scopes are enforced.
- AccountsAdmin reports the verdict: `Account.missing_scopes`, `PersonalServer.missing_scopes`,
  `AccountGrant.missing_scopes` / `scope_check`, and `ListAccountConsumers{account_id}`
  (Operator) lists every consumer of an account — what breaks if it is revoked.

## System catalog (premium, ADR-0141 D5)

`ListSystems` (any role; a non-Operator sees only their own accounts) returns the external
systems — Slack, Google Workspace, Microsoft 365, GitHub, Jira/Confluence, Linear, Notion,
filesystem — declared as data in `cambrian-premium/accounts/systems.json` (overridable with
`CAMBRIAN_ACCOUNTS_SYSTEMS_FILE`), each with its installed MCP servers (by this catalog's
`catalog_id`, else the credential provider), org accounts, consumers, an account summary,
health and whether an org install is available. Capability `accounts-systems`; a console
without it derives systems itself.

## Follow-up tickets

| Ticket | What |
| --- | --- |
| `T-20261105-curate-verify-and-pin-the-mcp-server-set` | This curation. Released, not closed: the live handshake is outstanding. |
| `T-20261105-mcp-auth-is-one-static-credential-per-server-no` | Per-principal MCP credentials. Done by `T-20261106-per-user-integration-credentials-oauth-sign-in` (ADR-0140). |
| `T-20261106-integrations-manage-mcp-servers-from-the` | Integrations from the console: catalog, per-id store state, enabled flag, literal env values, per-server secrets, live lifecycle, health on the feed, test. Done. |
| `T-20261105-mcp-servers-have-no-enabled-flag-every` | The `enabled` flag. Done (integrations). |
| `T-20261105-mcp-server-health-is-not-visible-on-the` | MCP server health on the operator plane. Done (integrations). |
| `T-20261106-windows-appcontainer-runner-for-native-network` | native network deny on Windows. |
| `T-20261106-egress-host-allowlist-declared-by-mcp-servers` | enforce declared egress hosts (proxy). |
| `T-20261106-live-verification-catalog-stdio-servers-under` | live check of catalog servers under containment and the docker runner. |
| `T-20261106-integrationop-has-no-structured-containment` | structured containment on the operator plane. |
| `T-20261105-live-install-and-handshake-verification-of-the` | Live install and handshake of the curated set, once the owner approves running third-party code. |
| `T-20261105-mcp-server-config-cannot-set-literal-env-values` | Literal per-server env values (`env_values`). Done (integrations); the catalog's code-sandbox entry defaults to the hardened settings. |

## Integrations live check (2026-11-06)

`cambrian-benchmarks/scripts/integrations_live_check.py`, through the operator plane only, on the
disposable smoke kernel (guarded launcher, cambrian-pg-test `cambrian_smoke`), with a kernel
built from the tree that has the health-ping fix. For each server: install, enable, wait for
`ready`, `TestIntegration` (with probe), disable, re-enable. Then a 5-minute soak with all 13
running at once. Run `20261106T034821Z`, Windows 11.

| Server | Ready (s) | Tools: test / health / ListMCPServers | Probe | Soak health events | Verdict |
| --- | --- | --- | --- | --- | --- |
| filesystem | 5.0 | 14 / 14 / 14 | — | 0 | PASS |
| fetch | 3.0 | 1 / 1 / 1 | — | 0 | PASS |
| web-search | 14.0 | 3 / 3 / 3 | — | 0 | PASS |
| git | 6.0 | 14 / 14 / 14 | — | 0 | PASS |
| sqlite | 22.1 | 2 / 2 / 2 | — | 0 | PASS |
| time | 2.0 | 3 / 3 / 3 | ok | 0 | PASS |
| pdf-reader | 3.0 | 4 / 4 / 4 | — | 0 | PASS |
| sequential-thinking | 9.0 | 1 / 1 / 1 | — | 0 | PASS |
| calculator | 8.0 | 1 / 1 / 1 | ok | 0 | PASS (first live handshake) |
| markitdown | 14.0 | 1 / 1 / 1 | — | 0 | PASS |
| terminal (Desktop Commander) | 5.0 | 25 / 25 / 25 | — | 0 | PASS |
| playwright | 7.0 | 25 / 25 / 25 | — | 0 | PASS |
| code-sandbox | 4.0 | 3 / 3 / 3 | — | 0 | PASS |

- **Health.** 9 of these servers answer a health ping with a JSON-RPC error: fetch, web-search,
  git, time, markitdown and code-sandbox say `Method not found`, sqlite `invalid method ping`,
  pdf-reader `ping`. They speak the stateless 2026-07-28 protocol, which has no `ping`. Each one
  stayed `ready` with 0 restarts; before the fix each was dropped every ~33 s.
- **Credentials.** With no credential supplied, enabling is refused with the missing field for
  linear, notion, brave-search, tavily, google-workspace (both OAuth client fields) and postgres
  (`POSTGRES_PASSWORD`).
  - **slack** is accepted: both token fields are optional
    (`T-20261106-slack-integration-can-be-enabled-with-no`).
  - **github** is accepted: it is now a per-user credential (ADR-0140) and its shared token is
    optional.
- **Revert to file.** Passed. A file server was disabled from the console (`overrides_file`),
  then reverted: it came back as source `file`, enabled, `ready` in 3 s. A second revert is
  rejected.

## Live handshake results (2026-11-05)

Each server below was installed at its pinned version into a scratch folder and started with only the OS-essential environment (no credentials). The probe sent MCP `initialize` and `tools/list`, then killed the server tree by PID. Windows 11 host, node 22.14, uv 0.11, Docker 27. The probe client is in the job's tmp folder.

| Server | Result | Tools exposed |
| --- | --- | --- |
| time 2026.11.4 | ✅ verified (incl. a live `get_current_time` call) | get_current_time, convert_time, get_time_ago |
| fetch 2026.11.4 | ✅ verified | fetch |
| git 2026.11.4 | ✅ verified | 14 git_* tools (status, diff, commit, add, reset, log, branch, checkout, merge, show, remote) |
| web-search (duckduckgo-mcp-server 0.7.0) | ✅ verified | search, fetch_content, expand_link |
| code-sandbox (llm-sandbox[mcp-docker] 0.3.46) | ✅ verified (server start; container execution not exercised) | execute_code, get_supported_languages, get_language_details |
| filesystem 1.0.0 | ✅ verified | 14 tools (read/write/edit/list/search/move/info) |
| sequential-thinking 1.0.0 | ✅ verified | sequentialthinking |
| pdf-reader 8.5.9 | ✅ verified | inspect, outline, read, search |
| sqlite (toolbox 1.15.0) | ✅ verified | execute_sql, list_tables |
| searxng 2.10.0 | ✅ verified (server start; needs a SearXNG instance for results) | searxng_web_search, searxng_search_suggestions, searxng_instance_info, web_url_read |
| playwright 0.0.84 | ✅ verified | 25 browser_* tools (navigate, click, fill_form, screenshot, snapshot, evaluate, run_code_unsafe, …) |
| markitdown 0.0.1a8 | ✅ verified with `--prerelease=allow` (a transitive Azure dependency is pre-release only) | convert_to_markdown |
| calculator (mcp-server-calculator 0.2.1) | ⏳ **not verified**: added after this probe (registry-checked only); the owner's MCP verification run must handshake it | calculate (per docs) |
| shell (mcp-shell-server 1.1.14) | ❌ **does not run on Windows**: it imports the Unix-only `pwd` module. Linux/macOS deployments only. | — |

Keyed integrations (GitHub, Google, Microsoft 365, Slack, Brave/Tavily/Exa, Postgres, Notion, Atlassian, Linear) were checked on their registries only. They need credentials to verify.

**Owner's local `configs/mcp.json`:**
- backed up to `mcp.json.bak-20261105`;
- super-shell removed;
- `filesystem` and `sequential-thinking` pinned to 1.0.0 (2.x needs node 24);
- `fetch` and `pdf-reader` pinned;
- pdf-reader reads now carry egress;
- added: `web-search`, `code-sandbox`, `time`, `markitdown`.

**Code sandbox hardening** (SANDBOX_NETWORK_MODE=none, SANDBOX_READ_ONLY=true, SANDBOX_CAP_DROP=ALL, no-new-privileges, memory/CPU caps) is now a per-server `env_values` setting; installing `code-sandbox` from the console applies those values by default. With the network off, the sandbox image must already contain pandas, numpy and matplotlib.

### Windows terminal: Desktop Commander (added 2026-11-05)

`terminal` → `@wonderwhy-er/desktop-commander@0.3.1` (MIT, about 10.2k stars, actively maintained).
- **Handshake:** verified on Windows, 25 tools. `mcp-shell-server` cannot run on Windows (it imports `pwd`). `@simonb97/server-win-cli` is deprecated on npm in its favour.
- **Not dangerous:**
  - reads: `get_config`, `read_multiple_files`, `list_directory`, `get_file_info`;
  - search: `start_search`, `get_more_search_results`, `list_searches`, `stop_search`;
  - processes: `read_process_output`, `list_sessions`, `list_processes`;
  - history: `get_usage_stats`, `get_recent_tool_calls`;
  - `read_file` gets [read, egress], because it reads URLs.
- **Dangerous:**
  - file writes: `write_file`, `write_pdf`, `create_directory`, `move_file`, `edit_block` (write);
  - terminal: `start_process`, `interact_with_process` (write+egress);
  - process control: `force_terminate`, `kill_process` (write);
  - **never grant** `set_config_value` (it can widen its own `allowedDirectories`/`blockedCommands`) or `give_feedback_to_desktop_commander` (opens a browser).
- **Caveats** (upstream README):
  - it is not a sandbox: `allowedDirectories` restricts only the file tools, never terminal commands, and `blockedCommands` can be bypassed;
  - telemetry is on by default: set `"telemetryEnabled": false` in `~/.claude-server-commander/config.json` (done on the owner's host);
  - `claude_tool_call.log` stores tool arguments unredacted;
  - for isolation, run its Docker image (ADR-0139 D8).
