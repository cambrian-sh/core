package awareness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// ── Planner prompt constants ─────────────────────────────────────────────────
// Only the static portions are hashed (plannerPromptHash). Dynamic sections
// (capability clusters, model list, LTM blocks, user input) are injected at
// runtime and excluded from the hash so it remains stable across requests.

const plannerRole = "You are the Cambrian Planner. Your goal: resolve requests in MINIMAL steps."

// plannerLTMRules explains the memory sections. The <DiscoveryLTM> and
// <environment> rules were removed 2026-08-08: the Scout was their only producer
// and it was retired, so they described sections that could never appear. The
// host-path rule that had been living inside that dead branch now has two
// halves — a static division-of-labour rule in plannerDecisionRules, and the
// dynamic <host> block built by buildHostBlock. See ADR-0100's P3 residuals.
const plannerLTMRules = `LTM CONTEXT RULES (applies when <FactLTM>, <PlanLTM>, or <NegativeLTM> sections appear in <Context>):
- <FactLTM> contains facts from prior sessions retrieved for relevance to this request. Use facts with high relevance scores to enrich your plan steps where applicable. Ignore facts that are not relevant — do NOT invent steps just to use a fact.
- <PlanLTM> contains a prior successful plan for a similar request. Use it as a structural reference but adapt it to the current request; do not copy it blindly.
- <NegativeLTM> contains failure records from prior sessions. Avoid assigning the same task to the agent that previously failed it.`

const plannerDecisionRules = `STRICT DECISION RULES:
- The "query" field MUST contain the full natural-language instructions for the step. NEVER truncate the user's intent; include the complete action required.
- Describe what the step needs in natural language. The runtime will discover the right agent automatically.
- If the user provides explicit answers, construct steps effectively without redundant actions.
- IMPORTANT: The example "uppercase the Name column in data.csv" is ONLY a format example. NEVER use it as the actual task unless the user explicitly requests it.
- Describe steps by CAPABILITY, not by agent. The runtime resolves each step to a concrete agent at execution time (discovery + selection); do not assume a particular selection mechanism. If the USER named an agent, express that with "preferred_agent" (see AGENT PINNING) — never by naming an agent inside "query" alone, and never as a capability tag.
- When a step requires analysis, comparison, evaluation, or justification, start the query with verbs like "Analyse...", "Compare...", or "Evaluate...". Do NOT start analysis steps with "Summarise..." — that will route the task to the wrong agent.
- NEVER set "is_thought": true for steps that require analysis, comparison, evaluation, code generation, or summarisation. These MUST be routed to the corresponding cognitive agent. Only use "is_thought": true for trivial synthesis or routing decisions that do not require domain expertise.
- FILE AND FOLDER PATHS ARE THE EXECUTOR'S JOB, NOT YOURS. Describe the TARGET in natural language — "a folder named Reports on the desktop", "the project's README" — and let the step's agent resolve it. That agent runs ON the host and is given its absolute paths; you are not. So do NOT construct absolute paths, do NOT guess a home directory or a user name, and do NOT write "~" or "~/Desktop" (which the shell does not expand on every platform). The <host> section tells you the OS so you can phrase a step in terms that platform understands; it is not an invitation to build paths.`

// plannerAgentPinRules is the agent-pinning instruction block, shared by both
// planner arms. It gives the planner a legitimate field for naming an agent: the
// prompt previously invited "you may reference a specific agent ID" with nowhere
// to put it, so the name leaked into required_capabilities and, once L1 actually
// enforced the capability contract, filtered every candidate and killed the step.
const plannerAgentPinRules = `AGENT PINNING:
- Default: do NOT name an agent. Leave "preferred_agent" absent and let the runtime select.
- ONLY when the USER's request itself names an agent, set "preferred_agent" to that exact agent ID from the CAPABILITY CLUSTERS list.
- Set "agent_pin":"hard" when the user is directing ("use terminal_agent to ...", "run this with X"): the named agent is bound and the step FAILS if it is unavailable.
- Set "agent_pin":"soft" when the agent is a suggestion or your own inference ("probably the research agent"): the named agent is prioritised but the runtime may still choose better.
- NEVER put an agent ID in "required_capabilities" — those are capability tags only, and an agent ID there matches nothing and kills the step.

`

const plannerDependencyRules = `DEPENDENCY RULES:
- Each step has a "depends_on" field: a list of zero-based indices of steps that MUST complete before this step can run.
- Root steps with no prerequisites MUST have "depends_on": [].
- Independent steps MUST have empty depends_on so the runtime can execute them in parallel.
- NEVER reference an index that does not exist in the steps array.
- NEVER create a cycle (e.g. step 0 depending on step 1 which depends on step 0).`

