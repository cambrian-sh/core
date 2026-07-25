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
import re

from cambrian_agent_sdk import CognitiveAgent, AgentResult, AgentTask, tool
from cambrian_agent_sdk.types import ProposalResponse
from cambrian_agent_sdk._logging import configure_logging

_log = logging.getLogger("cambrian.chat_agent")


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

# Leading section labels a reasoning-style prompt may emit; only the spoken part is kept.
_SECTION_LABEL = re.compile(r"^\s*(Observations?|Reasoning|Thought|Analysis)\s*:\s*.*?$",
                            re.I | re.M)

# Non-committal on purpose. When a turn fails we must never fabricate an action the agent did
# not take ("I've cancelled that for you") — an honest stumble beats a confident false claim.
_SAFE_FALLBACK = "Sorry — I didn't quite catch that. Could you say it again?"

# When the model merely PROMISES an action ("I'll delegate this", "I'll now create the file")
# instead of emitting the delegate_to_planner tool action, NOTHING happens — the turn ends and
# no plan ever runs. This detects that narration so run() can force a real delegation. Anchored
# on a first-person future intent + an action verb; broad on the verb, narrow on the intent.
_ACTION_PROMISE = re.compile(
    r"\b(i['’]?ll|i\s+will|i['’]?m\s+going\s+to|going\s+to|let\s+me|now\s+i['’]?ll)\b"
    r"[^.!?\n]{0,80}?"
    r"(delegat|creat|mak(e|ing)|writ|build|generat|set\s*up|hand(ing)?\s+(this|it|the)|forward|pass(ing)?\s+(this|it))",
    re.I,
)

# Retry instruction when the model narrated a delegation it never performed.
_FORCE_DELEGATE = (
    "Last turn you replied in prose that you would delegate or do something, but you did NOT call "
    "the delegate_to_planner tool — so NOTHING happened and no plan ran. Emit the delegate_to_planner "
    "tool action NOW, with a concrete, self-contained task_description built from the conversation "
    "(what to produce, where, and the key facts). Do not reply in prose this turn — call the tool."
)


