# CONTEXT.md — cambrian-core (Go kernel and runtime, source-available)

The Go kernel and runtime of Cambrian (`github.com/cambrian-sh/core`), source-available under the Elastic
License 2.0 (ADR-0057 D6) — not OSI open source. Pre-1.0: the gRPC/proto surface and the config schema are
the stable contracts; the Go package API is not. It owns execution lanes, memory and retrieval, the
knowledge substrate, identity and scope enforcement, tools/MCP and both gRPC planes; premium, ui, sdk and
the benchmark harness build on it and it imports none of them. Layering and domain terms: `docs/ARCHITECTURE.md`.

## Build and test

```bash
go build ./... && go vet ./... && go test ./...
go build -o bin/cambrian-orchestrator ./cmd/orchestrator   # there is no `make build` target
make test-race        # unit tests with -race
make per-pr           # proto-check proto-breaking vet-tagged test-race separability integration chaos
make proto            # regenerate api/proto bindings — never hand-edit generated code
make separability     # core never imports premium
deadcode ./cmd/...    # always all of ./cmd/..., never one binary
cambrian-orchestrator setup | status | stop   # self-installing binary (Postgres, Python runtime, agents, config, migrate, start)
```

## Where the documentation is

- **Areas of this repo** are documented by knowledge cards in HQ. For any file or folder run
  `brief <path>` (HQ MCP tool, or `C:/Users/afsin/Dev/hq/bin/hq.exe brief <path> --config hq.yaml` from the
  workspace root); a card also loads automatically when you open a matching file. Cards are the area documentation.
- **Changing what a card says**: `read_doc cards/<name>.md`, edit, `write_doc` with a reason. Do not add facts here.
- **Why it is this shape**: ADRs and decisions in HQ (`search_docs`, `read_doc adrs/…`; status `list_decisions`).
- **Open work and known gaps**: HQ tickets (`list_tickets repo=cambrian-core`).
- **Rules for agents**: [`AGENTS.md`](AGENTS.md) and the workspace [`../AGENTS.md`](../AGENTS.md).
