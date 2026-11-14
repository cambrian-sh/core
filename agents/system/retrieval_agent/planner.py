"""Retrieval agent — the LLM-step logic of the agentic retrieval loop.

This module is the "thin stateless Python endpoint" from AGENTIC_RETRIEVAL_SPEC
§2.1: pure functions that take the loop state + an injected LLM callable and
return a decision. Go owns the loop + retrieval tiers and calls these per step.

Ops: the lexical query planner (``plan_step``), the stop/continue decision
(``decide_continue``), typed
synthesis (``synthesize`` / ``synthesize_cited``), up-front decomposition
(``decompose`` / ``decompose_routed``), grounded sub-question extraction
(``answer_subquestion``).

Prompts (ADR-0139 C4/D2). Every prompt is a file in the kernel's prompt library,
``cambrian-core/prompts/retrieval_<op>/v<N>.md``, rendered with the SDK's
``prompt_library`` (the same rules and static hash as the Go loader). Each call
is a stable, slot-free system block the provider caches (with the reply's JSON
schema) plus a volatile user message in which the untrusted text (retrieved
documents, the question, the item text) sits in named tags, documents first and
the question last.

The LLM is injected as ``LLM = Callable[[LLMRequest], Reply | str]`` (temp=0
expected) so this module is unit-testable with a fake and backend-agnostic. The
agent (``agent.py``) binds it to ``SubstrateClient.generate_request``; a fake may
return a plain string, which is read like a reply whose schema was not checked.

Structured outputs: a reply the kernel validated against the response schema is
decoded with ``json.loads`` as is. Only a reply whose schema was NOT checked (the
kernel reported ``response_schema`` as ignored, or predates the structured
fields) goes through the tolerant decode, which finds the first JSON value in
prose or a code fence. The old first-``{``/last-``}`` scraper is gone.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import unicodedata
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from cambrian_agent_sdk.prompt_library import PromptLibrary

# --------------------------------------------------------------------------
# The LLM seam: a structured request in, a reply out
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class LLMRequest:
    """One model call in the ADR-0139 C1 shape.

    ``system`` is ``[{"text", "cache"}]`` (stable, last block cached),
    ``messages`` the volatile user turn, ``response_schema`` the
    ``{"name", "schema", "strict"}`` dict ``generate_request`` takes (or None),
    ``prompt_version`` the static hash of every prompt file the call sent.
    """

    op: str
    system: list[dict]
    messages: list[dict]
    response_schema: dict | None
    prompt_version: str

    def flatten(self) -> str:
        """The request as one prompt string, for a backend with no system field."""
        parts = [b["text"].strip() for b in self.system if b.get("text", "").strip()]
        parts += [m["content"].strip() for m in self.messages if m.get("content", "").strip()]
        return "\n\n".join(parts)

    @property
    def user(self) -> str:
        return "\n\n".join(m["content"] for m in self.messages)


@dataclass(frozen=True)
class Reply:
    """A model reply. ``schema_checked`` is true only when the kernel validated
    the text against the request's response schema (so it is JSON as is)."""

    text: str
    schema_checked: bool = False


LLM = Callable[[LLMRequest], "Reply | str"]


def _call(llm: LLM, req: LLMRequest) -> Reply:
    r = llm(req)
    if isinstance(r, Reply):
        return r
    return Reply(text="" if r is None else str(r), schema_checked=False)


# --------------------------------------------------------------------------
# The prompt library (cambrian-core/prompts)
# --------------------------------------------------------------------------


def _prompts_root() -> Path:
    """The kernel prompt tree: ``$CAMBRIAN_PROMPTS_DIR``, else the nearest
    ancestor ``prompts/`` holding this agent's prompts: the repo, the image
    (/app/prompts) and a ``setup`` install (<prefix>/prompts, unpacked from the
    kernel embed) all put prompts/ beside agents/."""
    env = os.environ.get("CAMBRIAN_PROMPTS_DIR", "").strip()
    if env:
        return Path(env)
    for parent in Path(__file__).resolve().parents:
        cand = parent / "prompts"
        if (cand / "retrieval_synthesize" / "v1.md").is_file():
            return cand
    raise RuntimeError(
        "retrieval_agent: cannot find the kernel prompt library (a prompts/ directory "
        "above this file); set CAMBRIAN_PROMPTS_DIR")


# Loaded once, at import: a missing or invalid prompt file fails the agent at
# start, not on the first query.
LIBRARY = PromptLibrary.load(_prompts_root())

