"""Chat agent (CognitiveAgent) — the OSS conversational worker (ADR-0084 D4).

This is the default agent the kernel's chat worker pool runs. It owns exactly ONE
conversation turn: given the running transcript and the latest user message, it produces the
words to say back — in a single ReAct loop, never a decomposed plan.

That single-loop rule is the whole point. Feeding a conversational turn to the task planner
decomposes "reply to the user" into non-executable pseudo-steps ("Ask the user their name"),
which dead-loops in replan and leaks the failure as the spoken reply (ADR-0080). The planner
is still reachable — the ReAct loop can yield a subgoal for genuinely multi-step work — but
that is a choice the turn makes, never the default.

The agent is deliberately STATELESS per call: everything it needs (policy, transcript,
recall posture) arrives on the task. That is what lets the kernel run a bounded pool of
interchangeable workers instead of one process per conversation, so any worker can serve any
turn and a worker crash costs nothing but the in-flight call.

This agent is deliberately GENERIC. Everything domain-specific — persona, rules, the tool
menu it is allowed to lean on — arrives as data on the turn (``policy``) or as posture flags,
never as code here. A deployment or harness that needs specialised behaviour supplies a
policy, or ships its own agent and selects it with ``execution.chat_pool_agent_id``; it must
not push its specifics into this file.
"""

from __future__ import annotations

import logging
import os
import re
from pathlib import Path

from cambrian_agent_sdk import CognitiveAgent, AgentResult, AgentTask, tool
from cambrian_agent_sdk.prompt_library import PromptLibrary, sections
from cambrian_agent_sdk.working_memory import memory_provenance_attrs
from cambrian_agent_sdk._logging import configure_logging

_log = logging.getLogger("cambrian.chat_agent")


# ── Prompts (ADR-0139 C4/D2) ────────────────────────────────────────────────
# The texts live in the kernel prompt library (cambrian-core/prompts/chat_agent and
# chat_agent_turn), rendered with the SDK loader (same rules and static hash as Go).
# chat_agent is the STABLE system text (role, rules, the posture's answering rule,
# the policy); chat_agent_turn is the VOLATILE task text, where the untrusted
# transcript now lives as tagged data instead of inside <Role>.

def _prompts_root() -> Path:
    """``$CAMBRIAN_PROMPTS_DIR``, else the nearest ancestor ``prompts/`` holding this
    agent's prompts: the repo, the image (/app/prompts) and a ``setup`` install
    (<prefix>/prompts, unpacked from the kernel embed) all put prompts/ beside agents/."""
    env = os.environ.get("CAMBRIAN_PROMPTS_DIR", "").strip()
    if env:
        return Path(env)
    for parent in Path(__file__).resolve().parents:
        if (parent / "prompts" / "chat_agent" / "v1.md").is_file():
            return parent / "prompts"
    raise RuntimeError("chat_agent: cannot find the kernel prompt library; set CAMBRIAN_PROMPTS_DIR")


_LIBRARY = PromptLibrary.load(_prompts_root())
_SYSTEM = sections(_LIBRARY.get("chat_agent"))
_TURN = sections(_LIBRARY.get("chat_agent_turn"))
PROMPT_VERSIONS = {pid: _LIBRARY.get(pid).static_hash for pid in ("chat_agent", "chat_agent_turn")}

# Tags that hold untrusted text in the turn (and the policy in the system text).
_DATA_TAGS = ("conversation", "recalled_memory", "memory", "latest_message", "policy")


def _neutralise(value: str) -> str:
    """``</tag>`` -> ``[/tag]`` for every data tag, so a value cannot close its region
    (SPEC.md §3.1; Go: prompts.NeutraliseClosingTags)."""
    for t in _DATA_TAGS:
        value = re.sub(r"(?i)</\s*" + re.escape(t) + r"\s*>", f"[/{t}]", value)
    return value


AGENT_DESCRIPTION = (
    "Handles one conversational turn: replies to the user in natural language, using tools "
    "or escalating a multi-step task to the planner when needed. Stateless per turn — the "
    "conversation transcript is supplied by the kernel."
)

AGENT_MANIFEST = '''
{
  "version": "1.0.0",
  "trait": "cognitive",
  "capabilities": ["conversation", "chat", "general_purpose"],
  "supported_formats": ["text"],
  "tools": [],
  "release_notes": "ADR-0084 OSS chat worker: single-loop conversational turn owner.",
  "dependencies": []
}
'''

