"""Scout agent (CognitiveAgent) — ADR-0051 pre-plan discovery.

The system's pre-plan PERCEPTION step. Before the Planner commits a plan, the kernel
invokes Scout to OBSERVE the current state of the world the request depends on, so the
plan's shape and paths match reality instead of a guess.

Scout is a normal CognitiveAgent, so it gets the full ``run_think`` ReAct loop for free —
``find_tools`` (discover read tools across filesystem / web / endpoints / schemas / package
docs), ``tool_call`` (use them), ``memory_query`` (recall prior context), bounded by a tight
tool-round cap. It is confined to READ-ONLY discovery by its operator grant (the kernel's
``discovery-safe`` tool ceiling, ADR-0051 D6) — it cannot write or mutate. Its final answer
is a STRUCTURED report the kernel parses and feeds to the Planner as ``<DiscoveryLTM>``.

Reads only, never writes. Bounded. Emits a structured observation, not prose.
"""

from cambrian_agent_sdk import CognitiveAgent
from cambrian_agent_sdk.types import ProposalResponse
from cambrian_agent_sdk._logging import configure_logging

AGENT_DESCRIPTION = (
    "Pre-plan discovery: observes the current state of files, folders, endpoints, API "
    "schemas and package docs the request depends on, so the Planner grounds its plan in "
    "reality. Read-only."
)

AGENT_MANIFEST = '''
{
  "version": "1.0.0",
  "trait": "cognitive",
  "supported_formats": ["text"],
  "tools": ["discovery", "pre_plan_grounding", "observation"],
  "release_notes": "ADR-0051 pre-plan Scout — read-only world-state discovery.",
  "dependencies": []
}
'''


class ScoutAgent(CognitiveAgent):
    role = (
        "You are the Cambrian Scout, the system's pre-plan perception step. Before the "
        "Planner commits a plan, you OBSERVE the existing state the request depends on — "
        "files/folders, API endpoints and schemas, package/library docs — so the plan's "
        "shape and paths match reality instead of a guess. You are READ-ONLY: you only "
        "look, never create or modify anything."
    )
    output_schema = (
        "Your final answer MUST be ONLY this JSON object (the structured observation):\n"
        '{\n'
        '  "entities": [{"kind": "file|dir|api|url|package", "id": "<canonical id/path>", '
        '"exists": true|false, "summary": "<short factual gist: counts, names, shape>"}],\n'
        '  "interpretation": "<ONE sentence naming the pattern that matters for planning, '
        'or empty>",\n'
        '  "unobserved": ["<kind:id you could not reach within your budget>"]\n'
        "}\n"
        "Use [] / \"\" when there is nothing. State only what you actually observed — never "
        "invent files, endpoints, or contents you did not see."
    )
    constraints = (
        "OBSERVE when the plan's SHAPE or CORRECTNESS depends on what exists NOW: "
        "continuation/edit requests ('continue', 'the X folder', 'remaining', 'update', "
        "'fix', 'where we left off'); reading/analysing existing data; calling an existing "
        "API or library (observe its schema/docs so the plan uses the real signatures).",
        "A self-contained request that creates NEW content from scratch with no existing "
        "dependency needs NO observation — return empty entities. (Creating a brand-new "
        "file/folder does not require observing it; the runtime already provides WHERE.)",
        "You do not have every tool up front. Use find_tools to DISCOVER the read tool you "
        "need (describe it verb-first: 'list a directory', 'read a file', 'fetch an API "
        "schema', 'get a package's docs'), then call it. Never assume a tool is missing "
        "before find_tools.",
        "Use memory_query to recall what we already know about the referenced entities "
        "before scanning — if memory already holds it fresh, you may not need to look.",
        "Be FAST and BOUNDED: observe only what changes the plan, most-relevant first. Do "
        "not gather the task's CONTENT (that is the Planner/executor's job) — only its "
        "SHAPE and the existence/schema of what it touches.",
    )
    result_type = "discovery"
    max_tokens = 1024
    temperature = 0.3
    seed_recall = False  # Scout decides via memory_query when recall helps; no forced seed.

    def think(self, task, *, max_memory_queries: int = 2, max_tool_rounds: int = 3):
        """Tight bounds: discovery is a small constant number of observations (ADR-0051 D5)."""
        return super().think(task, max_memory_queries=max_memory_queries, max_tool_rounds=max_tool_rounds)

    def propose(self, request=None) -> ProposalResponse:
        # Scout is kernel-invoked directly (privileged organ), never auctioned — a low,
        # non-zero bid keeps it out of normal task selection.
        return ProposalResponse(confidence=0.0, rationale="Scout is invoked pre-plan, not auctioned.", estimated_latency_ms=4000)


agent = ScoutAgent(
    agent_id="scout_agent",
    version="1.0.0",
    description=AGENT_DESCRIPTION,
)


if __name__ == "__main__":
    configure_logging(agent_id="scout_agent")
    agent.serve()