_CLOSING_TAGS = ("documents", "question", "resolved_entities", "current_entity",
                 "sub_question")


def neutralise_closing_tags(value: str, tags: tuple[str, ...] = _CLOSING_TAGS) -> str:
    """Rewrite ``</tag>`` (any case, inner spaces) to ``[/tag]`` so untrusted
    text cannot close its data region (SPEC.md §3.1; the Go twin is
    ``prompts.NeutraliseClosingTags``)."""
    for t in tags:
        value = re.sub(r"(?i)</\s*" + re.escape(t) + r"\s*>", f"[/{t}]", value)
    return value


def _schema(prompt) -> dict | None:
    if prompt.response_schema is None:
        return None
    return {"name": prompt.id, "schema": prompt.response_schema.decode("utf-8"), "strict": False}


def _version(*prompts) -> str:
    """sha256 over the static hashes of every file sent, in order (the Go
    ``Pair.StaticHash`` rule, generalised to the synthesis triple)."""
    return hashlib.sha256("\n".join(p.static_hash for p in prompts).encode("utf-8")).hexdigest()


def build_request(op: str, system_ids: tuple[str, ...], input_id: str,
                  values: dict[str, str]) -> LLMRequest:
    """Render one call: slot-free system blocks (last one cached and owning the
    schema) and the input prompt as the single user message."""
    systems = [LIBRARY.get(i) for i in system_ids]
    for p in systems:
        if p.inputs:
            raise ValueError(f"{p.id}: a system block must be slot-free")
    inp = LIBRARY.get(input_id)
    user = inp.render({k: neutralise_closing_tags(str(v)) for k, v in values.items()}).text
    blocks = [{"text": p.body, "cache": i == len(systems) - 1} for i, p in enumerate(systems)]
    return LLMRequest(op=op, system=blocks, messages=[{"role": "user", "content": user}],
                      response_schema=_schema(systems[-1]),
                      prompt_version=_version(*systems, inp))


# --------------------------------------------------------------------------
# Decoding
# --------------------------------------------------------------------------

_FENCE_RE = re.compile(r"^```[A-Za-z0-9_-]*\s*\n?(.*?)\n?```$", re.DOTALL)


def _strip_fence(text: str) -> str:
    t = text.strip()
    m = _FENCE_RE.match(t)
    return m.group(1).strip() if m else t


def _tolerant(text: str, want: type) -> Any:
    """First JSON value of type ``want`` in prose or a code fence. Used ONLY for a
    reply whose schema the kernel did not check (legacy kernel, ignored schema,
    a test fake, the env-gated local decompose backend)."""
    t = _strip_fence(text)
    try:
        v = json.loads(t)
        if isinstance(v, want):
            return v
    except ValueError:
        pass
    dec = json.JSONDecoder()
    opener = "{" if want is dict else "["
    i = t.find(opener)
    while i != -1:
        try:
            v, _ = dec.raw_decode(t, i)
            if isinstance(v, want):
                return v
        except ValueError:
            pass
        i = t.find(opener, i + 1)
    return None


def decode(reply: Reply, want: type = dict) -> Any:
    """The reply as a ``want`` (dict or list), or None when it is not one."""
    if reply.schema_checked:
        try:
            v = json.loads(_strip_fence(reply.text))
        except ValueError:
            return None
        return v if isinstance(v, want) else None
    return _tolerant(reply.text, want)


def extract_json(text: str) -> dict | None:
    """Tolerant decode of a plain completion (kept for callers and tests)."""
    return _tolerant(text, dict)


def _strlist(v: object) -> list[str]:
    if not isinstance(v, list):
        return []
    out: list[str] = []
    for x in v:
        s = str(x).strip()
        if s:
            out.append(s)
    return out


def build_context(chunks: object, max_chars: int) -> str:
    """Join retrieved chunk texts into the documents region.

    Dedups by CONTENT first (a union-ingest of shared corpora surfaces the same
    paragraph under several ids, which interleaveDedup, keyed on doc id, does not
    collapse; without this the duplicates eat the char budget and push the real
    answer past the clip). Then truncates to ``max_chars``, kept generous so a
    long answer paragraph is not cut off mid-sentence.
    """
    items = chunks if isinstance(chunks, list) else []
    seen: set[str] = set()
    uniq: list[str] = []
    for c in items:
        t = str(c).strip()
        if not t or t in seen:
            continue
        seen.add(t)
        uniq.append(t)
    return "\n---\n".join(uniq)[:max_chars]


