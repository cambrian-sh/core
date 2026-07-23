"""Retrieval agent — the LLM-step logic of the agentic retrieval loop.

This module is the "thin stateless Python endpoint" from AGENTIC_RETRIEVAL_SPEC
§2.1: pure functions that take the loop state + an injected LLM callable and
return a decision. Go owns the loop + retrieval tiers and calls these per step.

Phase 2a scope: the **lexical query-planner** (`plan_step`) — the headline
"agent > fixed weights" lever (spec §2.2). Given the user's question, the
planner selects the MOST DISCRIMINATIVE lexical terms and phrases; the Go side
compiles the resulting :class:`QuerySpec` into a search string that BM25 /
``searchByType`` executes deterministically. `decide_continue` and `synthesize`
land in later phases; minimal forms are stubbed here so the op-router in
``agent.py`` has a stable surface.

The LLM is injected as ``LLM = Callable[[str], str]`` (temp=0 expected) so this
is unit-testable with a fake and backend-agnostic (CognitiveAgent.think, a
model agent, or a direct provider — resolved in ``agent.py``).
"""
from __future__ import annotations

import json
import re
from collections.abc import Callable
from dataclasses import dataclass, field

LLM = Callable[[str], str]

_FENCE_RE = re.compile(r"```(?:json)?\s*(.*?)\s*```", re.DOTALL)


def extract_json(text: str) -> dict | None:
    """Best-effort: pull the first JSON object out of an LLM completion."""
    candidates: list[str] = [m.group(1) for m in _FENCE_RE.finditer(text)]
    start, end = text.find("{"), text.rfind("}")
    if start != -1 and end > start:
        candidates.append(text[start : end + 1])
    for c in candidates:
        try:
            obj = json.loads(c)
        except json.JSONDecodeError:
            continue
        if isinstance(obj, dict):
            return obj
    return None


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
    """Join retrieved chunk texts into a context block for the LLM.

    Dedups by CONTENT first (the union-ingest of shared corpora — e.g. MuSiQue —
    surfaces the same paragraph under several ids, which interleaveDedup, keyed on
    doc id, does not collapse; without this the duplicates eat the char budget and
    push the real answer past the clip). Then truncates to ``max_chars`` — kept
    generous so a long answer paragraph is not cut off mid-sentence.
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

PLAN_STEP_PROMPT = """\
You are planning the FIRST retrieval step of a MULTI-HOP question. Such questions
describe their answer INDIRECTLY through nested intermediate entities — e.g.
"What league does the team that plays in Stadium X play for?" — where you must
FIRST find the inner entity (the team, anchored by "plays in Stadium X"); the
outer attribute (the league) is a LATER hop.

