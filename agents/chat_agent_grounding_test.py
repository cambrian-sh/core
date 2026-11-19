"""ADR-0145 phase 3, the chat side of agents/chat_agent.py (agent:claude-p3chat).

D8 composer (grounded_reply), D3 delegation rule and ledger tools, D2 turn index and
summary, D5a ask-back, and the D8 enforced reply check's one retry. Every behaviour is
behind a turn key the kernel sends (internal/chat); with no key the turn is the
pre-phase-3 one, which chat_agent_test.py keeps covering.
"""
from __future__ import annotations

import html
import json
import os
import re
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import chat_agent as ca  # noqa: E402
from chat_agent_test import _Stub, _agent, _answer, _delegate, _task, _DelegatingSubstrate, _all_content  # noqa: E402
from cambrian_agent_sdk.execute_result import ExecuteResult  # noqa: E402

_FENCE_BLOCK = re.compile(r'<untrusted_data [^\n]*>\n(PAYLOAD_FENCE_[0-9a-f]+)\n("(?:[^"\\\n]|\\.)*")\n\1\n</untrusted_data>')


def _unfenced(text: str) -> str:
    """The text with every payload-fence block (ENG-94: tool output reaches the model
    fenced) replaced by its decoded payload, so the assertions read the tool result."""
    return _FENCE_BLOCK.sub(lambda m: json.loads(m.group(2)), text)


LED_AGING = "led_0f1e2d3c4b5a69788796a5b4c3d2e1f0"
LED_STEP = "led_aa11bb22cc33dd44ee55ff6600778899"


def _o3x_result(input_required=None):
    structured = {
        "version": 1, "run_id": "run-7", "report": "Three drafts saved.", "failed_step": -1, "ledger": True,
        "actions": [
            {"step": 2, "tool": "mail_draft", "status": "done", "ids": ["MSG-000173"]},
            {"step": 2, "tool": "mail_draft", "status": "done", "ids": ["MSG-000174"]},
        ],
        "steps": [{"step": 0, "status": "completed", "ref": LED_STEP, "preview": "aging report: 3 customers"}],
        "requirements": {"checklist": {"version": 1}, "items": [
            {"item": {"id": "R1", "type": "figure", "text": "State the total over 60 days.", "quote": "the total"},
             "status": "satisfied",
             "values": [{"name": "over_60_total", "type": "money", "value": 66483.75, "evidence_ref": LED_AGING}],
             "evidence": [LED_AGING]},
            {"item": {"id": "R2", "type": "answer", "text": "Name the approver.", "quote": "who approved"},
             "status": "not_found", "reason": "no record names an approver"},
        ], "settled": True},
    }
    if input_required:
        structured["input_required"] = input_required
    return ExecuteResult("Three drafts saved.", {"_execute_result": json.dumps(structured)})


# ── keys off: nothing new reaches the model ──────────────────────────────────


def test_without_the_phase3_keys_no_new_section_is_rendered():
    stub = _Stub([_answer("Hi!")])
    _agent(stub).run(_task("hi"))
    req = stub.stream_reqs[0]
    system, user = req.system[0].text, _all_content(req)
    for marker in ("cite the evidence it comes from", "never answered from general knowledge, even when",
                   "ledger_get", "The turn index in the task", "ask the user exactly those questions"):
        assert marker not in system, marker
    for tag in ("<conversation_summary>", "<turn_index>", "<pending_request>", "<evidence>"):
        assert tag not in user, tag


# ── D8 composer ──────────────────────────────────────────────────────────────


def test_grounded_reply_writes_over_the_evidence_with_ledger_ids_and_item_tags():
    stub = _Stub([_delegate("List the over-60 customers and the total"),
                  _answer(f"The total is £66,483.75 [{LED_AGING}] [R1]. No approver was found [R2].")])
    a = _agent(stub)
    a.substrate = _DelegatingSubstrate(a.substrate, _o3x_result())
    out = a.run(_task("Which customers are over 60 days and what is the total?", grounded_reply="true"))
    assert "[R1]" in out.data.decode()  # the kernel strips the markers, not the agent
    system = stub.stream_reqs[0].system[0].text
    assert "cite the evidence it comes from by its id in square brackets" in system
    # The SDK writes the tool result into the trajectory XML-escaped.
    after = _unfenced(html.unescape(_all_content(stub.stream_reqs[1])))
    assert "<evidence>" in after
    assert f"over_60_total = 66483.75 [{LED_AGING}]" in after
    assert "- [R2] answer not_found: Name the approver. (asked as \"who approved\") - no record names an approver" in after
    assert "- mail_draft #2 done ids=MSG-000174" in after
    assert f"- step 0 [{LED_STEP}]: aging report: 3 customers" in after
    assert "Planner report:\nThree drafts saved." in after


# ── D3: the delegation rule and the verbatim request ─────────────────────────