def build_labeled_context(chunks: object, max_chars: int) -> str:
    """Number retrieved chunks [1], [2], ... in the ORDER GIVEN, for citation.

    Unlike ``build_context`` this does NOT content-dedup: the caller (AnswerSystem,
    ADR-0081) resolves citation marker n against evidence[n-1] on the kernel side,
    so the label→chunk mapping here must stay 1:1 with the order it was handed.
    """
    items = chunks if isinstance(chunks, list) else []
    parts: list[str] = []
    for i, c in enumerate(items, start=1):
        t = str(c).strip()
        if not t:
            continue
        parts.append(f"[{i}] {t}")
    return "\n---\n".join(parts)[:max_chars]


# --------------------------------------------------------------------------
# QuerySpec — the structured query contract the agent emits (spec §2.2)
# --------------------------------------------------------------------------


@dataclass
class QuerySpec:
    """What the planner emits; the Go side compiles it to a lexical query.

    ``must_terms`` are the discriminative tokens (the win over fixed BM25/IDF —
    the agent knows that in "what did the rose say in chapter 3 scene 2" the
    discriminative tokens are the anchors, not "say"). ``phrases`` are
    exact-phrase constraints. ``should_terms`` broaden recall.
    """

    nl_intent: str
    must_terms: list[str] = field(default_factory=list)
    should_terms: list[str] = field(default_factory=list)
    phrases: list[str] = field(default_factory=list)

    def to_lexical_query(self) -> str:
        """Compile to a search string.

        Phrases (quoted) and must-terms lead — they are the discriminative
        signal the planner selected; should-terms are appended to broaden.
        Falls back to the raw intent when the planner emitted nothing usable,
        so the loop degrades to today's behavior rather than an empty query.
        """
        parts: list[str] = [f'"{p}"' for p in self.phrases]
        parts += self.must_terms
        parts += self.should_terms
        compiled = " ".join(dict.fromkeys(p for p in parts if p)).strip()
        return compiled or self.nl_intent.strip()

    def to_dict(self) -> dict[str, object]:
        return {
            "nl_intent": self.nl_intent,
            "must_terms": self.must_terms,
            "should_terms": self.should_terms,
            "phrases": self.phrases,
            "lexical_query": self.to_lexical_query(),
        }


# --------------------------------------------------------------------------
# plan_step — the lexical query-planner
# --------------------------------------------------------------------------


def plan_step_request(query: str, scratchpad: str = "", history: list[str] | None = None,
                      hop_index: int = 0) -> LLMRequest:
    focus = scratchpad.strip()
    if not focus:
        return build_request("plan_step", ("retrieval_plan_first",), "retrieval_plan_first_input",
                             {"question": query})
    # history's last entry is `focus` itself (the loop records the bridge then
    # plans the next hop); show only the EARLIER links as "already resolved".
    hist = [str(h).strip() for h in (history or []) if str(h).strip() and str(h).strip() != focus]
    resolved = "\n".join(f"- {h}" for h in hist) or "(none yet)"
    return build_request("plan_step", ("retrieval_plan_hop",), "retrieval_plan_hop_input",
                         {"question": query, "resolved": resolved, "focus": focus,
                          "hop": str(hop_index)})


def plan_step(query: str, llm: LLM, scratchpad: str = "",
              history: list[str] | None = None, hop_index: int = 0) -> QuerySpec:
    """Select discriminative lexical terms for this hop's search.

    Hop 1 (empty scratchpad) plans from the raw question. Later hops plan FOR the
    bridge entity in ``scratchpad``, HOP-AWARE: ``history`` (entities resolved in
    earlier links) + ``hop_index`` let the planner work out which link of the
    chain this is and pick the relation for THAT link. Robust to bad/empty LLM
    output: on parse failure the QuerySpec carries no terms, so
    :meth:`QuerySpec.to_lexical_query` falls back to the bridge (later hops) or
    the raw query (hop 1) — the loop never emits an empty search.
    """
    focus = scratchpad.strip()
    obj = decode(_call(llm, plan_step_request(query, scratchpad, history, hop_index))) or {}
    return QuerySpec(
        nl_intent=focus or query,  # later-hop fallback = the bridge, not the raw question
        must_terms=_strlist(obj.get("must_terms")),
        should_terms=_strlist(obj.get("should_terms")),
        phrases=_strlist(obj.get("phrases")),
    )


# --------------------------------------------------------------------------
# context budget shared by decide_continue (the IRCoT reason_step op that also used
# it was deleted 2026-11-15, owner decision ENG-1321)
# --------------------------------------------------------------------------

