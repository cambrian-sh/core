---
number: 0097
title: Native Tool-Calling for the Agent Loop
status: Accepted
date: 2026-07-28
relates-to: [0018, 0039, 0048]
---

# ADR-0097: Native Tool-Calling for the Agent Loop

## Status

`Accepted`, not `Implemented`: the controlled vocabulary has no partial token, and
`Implemented` would claim the agent loop is fixed when it is not.

**Phase B is built, measured, and DEFECTIVE — default OFF pending D8.** Repeat-task,
six variants: text path 5/6, native 3/6, native with the JSON action withdrawn 0/6.

That is not evidence against native tool-calling. It is evidence that only HALF the
protocol was implemented. See D8.

**Phase A implemented 2026-07-28.** D1–D4 are live in the Go layer, with the D2
mechanism amended during implementation (see D2). Phase B — the agent-plane contract
change — remains Proposed and needs its own go/no-go. Nothing in the agent loop has
changed yet: agents still cannot receive a structured tool call.

## Context

The SDK agent loop asks the model for JSON in text and infers what the model meant by
whether that text parses:

```python
raw = agent.substrate.generate(...)      # a string
action = parse_action(raw)               # {"action": ...} or a guess
```

Any output without a parseable `{"action": ...}` was treated as a final answer, so a
model narrating its next step ended the task reporting success with the work undone.
ADR-0096-era work inverted that default — a non-action response is now re-prompted once
before being accepted — but **the inversion is compensation, not a fix.** It is a
heuristic standing in for a signal we already had and threw away.

Every mature agent loop routes on a structured field instead:

| Framework | Continue | Terminate |
|---|---|---|
| Anthropic | `stop_reason == "tool_use"` | `stop_reason == "end_turn"` |
| OpenAI / Codex | `finish_reason == "tool_calls"` | assistant message, no tool call |
| LangGraph | `last_message.tool_calls` non-empty | route to `END` |
| Hermes | `tool_calls` present | `finish_reason: stop` |

Anthropic states the rule as a whitelist — *"when `stop_reason` is NOT `end_turn`, treat
the response as incomplete"* — and models a third state, `pause_turn`, for "stopped but
not finished". Our loop has no way to express that state, which is precisely why it
mistook it for success.

Hermes is the closest analogue to our design (same JSON-in-text protocol) and carries the
same defects as filed bugs: *"Model outputs tool calls as text instead of using native
function calling"* (the agent "treats it as a plain text response and never executes the
tool") and raw tool tags leaking to users — the latter identical to our
`<workspace><fact precision="1.00">…` leak. This is a property of the parse-based
approach, not of our implementation of it.

**The signal is available to us.** The kernel already ships adapters for Anthropic,
OpenAI, Gemini and Ollama, all four of which expose native tool-calling, and the
configured generators are OpenAI-compatible. `internal/infrastructure/llm/openai_client.go`
contains zero references to `tools` or `tool_calls`. We are discarding it at the adapter.

## Decision

### D1 — An OPTIONAL generator interface, not a change to `domain.Generator`

`domain.Generator` is `Generate(ctx, prompt) (string, error)`, referenced in 22 files with
9 implementers in core plus the premium trace wrapper. Widening it would touch every one
of them, including several — the failover wrapper, the circuit breaker, the price ledger —
that have nothing to do with tools.

Instead add a second, optional interface that adapters MAY implement:

```go
type ToolCallingGenerator interface {
    GenerateWithTools(ctx context.Context, prompt string, tools []ToolDefinition) (ToolCallResponse, error)
}

type ToolCallResponse struct {
    Text       string
    ToolCalls  []ToolCall   // name + raw JSON args + provider call id
    StopReason string       // normalized: "tool_use" | "end_turn" | "max_tokens" | ...
}
```

Callers type-assert. A generator that does not implement it keeps today's behaviour with
no edit. Decorators (failover, circuit breaker, tracing) must forward the assertion or
they silently mask the capability of the generator they wrap — that is the one place this
design can go wrong quietly, and it needs an explicit test per decorator.

### D2 — Capability is DECLARED in config, not probed

`providers.json` gains a per-generator `"native_tools": true`. Probing at runtime means
the first request of a session decides behaviour, which makes an A/B unattributable and
a failure intermittent. A declared capability that turns out to be wrong fails loudly on
first use, which is the better failure.

**D2.1 (amended during implementation, 2026-07-28) — report the capability, do not
hide the type.**

