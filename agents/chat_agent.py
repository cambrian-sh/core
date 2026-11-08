"""Chat agent (CognitiveAgent) — the OSS conversational worker (ADR-0084 D4).

This is the default agent the kernel's chat worker pool runs. It owns exactly ONE
conversation turn: given the running transcript and the latest user message, it produces the
words to say back — in a single ReAct loop, never a decomposed plan.

That single-loop rule is the whole point. Feeding a conversational turn to the task planner
decomposes "reply to the user" into non-executable pseudo-steps ("Ask the user their name"),
which dead-loops in replan and leaks the failure as the spoken reply (ADR-0080). The planner
is still reachable — the turn hands work that needs tools to it through delegate_to_planner
(yield_subgoal is off for the front desk) — but that is a choice the turn makes, never the
default.

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
import time
from pathlib import Path

import grpc

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
# Today's date, from the kernel's calendar (the "current_date" turn metadata; never this
# process's clock): its own prompt, rendered before the latest message
# (T-20261107-planner-and-chat-agent-are-not-told-today-s).
_TODAY = _LIBRARY.get("current_date")
PROMPT_VERSIONS = {pid: _LIBRARY.get(pid).static_hash for pid in ("chat_agent", "chat_agent_turn")}

# Tags that hold untrusted or kernel-supplied text in the turn (and the policy and the
# work systems in the system text).
_DATA_TAGS = ("conversation", "recalled_memory", "memory", "latest_message", "policy",
              "work_systems", "planner_result", "user_request")


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
#
# Said in the conversation's language (owner decision 2026-11-07,
# T-20261107-chat-failure-texts-are-english-only-in-non): a fixed catalogue keyed by language,
# NOT a model translation — this sentence is spoken exactly when the model's output is unusable.
# The kernel detects the turn's language deterministically and sends it as the "language"
# metadata key (internal/chat locale.go); an absent or unknown language is English. Adding a
# language is one entry here plus its catalogue in internal/chat.
_SAFE_FALLBACKS = {
    "en": "Sorry — I didn't quite catch that. Could you say it again?",
    "tr": "Üzgünüm, tam anlayamadım. Tekrar söyler misiniz?",
}
_DEFAULT_LANGUAGE = "en"
_SAFE_FALLBACK = _SAFE_FALLBACKS[_DEFAULT_LANGUAGE]


def _language_code(language: str) -> str:
    """"tr", "tr-TR", "TR_tr" -> "tr"."""
    return (language or "").strip().lower().replace("_", "-").split("-")[0]


def _safe_fallback(language: str = "") -> str:
    """The fallback sentence in ``language`` ("tr", "tr-TR", ...), else English."""
    return _SAFE_FALLBACKS.get(_language_code(language), _SAFE_FALLBACKS[_DEFAULT_LANGUAGE])


# The language the reply is written in, named for the model (chat_agent_turn reply-language).
# T-20261108-turkish-asks-still-answered-in-english-in-3-of: with no rule on the reply's
# language, the front desk relayed English planner results in English to Turkish asks. The
# kernel's detected language (the "language" turn key) now reaches the model by name, after
# the reply instruction and after every planner result. A code without a name here renders
# no line; the system text's general rule (the latest message's language) still holds.
_LANGUAGE_NAMES = {
    "en": "English",
    "tr": "Turkish",
}


def _language_name(language: str) -> str:
    return _LANGUAGE_NAMES.get(_language_code(language), "")


# What the user is told when the turn runs out of time AND the one final reply call could
# not be made or failed (T-20261108-chat-turn-still-overruns-240-s-after-a-deadline). Fixed
# per language, like _SAFE_FALLBACKS: it is said exactly when the model is not available in
# time. With results, the planner's own text follows it, so the partial work still reaches
# the user instead of an empty turn.
_OUT_OF_TIME = {
    "en": {
        "results": "I ran out of time before I could finish this. Here is what came back so far:",
        "none": "I ran out of time on this one before any result came back. Some steps may already "
                "have run; ask me to continue and I'll check where it stands.",
    },
    "tr": {
        "results": "Bunu bitirmeden süre doldu. Şu ana kadar gelen sonuçlar:",
        "none": "Herhangi bir sonuç gelmeden bu iş için süre doldu. Bazı adımlar çalışmış olabilir; "
                "devam etmemi isterseniz nerede kaldığını kontrol ederim.",
    },
}


def _out_of_time_reply(language: str, results) -> str:
    texts = _OUT_OF_TIME.get(_language_code(language), _OUT_OF_TIME[_DEFAULT_LANGUAGE])
    body = "\n\n".join(r.strip() for r in results if (r or "").strip())
    return f"{texts['results']}\n\n{body}" if body else texts["none"]


class _ReplyNow(BaseException):
    """Ends the ReAct loop because the turn has no time left for another round.

    A BaseException on purpose: it is raised from inside run_think (the delegate tool and
    the deadline-guarded substrate), and both the SDK's tool registry and the loop turn an
    Exception into an error the model sees and answers in yet another round, which is
    exactly the round there is no time for. ChatAgent.run catches it and writes the reply
    in one call.
    """


class _DeadlineGuardedSubstrate:
    """The agent's substrate client for one turn, refusing to START a model call once the
    turn is inside its reply reserve.

    run_think's model calls carry no client deadline (the SDK sends timeout_ms=0 so a slow
    model is not strangled), and nothing bounded how many rounds the loop took after a
    deadline-cut plan: a further tool call, the tool-budget nudge, an inferred-answer
    reprompt, 7-14 s each, ended three turns past their deadline with no reply
    (T-20261108-chat-turn-still-overruns-240-s-after-a-deadline). Any model call the loop
    would start inside the reserve raises _ReplyNow instead. Everything else (execute, the
    tool and memory calls) passes straight through.
    """

    _MODEL_CALLS = frozenset({"generate", "generate_request", "generate_stream", "generate_with_tools"})

    def __init__(self, inner, agent):
        self._inner, self._agent = inner, agent

    def __getattr__(self, name):
        attr = getattr(self._inner, name)
        if name not in self._MODEL_CALLS or not callable(attr):
            return attr

        def guarded(*args, **kwargs):
            if self._agent._inside_reply_reserve():
                _log.warning("chat_agent: %.0f s left in the turn; writing the reply now instead of "
                             "another loop round", self._agent._time_left() or 0.0)
                raise _ReplyNow()
            return attr(*args, **kwargs)

        return guarded


def _plain_reply(text: str) -> str:
    """The words of a final reply written as plain text. A model that answers the one final
    call with its usual final_answer envelope anyway still gets its answer through."""
    t = (text or "").strip()
    if t.startswith("{"):
        try:
            import json
            obj = json.loads(t)
        except ValueError:
            return t
        if isinstance(obj, dict):
            answer = obj.get("answer")
            return answer.strip() if isinstance(answer, str) else ""
    return t

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


# After delegate_to_planner RETURNED a result this turn, a reply that still says "I don't
# have a tool / access for that" contradicts the work the planner's agents just did on the
# user's systems (T-20261107-chat-agent-tells-the-user-it-has-no-tool-for, o3-ar-reminders:
# the AR data came back and the reply said "the tools available to me don't include direct
# access to your accounts receivable system"). run() then retries once with the result as
# data. Same nature as _ACTION_PROMISE: English-only and anchored on a first-person denial;
# in another language it never fires, which is the behaviour without the guard.
_NO_ACCESS_CLAIM = re.compile(
    r"\b(?:i\s+(?:do\s+not|don['’]?t|cannot|can['’]?t|have\s+no)|i['’]?m\s+(?:not\s+able|unable)"
    r"|i\s+am\s+(?:not\s+able|unable))\b[^.!?\n]{0,60}?\b(?:access|tool)"
    r"|\btools?\s+(?:available\s+to\s+me|i\s+have)\b[^.!?\n]{0,40}?\b(?:do\s+not|don['’]?t)\s+include",
    re.I,
)

# A planner result that itself reports a refusal or a denial is not contradicted by a
# reply that relays it: the guard must not tell the model "the planner reached it" then.
_RESULT_REPORTS_DENIAL = re.compile(
    r"\b(?:access|permission)\s+(?:was\s+|is\s+)?denied\b|\bunable\s+to\s+access\b|\bnot\s+authori[sz]ed\b",
    re.I,
)


# A refusal the kernel marks as final: an approval that a human denied, or one that could not
# be routed to anyone (domain.ApprovalDenyReason: "not approved: denied ...", "not approved:
# unroutable ...", "not approved: needs_hierarchy ..."; the last two also carry "Retrying this
# call will be refused the same way"). These are the kernel's own fixed strings, not user
# language, so matching them is a contract check, not an English heuristic. Once a result this
# turn carries one, delegate_to_planner serves that result instead of running another plan:
# the front desk had re-delegated reworded plans after such refusals until the 240 s turn
# deadline (T-20261107-chat-agent-re-delegates-after-a-deterministic, run
# 20261107T114033Z_usecases_full). "Expired" and "withdrawn" are left out on purpose: asking
# again raises a new approval that someone may answer.
_FINAL_DENIAL = re.compile(
    r"not approved: (?:denied|unroutable|needs_hierarchy)\b|Retrying this call will be refused the same way",
)


def _reports_final_denial(planner_result: str) -> bool:
    return bool(_FINAL_DENIAL.search(planner_result or ""))


# The turn clock. A module-level indirection so tests can drive a fake clock without
# patching time.monotonic for the SDK as well.
_now = time.monotonic


def _is_deadline(exc: BaseException) -> bool:
    """True when a kernel call failed because its gRPC deadline passed."""
    code = getattr(exc, "code", None)
    try:
        return callable(code) and code() == grpc.StatusCode.DEADLINE_EXCEEDED
    except Exception:  # noqa: BLE001 — a malformed error is simply not a deadline
        return False


def _contradicts_delegation(reply: str, planner_result: str) -> bool:
    """True when the planner returned a usable result this turn and the reply still
    claims there is no tool or no access."""
    result = (planner_result or "").strip()
    if not result or _RESULT_REPORTS_DENIAL.search(result) or _NO_ACCESS_CLAIM.search(result):
        return False
    return bool(_NO_ACCESS_CLAIM.search(reply or ""))


def _spoken_only(text: str, language: str = "") -> str:
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
        return _safe_fallback(language)
    if any(p.search(t) for p in _LEAK_PATTERNS):
        return _safe_fallback(language)
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
    # The front desk is where a person says "every Monday at 8, send me ...": it may
    # PROPOSE a recurring task for them and manage theirs (capability
    # "scheduled-tasks"). The kernel takes the owner from the turn's lease, asks that
    # person's approval, and creates nothing without it
    # (T-20261106-the-assistant-cannot-set-up-a-recurring-task). Offered only when the
    # kernel serves the RPCs.
    allow_scheduled_tasks = True

    # The managed-LLM budget lease for the turn in flight (ADR-0018). Stashed in run() so the
    # planner-delegation tool can charge a sub-plan to the same lease.
    _turn_token = ""
    # Set True the moment delegate_to_planner actually runs this turn, so run() can tell a real
    # delegation from a model that merely NARRATED one and force a retry.
    _delegated = False
    # The planner's result for this turn, and whether a repeated delegate_to_planner is
    # served from it (the report-delegated retry must never run a second plan).
    _delegation_result = ""
    _replay_delegation = False
    # The result text of a delegation this turn that reported a final refusal, or "".
    _final_denial = ""

    # Turn-deadline budget (T-20261107-gov-use-case-attempts-end-with-an-empty). The kernel
    # bounds a chat turn (internal/chat TurnTimeout) and the deadline reaches this agent as
    # task.deadline_remaining_ms. When it passes, the turn ends with NO reply: the kernel
    # cancels the work and revokes the lease. The front desk ignored it, so after a 190 s plan
    # it started a second one with 46 s left and the user got nothing (run
    # 20261107T120552Z_usecases_full, every empty gov reply). So each delegation is given the
    # time left minus reply_reserve_s, the time kept back to write the reply (replies after a
    # plan took 7-15 s in that run); with less than min_delegation_s to give, no plan is
    # started and the model is told to answer with what it has (the 184 delegations in that
    # run took 18-233 s, median ~40 s; about a third finished within 30 s). A deployment
    # with a slower model raises these on a subclass.
    #
    # Inside the reply reserve the loop gets no further round: the turn's reply is then
    # written in ONE call (_reply_now), bounded by the time left minus final_margin_s, the
    # time kept for the reply to travel back to the kernel. A plan that returns with less
    # than reply_reserve_s + min_delegation_s left ends the loop the same way: no plan can
    # start after it, so every further round would only spend the reserve
    # (T-20261108-chat-turn-still-overruns-240-s-after-a-deadline).
    reply_reserve_s = 25.0
    min_delegation_s = 30.0
    final_margin_s = 3.0
    # time.monotonic() value of the turn deadline, or 0.0 when the kernel sent none.
    _turn_deadline = 0.0
    # Every planner result this turn, in order: the material of the one final reply.
    _turn_results: list = []
    # The turn's language (the kernel's "language" key) and its seed recall, once per turn.
    _turn_language = ""
    _seed_cache = None
    # The latest message, and whether the turn's first delegation carried it to the planner.
    _turn_message = ""
    _scope_sent = False

    def _time_left(self):
        """Seconds until the turn deadline, or None when the turn has none."""
        if not self._turn_deadline:
            return None
        return self._turn_deadline - _now()

    def _has_time_for(self, seconds: float) -> bool:
        left = self._time_left()
        return left is None or left >= seconds

    def _inside_reply_reserve(self) -> bool:
        return not self._has_time_for(self.reply_reserve_s)

    @tool
    def delegate_to_planner(self, task_description: str) -> str:
        """Hand work that needs tools to the planner and return its result: any lookup or
        action in the user's work systems (records, messages, mail, accounts), files, code,
        research or a multi-step job, and any draft, message, event, ticket, document or
        record the user wants created in one of their systems (it is saved only if an
        agent creates it there). Not for text the user only wants to read in this reply.
        Pass the whole request, including the final create or send step and every limit
        the user set, in the user's own terms: a first-person ask stays about the user's
        own data.

        This calls the kernel's Execute path, which runs the full planner; its agents act
        as the user. It is the only way to reach the planner from a chat turn.
        """
        if self._replay_delegation and self._delegated:
            # The report retry: the plan already ran this turn; hand back its result.
            return self._delegation_result
        if self._final_denial:
            # A result this turn carried a refusal a retry cannot change: never run another
            # plan for it (reworded or split up, it is refused the same way).
            _log.warning("chat_agent: re-delegation after a final refusal suppressed")
            return _TURN["denial-final"].render({"planner_result": _neutralise(self._final_denial)})
        left = self._time_left()
        budget = None if left is None else left - self.reply_reserve_s
        if budget is not None and budget < self.min_delegation_s:
            # A plan started now would be killed by the turn deadline, and the reply with it.
            # Answering the model with a note let the loop run on for rounds the turn did not
            # have; the reply is written now, in one call, from what this turn already has.
            _log.warning("chat_agent: %.0f s left in the turn; not starting another plan", left)
            raise _ReplyNow()
        self._delegated = True
        try:
            result = self.substrate.execute(
                self._scoped_task(task_description), session_token_id=self._turn_token,
                # The plan's share of the turn: its gRPC deadline reaches the kernel, which
                # stops the plan a little before it (execution.plan.caller_deadline_reserve_ms)
                # and returns what completed plus the ledger of its writes as an ordinary
                # "The plan did not finish: ..." result, leaving reply_reserve_s for the reply.
                # DEADLINE_EXCEEDED below is then only a kernel that could not answer in time.
                timeout_ms=int(budget * 1000) if budget is not None else 0)
        except grpc.RpcError as e:
            if not _is_deadline(e):
                raise
            _log.warning("chat_agent: delegated plan did not finish within %.0f s; replying without it",
                         budget or 0.0)
            self._delegation_result = ""
            raise _ReplyNow()
        self._delegation_result = _result_text(result)
        self._turn_results.append(self._delegation_result)
        if _reports_final_denial(self._delegation_result):
            self._final_denial = self._delegation_result
        if not self._has_time_for(self.reply_reserve_s + self.min_delegation_s):
            # No plan can start after this one, so every further loop round (a memory query,
            # a refused delegation, the tool-budget nudge, a reprompt) only spends the time
            # the reply needs: write the reply now from the results.
            _log.warning("chat_agent: %.0f s left after the plan; writing the reply now",
                         self._time_left() or 0.0)
            raise _ReplyNow()
        # The result is often in another language than the conversation (the planner and its
        # agents work in English); the reply's language is restated right where the model
        # reads the result it is about to relay.
        lang_line = self._reply_language_line()
        return f"{self._delegation_result}\n\n{lang_line}" if lang_line else result

    def _scoped_task(self, task_description: str) -> str:
        """The task sent to the planner. The FIRST delegation of a turn carries the user's
        latest message verbatim after the front desk's task_description, with the instruction
        to plan the whole request including its final create, record, save or send step
        (chat_agent_turn delegation-scope).

        T-20261108-chat-front-desk-delegates-a-draft-and-save: asked to draft an FOI response
        and record it in the case system, the front desk delegated lookup after lookup (2.71
        delegations per attempt), its three tool rounds ran out, and it wrote the response in
        its own reply: 0 writes in 6 attempts. The rule "never delegate only the lookup"
        (chat_agent v3) did not hold, so the planner now always sees the whole request once
        and plans its write step itself. Later delegations in the turn are follow-ups and go
        as written; a task_description that already holds the message is sent unchanged.
        """
        message = (self._turn_message or "").strip()
        if self._scope_sent or not message or message in task_description:
            return task_description
        self._scope_sent = True
        return _TURN["delegation-scope"].render({
            "task_description": task_description.strip(),
            "message": _neutralise(message),
        })

    def _reply_language_line(self) -> str:
        name = _language_name(self._turn_language)
        return _TURN["reply-language"].render({"language": name}) if name else ""

    def _reply_now(self, task: "AgentTask", substrate, recall: bool) -> str:
        """The turn's reply in ONE model call, when the turn has no time for another loop
        round: the conversation, the latest message and every planner result of the turn,
        with the reply-now note. Its gRPC deadline is the time left minus final_margin_s, so
        it cannot carry the turn past its deadline; when there is no time even for that, or
        the call fails, the user gets the fixed out-of-time sentence with the results."""
        language = self._meta(task, "language")
        left = self._time_left()
        budget_s = None if left is None else left - self.final_margin_s
        if budget_s is not None and budget_s < 1.0:
            _log.warning("chat_agent: no time left for the final reply call; sending the results as they are")
            return _out_of_time_reply(language, self._turn_results)
        role, constraints, turn_text, _ = self.compose(task, recall=recall, reply_now=True)
        system = "\n\n".join([role, self.output_schema, "\n".join(constraints)])
        try:
            res = substrate.generate_request(
                task.session_token_id,
                system=[{"text": system, "cache": True}],
                messages=[{"role": "user", "content": turn_text}],
                cache_ttl=self.cache_ttl,
                max_tokens=self.max_tokens,
                temperature=self.temperature,
                timeout_ms=int(budget_s * 1000) if budget_s is not None else 0,
            )
            text = _plain_reply(getattr(res, "text", res) if not isinstance(res, str) else res)
        except Exception as e:  # noqa: BLE001 — a failed last call must still leave a reply
            _log.warning("chat_agent: final reply call failed (%r); sending the results as they are", e)
            return _out_of_time_reply(language, self._turn_results)
        return text or _out_of_time_reply(language, self._turn_results)

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

    def compose(self, task: "AgentTask", *, recall: bool, force_delegate: bool = False,
                report_result: str = "", reply_now: bool = False):
        """The turn's prompt parts: ``(role, constraints, turn_text, max_memory_queries)``.

        System (stable for the conversation, cached): role, rules, the posture's answering
        rule, the work systems the planner's agents can use, and the policy. User turn
        (volatile): the transcript, recalled memory and the latest message, each in its own
        tag, the reply instruction; on a retry, the retry note after it; for the one final
        reply call (``reply_now``), the turn's planner results and the reply-now note; the
        reply-language line last.
        """
        from cambrian_agent_sdk.react import _DEFAULT_MAX_MEMORY_QUERIES

        policy = self._meta(task, "policy").strip()
        transcript = self._meta(task, "transcript").strip()
        work_systems = self._meta(task, "work_systems").strip()
        user_msg = (task.text or "").strip()

        role = self.role
        # The MCP systems the planner's agents can call, rendered by the kernel from the
        # planner's live tool menu (internal/chat buildHandoff "work_systems"). The front
        # desk's own skills menu has no entry for them, so without this list it answered
        # "I have no tool for your CRM" instead of delegating
        # (T-20261107-chat-agent-tells-the-user-it-has-no-tool-for).
        if work_systems:
            role += "\n\n" + _SYSTEM["work-systems"].render({"work_systems": _neutralise(work_systems)})
        if policy:
            role += "\n\n" + _SYSTEM["policy"].render({"policy": _neutralise(policy)})
        posture = "answer-memory" if recall else "answer-tools-only"
        constraints = list(self.constraints) + _SYSTEM[posture].render().splitlines()

        parts = []
        if transcript:
            parts.append(_TURN["conversation"].render({"transcript": _neutralise(transcript)}))
        # Recalled once per turn: a retry or the final reply call reuses it rather than
        # spending another recall round trip.
        if recall and self._seed_cache is None:
            self._seed_cache = self._seed_memories(task, user_msg)
        memories = self._seed_cache if recall else ""
        if memories:
            parts.append(_TURN["memory"].render({"memories": memories}))
        today = self._meta(task, "current_date").strip()
        if today:
            today = re.sub(r"(?i)</\s*today\s*>", "[/today]", today)
            parts.append(_TODAY.render({"today": today}).text)
        parts.append(_TURN["message"].render({"message": _neutralise(user_msg)}))
        if force_delegate:
            parts.append(_TURN["force-delegate"].render())
        if report_result:
            parts.append(_TURN["report-delegated"].render({"planner_result": _neutralise(report_result)}))
        if reply_now:
            results = "\n\n---\n\n".join(r.strip() for r in self._turn_results if (r or "").strip())
            parts.append(_TURN["reply-now"].render({"planner_result": _neutralise(results or "(none)")}))
        lang_line = self._reply_language_line()
        if lang_line:
            parts.append(lang_line)
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
        # The conversation's language, detected by the kernel (internal/chat buildHandoff).
        fallback = _safe_fallback(self._meta(task, "language"))

        def _think(force_delegate: bool, report_result: str = ""):
            role, constraints, turn_text, max_mem = self.compose(
                task, recall=recall, force_delegate=force_delegate, report_result=report_result)
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
        self._delegation_result = ""
        self._replay_delegation = False
        self._final_denial = ""
        self._turn_results = []
        self._turn_language = self._meta(task, "language")
        self._seed_cache = None
        self._turn_message = task.text or ""
        self._scope_sent = False
        remaining_ms = getattr(task, "deadline_remaining_ms", 0) or 0
        self._turn_deadline = _now() + remaining_ms / 1000.0 if remaining_ms > 0 else 0.0
        substrate = self.substrate
        if self._turn_deadline:
            self.substrate = _DeadlineGuardedSubstrate(substrate, self)
        try:
            return self._run_turn(task, _think, recall, fallback, substrate, ReActLoopError)
        finally:
            self.substrate = substrate

    def _run_turn(self, task, _think, recall, fallback, substrate, ReActLoopError):
        result = None
        replied_now = False  # the reply came from the one final call (_reply_now)
        try:
            result = _think(False)
            raw = _result_text(result)
            # Guardrail: the model unreliably NARRATES a delegation ("I'll delegate this to the
            # planner") without emitting the delegate_to_planner tool action, so nothing runs. If
            # the reply promises an action but no delegation happened, force one retry that must
            # call the tool. The retry note goes in the user turn, so the system text is unchanged.
            # Only with time for a plan and a reply: a retry the deadline cuts off replaces a
            # reply with none.
            if (not self._delegated and _ACTION_PROMISE.search(raw or "")
                    and self._has_time_for(self.reply_reserve_s + self.min_delegation_s)):
                _log.warning("chat_agent: reply promised delegation but no tool call fired; forcing retry")
                result = _think(True)
                raw = _result_text(result)
            # Guardrail: the planner returned a result this turn, yet the reply says there is
            # no tool or no access. Retry once with the result as data; a repeated delegation
            # in that retry is served from the turn's result, never a second plan. Only with
            # time left for one more reply.
            if (self._delegated and _contradicts_delegation(raw, self._delegation_result)
                    and self._has_time_for(self.reply_reserve_s)):
                _log.warning("chat_agent: reply denied access after a delegation returned a result; "
                             "retrying with the result")
                self._replay_delegation = True
                result = _think(False, self._delegation_result)
                raw = _result_text(result)
        except _ReplyNow:
            # The turn is out of loop time: one call writes the reply (bounded by the time
            # left), or the fixed out-of-time sentence carries the results.
            raw = self._reply_now(task, substrate, recall)
            replied_now = True
        except ReActLoopError as e:
            # The loop died on a budget (T-20261108-chat-agent-throws-away-a-drafted-answer-and).
            # At the end of a long turn that had real work behind it, "could you say it again?"
            # threw the work away and invited the user to re-run a four-minute job. Keep what
            # the turn has: the answer the model had already drafted (the inferred answer it
            # was re-prompted on), else one reply call over the planner's results. Only a turn
            # with neither gets the fallback sentence.
            draft = getattr(e, "draft", "") or ""
            if draft.strip() and _spoken_only(draft, self._turn_language) != fallback:
                _log.warning("chat_agent: ReActLoopError (%s); returning the drafted answer", e)
                raw = draft
            elif self._turn_results:
                _log.warning("chat_agent: ReActLoopError (%s); replying from the turn's results", e)
                raw = self._reply_now(task, substrate, recall)
                replied_now = True
            else:
                _log.warning("chat_agent: ReActLoopError -> fallback: %s", e)
                return AgentResult(data=fallback.encode("utf-8"), type="text", confidence=0.2)
        except Exception as e:
            # Not a reply: the model provider refused, the turn's lease is gone, the kernel is
            # unreachable. Speaking the fallback here ("I didn't quite catch that, could you
            # say it again?") blamed the user's message for an outage, invited a retry that
            # could not succeed, was stored in the transcript as a real reply, and hid the
            # failure from the kernel: a revoked provider token read as "the agent cannot
            # understand Turkish" (T-20261107-chat-agent-answers-turkish-government-asks-with).
            # Raising fails the turn instead; the SDK runtime returns INTERNAL (the worker
            # lives on), and the kernel's turn-failure path stores nothing and tells the user,
            # on the status line, that something went wrong on its side (internal/chat
            # humanFailure).
            _log.warning("chat_agent: run_think raised; failing the turn: %r", e)
            raise

        spoken = _spoken_only(raw, self._meta(task, "language"))
        if spoken == fallback:
            _log.warning("chat_agent: fallback substituted. raw_len=%d raw_head=%r delegated=%s",
                         len(raw), raw[:200], self._delegated)
            if self._turn_results and not replied_now:
                # The reply was unusable (a step-trace dump, a leak) but the planner returned
                # results this turn: one reply call over them instead of "say it again".
                spoken = _spoken_only(self._reply_now(task, substrate, recall), self._turn_language)
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