Identify the INNERMOST entity that can be looked up directly — the one anchored by
a concrete identifier (a proper noun, name, title, place, date). Build a query to
find THAT entity ONLY: use its distinctive identifying tokens, and DROP the outer
attributes/relations that depend on it (the final answer's property) plus all filler.

Example: "What league does the team that plays in Stadio Ciro Vigorito play for?"
  -> innermost entity = the team, anchored by "Stadio Ciro Vigorito"
  -> {{"phrases": ["Stadio Ciro Vigorito"], "must_terms": ["team"], "should_terms": []}}
     (DROP "league" / "play for" — that resolves on the NEXT hop.)

Return ONLY JSON, no prose:
{{
  "must_terms": ["<discriminative token>", ...],   // tight (1-4)
  "should_terms": ["<supporting token>", ...],      // optional
  "phrases": ["<exact distinctive anchor phrase>", ...]
}}

Question: {query}"""

# Hop >= 2: the loop carries the resolved-entity history + hop index so the planner
# can work out WHICH LINK of the chain this is and pick the relation for THAT link —
# not the question's final attribute (the bug that stalled 3-4 hop: applying
# "president" to an intermediate country instead of seeking the next country).
PLAN_STEP_HOP_PROMPT = """\
You are on hop {hop} of a multi-hop retrieval loop, resolving the QUESTION one link
at a time. A multi-hop question is a CHAIN of relations; you resolve them in order.

QUESTION:
  {query}
ENTITIES ALREADY RESOLVED (earlier links — do NOT look these up again):
{resolved}
The entity you just reached, to build on NOW, is:
  {focus}

Work out which link comes NEXT. Find the relation of "{focus}" that leads to the
NEXT entity in the chain — or, ONLY if every earlier link is resolved and this is
the FINAL link, the question's answer attribute. It is usually NOT the question's
final attribute yet. Example: for "the president of the OTHER country in the
Commission with [X]", once "{focus}" is X (a country), the next link is the OTHER
Commission country — NOT X's president.

Give TWO things:
- must_terms: the specific attribute/relation NOUN for THIS link — e.g. "commission
  partner", "other country", "capital", "league" — the one that ADVANCES the chain.
  Never the question's generic main verb ("participated", "is") or filler.
- should_terms: the question's ULTIMATE answer attribute — the single thing the WHOLE
  question ultimately asks for (e.g. "president", "birthplace", "abbreviation").
  ALWAYS include it: if "{focus}" turns out to BE the answer entity, this is what
  makes its answer paragraph rank. (When THIS link already is the final one,
  must_terms and should_terms may be the same word — that is fine.)

1-2 terms each; no other named entities from the question.

Return ONLY JSON:
{{
  "phrases": ["{focus}"],
  "must_terms": ["<relation that advances THIS link>"],
  "should_terms": ["<the question's ultimate answer attribute>"]
}}"""


def plan_step(query: str, llm: LLM, scratchpad: str = "",
              history: list[str] | None = None, hop_index: int = 0) -> QuerySpec:
    """Select discriminative lexical terms for this hop's search.

    Hop 1 (empty scratchpad) plans from the raw question. Later hops plan FOR the
    bridge entity in ``scratchpad``, now HOP-AWARE: ``history`` (entities resolved
    in earlier links) + ``hop_index`` let the planner work out which link of the
    chain this is and pick the relation for THAT link — instead of reusing the
    question's final attribute (the bug that stalled 3-4 hop). Robust to bad/empty
    LLM output: on parse failure the QuerySpec carries no terms, so
    :meth:`QuerySpec.to_lexical_query` falls back to the bridge (later hops) or the
    raw query (hop 1) — the loop never emits an empty search.
    """
    focus = scratchpad.strip()
    if focus:
        # history's last entry is `focus` itself (the loop records the bridge then
        # plans the next hop); show only the EARLIER links as "already resolved".
        hist = [str(h).strip() for h in (history or []) if str(h).strip() and str(h).strip() != focus]
        resolved = "\n".join(f"- {h}" for h in hist) or "(none yet)"
        prompt = PLAN_STEP_HOP_PROMPT.format(
            query=query, focus=focus, resolved=resolved, hop=hop_index,
        )
    else:
        prompt = PLAN_STEP_PROMPT.format(query=query)
    raw = llm(prompt)
    obj = extract_json(raw) or {}
    return QuerySpec(
        nl_intent=focus or query,  # later-hop fallback = the bridge, not the raw question
        must_terms=_strlist(obj.get("must_terms")),
        should_terms=_strlist(obj.get("should_terms")),
        phrases=_strlist(obj.get("phrases")),
    )


# --------------------------------------------------------------------------
# hyde — hypothetical document embedding (HyDE) for hop-1 dense retrieval
# --------------------------------------------------------------------------

HYDE_PROMPT = """\
Write a single short factual passage (1-3 sentences) that would DIRECTLY answer
the question below, worded as if copied from the reference document that answers
it — an encyclopedia article, a technical/support note, a policy, whatever the
question's domain implies. Use the domain's own terminology (product names, error
codes, field names, procedure steps) and invent plausible specifics if needed —
this text is only used to search for the real passage by similarity, so concrete,
domain-matched specifics help. No preamble, passage only.

Question: {query}"""

_HYDE_MAX_CHARS = 600


def hyde_passage(query: str, llm: LLM) -> str:
    """Generate a hypothetical answer passage to embed for dense retrieval
    (HyDE). Fail-safe: on empty/again-the-question output, returns "" so the
    caller falls back to embedding the real query (no regression)."""
    q = query.strip()
    if not q:
        return ""
    raw = (llm(HYDE_PROMPT.format(query=q)) or "").strip()
    if not raw or raw.lower() == q.lower():
        return ""
    return raw[:_HYDE_MAX_CHARS]


# --------------------------------------------------------------------------
# reason_step — IRCoT: generate the next chain-of-thought step, retrieve on it
# --------------------------------------------------------------------------

IRCOT_PROMPT = """\
You are answering a MULTI-HOP question by REASONING one step at a time, retrieving
evidence as you go. Given the QUESTION, the reasoning SO FAR, and the CONTEXT
retrieved, write the NEXT single reasoning sentence that makes concrete progress.

Rules:
- Prefer facts from the CONTEXT. You MAY also use well-known knowledge to NAME an
  intermediate entity the question implies but the context hasn't surfaced yet —
  e.g. "The team with the most World Series titles is the New York Yankees."
- If you can now state the FINAL answer, write it starting with "So the answer is"
  and set done=true.
- Otherwise set done=false and give a SHORT search query (a name + the one attribute
  you need next) to retrieve the next fact.

Return ONLY JSON:
{{"thought": "<one reasoning sentence>", "done": <true|false>, "search_query": "<what to retrieve next; empty if done>"}}

QUESTION: {query}

REASONING SO FAR:
{cot}

CONTEXT:
{context}"""


def reason_step(state: dict, llm: LLM) -> dict:
    """IRCoT step: emit the next CoT sentence (which may NAME an intermediate entity
    from reasoning, not just extract it from text), decide done, and give the next
    search query. Fail-safe: parse failure ⇒ done (stop), so the loop degrades to
    the chunks already accumulated."""
    query = str(state.get("query", ""))
    cot = state.get("cot") if isinstance(state.get("cot"), list) else []
    cot_str = "\n".join(f"- {t}" for t in cot if str(t).strip()) or "(nothing yet)"
    context = build_context(state.get("chunks"), _DECIDE_CONTEXT_MAXCHARS)
    raw = llm(IRCOT_PROMPT.format(query=query, cot=cot_str, context=context))
    obj = extract_json(raw) or {}
    thought = str(obj.get("thought", "")).strip()
    search_query = str(obj.get("search_query", "")).strip()
    done = bool(obj.get("done")) or thought.lower().startswith("so the answer is")
    if done:
        search_query = ""
    return {"thought": thought, "done": done, "search_query": search_query}


# --------------------------------------------------------------------------
# decide_continue — the loop's stop/iterate decision (spec §2.2)
# --------------------------------------------------------------------------

DECIDE_CONTINUE_PROMPT = """\
You are resolving a MULTI-HOP question step by step (like a ReAct loop). Multi-hop
questions name their answer INDIRECTLY through intermediate entities that must be
resolved IN ORDER — e.g. "the league of the team that plays in Stadium X" means:
first find the TEAM, then look up that team's LEAGUE.

ENTITIES ALREADY RESOLVED in earlier steps (do NOT look these up again):
{history}

Given the QUESTION and the CONTEXT gathered so far, output ONLY JSON:

1. If the CONTEXT explicitly STATES the FINAL answer to the whole QUESTION:
     {{"decision": "stop_answer"}}
2. Otherwise you are NOT done yet — CONTINUE. Using the question and what you have
   resolved so far, find the NEXT intermediate entity to look up. Give its EXACT
   name as revealed in the CONTEXT (no title, no description), and return:
     {{"decision": "continue", "bridge": "<exact entity name>"}}

BIAS STRONGLY TOWARD CONTINUE. Stop ONLY when a sentence in the CONTEXT literally
states the final answer to the WHOLE question. A CONTEXT that names an intermediate
entity (a team, a person, a place, a work, a system) but not the final answer means
you are NOT done — you MUST continue on that entity. Do not stop because the context
is long or noisy.

The bridge MUST be a CONCRETE NAMED entity (a proper noun) that appears in the
CONTEXT and is NOT already in the resolved list above. Never repeat a resolved
entity. If several new named entities appear, pick the one the QUESTION's next
unresolved step points to (the entity you now need in order to take the next hop).

QUESTION: {query}

CONTEXT:
{context}"""

_DECIDE_CONTEXT_MAXCHARS = 12000

_WORD_RE = re.compile(r"[0-9a-z]+")


def _word_set(text: str) -> set[str]:
    """Lowercased alphanumeric token set, for grounding/echo checks."""
    return set(_WORD_RE.findall(text.lower()))


def _bridge_reject_reason(bridge: str, query: str, context: str) -> str:
    """Why a proposed bridge is rejected, or "" if it is valid. A bridge is a
    *newly discovered* intermediate entity we must look up; reject it (⇒
    stop_answer) when it fails to advance the loop:
      - ``empty``: no bridge proposed;
      - ``echo_question``: every word already appears in the QUESTION, so the
        model just restated a question entity (the ``Timor Leste`` spin);
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
    bridge entity next). ``state = {query, chunks: [text, ...]}``.

    Fail-safe: any parse failure, a missing/empty bridge, or a bridge that
    merely echoes the question / isn't grounded in the context ⇒ ``stop_answer``
    (the loop stops rather than spin), so a bad LLM step degrades to the
    single-pass result already accumulated.
    """
    query = str(state.get("query", ""))
    history = state.get("history") if isinstance(state.get("history"), list) else []
    hist_str = "\n".join(f"- {h}" for h in history if str(h).strip()) or "(none yet — this is the first step)"
    context = build_context(state.get("chunks"), _DECIDE_CONTEXT_MAXCHARS)
    raw = llm(DECIDE_CONTINUE_PROMPT.format(query=query, history=hist_str, context=context))
    obj = extract_json(raw) or {}
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

SYNTHESIZE_PROMPT = """\
Given the QUESTION and the CONTEXT retrieved from memory, produce a TYPED answer.
Decide among three cases, in this order:

1. ANSWER — the CONTEXT contains the answer to the QUESTION. The "text" is the
   answer, grounded in the context: lead with the direct answer (the name, value,
   or yes/no), as concisely as the question allows — a short factual question wants
   a short span; a "how do I / what should I do" question may need the key step or
   two. Do NOT pad or restate the question. Respond:
     {{"status": "answer", "text": "<the answer, grounded in the context>"}}
2. ABSTENTION — the QUESTION is specific but the CONTEXT does NOT contain the
   answer; the information is simply not present. Do NOT guess. Respond:
     {{"status": "abstention", "text": "not found in memory"}}
3. CLARIFICATION — use this ONLY when the QUESTION points at its subject with a
   BARE GENERIC reference and gives NO description that could pin it down: "the
   manager", "the project", "the migration", "the ticket", "we", "last quarter",
   with no defining clause. Only the ASKER can resolve such a reference. Respond:
     {{"status": "clarification", "text": "which <thing> do you mean?"}}

CRITICAL — do NOT confuse a MULTI-HOP question with an ambiguous one. A question
that DESCRIBES its target through a relationship, a defining clause, or a chain of
them — "the team that plays in Stadio Ciro Vigorito", "the game system with a
3-letter abbreviation that features a game named after the league the Rams play
in", "the country whose military contains the Air Defense Artillery" — is FULLY
SPECIFIED. It is NOT ambiguous just because resolving it takes several steps. For
any described or multi-hop question you MUST return ANSWER (if the context has it)
or ABSTENTION (if it does not) — NEVER clarification.

