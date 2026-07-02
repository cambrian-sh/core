# System Agents

These are **privileged kernel organs** — the runtime dispatches to them *directly* by ID
(bypassing the auction/interview; see `domain/agent.go` `systemAgentIDs`). The kernel is
incomplete without them, so they ship with the OSS runtime.

| Agent | Role | ADR |
|---|---|---|
| `scout_agent` | pre-plan discovery | 0051 |
| `kg_extractor_agent` | deterministic triplet extraction (ingest path) | 0053 D2 |
| `reranker_agent` | bge cross-encoder relevance scoring (recall path) | 0054 |

Each system organ ships as a Python package (its own directory) so the
seeder (`internal/storage/bbolt_adapter.go::Seed`) detects it via the
`__init__.py` + `agent.py` convention and registers it as a single
agent. The `kg_extractor_agent` package bundles its `kg_extractors/`
subpackage inside (the production modules the agent imports), so there
is no sys.path glue at runtime.

```
agents/system/
├── scout_agent/             # ADR-0051
│   ├── __init__.py
│   └── agent.py             # the gRPC entry point
├── kg_extractor_agent/      # ADR-0053 D2
│   ├── __init__.py
│   ├── agent.py
│   └── kg_extractors/       # subpackage, scoped to the agent
│       ├── __init__.py
│       ├── common.py
│       ├── metadata_extractor.py
│       └── spacy_pattern_extractor.py
└── reranker_agent/          # ADR-0054
    ├── __init__.py
    └── agent.py
```

## Running

These agents are Python processes built on the SDK:

```sh
pip install cambrian-agent-sdk   # the SDK (published separately)
python agents/system/scout_agent/agent.py
python agents/system/reranker_agent/agent.py
python agents/system/kg_extractor_agent/agent.py
```

Point the runtime's `agents_dir` at this folder (or your own) so the kernel can
launch them. Example/demonstration agents live in the SDK repo, not here.