// plannerUsageRules contains usage examples that shape reasoning (thought steps,
// checkpoint steps, structured reasoning). These belong in <Constraints>, not
// <OutputSchema>, because they describe HOW to reason, not what to output.
const plannerUsageRules = `THOUGHT STEPS:
- If a step only requires reasoning, synthesis, or planning based on previous steps (no external action needed), set "is_thought": true.

CHECKPOINT STEPS:
- After any step whose output gates irreversible or costly downstream work, set "checkpoint_after": true.
- Optionally supply "checkpoint_query" with a specific coherence question for that step. If omitted, the runtime generates a default template.
- Typical triggers: file writes, external API calls, format-transforming steps, any step that feeds 3 or more dependent steps.
- Example: {"query": "Convert CSV to JSON schema", "depends_on": [0], "checkpoint_after": true, "checkpoint_query": "Is the output valid JSON schema compatible with the downstream validator?"}

STRUCTURED REASONING (OPTIONAL):
- You may emit <thought>...</thought> blocks BEFORE the JSON plan to reason through the problem.
- Example: <thought>The user wants X. I should use agent Y for step 1...</thought>{"steps":[...]}
- The Substrate will extract and discard thought blocks; only the JSON plan is processed.
- Never include thought blocks INSIDE the JSON structure.

FAN-OUT (PARAMETRIC) STEPS:
- When a step must run ONCE PER ITEM of a set whose SIZE is only known after an earlier step observes it (e.g. "for each missing section, write it" after a step scans the folder), emit ONE parametric step — do NOT guess the count and do NOT enumerate the items yourself.
- Set "fan_out_over" to the zero-based index of the earlier step whose output is that set, and write the query with a "{item}" placeholder (or set "fan_out_var":"x" to use "{x}"). The runtime expands it into one concrete step per discovered item at execution time.
- A step that must run AFTER all items (a summary/reduce) should "depends_on" the parametric step's index.
- Example: {"query":"write the file for {item}","depends_on":[0],"fan_out_over":0}`

// plannerCapabilityRules is the ROUTE-03 capability-contract instruction block,
// injected into <Constraints> ONLY under the capability_contract arm. It tells
// the planner to tag each step with the capability strings its executor must
// declare, drawn from the live CAPABILITY CLUSTERS vocabulary (no invented tags).
const plannerCapabilityRules = `CAPABILITY REQUIREMENTS:
- For each step, add a "required_capabilities" array: the capability tags the executing agent MUST declare. Choose ONLY from the tags shown in CAPABILITY CLUSTERS above (the label before each ":"), using the exact tag strings.
- Pick the minimal set a correct executor needs (usually 1-2 tags). For a pure reasoning/thought step that needs no special capability, use an empty array [].
- NEVER invent a capability tag that is not listed in CAPABILITY CLUSTERS. If none fits, use [].`

// PlanOutputSchema is the shared JSON Schema + format example for plans produced
// by both the Planner and the ReplanHandler. Shared so both registry entries
// reference an identical contract.
const PlanOutputSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {
    "steps": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "query":            { "type": "string", "minLength": 1 },
          "depends_on":       { "type": "array", "items": { "type": "integer" } },
          "is_thought":       { "type": "boolean" },
          "checkpoint_after": { "type": "boolean" },
          "checkpoint_query": { "type": "string" },
          "fan_out_over":     { "type": "integer" },
          "fan_out_var":      { "type": "string" },
          "preferred_agent":  { "type": "string" },
          "agent_pin":        { "type": "string", "enum": ["soft", "hard"] }
        },
        "required": ["query", "depends_on"]
      }
    },
    "subject":      { "type": "string" },
    "cache_policy": { "type": "string" }
  },
  "required": ["steps", "subject"]
}

Set cache_policy based on the dominant capability of the request:
- "codegen" — when the plan involves writing, generating, or refactoring code
- "cognitive" — when the plan involves analysis, summarisation, comparison, or reasoning
- "tool" — when the plan involves file reads, data transforms, or deterministic operations
- "research" — when the plan involves web search, paper reading, or information gathering
- "default" — when none of the above clearly applies

Example:
{"steps":[{"query":"full natural-language instruction","depends_on":[]},{"query":"Synthesize results from step 0","depends_on":[0],"is_thought":true}],"subject":"The primary entity or goal","cache_policy":"cognitive"}`

// planOutputSchema is an alias used inside this package.
const planOutputSchema = PlanOutputSchema

// planOutputSchemaCap is the ROUTE-03 variant: identical to PlanOutputSchema but
// with the per-step "required_capabilities" array in the contract + example.
// Used only under the capability_contract arm.
const planOutputSchemaCap = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {
    "steps": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "query":                 { "type": "string", "minLength": 1 },
          "depends_on":            { "type": "array", "items": { "type": "integer" } },
          "required_capabilities": { "type": "array", "items": { "type": "string" } },
          "is_thought":            { "type": "boolean" },
          "checkpoint_after":      { "type": "boolean" },
          "checkpoint_query":      { "type": "string" },
          "fan_out_over":          { "type": "integer" },
          "fan_out_var":           { "type": "string" },
          "preferred_agent":       { "type": "string" },
          "agent_pin":             { "type": "string", "enum": ["soft", "hard"] }
        },
        "required": ["query", "depends_on", "required_capabilities"]
      }
    },
    "subject":      { "type": "string" },
    "cache_policy": { "type": "string" }
  },
  "required": ["steps", "subject"]
}

Set cache_policy based on the dominant capability of the request:
- "codegen" — when the plan involves writing, generating, or refactoring code
- "cognitive" — when the plan involves analysis, summarisation, comparison, or reasoning
- "tool" — when the plan involves file reads, data transforms, or deterministic operations
- "research" — when the plan involves web search, paper reading, or information gathering
- "default" — when none of the above clearly applies

Example:
{"steps":[{"query":"full natural-language instruction","depends_on":[],"required_capabilities":["file_read"]},{"query":"Synthesize results from step 0","depends_on":[0],"required_capabilities":[],"is_thought":true}],"subject":"The primary entity or goal","cache_policy":"cognitive"}`