def test_every_delegation_carries_the_message_verbatim_under_the_delegation_rule():
    stub = _Stub([_delegate("look up the fee"), _delegate("look up the deadline"), _answer("Done.")])
    a = _agent(stub)
    sub = _DelegatingSubstrate(a.substrate, "ok")
    a.substrate = sub
    a.run(_task("Ruhsat harcı ne kadar ve son başvuru tarihi ne?", delegation_rule="true"))
    assert len(sub.executed) == 2
    for sent in sub.executed:
        assert "<user_request>\nRuhsat harcı ne kadar ve son başvuru tarihi ne?\n</user_request>" in sent
    system = stub.stream_reqs[0].system[0].text
    assert "never answered from general knowledge, even when you believe you know the answer" in system


def test_without_the_rule_only_the_first_delegation_carries_it():
    stub = _Stub([_delegate("look up the fee"), _delegate("look up the deadline"), _answer("Done.")])
    a = _agent(stub)
    sub = _DelegatingSubstrate(a.substrate, "ok")
    a.substrate = sub
    a.run(_task("What is the permit fee and the deadline?"))
    assert "<user_request>" in sub.executed[0] and "<user_request>" not in sub.executed[1]


def test_the_agent_points_the_sdk_evidence_reprompt_at_delegation():
    assert ca.ChatAgent.delegation_tool == "delegate_to_planner"


# ── D2/D3: ledger tools for follow-ups ───────────────────────────────────────


class _ToolSubstrate(_DelegatingSubstrate):
    def __init__(self, inner, result):
        super().__init__(inner, result)
        self.tool_calls = []

    def execute_tool(self, tool_name, args_json="", session_token_id="", **kw):
        self.tool_calls.append((tool_name, json.loads(args_json), session_token_id))
        return {"result_json": json.dumps({"id": LED_AGING, "body": {"over_60_total": 66483.75}}),
                "denied": False, "deny_reason": "", "error": ""}


def test_ledger_tools_are_offered_only_with_the_key_and_go_through_execute_tool():
    stub = _Stub([json.dumps({"action": "tool_call", "tool": "ledger_get", "args": {"id": LED_AGING}}),
                  _answer("The total was £66,483.75.")])
    a = _agent(stub)
    sub = _ToolSubstrate(a.substrate, "unused")
    a.substrate = sub
    out = a.run(_task("What was the total again?", ledger_tools="true"))
    assert out.data.decode() == "The total was £66,483.75."
    assert sub.tool_calls == [("ledger_get", {"id": LED_AGING}, "lease-1")]
    assert "read the recorded result with ledger_get" in stub.stream_reqs[0].system[0].text

    stub = _Stub([_answer("Hi!")])
    a = _agent(stub)
    a.run(_task("hi"))
    assert "ledger_get" not in a.tools and "ledger_find" not in a.tools
    assert "delegate_to_planner" in a.tools


# ── D2: summary and turn index reach the task as data ────────────────────────


def test_summary_and_turn_index_reach_the_task_and_resolve_the_second_draft():
    index = ("T1 (latest) turn m1: \"Draft reminders\"\n  run run-7\n"
             "    action mail_draft #1 done ids=MSG-000173\n    action mail_draft #2 done ids=MSG-000174")
    stub = _Stub([_delegate("Send draft MSG-000174 (the second mail_draft) as drafted"), _answer("Sent.")])
    a = _agent(stub)
    sub = _DelegatingSubstrate(a.substrate, "sent MSG-000174")
    a.substrate = sub
    a.run(_task("Send the second draft.", turn_index=index, conversation_summary="Drafts were prepared for overdue customers."))
    user = _all_content(stub.stream_reqs[0])
    assert user.index("<conversation_summary>") < user.index("<turn_index>") < user.index("<latest_message>")
    assert "action mail_draft #2 done ids=MSG-000174" in user
    assert "Resolve a reference such as \"the second draft\"" in stub.stream_reqs[0].system[0].text
    assert "MSG-000174" in sub.executed[0]


# ── D5a: ask-back ────────────────────────────────────────────────────────────


def test_input_required_ends_the_turn_with_the_questions_and_runs_no_second_plan():
    ir = {"version": 1, "run_id": "run-9", "round": 1, "stage": 1,
          "questions": [{"id": "Q1", "question": "Which month should the report cover?", "kind": "missing"}]}
    stub = _Stub([_delegate("prepare the report"), _delegate("prepare the report for October"),
                  _answer("Raporu hangi ay için hazırlayayım?")])
    a = _agent(stub)
    sub = _DelegatingSubstrate(a.substrate, _o3x_result(input_required=ir))
    a.substrate = sub
    out = a.run(_task("Raporu hazırla.", language="tr", ask_back="true"))
    assert out.data.decode() == "Raporu hangi ay için hazırlayayım?"
    assert len(sub.executed) == 1, "no plan runs after an ask-back in the same turn"
    after = _unfenced(html.unescape(_all_content(stub.stream_reqs[1])))
    assert "<questions>\n- Which month should the report cover?\n</questions>" in after
    assert "in your own words and in Turkish" in after
    assert "ask the user exactly those questions" in stub.stream_reqs[0].system[0].text