# Spoken-reply guardrail. If the loop produces an internal error, a reasoning dump, or
# workspace markup, it must NOT be spoken — that leak is the original ADR-0080 bug, where a
# "plan partially failed" string was read out to a customer.
_LEAK_PATTERNS = (
    re.compile(r"plan partially failed", re.I),
    re.compile(r"replan validation", re.I),
    re.compile(r"no JSON object found", re.I),
    re.compile(r"Traceback \(most recent call last\)", re.I),
    re.compile(r"ReActLoop", re.I),
    re.compile(r"<thought>|</thought>", re.I),
    re.compile(r"<workspace|<Context>|<System>", re.I),
    re.compile(r"<step\b", re.I),          # working-memory trajectory dump
    re.compile(r"budget exhausted", re.I),
    re.compile(r"\bcid:\w", re.I),         # leaked offload markup
)

# Non-committal on purpose. When a turn fails we must never fabricate an action the agent did
# not take ("I've cancelled that for you") — an honest stumble beats a confident false claim.
_SAFE_FALLBACK = "Sorry — I didn't quite catch that. Could you say it again?"

# When the model merely PROMISES an action ("I'll delegate this", "I'll now create the file")
# instead of emitting the delegate_to_planner tool action, NOTHING happens — the turn ends and
# no plan ever runs. This detects that narration so run() can force a real delegation. Anchored
# on a first-person future intent + an action verb; broad on the verb, narrow on the intent.
#
# KEPT after the ADR-0139 prompt migration, and English-only by nature: the new rules state
# plainly that narration does nothing, but no prompt makes a model reliable here, and a missed
# delegation is a silent no-op for the user. In another language it simply never fires, which
# is the behaviour without the guard (no retry), not a false positive.
_ACTION_PROMISE = re.compile(
    r"\b(i['’]?ll|i\s+will|i['’]?m\s+going\s+to|going\s+to|let\s+me|now\s+i['’]?ll)\b"
    r"[^.!?\n]{0,80}?"
    r"(delegat|creat|mak(e|ing)|writ|build|generat|set\s*up|hand(ing)?\s+(this|it|the)|forward|pass(ing)?\s+(this|it))",
    re.I,
)


def _spoken_only(text: str) -> str:
    """Return only words that are safe to say to a user, or a neutral fallback.

    Only the language-independent leak check remains. The old English label scrubbing
    (drop any line starting "Reasoning:"/"Analysis:", keep only what followed
    "Answer:"/"Reply:") is gone: the reply is now the final_answer action's answer
    field, which the turn contract defines as the deliverable alone, so the labels it
    targeted no longer come from the prompt — and it deleted real content ("Analysis:
    the numbers show ..." lost the whole line; text before a mid-reply "Answer:" was cut).
    """
    t = (text or "").strip()
    if not t:
        return _SAFE_FALLBACK
    if any(p.search(t) for p in _LEAK_PATTERNS):
        return _SAFE_FALLBACK
    return t


def _result_text(result) -> str:
    """Decode an AgentResult / str / bytes into text."""
    if result is None:
        return ""
    if isinstance(result, (bytes, bytearray)):
        return bytes(result).decode("utf-8", "replace")
    if isinstance(result, str):
        return result
    data = getattr(result, "data", None)
    if isinstance(data, (bytes, bytearray)):
        return bytes(data).decode("utf-8", "replace")
    if isinstance(data, str):
        return data
    return str(result)


