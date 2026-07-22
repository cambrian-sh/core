"""Chat Session agent (CognitiveAgent) — ADR-0080 (Chat Daemon Ownership).

One instance per conversation. Owns a single customer-service *conversation turn*:
given the running transcript and the latest user message, it decides — in ONE ReAct
loop, never a decomposed plan — whether to:

  - ``final_answer``  : speak to the user (greeting, clarify, refuse, or answer),
  - ``tool_call``     : call a domain tool (e.g. airline MCP tools) to look up / mutate
                        authoritative state (never invent reservation/user facts),
  - ``yield_subgoal`` : escalate a genuinely multi-step task to the kernel planner via the
                        YieldCoordinator (ADR-0037 D10), then narrate the result back.

This is the fix for the τ²-bench airline failure where conversational turns were fed to the
task PLANNER, which decomposed "reply to the customer" into non-executable pseudo-steps
("Ask the customer their name"), dead-looped in replan, and leaked the error as the spoken
reply. Here a turn is owned by ONE agent loop — no planner decomposition.

The spoken output is passed through :meth:`_spoken_only` so an internal error / reasoning
marker / workspace markup can NEVER reach the user (ADR-0080 D5): on any such leak it
substitutes a safe clarifying fallback and the real text is dropped from the reply.

Spawn-time ``params`` (policy text, domain id) arrive as ``self.params`` when run as a
per-conversation daemon; when dispatched statelessly they arrive on the task
(``metadata``/``context``). Both are supported so the same agent works under the MVP
(direct dispatch) and the full Manager→Session daemon wiring.
"""

from __future__ import annotations

import re

from cambrian_agent_sdk import CognitiveAgent, AgentResult, AgentTask
from cambrian_agent_sdk.types import ProposalResponse
from cambrian_agent_sdk._logging import configure_logging


AGENT_DESCRIPTION = (
    "Handles one customer-service conversation turn: replies, calls domain tools to look up "
    "or change authoritative state, or escalates a multi-step task to the planner. One "
    "instance per conversation; never decomposes a turn into a plan."
)

AGENT_MANIFEST = '''
{
  "version": "1.0.0",
  "trait": "cognitive",
  "capabilities": ["conversation", "customer_service", "chat_session"],
  "supported_formats": ["text"],
  "tools": [],
  "release_notes": "ADR-0080 chat session daemon: single-loop conversational turn owner.",
  "dependencies": []
}
'''

# Spoken-reply guardrail (ADR-0080 D5). If the LLM's final answer is actually an internal
# error or leaks reasoning/markup, we must not speak it. These patterns detect the leak.
_LEAK_PATTERNS = (
    re.compile(r"plan partially failed", re.I),
    re.compile(r"replan validation", re.I),
    re.compile(r"no JSON object found", re.I),
    re.compile(r"^\s*Execute:\s", re.I),
    re.compile(r"Traceback \(most recent call last\)", re.I),
    re.compile(r"ReActLoop", re.I),
    re.compile(r"<thought>|</thought>", re.I),
    re.compile(r"\bcid:\w", re.I),          # leaked workspace/offload cid markup
    re.compile(r"<workspace|<Context>|<System>", re.I),
)

# Leading section labels an analyst-style prompt can emit; we keep only the spoken part.
_SECTION_LABEL = re.compile(r"^\s*(Observations?|Reasoning|Thought|Analysis)\s*:\s*.*?$",
                            re.I | re.M)

_SAFE_FALLBACK = "I'm sorry — could you say that once more, please?"


def _spoken_only(text: str) -> str:
    """Return ONLY words safe to speak to a user. Substitutes a fallback on any leak."""
    t = (text or "").strip()
    if not t:
        return _SAFE_FALLBACK
    if any(p.search(t) for p in _LEAK_PATTERNS):
        return _SAFE_FALLBACK
    # If the model emitted a labelled "Conclusion:" section, speak only that.
    m = re.search(r"(?:Conclusion|Answer|Reply)\s*:\s*(.+)\Z", t, re.I | re.S)
    if m:
        t = m.group(1).strip()
    # Drop any residual leading reasoning-section lines.
    t = _SECTION_LABEL.sub("", t).strip()
    return t or _SAFE_FALLBACK