The first implementation expressed "not tool-capable" structurally: when `native_tools`
was unset the factory returned the client wrapped in a value embedding only
`domain.Generator`, so the `ToolCallingGenerator` assertion failed by construction. That
was appealing — the assertion could not lie — and it was **wrong**, because embedding an
interface narrows the method set to *exactly* that interface. The wrapper also hid
`GenerateStream`. Every OpenAI generator without `native_tools` would have silently lost
streaming.

An existing test (`TestGeneratorRegistry_PropagatesDisableThinking`, which asserts the
factory returns a concrete `*OpenAIClient`) caught it. Hiding every optional capability
in order to hide one is the same silent-drop shape this ADR exists to remove.

Replaced by a narrower mechanism: `domain.ToolCallingReporter` (`NativeToolsEnabled()
bool`). A generator implementing `ToolCallingGenerator` is capable UNLESS it reports
otherwise, `SupportsToolCalling` consults the report, and nothing else about the value
changes. `GenerateWithTools` additionally refuses when undeclared, so a caller that
skipped the check fails loudly rather than silently getting a completion with the tools
dropped.

The cost is that decorators must now forward BOTH the call and the report — forwarding
one without the other over-reports a disabled inner. Both directions are tested per
decorator.

### D3 — Normalize the stop reason at the adapter

Each adapter maps its provider's vocabulary onto one internal set, so the loop never
learns provider names. The set follows Anthropic's, since it is the most complete:
`tool_use`, `end_turn`, `max_tokens`, `stop_sequence`, `refusal`, and an `unknown` that is
treated as NOT-finished.

`unknown` defaulting to not-finished is the whole point of the exercise: an unrecognised
signal must never be read as "done".

### D4 — Check BOTH signals

A response is finished only when it declares finish **and** carries no tool call.

This is not belt-and-braces pedantry. It is a live failure in the ecosystem: opencode
issue #14972 reports Gemini and LiteLLM returning `finish_reason: "stop"` on responses
that *do* contain tool calls, so a loop trusting the declared reason alone stops after
one tool. Trust the action over the narration — the same principle as the rest of this
ADR, applied to the provider rather than the model.

### D5 — The parse path stays, as the declared fallback

Both paths converge on the same internal action dict before the loop sees them, so the
loop keeps one shape and one set of guards (recurrence veto, tool-round budget, the
inverted final-answer default). Local and self-hosted models without tool support are not
second-class; they take the documented fallback.

Deleting the parse path is explicitly NOT part of this ADR. It remains the only route for
models that cannot do native calls, and it is what the SDK's existing action menu,
`use_skill`, `yield_subgoal` and memory-query actions are expressed in.

### D6 — Phasing, because Phase B is a contract change

**Phase A — kernel-internal. IMPLEMENTED.** D1–D4 in the Go layer: the optional interface,
`openai_client` implementing it, decorator forwarding, adapter stop-reason normalization,
config capability. No proto change, no contract bump, no re-vendoring. Kernel-side callers
(planner, router) can adopt structured outputs immediately, which is worth having on its
own: the router defect fixed on 2026-07-28 — a code-fenced classification killing the
request before a plan existed — is exactly the class this removes.

**Phase B — the agent plane. IMPLEMENTED 2026-07-28** (see D7 for what the wire
actually demands). `GenerateStreamRequest` carries only
`{session_token_id, prompt, GenerateOptions, lease_id}`; `GenerateChunk` carries only
text. The proto has zero references to tool calls. Agents therefore CANNOT receive a
structured tool call today, which means **the SDK loop — where the original bug lives —
only benefits at Phase B.** That requires: tool definitions on the request, structured
calls plus stop reason on the response, `make proto-breaking` / `make proto` /
`make proto-check`, a `contract_version` bump, and re-vendoring the UI and CLI copies.

Phase A does not fix the agent loop. Stating that plainly so Phase A is not mistaken for
completion of this ADR.

### D7 — What the wire actually demanded (learned by shipping it)

The design above was correct and insufficient. Two provider constraints only surfaced
against the live endpoint, each costing a full benchmark run that failed 0/6 with an
opaque `HTTP 400: {"message":"Error from provider (Console Go): Upstream request
failed"}` — a body that names no field.

**D7.1 — Tool names must be sanitized, with a reverse map.** Providers constrain a
function name to `^[A-Za-z0-9_-]{1,64}$`. Cambrian's kernel-owned tools are named
`mcp:filesystem/write_file`. Isolated probe, everything else held constant:

| name offered | result |
|---|---|
| `write_file` | 200, `tool_calls` |
| `mcp:filesystem/write_file` | **400** |
| `mcp_filesystem_write_file` | 200, `tool_calls` |