// plannerStaticTextCap / plannerPromptHashCap are the capability_contract-arm
// counterparts of plannerStaticText / plannerPromptHash. The added capability
// rules + schema field change the hash, so PlanEvent provenance distinguishes the
// two arms for free.
const plannerStaticTextCap = plannerRole + plannerLTMRules + plannerDecisionRules + plannerCapabilityRules + plannerAgentPinRules + plannerDependencyRules + plannerUsageRules + planOutputSchemaCap

var plannerPromptHashCap = domain.PromptHashOf(plannerStaticTextCap)

// plannerStaticText is the concatenation of all static planner prompt parts.
// Only this text is hashed — dynamic injections are excluded so the hash
// remains stable across requests.
const plannerStaticText = plannerRole + plannerLTMRules + plannerDecisionRules + plannerAgentPinRules + plannerDependencyRules + plannerUsageRules + planOutputSchema

// plannerPromptHash is the 8-char SHA-256 of the planner's static prompt text.
// Written to ExecutionPlan.PlannerPromptVersion and forwarded to PlanEvent.
var plannerPromptHash = domain.PromptHashOf(plannerStaticText)

func init() {
	domain.PromptRegistry[plannerPromptHash] = domain.PromptEntry{
		ID:      "planner.plan",
		Version: "1.0.0",
		Hash:    plannerPromptHash,
		Schema:  PlanOutputSchema,
	}
	// ROUTE-03 capability_contract arm variant.
	domain.PromptRegistry[plannerPromptHashCap] = domain.PromptEntry{
		ID:      "planner.plan.capability_contract",
		Version: "1.0.0",
		Hash:    plannerPromptHashCap,
		Schema:  planOutputSchemaCap,
	}
}

// ── End planner prompt constants ─────────────────────────────────────────────

// Generator is the consumer-side interface for LLM text generation.
// LLMClient satisfies this interface; tests can inject a fake.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// AgentProvider is the interface for fetching agents during planning.
type AgentProvider interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// TokenUsageAdvisor provides adaptive MaxEnergy recommendations for step types
// based on observed token utilisation patterns. nil = no adaptation.
type TokenUsageAdvisor interface {
	GetAdaptiveMaxEnergy(stepType string, currentLimit int) int
}

// Planner builds an ExecutionPlan by calling the LLM with a structured system
// prompt. If a ProceduralMemory is wired in, it attempts to retrieve a prior
// successful plan template and injects it into the prompt before the user
// request so the Cortex can use it as procedural context.
type Planner struct {
	client         Generator
	provider       AgentProvider
	hippocampus    domain.ProceduralMemory // nil means no procedural memory
	policyProvider domain.PolicyProvider   // ADR-0027: nil skips cache_policy validation
	WorkspaceStage domain.WorkspaceStage   // ADR-0016: may be nil; nil disables enrichment
	advisor        TokenUsageAdvisor       // nil means no adaptive energy tuning
	spcAlarm       *SPCAlarm               // nil means no PLAN_BUDGET_INSUFFICIENT alarm
	// capabilityContract enables the ROUTE-03 arm: the planner emits per-step
	// required_capabilities (extended prompt + schema, distinct prompt hash).
	// Default false ⇒ byte-identical to the pre-ROUTE-03 planner.
	capabilityContract bool
	// canonicalVocab (ROUTE-04 / ADR-0067) normalizes the displayed capability
	// vocabulary deterministically (format/typo folding). Default false.
	canonicalVocab bool
	// goos is the OS the kernel runs on, shown to the planner in the dynamic
	// <host> block. A field rather than a direct runtime.GOOS read so a test can
	// pin it; EMPTY means "resolve the real host at prompt-build time", so a
	// Planner built as a bare struct literal still reports truthfully instead of
	// silently claiming no OS.
	goos string
}

// hostGOOS is the OS the planner should be told about. Never hardcoded: it is
// the live host unless a test pinned it.
func (p *Planner) hostGOOS() string {
	if p.goos != "" {
		return p.goos
	}
	return runtime.GOOS
}

// NewPlanner creates a Planner. Pass nil for hippocampus to disable procedural
// memory injection; existing callers are unaffected.
func NewPlanner(client Generator, provider AgentProvider, hippocampus domain.ProceduralMemory) *Planner {
	return &Planner{client: client, provider: provider, hippocampus: hippocampus}
}