Examples:
- "What did the manager approve?" (no which manager) -> clarification
- "What did Devon Park approve?" (context says he approved Aurora) -> answer: "Aurora"
- "What is the league of the team that plays in Stadio Ciro Vigorito?"
    -> answer if the context resolves it, else abstention; NEVER clarification
- "What were the Genesis's advantages over the 3-letter game system featuring a
    game named after the Rams' league?" -> answer or abstention; NEVER clarification
- "What did Bob Chen say about Project Zephyr?" (no Zephyr in context) -> abstention

Return ONLY JSON.

QUESTION: {query}

CONTEXT:
{context}"""

_SYNTH_CONTEXT_MAXCHARS = 16000
_VALID_STATUS = ("answer", "abstention", "clarification")


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


SYNTHESIZE_CITED_PROMPT = """\
Given the QUESTION and the numbered CONTEXT sources retrieved from memory, produce
a TYPED, CITED answer. Follow the SAME three-way decision as an uncited answer
(answer / abstention / clarification), with ONE addition: in the "answer" case,
cite EVERY grounded claim inline with the bracketed number(s) of the source(s) it
came from, placed at the END of the sentence or clause it supports.

Rules for citations:
- Cite ONLY from the numbered sources. A claim you cannot ground in a source must
  be dropped, not cited — never invent a citation or use outside knowledge.
- A sentence may cite multiple sources: "... within SLA [1][3]."
- Do not cite the abstention or clarification text.

  {{"status": "answer", "text": "<grounded answer with inline [n] citations>"}}
  {{"status": "abstention", "text": "not found in memory"}}
  {{"status": "clarification", "text": "which <thing> do you mean?"}}

A described or multi-hop question is FULLY SPECIFIED — return answer or abstention,
NEVER clarification (a bare generic reference with no defining clause is the only
clarification case).

Example:
QUESTION: Where did the little prince come from, and who did he meet?
CONTEXT:
[1] I have serious reason to believe that the planet from which the little prince
    came is the asteroid known as B612.
[2] "What is that big book?" said the little prince ... "I am a geographer," said
    the old gentleman.
-> {{"status": "answer", "text": "The little prince came from the asteroid B612 [1]. He met a geographer [2]."}}