def _spoken_only(text: str) -> str:
    """Return only words that are safe to say to a user, or a neutral fallback."""
    t = (text or "").strip()
    if not t:
        return _SAFE_FALLBACK
    if any(p.search(t) for p in _LEAK_PATTERNS):
        return _SAFE_FALLBACK
    # If the model labelled its spoken part, keep only that.
    m = re.search(r"(?:Conclusion|Answer|Reply)\s*:\s*(.+)\Z", t, re.I | re.S)
    if m:
        t = m.group(1).strip()
    t = _SECTION_LABEL.sub("", t).strip()
    return t or _SAFE_FALLBACK


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
    role = (
        "You are the conversational front desk of an AI system. You speak directly to the user, "
        "one turn at a time, in natural first-person language. You do NOT perform tasks yourself "
        "and you have no tools for doing work — your job is to converse, and to hand real work to "
        "the planner and then report back what it did."
    )
    output_schema = (
        "Your final answer is ONLY the words you say to the user"
        "No internal reasoning, no section labels, no tool names, no markup."
    )
    constraints = (
        # The core rule of the front desk: never do the work, delegate it — by CALLING THE TOOL.
        "You have NO tools for doing work — you cannot create files, run commands, or browse. The "
        "ONLY way any work gets done is by calling the delegate_to_planner tool, which hands the "
        "task to the planner. CRITICAL: delegating is a TOOL ACTION you must emit. Writing 'I'll "
        "delegate this' or 'I'll create the file' in a reply does NOTHING — the turn just ends and "
        "no plan ever runs. If a request needs an action or multi-step work (create/edit a file, "
        "look something up externally, build/do/find X), OR the user confirms a delegation you "
        "offered (e.g. replies 'yes'), your action THIS TURN must be the delegate_to_planner tool "
        "call with a clear, self-contained task_description — never a prose reply describing it.",
        "Only AFTER delegate_to_planner returns the planner's result do you reply, summarising THAT "
        "result. Never say you did — or will do — something unless you actually called the tool this turn.",
        "For a plain conversational turn (greeting, chit-chat, or a question you can answer from "
        "the conversation or from recalled memory), just reply — do not delegate.",
        "Answer only from the conversation and from recalled memory; do not invent facts. If you "
        "don't know something and it isn't a task to delegate, say so plainly.",
        "Keep replies conversational and brief unless the user asks for detail.",
    )
    result_type = "text"
    max_tokens = 1024
    temperature = 0.5

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

        The kernel sends ``recall_lookup`` per turn, derived from the conversation's profile:
        customer-facing conversations are tools-only, so an external user cannot pull on the
        deployment's shared memory. Reading it here — rather than hardcoding a class default —
        is what makes that posture a property of the CONVERSATION instead of a setting someone
        can flip once and forget.
        """
        return self._meta(task, "recall_lookup", "false").strip().lower() == "true"

    def run(self, task: "AgentTask"):
        from cambrian_agent_sdk.react import run_think, ReActLoopError

        # Make the turn's managed-LLM lease available to delegate_to_planner (charges any
        # sub-plan to the same lease rather than bypassing the budget).
        self._turn_token = task.session_token_id or ""

        policy = self._meta(task, "policy")
        transcript = self._meta(task, "transcript")
        user_msg = (task.text or "").strip()

        # Compose policy + transcript into the per-turn ROLE, deliberately NOT into task.text:
        # the ReAct seed passes task.text verbatim as the x-tool-query gRPC metadata header,
        # and a multi-line header value is illegal. Keeping task.text as the single-line user
        # message keeps that header valid.
        role_parts = [self.role]
        if policy:
            role_parts.append(
                "Follow this policy strictly; refuse anything it forbids:\n"
                f"<policy>\n{policy}\n</policy>"
            )
        if transcript:
            role_parts.append(f"The conversation so far:\n{transcript}")
        composed_role = "\n\n".join(role_parts)

        turn = AgentTask(
            text=user_msg,
            type="text",
            metadata=task.metadata,
            context=task.context,
            session_token_id=task.session_token_id,
            deadline_remaining_ms=task.deadline_remaining_ms,
        )

        def _think(extra_constraints):
            # The chat front desk has NO system tools — it never executes a task itself. Its only
            # "tool" is delegate_to_planner (an in-process @tool), plus memory recall.
            # seed_system_tools=False strips the kernel tool menu and find_tools. yield_subgoal is
            # disabled: a subgoal is a step INSIDE a plan bound by a kernel coordinator a directly-
            # dispatched chat worker doesn't have; the front desk delegates WHOLE work instead.
            cons = list(self.constraints) if self.constraints else []
            cons += extra_constraints
            return run_think(
                self, turn,
                role=composed_role,
                output_schema=self.output_schema,
                constraints=cons,
                result_type=None,
                seed_recall=self._recall(task),
                seed_system_tools=False,
                allow_yield_subgoal=False,
                # Rounds: delegate_to_planner, then summarise its result (+1 slack).
                max_tool_rounds=3,
                max_tokens=self.max_tokens,
                temperature=self.temperature,
            )

        self._delegated = False
        try:
            result = _think([])
            raw = _result_text(result)
            # Guardrail: the model unreliably NARRATES a delegation ("I'll delegate this to the
            # planner") without emitting the delegate_to_planner tool action, so nothing runs. If
            # the reply promises an action but no delegation happened, force one retry that must
            # call the tool. Only then is the summary grounded in a real planner result.
            if not self._delegated and _ACTION_PROMISE.search(raw or ""):
                _log.warning("chat_agent: reply promised delegation but no tool call fired; forcing retry")
                result = _think([_FORCE_DELEGATE])
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

    def propose(self, request=None) -> ProposalResponse:
        # Chat workers are summoned by the kernel's pool, not won in the auction. Bid low so
        # this agent is never picked for ordinary task planning.
        return ProposalResponse(
            confidence=0.2,
            rationale="conversation worker (summoned by the chat pool, not bid)",
            estimated_latency_ms=4000,
        )


agent = ChatAgent(
    agent_id="chat_agent",
    version="1.0.0",
    description=AGENT_DESCRIPTION,
)


if __name__ == "__main__":
    configure_logging(agent_id="chat_agent")
    agent.serve()