// SetPolicyProvider wires a PolicyProvider for cache_policy validation (ADR-0027).
// Unknown policy names emitted by the LLM are normalised to "" so the Hippocampus
// falls back to its default policy at retrieval time.
func (p *Planner) SetPolicyProvider(pp domain.PolicyProvider) {
	p.policyProvider = pp
}

// SetAdvisor wires an adaptive token usage advisor (nil clears it).
func (p *Planner) SetAdvisor(a TokenUsageAdvisor) {
	p.advisor = a
}

// SetSPCAlarm wires a PLAN_BUDGET_INSUFFICIENT alarm (nil clears it).
func (p *Planner) SetSPCAlarm(a *SPCAlarm) {
	p.spcAlarm = a
}

// SetCapabilityContract toggles the ROUTE-03 capability contract (arm toggle;
// wired from execution.capability_contract). When true the planner emits per-step
// required_capabilities and stamps the capability-arm prompt hash.
func (p *Planner) SetCapabilityContract(on bool) {
	p.capabilityContract = on
}

// SetCanonicalVocab toggles ROUTE-04 / ADR-0067 deterministic capability normalization
// in the planner's displayed vocabulary (wired from execution.canonical_vocab), so
// format-variant declared caps group under one normalized tag the planner emits.
func (p *Planner) SetCanonicalVocab(on bool) {
	p.canonicalVocab = on
}

// Generate delegates to the underlying LLM client.
func (p *Planner) Generate(ctx context.Context, prompt string) (string, error) {
	return p.client.Generate(ctx, prompt)
}