The SDK therefore sanitizes on the way out and maps back on the way in. The reverse map
is not optional: the provider echoes the name IT was given, and the kernel's tool
registry only knows the original, so an unmapped call dispatches to a tool that does not
exist. Collisions are disambiguated deterministically — two runs of one agent must offer
identical names or provider-side caching and our own logs stop lining up.

**D7.2 — Argument schemas need a top-level `"type": "object"`.** The kernel's tool
registry stores MCP schemas in a degenerate form, `{"properties": {"content": {}, "path":
{}}}`. That is adequate for the prompt-encoded menu, which renders only property NAMES,
and is rejected outright by a provider, which validates the schema:

| parameters | result |
|---|---|
| `{"properties": {...}}` | **400** |
| `{"type": "object", "properties": {...}}` | 200, `tool_calls` |
| `{"type": "object", "properties": {"path": {"type": "string"}}}` | 200, `tool_calls` |

Normalization adds the missing `type` and nothing else. It deliberately does NOT invent
property types: an empty `{}` means "any", which is legal and accepted once the
top-level type is present, whereas guessing `"string"` would be a lie the provider then
enforces.

Both failures shared a shape worth naming: **the prompt-encoded path was tolerant of
malformed tool metadata because it only ever rendered it as prose.** Moving to a channel
that VALIDATES that metadata surfaced defects which had been latent for as long as the
registry has existed.

**D7.3 — The two encodings must not both be advertised.** The action menu tells the
model to emit `{"action": "tool_call", ...}`. Attaching real tool schemas while leaving
that instruction in place gives the model two ways to do one thing, and it reliably takes
the one that does nothing. The menu is therefore split by encoding: under native
tool-calling the JSON tool_call action is replaced by a short section stating that tools
are attached and must be called, not described. The menu is composed once per run, so a
mid-run fallback recomposes it — a prompt promising attached tools that are no longer
offered is worse than the extra call.

### D8 — The missing half: tool-calling is CONVERSATIONAL, and we made it stateless

Native tool-calling is not a request shape, it is a multi-message protocol:

1. the caller sends `messages` + `tools`;
2. the model replies with an **assistant message carrying `tool_calls`**;
3. the caller appends that assistant message VERBATIM, plus one **`role: "tool"`
   message per call**, correlated by `tool_call_id`;
4. the model continues, now able to see that its call happened and what it returned.

Phase B implements step 1 and step 2 and then drops the thread. `openai_client.go`
builds `Messages: []openAIChatMsg{{Role: "user", Content: prompt}}` — **one user message,
every turn.** The proto carries a single `prompt` string, so there is no way to express
anything else.

The consequence is that every round is a brand-new conversation in which the model has
never called anything. Its previous call is absent; the result is absent. What it gets
instead is our working-memory trajectory rendered as PROSE inside the user message.

The tell was already in the code: `ModelToolCall.ID` is plumbed through the adapter, the
domain type, the proto and the SDK — and then dropped. `action_from_native_turn` reads
`name` and `arguments` and never touches `id`. This ADR's own D7 note says the id "MUST
be echoed back on the corresponding tool result". Nothing echoes it, because there is no
tool-result message to echo it on.

This predicts exactly what the 0/6 run did, and the run matches:

- heavy re-exploration — `fast_search_files` 16, `read_file` 12,
  `list_allowed_directories` 10, `get_file_info` 5 — because each turn re-decides from
  scratch;
