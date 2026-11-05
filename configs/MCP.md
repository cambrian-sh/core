# MCP servers Cambrian ships

Cambrian gets its tools only from MCP servers. This is an owner directive,
[`decisions/agent-stack-v2-mcp-tools-and-profiles.md`](../../cambrian-knowledge/decisions/agent-stack-v2-mcp-tools-and-profiles.md).
Every MCP tool still goes through `ToolExecutor`, which applies grants, effect classes,
approvals and provenance (ADR-0043, ADR-0086, ADR-0139 D8).

The server set lives in two files:

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
| Google Workspace | `google-workspace` | PyPI `workspace-mcp==2.3.0` (2026-10-30), MIT, Taylor Wilsdon (community) | uvx | `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `USER_GOOGLE_EMAIL`, plus a browser consent | R+E with `--read-only` | Y | not verified | Gmail, Drive and Calendar search/read (core tier) |
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
| Vendor remote servers: Google Workspace MCP, Slack `mcp.slack.com`, Microsoft Agent 365 / Work IQ, Notion `mcp.notion.com` | These are the preferred long-term targets, but all of them require **per-user OAuth 2.1**. Cambrian's connector sends one static credential per server. See "Per-user credentials" below. |

## Prerequisites

| Runtime | Needed by | Note |
| --- | --- | --- |
| Node.js **22+** with `npx` | filesystem, sequential-thinking, pdf-reader, sqlite/postgres (Toolbox), searxng, Brave, Tavily, Exa, Notion, Microsoft 365, Slack, Playwright | The dev host has 22.14. The official 2.x reference servers need node **24**: upgrade node before you move those pins. `npx -y pkg@x.y.z` downloads on first run, so the first boot is slow. |
| `uv` / `uvx` | fetch, web-search, shell, git, time, calculator, markitdown, code-sandbox, Google Workspace, Excel, Word | `uvx pkg==x.y.z` resolves the pinned version in an isolated, cached environment. |
| **Docker** (daemon running) | code-sandbox | llm-sandbox starts one container per execution; the first call pulls its language image. Podman or Kubernetes backends also work (`BACKEND`, plus the `llm-sandbox[mcp-podman]` or `llm-sandbox[mcp-k8s]` extra). |
| Deno | nothing shipped | Only the rejected `mcp-run-python` needed it. |
| A browser build | playwright | It is downloaded on first use. |

## Enabling an integration

1. Pick its entry in `mcp.integrations.json.example`, read its `_note`, and copy the object into
   the `mcp.servers` array of `configs/mcp.json`. Unknown keys such as `_note` are ignored by the
   loader.
2. Provide the credential.
   - **stdio servers**: set each variable named in the entry's `env` list in the **kernel's**
     environment. `env` is a passthrough list of names, never values. A credential-shaped name
     reaches the child only because it is listed (`procenv.AllowlistedNamedSecrets`), and
     `CAMBRIAN_*`/`LANGFUSE_*` never pass.
   - **http servers**: set `auth.token_env`, or save the token from the operator console. The
     console stores it encrypted under `mcp:<id>:token` (ADR-0101), and a set environment
     variable wins over the stored token.
3. Restart the kernel, or save the server from the console (contract 0097). The console's
   **Probe** connects once and lists the advertised tool names.
4. Compare the probed names with the entry's `tools` list:
   - For a server that can write, list its read tools as non-dangerous. Leave writes dangerous.
   - Never add a `default_tool_policy` to a server that has write tools. That would make
     every tool a future version adds safe by default.
5. Grant the tools to a profile or skill by their full name, `mcp:<id>/<tool>`.

**Do not paste the whole integrations file into `mcp.json`.** The kernel dials every configured
server at boot, concurrently (`Connector.ConnectAll`). A server with no credentials only fails,
gets skipped and is retried, but it still costs a process spawn and a reconnect loop. There is
no `enabled` flag yet (see the tickets below).

## Trust and sandbox notes

- **The policy is ours, not the server's.** Tool annotations a server advertises
  (`readOnlyHint`, `destructiveHint`) are descriptive only (A1.5). The `tools` and
  `default_tool_policy` entries in config are what the kernel enforces.
- **stdio servers run as the kernel user.** Today only the environment is restricted
  (ADR-0139 D8 Phase 0). A stdio server can read any file the kernel can read and open any
  network connection. Process sandboxing by trust tier (ADR-0139 D8) is not built yet. Until it
  is, treat every stdio server as code with the kernel's privileges, and prefer official servers.
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

## Per-user credentials (known gap)

MCP auth today is **one static credential per server** (ADR-0043 D9:
`auth.type` none, bearer or header, with `auth.token_env` or the console store). Every principal
that calls `mcp:slack/...` acts as the single Slack token. The same holds for the Google and
Microsoft servers, whose cached OAuth token belongs to whoever completed the consent.

Two consequences follow:

- **Data scope:** a principal sees whatever that one account sees. Cambrian's classification
  tags restrict which tools a principal may call, not what the account behind them may read.
- **Attribution:** actions are attributed to the shared account, not the human.

Per-principal OAuth "on behalf of" support is not built. That means MCP spec 2025-11-25
authorization, protected-resource metadata, a token per principal and server, and refresh. It
is also why the vendor remote servers for Google Workspace, Slack, Microsoft 365 and Notion are
listed but not shipped.

## Follow-up tickets

| Ticket | What |
| --- | --- |
| `T-20261105-curate-verify-and-pin-the-mcp-server-set` | This curation. Released, not closed: the live handshake is outstanding. |
| `T-20261105-mcp-auth-is-one-static-credential-per-server-no` | Per-principal MCP credentials: OAuth 2.1, MCP 2025-11-25 authorization. |
| `T-20261105-mcp-servers-have-no-enabled-flag-every` | An `enabled` flag, so integrations can sit in `mcp.json` without being dialled at boot. |
| `T-20261105-mcp-server-health-is-not-visible-on-the` | MCP server health (connected, retrying, last error) on the operator plane. |
| `T-20261105-stdio-mcp-servers-run-with-the-kernel-user-s` | stdio sandboxing by trust tier (ADR-0139 D8). |
| `T-20261105-live-install-and-handshake-verification-of-the` | Live install and handshake of the curated set, once the owner approves running third-party code. |
| `T-20261105-mcp-server-config-cannot-set-literal-env-values` | Literal per-server env values. `env` is passthrough only, so the sandbox hardening and `ALLOW_COMMANDS` must be set kernel-wide. |

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

**Code sandbox hardening** (SANDBOX_NETWORK_MODE=none, SANDBOX_READ_ONLY=true, SANDBOX_CAP_DROP=ALL, no-new-privileges, memory/CPU caps) has to be set in the KERNEL environment today, because per-server `env` only passes names through (T-20261105-mcp-server-config-cannot-set-literal-env-values). With the network off, the sandbox image must already contain pandas, numpy and matplotlib.

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