class ChatSessionAgent(CognitiveAgent):
    role = (
        "You are a customer-service agent in a live conversation. You speak directly to the "
        "customer, one turn at a time. Follow the given policy STRICTLY; refuse any action it "
        "forbids even if the customer pushes back."
    )
    output_schema = (
        "Your final answer is ONLY the words you speak to the customer — one or two natural, "
        "first-person sentences. No internal reasoning, no 'Observations'/'Reasoning' labels, "
        "no tool names, no XML/workspace/cid markup."
    )
    constraints = (
        "Use a tool_call to look up or change any reservation/user/account fact — NEVER invent "
        "such facts.",
        "If the request needs several coordinated steps beyond a single lookup, yield_subgoal "
        "with a clear intent and then tell the customer the outcome.",
        "If you already know enough to answer, reply directly with final_answer.",
        "Stay within policy; if policy forbids the request, say so politely and briefly.",
    )
    # No forced result_type: the spoken reply is plain text.
    result_type = None
    max_tokens = 512
    temperature = 0.5
    # Conversation recall is threaded via the prompt (transcript), not a fresh LTM seed each
    # turn — turn continuity comes from the transcript the manager/daemon provides.
    seed_recall = False

    def _policy(self, task: "AgentTask") -> str:
        params = getattr(self, "params", None) or {}
        return (
            params.get("policy")
            or task.metadata.get("policy")
            or task.context.get("policy")
            or ""
        )

    def _transcript(self, task: "AgentTask") -> str:
        # Optional running transcript ("agent: …\ncustomer: …"). Provided by the manager in
        # the stateless MVP; a true per-conversation daemon would hold it in-process instead.
        return task.metadata.get("transcript") or task.context.get("transcript") or ""

    def run(self, task: "AgentTask"):
        from cambrian_agent_sdk.react import run_think, ReActLoopError

        policy = self._policy(task)
        transcript = self._transcript(task)
        user_msg = (task.text or "").strip()

        # Compose the policy + running transcript into the per-turn ROLE (the <Role> prompt
        # section). Deliberately NOT into task.text: the ReAct seed uses task.text verbatim as
        # the x-tool-query gRPC metadata header, and a multi-line value is an illegal header.
        # Keeping task.text = the single-line user message keeps that header valid.
        role_parts = [self.role]
        if policy:
            role_parts.append(f"Follow this policy strictly; refuse anything it forbids:\n"
                              f"<policy>\n{policy}\n</policy>")
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
        try:
            result = run_think(
                self, turn,
                role=composed_role,
                output_schema=self.output_schema,
                constraints=list(self.constraints) if self.constraints else None,
                result_type=None,
                seed_recall=self.seed_recall,
                max_tokens=self.max_tokens,
                temperature=self.temperature,
            )
        except ReActLoopError:
            return AgentResult(data=_SAFE_FALLBACK.encode("utf-8"), type="text", confidence=0.2)
        except Exception:  # noqa: BLE001 — a turn must never crash the session
            return AgentResult(data=_SAFE_FALLBACK.encode("utf-8"), type="text", confidence=0.2)

        raw = _result_text(result)
        return AgentResult(data=("RAW>>>" + raw).encode("utf-8"), type="text", confidence=0.7)  # DEBUG
        spoken = _spoken_only(raw)
        return AgentResult(data=spoken.encode("utf-8"), type="text",
                           confidence=getattr(result, "confidence", 0.7) or 0.7)

    def propose(self, request=None) -> ProposalResponse:
        # Sessions are summoned by the manager, not won in the auction; bid low so it is not
        # picked for ordinary task planning.
        return ProposalResponse(confidence=0.2, rationale="conversation session (summoned, not bid)",
                                estimated_latency_ms=4000)


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
    return str(data or "")


agent = ChatSessionAgent(
    agent_id="chat_session_agent",
    version="1.0.0",
    description=AGENT_DESCRIPTION,
)


if __name__ == "__main__":
    configure_logging(agent_id="chat_session_agent")
    agent.serve()