func (p *Planner) GetExecutionPlan(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
	// Fast-path for explicit JIT reasoning signals (Issue #032 / #033)
	// If the request is a direct call to the synthesis engine, bypass the LLM
	// planner and return a single-step Thought Plan.
	if strings.Contains(userInput, "[SYSTEM_REASONING_SIGNAL: JIT_LOGIC_SYNTHESIS]") {
		slog.Info("🧠 JIT reasoning signal detected, bypassing LLM planner for Thought Step")
		return &domain.ExecutionPlan{
			Subject: "JIT Logic Synthesis",
			Steps: []domain.Step{
				{
					Query:     userInput,
					IsThought: true,
				},
			},
		}, nil
	}

	agents, err := p.provider.GetAllAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch agent list: %v", err)
	}

	// ADR-0016: Enrich Planner with cross-session LTM facts.
	// ADR-0025: Retrieve typed LTM enrichment and prior plan for XML-tag injection.
	var ltmEnrichment domain.LTMEnrichment
	if p.WorkspaceStage != nil {
		if enriched, err := p.WorkspaceStage.PrimeForPlanning(ctx, userInput); err == nil {
			ltmEnrichment = enriched
		}
	}

	// Retrieve prior successful plan from Hippocampus (procedural memory).
	var planEntry *domain.PlanLTMEntry
	if p.hippocampus != nil {
		priorPlan, similarity, conf, _ := p.hippocampus.Retrieve(ctx, userInput)
		if priorPlan != nil {
			// REQ-CACHE-1: exact-match fast-path — bypass LLM entirely when
			// similarity and confidence are extremely high and the prompt version
			// matches (ensuring the agent pool and ruleset are identical).
			if similarity >= 0.95 && conf >= 0.90 && priorPlan.PlannerPromptVersion == plannerPromptHash {
				slog.Info("planner_exact_match_cache_hit", "similarity", similarity,
					"confidence", conf, "prompt_version", priorPlan.PlannerPromptVersion)
				return priorPlan.Clone(), nil
			}
			priorJSON, err := json.Marshal(priorPlan)
			if err == nil {
				planEntry = &domain.PlanLTMEntry{
					PlanJSON:   string(priorJSON),
					Confidence: conf,
					Similarity: similarity,
				}
			}
		}
	}

	// PLANNERREQ: canonical 4-section prompt via domain.PromptBuild.
	// Dynamic sections (capability clusters, model list) are constraint strings;
	// static constraint groups are constants hashed into plannerPromptHash.
	// LTM enrichment and user request are context/task — excluded from the hash.
	ltmBlock := buildLTMBlock(planEntry, ltmEnrichment)
	// ROUTE-03: under the capability_contract arm, inject the capability rules
	// (after the decision rules) and the capability-schema variant. When off, the
	// section list, schema, and prompt hash are exactly the pre-ROUTE-03 values,
	// so the control arm is byte-identical.
	constraints := []string{
		buildCapabilityCluster(agents), // dynamic — excluded from hash
		buildHostBlock(p.hostGOOS()),   // dynamic — per-host, MUST stay out of the hash
		plannerLTMRules,
		plannerDecisionRules,
		plannerDependencyRules,
		plannerUsageRules,
	}
	schema := planOutputSchema
	promptVersion := plannerPromptHash
	// The capability vocabulary the planner was actually shown. nil when the contract
	// arm is off, which disables the guard below along with the feature itself.
	var capVocab map[string]struct{}
	if p.capabilityContract {
		clusterBlock, vocab := buildCapabilityClusterFromManifests(ctx, agents, p.provider, p.canonicalVocab)
		capVocab = vocab
		constraints = []string{
			// manifest-derived vocabulary so the emitted required_capabilities
			// match what L1 Declaration enforces (NOT the clusterer's labels).
			clusterBlock,
			buildHostBlock(p.hostGOOS()), // dynamic — per-host, MUST stay out of the hash
			plannerLTMRules,
			plannerDecisionRules,
			plannerCapabilityRules,
			plannerDependencyRules,
			plannerUsageRules,
		}
		schema = planOutputSchemaCap
		promptVersion = plannerPromptHashCap
	}
	fullPrompt := domain.PromptBuild(
		domain.PromptSystem(plannerRole, constraints...),
		domain.PromptContext(ltmBlock),
		domain.PromptTask("User Request: "+userInput),
		domain.PromptOutputSchemaJSON(schema),
	)
	slog.Debug("Sending full prompt to LLM", "full_prompt", fullPrompt)

	responseStr, err := p.client.Generate(ctx, fullPrompt)
	if err != nil {
		// Log it HERE. A bare `return nil, err` made a provider refusal invisible
		// server-side: on 2026-08-07 a chat turn logged "Sending full prompt to LLM"
		// and then nothing at all, for four minutes, while the provider had already
		// answered "Weekly usage limit reached" in half a second. The operator's only
		// evidence was silence, which reads as "the kernel is broken" rather than
		// "the model provider said no". The error still propagates unchanged — this
		// only stops it leaving the kernel without a trace.
		slog.Error("planner: LLM call failed — no plan produced",
			"err", err,
			"prompt_chars", len(fullPrompt),
			"prompt_version", promptVersion)
		return nil, err
	}
	slog.Debug("Received raw LLM response", "raw_response", responseStr)
	thoughts, planJSON := ParseThoughts(responseStr)
	for i, t := range thoughts {
		slog.Debug("planner thought", "index", i, "thought", t)
	}
	if len(thoughts) > 0 {
		slog.Debug("planner extracted thoughts", "count", len(thoughts), "subject_input", userInput)
	}
	match := domain.ExtractJSONObject(planJSON)
	slog.Debug("Successfully extracted JSON from response", "extracted_json", match)
	if match == "" {
		return nil, fmt.Errorf("no JSON object found in LLM response: %s", responseStr)
	}

	var plan domain.ExecutionPlan
	if err := json.Unmarshal([]byte(match), &plan); err != nil {
		return nil, fmt.Errorf("Parse error: %v | Raw: %s", err, responseStr)
	}

	// AGENTCONTEXTREQ REQ1: forward planning-time facts to DAGExecutor so agents
	// execute with the same background knowledge that informed the Planner.
	plan.PlanningFacts = ltmEnrichment.Facts
	// ADR-0094 D8: record WHICH routines informed this plan, so the outcome can feed
	// their confidence. Every routine the planner was shown counts as followed — the
	// block is advisory and the model may ignore it, but a routine that was present
	// and did not help is exactly the evidence that should lower its confidence.
	// Requiring proof of use would mean only successes were ever recorded.
	for _, proc := range ltmEnrichment.Procedures {
		if proc.ID != "" {
			plan.FollowedProcedures = append(plan.FollowedProcedures, proc.ID)
		}
	}
	// PROMPTREQ: record which static prompt template produced this plan
	// (capability-arm hash when the contract is on — free provenance).
	plan.PlannerPromptVersion = promptVersion

	// ROUTE-03 vocabulary guard: drop emitted capabilities the planner was not shown.
	//
	// The prompt says "NEVER invent a capability tag that is not listed in CAPABILITY
	// CLUSTERS" and the planner invents them anyway — measured 2026-07-28, it emitted
	// `required_capabilities: ["file_write"]` when no agent declared `file_write` and
	// the tag was absent from the rendered vocabulary. Once L1 Declaration genuinely
	// enforces, an undeclared tag matches nothing, filters every candidate, and the
	// step dies with `no candidates found`. A hallucinated tag must not be able to
	// hard-fail a step.
	//
	// Dropping is fail-OPEN for that step: it falls back to unconstrained routing,
	// which is exactly the pre-ROUTE-03 behaviour. That is a deliberate and bounded
	// choice — the capability contract is a ROUTING refinement derived from an
	// LLM emission, not an authorization boundary. Scope (ADR-0034/0035), the policy
	// PDP (ADR-0085/0087) and approval gates are separate and untouched; none of them
	// consult this field. Were it load-bearing for access, the right answer would be
	// to fail the step instead.
	//
	// Same shape as the cache_policy validation directly below: an LLM-emitted name is
	// checked against the configured set and silently normalised when unknown.
	//
	// Guarded on NON-EMPTY: an empty vocabulary means "no manifest declared any
	// capability" — no knowledge — not "every emitted tag is invented". Filtering on
	// an absence of information would silently strip every requirement the moment the
	// registry were empty or a manifest read failed, which is the same silent-failure
	// shape this guard exists to prevent.
	if len(capVocab) > 0 {
		for i := range plan.Steps {
			kept := plan.Steps[i].RequiredCapabilities[:0]
			for _, c := range plan.Steps[i].RequiredCapabilities {
				lookup := c
				if p.canonicalVocab {
					lookup = domain.NormalizeCapability(c)
				}
				if _, ok := capVocab[lookup]; ok {
					kept = append(kept, c)
					continue
				}
				slog.WarnContext(ctx, "planner: dropping invented capability tag",
					"capability", c, "step", i)
			}
			plan.Steps[i].RequiredCapabilities = kept
		}
	}

	// ADR-0027: validate LLM-emitted cache_policy against the configured policy set.
	// Unknown names are normalised to "" so the Hippocampus falls back to default.
	if plan.CachePolicy != "" && p.policyProvider != nil {
		if _, ok := p.policyProvider.GetPolicy(plan.CachePolicy); !ok {
			plan.CachePolicy = ""
		}
	}

	for i, step := range plan.Steps {
		fanOut := -1
		if step.FanOutOver != nil {
			fanOut = *step.FanOutOver
		}
		slog.Info("planner_step_generated", "index", i, "query", step.Query, "is_thought", step.IsThought, "depends_on", step.DependsOn, "fan_out_over", fanOut, "subject", plan.Subject)
	}

	if p.advisor != nil {
		for i := range plan.Steps {
			stepType := extractStepType(plan.Steps[i].Query)
			currentLimit := int(plan.Steps[i].MaxEnergy)
			if currentLimit == 0 {
				currentLimit = 4096
			}
			plan.Steps[i].MaxEnergy = float64(p.advisor.GetAdaptiveMaxEnergy(stepType, currentLimit))
		}
	}

	return &plan, nil
}

