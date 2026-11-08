# prompts

The kernel's prompt library: every model-facing prompt a kernel organ or system
agent sends, one file per version, embedded into the binary (ADR-0139 C4 / D2).
The format, rendering rules, hash and lint are specified in [SPEC.md](SPEC.md);
the SDK's prompts follow the same spec in `sdk/cambrian_agent_sdk/prompts/`.

```go
p, err := prompts.Get("clarifier")          // latest version; GetVersion(id, n) for one
r, err := p.Render(map[string]string{       // missing or extra slots are an error
    "classification_tags": "finance, legal",
    "goal":                goal,
})
// r.Text is what to send; r.StaticHash identifies the static template sent;
// r.ResponseSchema is the reply's JSON Schema, when the prompt declares one.
```

## Adding or changing a prompt

1. Create `<id>/v<N+1>.md` (never edit a version that has been benchmarked).
2. Keep untrusted text inside a named tag, with the task last (SPEC.md §3.1).
3. Run `go test ./prompts/...`. The lint fails on structural errors, ADR numbers
   and shouting, and the fixture test fails until you regenerate it:
   `go test ./prompts -run TestHashCasesFixture -update-hash-cases`.
   Then run the SDK parity test (`cd ../sdk && python -m pytest -q tests/test_prompt_hash_parity.py`).
4. Update the golden test of the code that renders it.

An organ call is usually a pair (`prompts.LoadPair` / `MustPair`): `<id>`, the
slot-free stable system block the provider caches, and `<id>_input`, the
volatile user message whose slots carry the call's data in tagged regions, task
last. `Pair.StaticHash()` is the identity of both files' static text.

| Prompt | Rendered by |
| --- | --- |
| `clarifier` + `clarifier_input` | `internal/awareness/clarify.go` |
| `planner` / `planner_cap` + `planner_input` | `internal/awareness/planner.go` |
| `replan` / `replan_cap` + `replan_input` | `internal/awareness/replan_handler.go` |
| `router` + `router_input` | `internal/router/default_router.go` |
| `fanout_source` (fragment appended to a fan-out source step's task) | `internal/metabolism/executer/fanout_source.go` |
| `chat_agent` + `chat_agent_turn` (fragment sets) | `agents/chat_agent.py` (Python, via the SDK loader) |
| `retrieval_<op>` + `retrieval_<op>_input`, `retrieval_synthesis_guidance` | `agents/system/retrieval_agent/planner.py` (Python, via the SDK loader) |

The Python system agents load this directory from disk with
`cambrian_agent_sdk.prompt_library.PromptLibrary.load(path)` (same rules and
static hash as the Go loader): `$CAMBRIAN_PROMPTS_DIR`, else the nearest ancestor
`prompts/` of the agent file. The image copies it to `/app/prompts` and `setup`
unpacks the embed to `<prefix>/prompts`, beside the agents.
