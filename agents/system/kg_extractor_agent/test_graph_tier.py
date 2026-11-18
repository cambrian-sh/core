"""The kg_extractor picks its graph-node tier from the value the kernel delivers.

execution.ingestion.kg_graph_tier reaches the agent as CAMBRIAN_KG_GRAPH_TIER in
the kernel's config-derived agent environment
(T-20261118-kg-extractor-graph-tier-cambrian-kg-graph-tier). The spaCy-backed
extractors are replaced by stubs so this runs without spaCy or its model; the
real tiers on the managed env are covered by the Go e2e
(app TestE2E_KgGraphTierFromConfigReachesSandboxedAgent).

Run through the capped runner (AGENTS.md section 4.7), from this directory:
    powershell -NoProfile -File <sdk>/scripts/pytest_capped.ps1 test_graph_tier.py -q
"""
from __future__ import annotations

import os
import sys
import types

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


class _StubPatterns:
    name = "spacy_patterns"


class _StubNER:
    name = "ner_coref"


def _stub(modname: str, attr: str, cls) -> None:
    mod = types.ModuleType(modname)
    setattr(mod, attr, cls)
    sys.modules[modname] = mod


@pytest.fixture(scope="module")
def agent_mod():
    # Stub the two spaCy tiers before agent.py imports them (it builds a
    # module-level agent at import, which would load the spaCy model).
    _stub("kg_extractors.spacy_pattern_extractor", "SpaCyPatternExtractor", _StubPatterns)
    _stub("kg_extractors.ner_entity_extractor", "NEREntityExtractor", _StubNER)
    saved = os.environ.pop("CAMBRIAN_KG_GRAPH_TIER", None)
    try:
        import agent  # noqa: PLC0415
        yield agent
    finally:
        if saved is not None:
            os.environ["CAMBRIAN_KG_GRAPH_TIER"] = saved


def test_unset_defaults_to_svo(agent_mod, monkeypatch):
    monkeypatch.delenv("CAMBRIAN_KG_GRAPH_TIER", raising=False)
    a = agent_mod.KgExtractorAgent(agent_id="kg_t1")
    assert a.graph_tier == "svo"
    assert a._graph_name == "spacy_patterns"
    assert isinstance(a._graph, _StubPatterns)


def test_delivered_ner_selects_ner_tier(agent_mod, monkeypatch):
    monkeypatch.setenv("CAMBRIAN_KG_GRAPH_TIER", "ner")
    a = agent_mod.KgExtractorAgent(agent_id="kg_t2")
    assert a.graph_tier == "ner"
    assert a._graph_name == "ner_coref"
    assert isinstance(a._graph, _StubNER)


def test_delivered_svo_and_empty_select_svo(agent_mod, monkeypatch):
    for value in ("svo", " SVO ", ""):
        monkeypatch.setenv("CAMBRIAN_KG_GRAPH_TIER", value)
        assert agent_mod.KgExtractorAgent(agent_id="kg_t3")._graph_name == "spacy_patterns"


def test_unknown_tier_refuses_boot(agent_mod, monkeypatch):
    monkeypatch.setenv("CAMBRIAN_KG_GRAPH_TIER", "llm")
    with pytest.raises(ValueError, match="CAMBRIAN_KG_GRAPH_TIER"):
        agent_mod.KgExtractorAgent(agent_id="kg_t4")


def test_graph_tier_from_env_reads_the_mapping(agent_mod):
    assert agent_mod.graph_tier_from_env({}) == "svo"
    assert agent_mod.graph_tier_from_env({"CAMBRIAN_KG_GRAPH_TIER": "ner"}) == "ner"
    with pytest.raises(ValueError):
        agent_mod.graph_tier_from_env({"CAMBRIAN_KG_GRAPH_TIER": "x"})