- **zero `write_file` calls in the entire run**;
- final answers that SUMMARISE the trajectory rather than act on it ("From the
  trajectory, I can see that the file `src/two.md` was successfully read in steps 5 and
  6"), because a prose report of someone's actions reads as something to report on.

It also explains why native was WORSE than the text path rather than merely equal. On
the text path the trajectory is rendered in the same JSON-action format the model must
emit, so it reads as *my previous moves*. In native mode the model must emit structured
calls while its history arrives as prose — a format mismatch that invites narration.

**The fix is a message list, not a prompt string.** `GenerateWithToolsRequest` must carry
`repeated Message messages` (role, content, tool_calls, tool_call_id) rather than a
single `prompt`, the SDK must retain the assistant turn and append tool results against
their ids, and the adapter must pass the whole array through. That is a second proto
revision and a real change to how the ReAct loop keeps state — it currently keeps it in
working memory, which is the right place for the SDK's own purposes but is not the
provider's conversation.

Until then `native_tools` stays off. Not because the approach lost, but because half a
protocol is worse than none: it gives the model tools while hiding their results.

### D9 — A tool turn must IMMEDIATELY follow the assistant turn that requested it

D8 shipped and still returned `400 "Upstream request failed"`. The first reading —
"`deepseek-v4-flash` cannot do tool-calling conversations, only `mimo-v2.5` can" — was
WRONG, and wrong in an instructive way: the probe behind it fabricated a call id
(`call_1`). Feeding a model its OWN id changes the answer:

| shape (deepseek-v4-flash) | result |
|---|---|
| `user, assistant(call), tool` with a **fabricated** id `call_1` | **400** |
| `user, assistant(call), tool` with the model's **own** id `call_00_X8yY…` | **200** |

deepseek issues `call_00_…` ids and validates them; `mimo-v2.5` accepts anything, which
is why the two models appeared to differ in capability when they differ only in
strictness. **A capability probe must replay the provider's own output, not a
hand-written imitation of it** — otherwise it measures the probe's fidelity, not the
provider's support.

With real ids, the actual constraint isolates cleanly:

| shape | result |
|---|---|
| `user, assistant(call), tool` | 200 |
| `user, assistant(call), NOTE, tool` | **400** |
| `user, assistant(call), tool, NOTE` | 200 |

**A `role:"tool"` message must directly follow the assistant turn that requested it.**
Anything in the gap breaks the correlation.

And the thing in the gap was ours. `_ConversationMirror` (added in D8 so working-memory
writes reach the model) appends notes the moment they are written — and the loop writes
notes between REQUESTING a tool and EXECUTING it: recurrence vetoes, budget warnings,
discovery results. Every one landed squarely between the assistant turn and its tool
turn. D8 fixed the missing conversation and introduced a malformed one.

**Fix.** The mirror holds while a call is pending and flushes after the tool turn.
Separately, any call that is never executed — vetoed as a repeat, budget-tripped, tool
not granted — now gets an explicit tool turn recording that, so no assistant turn is left
with an unanswered call.

Both are mutation-checked: removing the hold, or the unexecuted-call answer, fails the
adjacency tests.

## Consequences

**Positive.** The loop stops guessing. `parse_action`'s coercion, the inferred-answer
re-prompt, `_looks_like_truncated_action`, and the tool-tag-leak class all become
fallback-only concerns rather than the primary path. Provider-side schema enforcement
removes a category of malformed-argument failures we currently absorb.

**Negative.** Two paths to maintain, and the fallback is the one that will rot because it
stops being exercised by the default configuration. Mitigation: keep at least one
benchmark arm pinned to a non-native generator.

**Negative.** Phase B is a contract change across three repos. The change-control rule
that a proto change without a `contract_version` bump is invisible and undebuggable
applies in full; the `SetRuntimeConfig` incident is the precedent.

**Risk.** A decorator that forwards `Generate` but not the tool-calling assertion silently
downgrades a capable generator to the parse path — the same silent-wiring shape as the
EFE task rebuild and the shadowed `max_tool_rounds` default, both of which cost a day
each. Per-decorator tests are not optional here.

## Open questions

1. ~~Does the configured endpoint honour `tools`?~~ **ANSWERED 2026-07-28: yes.** Probed
   live against `opencode.ai/zen/go/v1/chat/completions`. Both configured models return
   the standard OpenAI shape — `finish_reason: "tool_calls"` with
   `message.tool_calls[].function.{name, arguments}` (arguments a JSON string):

   | model | generators | with `tools` | without `tools` |
   |---|---|---|---|
   | `deepseek-v4-flash` | `deepseek` | `finish_reason: tool_calls` + call | `finish_reason: length`, no call |
   | `mimo-v2.5` | `mimo`, `mimo-flash` | `finish_reason: tool_calls` + call | — |

   So the signal is available for 100% of the current fleet and this ADR is worth
   building. Two incidental findings from the probe, both worth carrying:
   the endpoint 403s on the default `python-urllib` User-Agent (a probe must set one, or
   it will misreport "no tool support" — the control run is what caught this), and
   `tool_call.id` must be echoed back on the tool result, so Phase B's proto needs a field
   for it and cannot synthesize its own ids.
2. Should the SDK's non-tool actions (`use_skill`, `yield_subgoal`, memory queries) be
   expressed as native tools too, or stay in the parsed envelope? Expressing them natively
   is more uniform but hands the provider a menu that includes control-flow verbs.
3. Streaming: `GenerateViaModelStream` returns chunks. Tool calls arrive incrementally in
   most provider APIs; whether Phase B assembles them adapter-side or streams partial
   calls to the agent is unresolved.

## Related

- ADR-0048 — the agent action protocol this would supplement
- ADR-0039 — kernel-owned tool execution and authorization, unchanged by this
- ADR-0018 — the budget lease carried on the generate path