class ChatAgent(CognitiveAgent):
    # The stable texts come from the prompt library (prompts/chat_agent). The class
    # attributes keep their old meaning for anything that introspects an agent.
    role = _SYSTEM["role"].render()
    output_schema = _SYSTEM["output"].render()
    constraints = tuple(_SYSTEM["rules"].render().splitlines())
    result_type = "text"
    # The reply is ONE final_answer action and cannot be chunked across turns, so the cap
    # must hold the longest reply the rules tell the model to write out in full (a 700-line
    # list is ~3.6k output tokens). 1024 was harmless while providers ignored max_tokens;
    # since ADR-0139 C1 the kernel honours it, and 1024 would cut such a reply mid-list
    # (E2E 20261105T000624Z, E14). Only generated tokens are billed, so the headroom is free.
    max_tokens = 8192
    temperature = 0.5
    # A chat is human-paced: the stable system block is worth the 1-hour cache window.
    cache_ttl = "long"

    # ADR-0139 D14: the front desk writes the principal's durable memory notes (the
    # place a stated preference belongs). D12 subagents and D13 skill proposals stay
    # OFF: the front desk delegates whole work to the planner instead, and a chat
    # turn is not where a reusable procedure is authored. A deployment that wants
    # either sets the attribute on a subclass (ticket
    # T-20261105-adr-0139-d12-in-agent-subagents-via-spawn records the decision).
    allow_memory_note_writes = True
    allow_subagents = False
    allow_skill_proposals = False

    # The managed-LLM budget lease for the turn in flight (ADR-0018). Stashed in run() so the
    # planner-delegation tool can charge a sub-plan to the same lease.
    _turn_token = ""
    # Set True the moment delegate_to_planner actually runs this turn, so run() can tell a real
    # delegation from a model that merely NARRATED one and force a retry.
    _delegated = False

    @tool
    def delegate_to_planner(self, task_description: str) -> str:
        """Delegate a genuinely MULTI-STEP task to the kernel's task planner and return its
        result. Use this ONLY when a request needs real work orchestrated across steps or
        agents (research, multi-tool operations, "build/find/do X"), NOT for a normal reply.

        This calls the kernel's Execute path, which
        runs the full planner. It is the correct way to reach the planner from a chat turn
        """
        self._delegated = True
        return self.substrate.execute(task_description, session_token_id=self._turn_token)

    def _meta(self, task: "AgentTask", key: str, default: str = "") -> str:
        return task.metadata.get(key) or task.context.get(key) or default

    def _recall(self, task: "AgentTask") -> bool:
        """Honour the conversation's recall posture (ADR-0084 D7).

        The kernel sends ``recall_lookup`` per turn, derived from the conversation's profile
        (``domain.ConversationProfile.Recall``, ``internal/chat/service.go`` buildHandoff):
        customer-facing conversations are tools-only, so an external user cannot pull on the
        deployment's shared memory. Reading it here — rather than hardcoding a class default —
        is what makes that posture a property of the CONVERSATION instead of a setting someone
        can flip once and forget.

        Fail-closed: recall needs an explicit ``recall_lookup=true``; an absent value, or a
        ``profile`` of ``customer`` (defence in depth, should the two ever disagree), is
        tools-only. The kernel does NOT enforce this posture server-side (no QueryMemory clamp
        exists for a chat lease), so this agent is the enforcement point: it governs both the
        seed recall and whether memory_query is on the menu at all.
        """
        if self._meta(task, "profile").strip().lower() == "customer":
            return False
        return self._meta(task, "recall_lookup", "false").strip().lower() == "true"

    def _seed_memories(self, task: "AgentTask", query: str) -> str:
        """The turn's initial recall, rendered as ``<memory>`` entries, or "" when the
        posture forbids it or nothing was found.

        Done here instead of by run_think's seed: run_think recalls on ``task.text``, which
        now carries the tagged transcript, and a recall query made of the whole conversation
        retrieves on the wrong thing. The latest message is the query.
        """
        memory = getattr(self, "memory", None)
        if memory is None or not query:
            return ""
        try:
            results = memory.recall(query, session_token_id=task.session_token_id or "")
        except TypeError:
            results = memory.recall(query)
        except Exception as e:  # noqa: BLE001 — recall is best-effort; a turn without it still answers
            _log.warning("chat_agent: seed recall failed: %r", e)
            return ""
        items = []
        for r in results or []:
            text = r.get("text", "") if isinstance(r, dict) else getattr(r, "text", "")
            text = str(text or "").strip()
            if text:
                items.append(f"<memory {memory_provenance_attrs(r)}>{_neutralise(text)}</memory>")
        return "\n".join(items)

    def compose(self, task: "AgentTask", *, recall: bool, force_delegate: bool = False):
        """The turn's prompt parts: ``(role, constraints, turn_text, max_memory_queries)``.

        System (stable for the conversation, cached): role, rules, the posture's answering
        rule, and the policy. User turn (volatile): the transcript, recalled memory and the
        latest message, each in its own tag, the reply instruction last.
        """
        from cambrian_agent_sdk.react import _DEFAULT_MAX_MEMORY_QUERIES

        policy = self._meta(task, "policy").strip()
        transcript = self._meta(task, "transcript").strip()
        user_msg = (task.text or "").strip()

        role = self.role
        if policy:
            role += "\n\n" + _SYSTEM["policy"].render({"policy": _neutralise(policy)})
        posture = "answer-memory" if recall else "answer-tools-only"
        constraints = list(self.constraints) + _SYSTEM[posture].render().splitlines()

        parts = []
        if transcript:
            parts.append(_TURN["conversation"].render({"transcript": _neutralise(transcript)}))
        memories = self._seed_memories(task, user_msg) if recall else ""
        if memories:
            parts.append(_TURN["memory"].render({"memories": memories}))
        parts.append(_TURN["message"].render({"message": _neutralise(user_msg)}))
        if force_delegate:
            parts.append(_TURN["force-delegate"].render())
        # Tools-only: the menu advertises NO memory query (max 0 => memory-none), so the
        # posture holds in what the model is offered, not only in the skipped seed recall.
        # With recall, the seed above spent one query of the loop's default budget.
        max_mem = max(0, _DEFAULT_MAX_MEMORY_QUERIES - 1) if recall else 0
        return role, constraints, "\n\n".join(parts), max_mem

    def run(self, task: "AgentTask"):
        from cambrian_agent_sdk.react import run_think, ReActLoopError

        # Make the turn's managed-LLM lease available to delegate_to_planner (charges any
        # sub-plan to the same lease rather than bypassing the budget).
        self._turn_token = task.session_token_id or ""
        recall = self._recall(task)

        def _think(force_delegate: bool):
            role, constraints, turn_text, max_mem = self.compose(
                task, recall=recall, force_delegate=force_delegate)
            # The transcript travels in the TASK (the user turn), as tagged data — never in
            # <Role>, where untrusted history sat inside the system prompt and changed it every
            # turn. task.text is no longer sent as a gRPC header here: seed_system_tools=False
            # skips the tool-menu query, and the skill-menu query header is sanitised by the
            # SDK (_header_safe).
            turn = AgentTask(
                text=turn_text,
                type="text",
                metadata=task.metadata,
                context=task.context,
                session_token_id=task.session_token_id,
                deadline_remaining_ms=task.deadline_remaining_ms,
            )
            # The chat front desk has NO system tools — it never executes a task itself. Its only
            # "tool" is delegate_to_planner (an in-process @tool), plus memory recall when the
            # posture allows it. seed_system_tools=False strips the kernel tool menu and
            # find_tools. yield_subgoal is disabled: a subgoal is a step INSIDE a plan bound by a
            # kernel coordinator a directly-dispatched chat worker doesn't have; the front desk
            # delegates WHOLE work instead.
            return run_think(
                self, turn,
                role=role,
                output_schema=self.output_schema,
                constraints=constraints,
                result_type=None,
                seed_recall=False,  # done in compose(), on the latest message
                seed_system_tools=False,
                allow_yield_subgoal=False,
                max_memory_queries=max_mem,
                # Rounds: delegate_to_planner, then summarise its result (+1 slack).
                max_tool_rounds=3,
                max_tokens=self.max_tokens,
                temperature=self.temperature,
                cache_ttl=self.cache_ttl,
            )

        self._delegated = False
        try:
            result = _think(False)
            raw = _result_text(result)
            # Guardrail: the model unreliably NARRATES a delegation ("I'll delegate this to the
            # planner") without emitting the delegate_to_planner tool action, so nothing runs. If
            # the reply promises an action but no delegation happened, force one retry that must
            # call the tool. The retry note goes in the user turn, so the system text is unchanged.
            if not self._delegated and _ACTION_PROMISE.search(raw or ""):
                _log.warning("chat_agent: reply promised delegation but no tool call fired; forcing retry")
                result = _think(True)
                raw = _result_text(result)
        except ReActLoopError as e:
            _log.warning("chat_agent: ReActLoopError -> fallback: %s", e)
            return AgentResult(data=_SAFE_FALLBACK.encode("utf-8"), type="text", confidence=0.2)
        except Exception as e:  # noqa: BLE001 — one bad turn must never kill the worker
            _log.warning("chat_agent: run_think raised -> fallback: %r", e)
            return AgentResult(data=_SAFE_FALLBACK.encode("utf-8"), type="text", confidence=0.2)

        spoken = _spoken_only(raw)
        if spoken == _SAFE_FALLBACK:
            _log.warning("chat_agent: fallback substituted. raw_len=%d raw_head=%r delegated=%s",
                         len(raw), raw[:200], self._delegated)
        return AgentResult(
            data=spoken.encode("utf-8"),
            type="text",
            confidence=getattr(result, "confidence", 0.7) or 0.7,
        )


agent = ChatAgent(
    agent_id="chat_agent",
    version="1.0.0",
    description=AGENT_DESCRIPTION,
)


if __name__ == "__main__":
    configure_logging(agent_id="chat_agent")
    _log.info("chat_agent prompt versions %s", PROMPT_VERSIONS)
    agent.serve()