Return ONLY JSON.

QUESTION: {query}

CONTEXT:
{context}"""


def synthesize_cited(state: dict, llm: LLM) -> dict:
    """Grounded synthesis with inline [n] citations (ADR-0081).

    ``state = {query, chunks: [text, ...]}`` — chunks are numbered [1..N] in order
    and the answer cites them inline. Same fail-safe as ``synthesize``: an unknown
    status defaults to ``answer``.
    """
    query = str(state.get("query", ""))
    context = build_labeled_context(state.get("chunks"), _SYNTH_CONTEXT_MAXCHARS) or "(nothing retrieved)"
    raw = llm(SYNTHESIZE_CITED_PROMPT.format(query=query, context=context))
    obj = extract_json(raw) or {}
    status = str(obj.get("status", "")).strip().lower()
    if status not in _VALID_STATUS:
        status = "answer"
    return {"status": status, "text": str(obj.get("text", ""))}


def synthesize(state: dict, llm: LLM) -> dict:
    """Produce the final typed three-way output for the loop's result.

    ``state = {query, chunks: [text, ...]}``. Returns
    ``{"status": "answer"|"abstention"|"clarification", "text": "..."}``.
    Fail-safe: any parse failure or an unknown status defaults to ``answer``
    (so a bad synthesis step never wrongly abstains on an answerable query and
    breaks the answer-category scoring).
    """
    query = str(state.get("query", ""))
    context = build_context(state.get("chunks"), _SYNTH_CONTEXT_MAXCHARS) or "(nothing retrieved)"
    raw = llm(SYNTHESIZE_PROMPT.format(query=query, context=context))
    obj = extract_json(raw) or {}
    status = str(obj.get("status", "")).strip().lower()
    if status not in _VALID_STATUS:
        status = "answer"
    return {"status": status, "text": str(obj.get("text", ""))}


# --------------------------------------------------------------------------
# up-front GROUNDED decomposition — plan the whole chain, then answer each
# sub-question from RETRIEVED memory (never parametric). Robust alternative to
# the greedy hop-by-hop bridge loop: one bad greedy hop derails the whole chain,
# whereas decomposing up front lets each sub-question be retrieved independently
# (the direct-retrieval probe showed each sub-question IS individually
# retrievable; the greedy loop just under-issued the right queries).
# --------------------------------------------------------------------------

DECOMPOSE_PROMPT = """\
Decompose a MULTI-HOP question into an ORDERED list of simple SUB-QUESTIONS, each
answerable by a SINGLE fact lookup. Later sub-questions refer to earlier ANSWERS
with placeholders {{1}}, {{2}}, ... (1-indexed by position). The LAST sub-question's
answer is the final answer to the whole question.

For EACH sub-question also give a short "ref": a NOUN PHRASE naming what its answer
IS, so that if the answer can't be found, later steps can still refer to it
descriptively (e.g. "the country that formed a Commission with {{1}}"). The ref may
use earlier placeholders too.

Example:
Q: "Who is the president of the other country that, with Kiwil's birth country,
    formed a Commission of Truth and Friendship?"
-> [{{"q": "What is Kiwil's birth country?", "ref": "Kiwil's birth country"}},
    {{"q": "Which other country formed a Commission of Truth and Friendship with {{1}}?",
      "ref": "the other country that formed a Commission of Truth and Friendship with {{1}}"}},
    {{"q": "Who is the president of {{2}}?", "ref": "the president of {{2}}"}}]

Rules:
- Each sub-question asks for exactly ONE entity/fact.
- Reference ONLY earlier answers via {{n}}; keep the original names/anchors.
- 1-4 sub-questions. A simple question may stay a single sub-question.
- Do NOT answer them. Return ONLY a JSON list of objects, each with "q" and "ref".