_DECIDE_CONTEXT_MAXCHARS = 12000


# --------------------------------------------------------------------------
# decide_continue — the loop's stop/iterate decision (spec §2.2)
# --------------------------------------------------------------------------

_WORD_RE = re.compile(r"[0-9a-z]+")


def _fold_marks(text: str) -> str:
    """Strip combining marks so 'Krakow' and 'Kraków' compare equal.

    The grounding guard tokenizes with [0-9a-z]+, which splits an accented
    letter out of its word ('kraków' -> 'krak','w'); an LLM that returns the
    unaccented spelling of an entity copied from context was then rejected as
    ungrounded even though it answered correctly. Folding both sides before
    tokenizing removes the false rejection without weakening the guard.
    """
    return "".join(ch for ch in unicodedata.normalize("NFD", text) if not unicodedata.combining(ch))


def _word_set(text: str) -> set[str]:
    """Lowercased alphanumeric token set, for grounding/echo checks."""
    return set(_WORD_RE.findall(_fold_marks(text.lower())))


def _bridge_reject_reason(bridge: str, query: str, context: str) -> str:
    """Why a proposed bridge is rejected, or "" if it is valid. A bridge is a
    *newly discovered* intermediate entity we must look up; reject it (⇒
    stop_answer) when it fails to advance the loop:
      - ``empty``: no bridge proposed;
      - ``echo_question``: every word already appears in the QUESTION, so the
        model just restated a question entity;
      - ``ungrounded``: some bridge word appears nowhere in the retrieved CONTEXT.
    """
    b = _word_set(bridge)
    if not b:
        return "empty"
    if b <= _word_set(query):
        return "echo_question"
    if not (b <= _word_set(context)):
        return "ungrounded"
    return ""


def _valid_bridge(bridge: str, query: str, context: str) -> bool:
    return _bridge_reject_reason(bridge, query, context) == ""


def decide_continue(state: dict, llm: LLM) -> dict:
    """Decide whether to stop (answer is retrievable) or continue (look up a
    bridge entity next). ``state = {query, chunks: [text, ...], history}``.

    Fail-safe: any parse failure, a missing/empty bridge, or a bridge that
    merely echoes the question / isn't grounded in the context ⇒ ``stop_answer``
    (the loop stops rather than spin), so a bad LLM step degrades to the
    single-pass result already accumulated.
    """
    query = str(state.get("query", ""))
    history = state.get("history") if isinstance(state.get("history"), list) else []
    hist_str = "\n".join(f"- {h}" for h in history if str(h).strip()) or "(none yet; this is the first step)"
    context = build_context(state.get("chunks"), _DECIDE_CONTEXT_MAXCHARS)
    req = build_request("decide_continue", ("retrieval_decide_continue",),
                        "retrieval_decide_continue_input",
                        {"documents": context or "(nothing retrieved)", "resolved": hist_str,
                         "question": query})
    obj = decode(_call(llm, req)) or {}
    raw_decision = str(obj.get("decision", "")).strip() or "(none)"
    raw_bridge = str(obj.get("bridge", "")).strip()
    # Diagnostic fields (raw_*/reject_reason) let the kernel log distinguish
    # "the LLM chose stop" from "the guard rejected a proposed bridge".
    if raw_decision == "continue":
        reason = _bridge_reject_reason(raw_bridge, query, context)
        if reason == "":
            return {
                "decision": "continue", "bridge": raw_bridge,
                "raw_decision": raw_decision, "raw_bridge": raw_bridge, "reject_reason": "",
            }
    else:
        reason = "llm_stop"
    return {
        "decision": "stop_answer", "bridge": "",
        "raw_decision": raw_decision, "raw_bridge": raw_bridge, "reject_reason": reason,
    }


# --------------------------------------------------------------------------
# synthesize — the final typed three-way output (spec §2.5)
# --------------------------------------------------------------------------

_SYNTH_CONTEXT_MAXCHARS = 16000
_VALID_STATUS = ("answer", "abstention", "clarification")

# The abstention text. The kernel uses the same sentinel for its own abstentions
# (internal/memory/query.go AnswerWithCitations); keep them equal.
ABSTENTION_TEXT = "not found in memory"