// buildCapabilityCluster groups agents by their Capabilities field and returns
// a formatted string for the planner prompt. TraitModel agents are excluded.
// Agents with no capabilities fall under "(uncategorized)" with their description.
// Cluster keys are sorted alphabetically for deterministic output.
// buildLTMBlock produces the typed XML-tag LTM prompt section. ADR-0025, ADR-0029.
// REQ-DEDUP-1: deduplicates facts by content hash before injection.
func buildLTMBlock(plan *domain.PlanLTMEntry, enrichment domain.LTMEnrichment) string {
	var sb strings.Builder
	if plan != nil && plan.PlanJSON != "" {
		fmt.Fprintf(&sb, "<PlanLTM similarity=\"%.2f\" confidence=\"%.2f\" outcome=\"%s\" replan_count=\"%d\">\n  %s\n</PlanLTM>\n",
			plan.Similarity, plan.Confidence, plan.Outcome, plan.ReplanCount, plan.PlanJSON)
	}
	if len(enrichment.Facts) > 0 {
		sb.WriteString("<FactLTM>\n")
		seen := make(map[string]struct{}, len(enrichment.Facts))
		id := 0
		for _, r := range enrichment.Facts {
			hash := sha256.Sum256([]byte(r.Document.Text))
			hashStr := hex.EncodeToString(hash[:])
			if _, ok := seen[hashStr]; ok {
				continue // skip duplicate fact
			}
			seen[hashStr] = struct{}{}
			fmt.Fprintf(&sb, "  <fact id=\"%d\" activation=\"%.2f\" relevance=\"%.2f\">%s</fact>\n",
				id, r.Document.ActivationStrength, r.RawScore, r.Document.Text)
			id++
		}
		sb.WriteString("</FactLTM>\n")
	}
	if len(enrichment.Negatives) > 0 {
		sb.WriteString("<NegativeLTM>\n")
		for _, r := range enrichment.Negatives {
			agentID, _ := r.Document.Metadata["agent_id"].(string)
			fmt.Fprintf(&sb, "  <failure agent=\"%s\">%s</failure>\n", agentID, r.Document.Text)
		}
		sb.WriteString("</NegativeLTM>\n")
	}
	// ADR-0029: episodic memory block — injected when past sessions are semantically relevant.
	if len(enrichment.Episodes) > 0 {
		if block := buildEpisodicBlock(enrichment.Episodes); block != "" {
			sb.WriteString(block)
		}
	}
	// ADR-0049 D11: precedent block — prior transitions for the situation being planned,
	// failure-weighted, for the LLM to anticipate which approach worked or failed.
	if block := buildPrecedentBlock(enrichment.Precedents); block != "" {
		sb.WriteString(block)
	}
	// ADR-0094 D5: induced routines for this situation — how this kind of work has
	// gone here before.
	if block := buildProcedureBlock(enrichment.Procedures); block != "" {
		sb.WriteString(block)
	}
	return sb.String()
}