Question: {query}"""


def _extract_json_list(text: str) -> list | None:
    """Pull the first JSON array out of an LLM completion."""
    for c in [m.group(1) for m in _FENCE_RE.finditer(text)] + [text]:
        s, e = c.find("["), c.rfind("]")
        if s != -1 and e > s:
            try:
                v = json.loads(c[s : e + 1])
            except json.JSONDecodeError:
                continue
            if isinstance(v, list):
                return v
    return None


def decompose(query: str, llm: LLM, max_subqs: int = 4) -> tuple[list[str], list[str]]:
    """Decompose a multi-hop question into ordered sub-questions (with {n}
    placeholders for earlier answers) AND a parallel list of "refs" — noun phrases
    naming each sub-question's answer, used as a coherent fallback when a
    sub-answer can't be found (so the chain isn't corrupted by an empty
    substitution). Fail-safe: on parse failure returns the raw question as a single
    sub-question with an empty ref (⇒ degrades to a single pass)."""
    q = query.strip()
    if not q:
        return [], []
    raw = llm(DECOMPOSE_PROMPT.format(query=q))
    lst = _extract_json_list(raw) or []
    subqs: list[str] = []
    refs: list[str] = []
    for x in lst:
        if isinstance(x, dict):
            sq = str(x.get("q", "")).strip()
            rf = str(x.get("ref", "")).strip()
        else:  # tolerate a bare string (older/degenerate output)
            sq, rf = str(x).strip(), ""
        if sq:
            subqs.append(sq)
            refs.append(rf)
    if not subqs:
        return [q], [""]
    return subqs[:max_subqs], refs[:max_subqs]


ANSWER_SUBQ_PROMPT = """\
Answer the SUB-QUESTION using ONLY the CONTEXT retrieved from memory. Return the
single specific entity (a name, place, org, or date) that answers it, copied from
the CONTEXT. If the CONTEXT does not contain the answer, return "" — do NOT guess
or use outside knowledge.

Return ONLY JSON: {{"answer": "<entity copied from context, or empty>"}}

SUB-QUESTION: {subq}

CONTEXT:
{context}"""

_SUBQ_CONTEXT_MAXCHARS = 12000


def answer_subquestion(subq: str, chunks: object, llm: LLM) -> str:
    """Extract the GROUNDED answer to one sub-question from retrieved chunks.

    Grounded-only: the answer must be present in the CONTEXT — if the model
    returns something whose words are not in the retrieved text, we reject it
    (return "") rather than let parametric knowledge leak in. Empty answer ⇒ the
    caller substitutes nothing and the chain proceeds on the raw sub-question."""
    context = build_context(chunks, _SUBQ_CONTEXT_MAXCHARS)
    if not context:
        return ""
    raw = llm(ANSWER_SUBQ_PROMPT.format(subq=subq, context=context))
    obj = extract_json(raw) or {}
    ans = str(obj.get("answer", "")).strip()
    if not ans:
        return ""
    # grounding guard: every answer word must appear in the retrieved context
    if not (_word_set(ans) <= _word_set(context)):
        return ""
    return ans


# --------------------------------------------------------------------------
# extract_provenance — ingest-time schema-bounded metadata (spec §3)
# --------------------------------------------------------------------------

EXTRACT_PROVENANCE_PROMPT = """\
Extract provenance metadata from a memory item. Fill ONLY these fields from what
is stated in the TEXT (and the optional HINT); do NOT invent. Normalize any date
to an absolute ISO-8601 value (YYYY-MM-DD). If a field is not stated, use "" (or
[] for actors). Return ONLY JSON:
{{
  "valid_time": "<absolute ISO-8601 date the event happened, or ''>",
  "actors": ["<person/system named as doing the action>", ...],
  "source_type": "<slack_thread|design_doc|git_change|incident_report|support_ticket|system_event|email|note|'' >"
}}

TEXT:
{text}

HINT: {hint}"""

_KNOWN_SOURCE_TYPES = {
    "slack_thread", "design_doc", "git_change", "incident_report",
    "support_ticket", "system_event", "email", "note",
}


def extract_provenance(text: str, llm: LLM, hint: str = "") -> dict:
    """Per-memory, schema-bounded provenance extraction (spec §3).

    Returns ``{"valid_time": str, "actors": [str], "source_type": str,
    "origin": "inferred"}``. Schema-bounded (fills fixed fields, never invents
    content); fail-safe to empty fields on any parse failure. The Go ingest hook
    stamps these into the document metadata so actor/time become QUERYABLE (which
    is what filter-first enumeration needs), and tags them ``origin=inferred`` so
    an inferred fact is never trusted like a caller-given one.
    """
    raw = llm(EXTRACT_PROVENANCE_PROMPT.format(text=text[:2000], hint=hint or "(none)"))
    obj = extract_json(raw) or {}
    source_type = str(obj.get("source_type", "")).strip().lower()
    if source_type not in _KNOWN_SOURCE_TYPES:
        source_type = ""
    return {
        "valid_time": str(obj.get("valid_time", "")).strip(),
        "actors": [str(a).strip() for a in _strlist(obj.get("actors"))],
        "source_type": source_type,
        "origin": "inferred",
    }
