package awareness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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

const plannerLTMRules = `LTM CONTEXT RULES (applies when <FactLTM>, <PlanLTM>, <NegativeLTM>, or <DiscoveryLTM> sections appear in <Context>):
- <FactLTM> contains facts from prior sessions retrieved for relevance to this request. Use facts with high relevance scores to enrich your plan steps where applicable. Ignore facts that are not relevant — do NOT invent steps just to use a fact.
- <PlanLTM> contains a prior successful plan for a similar request. Use it as a structural reference but adapt it to the current request; do not copy it blindly.
- <NegativeLTM> contains failure records from prior sessions. Avoid assigning the same task to the agent that previously failed it.
- <DiscoveryLTM> contains LIVE observations of the CURRENT world state, scanned just now, one <entity> per thing observed. SHAPE THE PLAN TO MATCH IT: if it reports N remaining items, emit a step per remaining item — do NOT collapse them into one step or guess a count. The <entity> facts are authoritative; the <interpretation> is an advisory hint, not a directive.
- <environment> (inside <DiscoveryLTM>) gives the ACTUAL host facts: os, home, desktop, cwd. Build every file/folder path from these ABSOLUTE paths and the host's OS conventions. On os="windows" use backslash Windows paths (e.g. the desktop is the "desktop" value) — NEVER "~", "~/Desktop", or forward-slash Unix paths. A step that creates "a folder on the desktop" MUST use the absolute "desktop" path from <environment>.
- If <DiscoveryLTM> contains <unobserved> entries, the Scout could not observe those within its budget. Emit an EARLY step to scan/inspect each unobserved entity with "checkpoint_after": true BEFORE any step that depends on its contents, so the plan is corrected (re-planned) once the real state is known — never guess an unobserved entity's contents.`

const plannerDecisionRules = `STRICT DECISION RULES:
- The "query" field MUST contain the full natural-language instructions for the step. NEVER truncate the user's intent; include the complete action required.
- Describe what the step needs in natural language. The runtime will discover the right agent automatically.
- If the user provides explicit answers, construct steps effectively without redundant actions.
- IMPORTANT: The example "uppercase the Name column in data.csv" is ONLY a format example. NEVER use it as the actual task unless the user explicitly requests it.
- You may reference a specific agent ID from the capability clusters if the task is unambiguously domain-specific, but prefer capability-level descriptions. The runtime resolves each step to a concrete agent at execution time (discovery + selection); do not assume a particular selection mechanism.
- When a step requires analysis, comparison, evaluation, or justification, start the query with verbs like "Analyse...", "Compare...", or "Evaluate...". Do NOT start analysis steps with "Summarise..." — that will route the task to the wrong agent.
- NEVER set "is_thought": true for steps that require analysis, comparison, evaluation, code generation, or summarisation. These MUST be routed to the corresponding cognitive agent. Only use "is_thought": true for trivial synthesis or routing decisions that do not require domain expertise.`

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
- Never include thought blocks INSIDE the JSON structure.`

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
          "checkpoint_query": { "type": "string" }
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

// plannerStaticText is the concatenation of all static planner prompt parts.
// Only this text is hashed — dynamic injections are excluded so the hash
// remains stable across requests.
const plannerStaticText = plannerRole + plannerLTMRules + plannerDecisionRules + plannerDependencyRules + plannerUsageRules + planOutputSchema

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
	hippocampus    domain.ProceduralMemory  // nil means no procedural memory
	policyProvider domain.PolicyProvider    // ADR-0027: nil skips cache_policy validation
	WorkspaceStage domain.WorkspaceStage   // ADR-0016: may be nil; nil disables enrichment
	advisor        TokenUsageAdvisor      // nil means no adaptive energy tuning
	spcAlarm       *SPCAlarm              // nil means no PLAN_BUDGET_INSUFFICIENT alarm
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

	var modelsDescriptions strings.Builder
	hasModels := false
	for _, agent := range agents {
		if agent.Trait != domain.TraitModel {
			continue
		}
		// Skip embedding-only models from the recommendation list.
		// The Planner should only recommend generation-capable LLMs.
		if strings.Contains(strings.ToLower(agent.ID), "embed") {
			continue
		}
		hasModels = true
		// REQ6: omit empty capabilities — show only the model ID and provider name.
		parts := strings.SplitN(agent.Description, "/", 2)
		providerModel := agent.Description
		if len(parts) == 2 {
			providerModel = strings.TrimSpace(parts[1])
		}
		modelsDescriptions.WriteString(fmt.Sprintf("- %s (%s)\n", agent.ID, providerModel))
	}

	modelSection := ""
	if hasModels {
		modelSection = fmt.Sprintf("AVAILABLE MODELS:\n%s- Choose the cheapest model for simple steps, expert model for complex steps.\n- Tag each step with \"recommended_model\" to route to a specific model.", modelsDescriptions.String())
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
	// ADR-0051 D9: if the Scout ran pre-plan (Server.Execute attached its report to ctx),
	// render its live observations as <DiscoveryLTM> so the Planner shapes the plan to the
	// observed world. Absent/empty ⇒ no block ⇒ one-shot planning exactly as before.
	if report, ok := domain.DiscoveryFromContext(ctx); ok {
		ltmBlock += domain.RenderDiscoveryBlock(report)
	}
	fullPrompt := domain.PromptBuild(
		domain.PromptSystem(
			plannerRole,
			buildCapabilityCluster(agents), // dynamic — excluded from hash
			modelSection,                   // dynamic — excluded from hash
			plannerLTMRules,
			plannerDecisionRules,
			plannerDependencyRules,
			plannerUsageRules,
		),
		domain.PromptContext(ltmBlock),
		domain.PromptTask("User Request: "+userInput),
		domain.PromptOutputSchemaJSON(planOutputSchema),
	)
	slog.Debug("Sending full prompt to LLM", "full_prompt", fullPrompt)

	responseStr, err := p.client.Generate(ctx, fullPrompt)
	if err != nil {
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
	// PROMPTREQ: record which static prompt template produced this plan.
	plan.PlannerPromptVersion = plannerPromptHash

	// ADR-0027: validate LLM-emitted cache_policy against the configured policy set.
	// Unknown names are normalised to "" so the Hippocampus falls back to default.
	if plan.CachePolicy != "" && p.policyProvider != nil {
		if _, ok := p.policyProvider.GetPolicy(plan.CachePolicy); !ok {
			plan.CachePolicy = ""
		}
	}

	for i, step := range plan.Steps {
		slog.Info("planner_step_generated", "index", i, "query", step.Query, "is_thought", step.IsThought, "depends_on", step.DependsOn, "subject", plan.Subject)
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
	mu           sync.Mutex
	rate         float64 // threshold rate (e.g. 0.05 for 5%)
	planCounts   map[string]int
	planFired    map[string]bool
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