def _typed(obj: dict | None) -> dict:
    """Read a synthesis reply into ``{status, text}``.

    An unreadable reply, or an "answer" with no text, is an explicit FAILURE,
    reported as an abstention carrying ``failure``: the kernel maps every status
    other than abstention/clarification to "answer", so the old default
    (``answer`` with ``""``) shipped an empty answer as though it were one. An
    abstention says what happened — nothing was answered — and keeps the
    kernel's "not found in memory" sentinel.
    """
    if obj is None:
        return {"status": "abstention", "text": ABSTENTION_TEXT, "failure": "unparseable_reply"}
    status = str(obj.get("status", "")).strip().lower()
    text = str(obj.get("text", "") or "").strip()
    if status == "abstention":
        return {"status": status, "text": text or ABSTENTION_TEXT}
    if status == "clarification" and text:
        return {"status": status, "text": text}
    if not text:
        return {"status": "abstention", "text": ABSTENTION_TEXT, "failure": "empty_answer"}
    # An answer — or an unknown status that still carries an answer text.
    return {"status": "answer", "text": text}


def synthesize_request(query: str, chunks: object, cited: bool) -> LLMRequest:
    if cited:
        docs = build_labeled_context(chunks, _SYNTH_CONTEXT_MAXCHARS)
        op_id, op = "retrieval_synthesize_cited", "synthesize_cited"
    else:
        docs = build_context(chunks, _SYNTH_CONTEXT_MAXCHARS)
        op_id, op = "retrieval_synthesize", "synthesize"
    # The shared guidance block comes first so both ops send one identical prefix.
    return build_request(op, ("retrieval_synthesis_guidance", op_id), "retrieval_synthesize_input",
                         {"documents": docs or "(nothing retrieved)", "question": query})


def synthesize(state: dict, llm: LLM) -> dict:
    """Produce the final typed three-way output for the loop's result.

    ``state = {query, chunks: [text, ...]}``. Returns
    ``{"status": "answer"|"abstention"|"clarification", "text": "..."}``, plus
    ``failure`` when the reply could not be read (see :func:`_typed`).
    """
    req = synthesize_request(str(state.get("query", "")), state.get("chunks"), cited=False)
    return _typed(decode(_call(llm, req)))


def synthesize_cited(state: dict, llm: LLM) -> dict:
    """Grounded synthesis with inline [n] citations (ADR-0081).

    ``state = {query, chunks: [text, ...]}`` — chunks are numbered [1..N] in order
    and the answer cites them inline. Same decision rules (one shared guidance
    block) and the same explicit failure as :func:`synthesize`.
    """
    req = synthesize_request(str(state.get("query", "")), state.get("chunks"), cited=True)
    return _typed(decode(_call(llm, req)))


# --------------------------------------------------------------------------
# up-front GROUNDED decomposition — plan the whole chain, then answer each
# sub-question from RETRIEVED memory (never parametric). Robust alternative to
# the greedy hop-by-hop bridge loop: one bad greedy hop derails the whole chain,
# whereas decomposing up front lets each sub-question be retrieved independently.
# --------------------------------------------------------------------------


def _subq_pairs(items: object, max_subqs: int) -> tuple[list[str], list[str]]:
    """Sub-question/ref pairs from a parsed list; tolerates bare strings and junk."""
    subqs: list[str] = []
    refs: list[str] = []
    if not isinstance(items, list):
        return subqs, refs
    for x in items:
        if isinstance(x, dict):
            sq = str(x.get("q", "") or x.get("question", "")).strip()
            rf = str(x.get("ref", "")).strip()
        elif isinstance(x, (str, int, float)):
            sq, rf = str(x).strip(), ""
        else:
            continue
        if sq:
            subqs.append(sq)
            refs.append(rf)
    return subqs[:max_subqs], refs[:max_subqs]


def decompose_request(query: str, routes: bool) -> LLMRequest:
    sys_id = "retrieval_decompose_routed" if routes else "retrieval_decompose"
    return build_request("decompose", (sys_id,), "retrieval_decompose_input", {"question": query})


def decompose(query: str, llm: LLM, max_subqs: int = 4) -> tuple[list[str], list[str]]:
    """Decompose a multi-step question into ordered sub-questions (with {n}
    placeholders for earlier answers) AND a parallel list of "refs" — noun phrases
    naming each sub-question's answer, used as a coherent fallback when a
    sub-answer can't be found. Fail-safe: on parse failure returns the raw
    question as a single sub-question with an empty ref (⇒ a single pass).

    The schema asks for ``{"sub_questions": [...]}``; an unchecked reply may still
    be the older bare list, which is accepted."""
    q = query.strip()
    if not q:
        return [], []
    reply = _call(llm, decompose_request(q, routes=False))
    obj = decode(reply, dict)
    if obj is not None and "sub_questions" in obj:
        items = obj.get("sub_questions")
    elif not reply.schema_checked:
        # An unchecked reply may be the older bare list (whose first item a dict
        # decode would otherwise mistake for the whole reply).
        items = _tolerant(reply.text, list)
    else:
        items = None
    subqs, refs = _subq_pairs(items, max_subqs)
    if not subqs:
        return [q], [""]
    return subqs, refs