def test_a_pending_request_is_resumed_with_the_answer_verbatim():
    pending = "round 1, run run-9\nThe earlier request:\nRaporu hazırla.\nThe questions asked:\n- Q1 Which month?"
    stub = _Stub([_delegate("prepare the report for October"), _answer("Hazır.")])
    a = _agent(stub)
    sub = _DelegatingSubstrate(a.substrate, "done")
    a.substrate = sub
    a.run(_task("Ekim.", pending_input=pending, pending_request="Raporu hazırla.", ask_back="true"))
    assert "<pending_request>\n" + pending in _all_content(stub.stream_reqs[0])
    assert "<user_request>\nRaporu hazırla.\n\nEkim.\n</user_request>" in sub.executed[0]


# ── D8 enforce: the one retry ────────────────────────────────────────────────


class _OneCall:
    def __init__(self, text):
        self.text, self.calls = text, []

    def generate_request(self, token, **kw):
        self.calls.append(kw)
        return self.text


def test_the_reply_revision_is_one_call_over_the_kernels_evidence():
    stub = _Stub([])
    a = _agent(stub)
    one = _OneCall(f"The total is £66,483.75 [{LED_AGING}] [R1].")
    a.substrate = one
    rev = json.dumps({"draft": "The total is £70,000.", "uncited": ["£70,000"], "gaps": ['R2 "who approved" (not_found, gap_not_stated)']})
    out = a.run(_task("total?", reply_revision=rev, reply_evidence=f"- [R1] figure satisfied\n    over_60_total = 66483.75 [{LED_AGING}]"))
    assert out.data.decode() == f"The total is £66,483.75 [{LED_AGING}] [R1]."
    assert len(one.calls) == 1 and not stub.stream_reqs, "one plain call, no loop"
    content = one.calls[0]["messages"][0]["content"]
    assert "<draft_reply>\nThe total is £70,000.\n</draft_reply>" in content
    assert "Values no evidence of this turn backs: £70,000" in content
    assert f"over_60_total = 66483.75 [{LED_AGING}]" in content


# ── guards retired under the phase-3 mechanisms (AGENTS.md §4.3) ─────────────


def test_no_access_guard_is_not_run_when_the_kernel_enforces_the_reply_check():
    denial = "The tools available to me don't include direct access to your accounts receivable system."
    stub = _Stub([_delegate("List the user's own overdue receivables"), _answer(denial)])
    a = _agent(stub)
    a.substrate = _DelegatingSubstrate(a.substrate, "Overdue > 60 days: Acme 4,200 (75 days).")
    out = a.run(_task("which customers are overdue?", reply_check="enforce"))
    assert out.data.decode() == denial  # the kernel's check retries it, in any language
    assert len(stub.stream_reqs) == 2


def test_narration_guard_is_not_run_under_the_sdk_evidence_reprompt(monkeypatch):
    monkeypatch.setattr(ca, "_evidence_reprompt_on", lambda: True)
    stub = _Stub([_answer("I'll delegate this to the planner now.")])
    a = _agent(stub)
    a.run(_task("create the report file"))
    assert len(stub.stream_reqs) == 1, "the force-delegate retry is the SDK's job then"


# ── D3: the SDK evidence re-prompt gets the delegation tool and the turn's evidence ──
# (T-20261108-chat-agent-pass-delegation-tool-and-turn)


def _capture_run_think(monkeypatch):
    import cambrian_agent_sdk.react as react
    seen = []
    real = react.run_think

    def spy(agent, task, **kw):
        seen.append(kw)
        return real(agent, task, **kw)

    monkeypatch.setattr(react, "run_think", spy)
    return seen


def test_run_think_gets_delegation_tool_and_no_turn_evidence_on_a_bare_turn(monkeypatch):
    assert ca._RUN_THINK_D3, "the workspace SDK has the D3 keywords"
    seen = _capture_run_think(monkeypatch)
    _agent(_Stub([_answer("Hi!")])).run(_task("hi"))
    assert seen[0]["delegation_tool"] == "delegate_to_planner"
    assert seen[0]["turn_evidence"] is False


def test_recalled_memory_or_a_turn_index_counts_as_turn_evidence(monkeypatch):
    from chat_agent_test import _Memory
    seen = _capture_run_think(monkeypatch)
    _agent(_Stub([_answer("ok")]), _Memory([{"text": "fee is 120 TL"}])).run(
        _task("what is the fee?", recall_lookup="true"))
    _agent(_Stub([_answer("ok")])).run(_task("send the second draft", turn_index="T1 (latest) ..."))
    assert seen[0]["turn_evidence"] is True and seen[1]["turn_evidence"] is True