// buildPrecedentBlock renders the <PrecedentLTM> world-model block (ADR-0049 D11). Each
// precedent is a transition the LLM REASONS over — situation → outcome → the action path
// taken. Failures are surfaced first (the retrieval already failure-weighted them). The
// block is presented as evidence, never as a routing directive (Zero-Hardcode rule).
func buildPrecedentBlock(precedents []domain.Precedent) string {
	if len(precedents) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<PrecedentLTM>\n")
	for _, p := range precedents {
		fmt.Fprintf(&sb, "  <precedent outcome=%q similarity=\"%.2f\">\n", p.Outcome, p.Similarity)
		fmt.Fprintf(&sb, "    <situation>%s</situation>\n", p.Situation)
		if len(p.Actions) > 0 {
			sb.WriteString("    <actions>\n")
			for _, a := range p.Actions {
				fmt.Fprintf(&sb, "      <action>%s</action>\n", a)
			}
			sb.WriteString("    </actions>\n")
		}
		sb.WriteString("  </precedent>\n")
	}
	sb.WriteString("</PrecedentLTM>\n")
	return sb.String()
}

// buildEpisodicBlock renders the <EpisodicMemory> XML block from SearchResult episodes.
// EpisodicMemory is deserialized from Document.Metadata["episodic"] at this injection site.
// Episodes with malformed or missing metadata are skipped with a WARN log. ADR-0029.
func buildEpisodicBlock(episodes []domain.SearchResult) string {
	var inner strings.Builder
	for _, ep := range episodes {
		em, ok := extractEpisodicMemory(ep)
		if !ok {
			slog.Warn("Planner: skipping episode with malformed Metadata[episodic]",
				"doc_id", ep.Document.ID)
			continue
		}
		fmt.Fprintf(&inner, "  <episode session_id=%q completed_at=%q>\n",
			em.SessionID, em.CompletedAt.Format("2006-01-02T15:04:05Z"))
		fmt.Fprintf(&inner, "    <goal>%s</goal>\n", em.Goal)
		if len(em.Decisions) > 0 {
			inner.WriteString("    <decisions>\n")
			for _, d := range em.Decisions {
				fmt.Fprintf(&inner, "      <decision source=%q>%s</decision>\n",
					string(d.SourceEventType), d.Text)
			}
			inner.WriteString("    </decisions>\n")
		}
		inner.WriteString("  </episode>\n")
	}
	if inner.Len() == 0 {
		return ""
	}
	return "<EpisodicMemory>\n" + inner.String() + "</EpisodicMemory>\n"
}

// extractEpisodicMemory deserializes an EpisodicMemory from SearchResult.Document.Metadata["episodic"].
// Returns (zero, false) when the field is absent or cannot be decoded.
func extractEpisodicMemory(r domain.SearchResult) (domain.EpisodicMemory, bool) {
	raw, ok := r.Document.Metadata["episodic"]
	if !ok {
		return domain.EpisodicMemory{}, false
	}
	// The value may already be a domain.EpisodicMemory (from tests / in-process path)
	// or a map[string]interface{} (after JSON serialization round-trip via pgvector).
	// Marshal→Unmarshal handles both uniformly.
	b, err := json.Marshal(raw)
	if err != nil {
		return domain.EpisodicMemory{}, false
	}
	var em domain.EpisodicMemory
	if err := json.Unmarshal(b, &em); err != nil {
		return domain.EpisodicMemory{}, false
	}
	// Require at minimum a non-empty SessionID to consider it valid.
	if em.SessionID == "" {
		return domain.EpisodicMemory{}, false
	}
	return em, true
}

// buildCapabilityClusterFromManifests groups agents by their MANIFEST-declared
// Capabilities (ROUTE-03). This is deliberately distinct from
// buildCapabilityCluster, which groups by the CapabilityClusterer's
// embedding-derived AgentDefinition.Capabilities (a single LLM cluster name).
// Under the capability_contract arm the planner must see the exact capability
// vocabulary that L1 Declaration enforces (manifest.Capabilities), or the
// emitted required_capabilities would never match. Agents with no declared
// capabilities are listed as (uncategorized) so the planner can still route them
// with an empty requirement set.
// It returns BOTH the rendered block and the vocabulary set it rendered, from one
// pass. Two passes would be two sources of truth for "what capabilities exist", and
// the guard in Plan() depends on the set being exactly what the planner was shown.
func buildCapabilityClusterFromManifests(ctx context.Context, agents []domain.AgentDefinition, provider AgentProvider, canonical bool) (string, map[string]struct{}) {
	clusters := make(map[string][]string)
	var uncategorized []domain.AgentDefinition
	for _, a := range agents {
		if a.Trait == domain.TraitModel {
			continue
		}
		var caps []string
		if m, err := provider.GetManifest(ctx, a.ID); err == nil && m != nil {
			caps = m.Capabilities
		}
		// ROUTE-04 / ADR-0067: fold format/typo-variant declared caps under one
		// normalized tag so the planner emits a tag L1 (also normalizing) will match.
		if canonical {
			caps = domain.NormalizeCapabilities(caps)
		}
		if len(caps) == 0 {
			uncategorized = append(uncategorized, a)
			continue
		}
		for _, cap := range caps {
			clusters[cap] = append(clusters[cap], a.ID)
		}
	}

	vocab := make(map[string]struct{}, len(clusters))
	for cap := range clusters {
		vocab[cap] = struct{}{}
	}

	var sb strings.Builder
	sb.WriteString("CAPABILITY CLUSTERS (active agents grouped by declared capability):\n")
	keys := make([]string, 0, len(clusters))
	for k := range clusters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "- %s: %s\n", k, strings.Join(clusters[k], ", "))
	}
	for _, a := range uncategorized {
		fmt.Fprintf(&sb, "- (uncategorized): %s — %q\n", a.ID, a.Description)
	}
	return sb.String(), vocab
}

