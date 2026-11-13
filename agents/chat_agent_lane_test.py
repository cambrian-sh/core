"""ADR-0150 D3: the chat agent in the single-agent lane (the merged conversation agent).

When the kernel stamps _lane=single_agent on the turn, the agent the person talks to
is the worker: it holds the person's permitted tools (the kernel menu, every call
through ExecuteTool with the turn's lease), is never offered delegate_to_planner,
renders the single-agent role and rules, and runs under the lane's stopping rule.
The planner lane's front desk is unchanged.

Named *_test.py, not test_*agent.py: the kernel's agent seeder registers every file
ending in "agent.py" as an agent.
"""
from __future__ import annotations

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import chat_agent as ca  # noqa: E402
from cambrian_agent_sdk import SubstrateClient  # noqa: E402
from cambrian_agent_sdk._proto import cambrian_pb2 as pb  # noqa: E402
from cambrian_agent_sdk.types import AgentTask  # noqa: E402

from chat_agent_test import _RpcError, _answer  # noqa: E402

import grpc  # noqa: E402

LANE = {"_lane": "single_agent", "_lane_max_tool_rounds": "500", "_lane_max_wall_seconds": "3600",
        "_lane_stuck_same_result": "4", "_lane_stuck_same_failure": "3"}


class _LaneStub:
    """A kernel with one granted tool, no native tool calling, scripted text replies."""

    def __init__(self, replies):
        self.replies = list(replies)
        self.stream_reqs = []
        self.tool_calls = []
        self.executes = 0

    def GenerateWithTools(self, req, timeout=None, metadata=None):
        raise _RpcError(grpc.StatusCode.FAILED_PRECONDITION)

    def GenerateViaModelStream(self, req, timeout=None, metadata=None):
        self.stream_reqs.append(req)
        yield pb.GenerateChunk(text=self.replies.pop(0), is_final=True, model="m",
                               usage=pb.GenerateUsage(input_tokens=1, output_tokens=1))

    def ListTools(self, req, metadata=None, timeout=None):
        return pb.ListToolsResponse(tools=[pb.ToolDescriptor(
            name="mcp:erp/invoices", description="List the invoices of a customer.",
            schema_json=json.dumps({"type": "object", "properties": {"customer": {"type": "string"}}}))])

    def ExecuteTool(self, req, timeout=None, metadata=None):
        self.tool_calls.append((req.tool_name, json.loads(req.args_json or "{}"), dict(metadata or [])))
        return pb.ExecuteToolResponse(result_json=json.dumps({"invoices": ["INV-7 120 EUR"]}))

    def Execute(self, req, timeout=None, metadata=None):  # delegate_to_planner's RPC
        self.executes += 1
        raise AssertionError("the single-agent lane must never call Execute")


def _agent(stub):
    a = ca.ChatAgent(agent_id="chat_test", version="1.0.0", description="t")
    a.substrate = SubstrateClient(stub=stub, agent_id="chat_test")
    a.memory = None
    return a


def _task(text, lane=True):
    return AgentTask(text=text, type="text", metadata={"language": "en"},
                     context=dict(LANE) if lane else {}, session_token_id="lease-1")


def _system_text(req) -> str:
    return "\n".join(b.text for b in req.system) if req.system else req.prompt


def _tool_call(name, args):
    return json.dumps({"action": "tool_call", "tool": name, "args": args})


def test_the_merged_agent_calls_the_persons_tools_itself():
    stub = _LaneStub([_tool_call("mcp:erp/invoices", {"customer": "ACME"}),
                      _answer("ACME has one open invoice: INV-7, 120 EUR.")])
    res = _agent(stub).run(_task("what does ACME owe?"))
    assert res.data.decode() == "ACME has one open invoice: INV-7, 120 EUR."
    assert len(stub.tool_calls) == 1 and stub.tool_calls[0][0] == "mcp:erp/invoices"
    assert stub.executes == 0, "no planner hand-off in the single-agent lane"


def test_delegate_to_planner_is_not_offered_and_the_lane_rules_are_rendered():
    stub = _LaneStub([_answer("hi!")])
    _agent(stub).run(_task("hello"))
    system = _system_text(stub.stream_reqs[0])
    assert "delegate_to_planner" not in system
    assert "mcp:erp/invoices" in system, "the person's tools are on the menu"
    assert "There is no planner and no one to hand the work to" in system
    assert "conversational front desk" not in system


def test_a_hidden_delegate_call_is_refused_without_running_a_plan():
    stub = _LaneStub([_tool_call("delegate_to_planner", {"task_description": "x"}),
                      _answer("done myself")])
    res = _agent(stub).run(_task("do it"))
    assert stub.executes == 0
    assert res.data.decode() == "done myself"


def test_the_planner_lane_front_desk_is_unchanged():
    stub = _LaneStub([_answer("hi!")])
    _agent(stub).run(_task("hello", lane=False))
    system = _system_text(stub.stream_reqs[0])
    assert "delegate_to_planner" in system
    assert "conversational front desk" in system
    assert "mcp:erp/invoices" not in system, "the front desk holds no system tools"


def test_lane_wall_cap_is_brought_inside_the_turn_deadline():
    t = AgentTask(text="x", context=dict(LANE))
    lim = ca._lane_limits_for_turn(t, time_left=100.0, reply_reserve_s=25.0)
    assert lim.max_wall_seconds == 75 and lim.max_tool_rounds == 500
    assert ca._lane_limits_for_turn(t, None, 25.0).max_wall_seconds == 3600
    assert ca._lane_limits_for_turn(AgentTask(text="x"), 100.0, 25.0) is None


def test_chat_agent_v7_kept_every_v6_section():
    v6 = ca.sections(ca._LIBRARY.get_version("chat_agent", 6))
    v7 = ca.sections(ca._LIBRARY.get_version("chat_agent", 7))
    for name, sec in v6.items():
        assert v7[name].body == sec.body, f"v7 changed the planner lane's section {name}"
    assert "role-single-agent" in v7 and "rules-single-agent" in v7
