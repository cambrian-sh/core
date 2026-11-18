package kernel

import (
	"fmt"

	"github.com/cambrian-sh/core/internal/config"
)

// AgentEnvExtras is the config-derived environment the kernel SETS on every
// agent it spawns (AgentManager.SetEnvExtras), as opposed to the operator's
// passthrough of the kernel's own environment: a knob must never depend on
// .env (owner rule 2026-08-11), and CAMBRIAN_* names never pass the ADR-0157
// agent env allowlist, so a kernel-to-agent setting travels only this way.
// Zero values ⇒ the agent's built-in default, no var set. NewMetabolismStack
// is the production caller; it is exported so an end-to-end test drives the
// same mapping a running kernel applies. One combined slice: SetEnvExtras
// REPLACES the list.
func AgentEnvExtras(cfg *config.Config) []string {
	var envExtras []string
	if k := cfg.Execution.Tools.ToolMenuK; k > 0 {
		envExtras = append(envExtras, fmt.Sprintf("CAMBRIAN_TOOL_MENU_K=%d", k))
	}
	if m := cfg.Execution.Retrieval.RerankerModel; m != "" {
		envExtras = append(envExtras, "RERANK_MODEL="+m)
	}
	// ADR-0053 D2 (revised): the kg_extractor agent's graph-node tier
	// (execution.ingestion.kg_graph_tier, agents/system/kg_extractor_agent),
	// T-20261118-kg-extractor-graph-tier-cambrian-kg-graph-tier.
	if tier := cfg.Execution.Ingestion.KgGraphTier; tier != "" {
		envExtras = append(envExtras, "CAMBRIAN_KG_GRAPH_TIER="+tier)
	}
	// ADR-0145 D10: agents trace their own model and tool calls into the kernel's
	// run-trace directory (sdk cambrian_agent_sdk/run_trace.py).
	envExtras = append(envExtras, traceEnv(cfg.Execution.Trace)...)
	// ADR-0145 D3: the SDK's evidence re-prompt (sdk react.py, EVIDENCE_REPROMPT_ENV),
	// and the composed runtime's per-step tool-call budget (execution.workers.max_tool_rounds).
	envExtras = append(envExtras, workersEnv(cfg.Execution.Workers)...)
	// execution.content: the SDK's tool-output cap, preview, resolve window and
	// prompt budget, so agent and kernel size content by one policy.
	envExtras = append(envExtras, contentEnv(cfg.Execution)...)
	// ADR-0157 S7: the shared model cache (HF_HOME, TORCH_HOME, DOCLING_CACHE_DIR),
	// with the sandbox on or off, so an organ's weights outlive its instance's
	// scratch (config.ModelCacheEnv; resolved by app.anchorAgentSandboxPaths).
	envExtras = append(envExtras, config.ModelCacheEnv(cfg.Execution.Agents.ModelCache)...)
	return envExtras
}