// buildHostBlock reports the OS the kernel is running on, as a DYNAMIC prompt
// section so the planner can phrase a step in terms the platform understands
// ("PowerShell" vs "bash", a service name, a path separator in prose).
//
// It deliberately carries the OS and NOTHING else — no home directory, no
// desktop, no cwd. Two reasons, and both are the point of ADR-0100's third P3
// residual rather than an oversight:
//
//   - The planner cannot see the host it is planning for; the AGENT executing
//     the step runs on it and is given the absolute paths (SDK `_host_facts`).
//     Concrete paths therefore belong to the executor. plannerDecisionRules
//     states that division of labour; this block is what makes the OS half of
//     it available without inviting the path half.
//   - Plans are STORED and replayed into later plans as <PlanLTM> structural
//     references. An absolute "C:\Users\<someone>\Desktop\..." baked into a step
//     is wrong the moment it is recalled under a different user or host, and it
//     leaks a username into memory. The OS is a property of the deployment; a
//     home directory is a property of one account.
//
// It must stay OUT of plannerStaticText: the hash gates plan-cache reuse
// (PlannerPromptVersion), so folding a per-host value into it would give each
// host a different hash and silently invalidate cached plans across a fleet.
func buildHostBlock(goos string) string {
	if goos == "" {
		return ""
	}
	return "<host>\n  os: " + goos + "\n</host>"
}

func buildCapabilityCluster(agents []domain.AgentDefinition) string {
	clusters := make(map[string][]string)
	var uncategorized []domain.AgentDefinition

	for _, a := range agents {
		if a.Trait == domain.TraitModel {
			continue
		}
		caps := a.Capabilities
		// REQ-CLUSTER-3: fallback to description-derived capability when empty
		if len(caps) == 0 && a.Description != "" {
			caps = []string{extractShortCapability(a.Description)}
		}
		if len(caps) == 0 {
			uncategorized = append(uncategorized, a)
			continue
		}
		for _, cap := range caps {
			clusters[cap] = append(clusters[cap], a.ID)
		}
	}

	var sb strings.Builder
	sb.WriteString("CAPABILITY CLUSTERS (active agents grouped by domain):\n")

	keys := make([]string, 0, len(clusters))
	for k := range clusters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", k, strings.Join(clusters[k], ", ")))
	}
	for _, a := range uncategorized {
		sb.WriteString(fmt.Sprintf("- (uncategorized): %s — %q\n", a.ID, a.Description))
	}
	return sb.String()
}

// extractShortCapability derives a short capability label from an agent description.
// Takes the first noun phrase or falls back to the first 3 words.
func extractShortCapability(desc string) string {
	// Simple heuristic: look for "X agent" or "X engine" patterns
	lower := strings.ToLower(desc)
	for _, suffix := range []string{" agent", " engine", " generator", " summariser", " analyst"} {
		if idx := strings.Index(lower, suffix); idx > 0 {
			// Find start of word
			start := idx
			for start > 0 && lower[start-1] != ' ' {
				start--
			}
			return desc[start:idx] + suffix
		}
	}
	// Fallback: first 3 words
	words := strings.Fields(desc)
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, " ")
}

// JSON extraction from LLM plan responses (reasoning-wrapper-tolerant) lives in
// domain as domain.ExtractJSONObject — shared with the memory Tier-2
// scorer so the two no longer drift.

// extractStepType derives a stable step-type label from a step query by taking
// the first word of the query as a cheap classifier.
func extractStepType(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return "unknown"
	}
	return strings.ToLower(fields[0])
}

// SPCAlarm tracks budget exhaustion signals per plan and fires a
// PLAN_BUDGET_INSUFFICIENT alarm when the threshold is crossed.
type SPCAlarm struct {
	mu         sync.Mutex
	rate       float64 // threshold rate (e.g. 0.05 for 5%)
	planCounts map[string]int
	planFired  map[string]bool
}

// NewSPCAlarm creates an SPCAlarm with the given alarm rate (e.g. 0.05).
func NewSPCAlarm(rate float64) *SPCAlarm {
	return &SPCAlarm{
		rate:       rate,
		planCounts: make(map[string]int),
		planFired:  make(map[string]bool),
	}
}

// RecordBudgetExhaustion records a budget exhaustion event for planID and
// stepType. Returns true when the signal should fire (≥2 steps AND > rate
// threshold, once per plan).
func (a *SPCAlarm) RecordBudgetExhaustion(planID, stepType string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.planFired[planID] {
		return false
	}
	a.planCounts[planID]++
	count := a.planCounts[planID]
	if count >= 2 {
		exhaustionRate := float64(count) / float64(count)
		_ = exhaustionRate // placeholder for step-level rate tracking
		a.planFired[planID] = true
		return true
	}
	return false
}