def decompose_routed(query: str, llm: LLM, max_subqs: int = 4) -> dict:
    """ADR-0135 D1: one planning call returning ``{"route", "sub_questions", "refs"}``.

    Parsed defensively:
      * the expected object → its route (lower-cased, passed through even if
        unknown — the kernel decides what an unusable route means and logs it);
      * a bare JSON list (the legacy shape, unchecked replies only) → that plan
        with route "" (⇒ chain);
      * nothing parseable → route "" and the question as its single sub-question,
        the fail-safe (an echo plan ⇒ the kernel runs one pass).
    A "direct" route with no sub-questions carries the question itself as its one
    sub-question, so a kernel that does not execute routes sees an echo plan.
    """
    q = query.strip()
    if not q:
        return {"route": "", "sub_questions": [], "refs": []}
    reply = _call(llm, decompose_request(q, routes=True))
    route = ""
    subqs: list[str] = []
    refs: list[str] = []
    obj = decode(reply, dict)
    if obj is not None and ("route" in obj or "sub_questions" in obj):
        r = obj.get("route", "")
        route = r.strip().lower() if isinstance(r, str) else ""
        subqs, refs = _subq_pairs(obj.get("sub_questions"), max_subqs)
    elif not reply.schema_checked:
        subqs, refs = _subq_pairs(_tolerant(reply.text, list), max_subqs)
    if not subqs:
        subqs, refs = [q], [""]
    return {"route": route, "sub_questions": subqs, "refs": refs}


# Second-chance extraction (bridge-binding fix). Measured on a full multi-hop run:
# ~72% of misses at every hop count were chains that broke when the strict pass
# returned "" even though the gold evidence WAS retrieved — the strict pass
# under-commits, the descriptive-ref fallback then sends an unresolved referent
# phrase back into retrieval, and the chain drifts. The grounded-only rule is
# enforced by the mechanical word-set guard below, NOT by model caution, so a
# candidate-forcing retry cannot leak parametric knowledge: an ungrounded pick is
# still rejected and the ref fallback still applies.
_SUBQ_CONTEXT_MAXCHARS = 12000
_SUBQ_PASSES = ("retrieval_answer_subq", "retrieval_answer_subq_force")


def answer_subquestion(subq: str, chunks: object, llm: LLM) -> str:
    """Extract the GROUNDED answer to one sub-question from retrieved chunks.

    Grounded-only: the answer must be present in the documents — if the model
    returns something whose words are not in the retrieved text, we reject it
    (return "") rather than let parametric knowledge leak in. A strict pass runs
    first; when it yields nothing usable, a candidate-forcing second pass asks
    the model to commit to the best in-context entity (one extra LLM call, only
    on failed extractions). Empty answer ⇒ the caller substitutes the
    descriptive ref and the chain proceeds on the raw sub-question."""
    context = build_context(chunks, _SUBQ_CONTEXT_MAXCHARS)
    if not context:
        return ""
    ctx_words = _word_set(context)
    for sys_id in _SUBQ_PASSES:
        req = build_request("answer_subq", (sys_id,), "retrieval_answer_subq_input",
                            {"documents": context, "subquestion": subq})
        obj = decode(_call(llm, req)) or {}
        ans = str(obj.get("answer", "")).strip()
        # grounding guard: every answer word must appear in the retrieved context
        if ans and _word_set(ans) <= ctx_words:
            return ans
    return ""


def prompt_versions() -> dict[str, str]:
    """``{op: static hash}`` of every request shape this module sends."""
    return {
        "plan_step/first": plan_step_request("q").prompt_version,
        "plan_step/hop": plan_step_request("q", "x").prompt_version,
        "synthesize": synthesize_request("q", [], cited=False).prompt_version,
        "synthesize_cited": synthesize_request("q", [], cited=True).prompt_version,
        "decompose": decompose_request("q", routes=False).prompt_version,
        "decompose_routed": decompose_request("q", routes=True).prompt_version,
    }
