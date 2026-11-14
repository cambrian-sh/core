package awareness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// ── Planner prompts ──────────────────────────────────────────────────────────
//
// The planner's text lives in the prompt library (ADR-0139 C4/D2): the stable
// system block per arm (prompts/planner, prompts/planner_cap — no slots, so the
// provider can cache it) and one user-message template shared by both arms
// (prompts/planner_input), whose tagged regions carry the per-call data: the
// capability clusters, the host, today's date, remembered context and, last, the
// request.
//
// The prompt version stamped on every plan is the pair's static hash
// (prompts.Pair.StaticHash), computed from the very files that are rendered. It
// gates plan-cache reuse (REQ-CACHE-1) and PlanEvent provenance, and it cannot
// certify text that was never sent — the drift class where the pin rules were
// hashed but not sent (T-20261104-prompt-code-contract-drift-planner-pin-rules).
// Per-host and per-request values are slot values, never static text, so the
// hash is the same on every host.

var (
	// The two legacy arms are PINNED to the versions they were measured on
	// (planner v3, planner_cap v6, planner_input v2): planner_cap v7 and
	// planner_input v3 belong to the checklist arm only (ADR-0145 phase 2), so a
	// kernel with execution.plan.checklist_enabled off sends byte-identical text.
	plannerPrompts    = prompts.MustPairVersions("planner", 3, "planner_input", 2)
	plannerCapPrompts = prompts.MustPairVersions("planner_cap", 6, "planner_input", 2)
	// plannerChecklistPrompts is the checklist arm (execution.plan.checklist_enabled,
	// ADR-0145 D5/D6): steps carry answers, inputs and for_each, and the user
	// message carries the kernel-derived checklist in <requirements>.
	plannerChecklistPrompts = prompts.MustPairVersions("planner_cap", 10, "planner_input", 3)
	// plannerStagedPrompts is the staged arm (execution.plan.staged_planner,
	// ADR-0145 D11): one call per stage, replying the next stage, ledger reads, a
	// question for the person or nothing more. With the flag off the checklist arm
	// stays on v10 + v3, byte for byte. v11: one input reference grammar, s<N> | e<K>
	// (T-20261112-staged-planner-binds-earlier-stage-results-with).
	plannerStagedPrompts = prompts.MustPairVersions("planner_cap", 11, "planner_input", 4)

	// plannerPromptHash / plannerPromptHashCap are written to
	// ExecutionPlan.PlannerPromptVersion and forwarded to PlanEvent. The two arms
	// differ, so provenance distinguishes them for free.
	plannerPromptHash          = plannerPrompts.StaticHash()
	plannerPromptHashCap       = plannerCapPrompts.StaticHash()
	plannerPromptHashChecklist = plannerChecklistPrompts.StaticHash()
	plannerPromptHashStaged    = plannerStagedPrompts.StaticHash()
)

// plannerDataTags are the data regions of planner_input. A closing tag of any of
// them inside a value is neutralised before rendering (prompts SPEC.md §3.1), so
// a fact, an agent description or the request cannot end its region early.
var plannerDataTags = []string{"capability_clusters", "host", "today", "documents", "request", "requirements", "stage", "ledger_reads"}

// plannerCacheTTL: planning is human-paced (an operator or user request), so the
// long breakpoint lifetime (ADR-0139 D1: 1h for human-paced work) keeps the
// system block warm between requests minutes apart.
const plannerCacheTTL = domain.CacheTTLLong

// planReply is the planner and replanner reply contract: the plan, plus a
// reasoning field the schema asks for FIRST. It replaces the free-form
// <thought>…</thought> preamble the model used to write before its JSON (which
// needed its own scraper): the model still reasons before committing to steps,
// and the reasoning is logged, never executed.
type planReply struct {
	Reasoning string `json:"reasoning"`
	domain.ExecutionPlan
	// The staged arm's reply fields (planner_cap v8; ADR-0145 D11). Absent on
	// every other arm, so they decode as empty there.
	Outcome     string                 `json:"outcome,omitempty"`
	LedgerReads []domain.LedgerLookup  `json:"ledger_reads,omitempty"`
	Questions   []domain.InputQuestion `json:"questions,omitempty"`
	Assumptions []string               `json:"assumptions,omitempty"`
}

// ── End planner prompts ──────────────────────────────────────────────────────

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
// prompt.
//
// The ADR-0027 procedural plan memory (a Hippocampus read injecting a prior plan
// as <PlanLTM>, and the cache_policy thresholds that tuned it) was deleted
// 2026-11-14 (owner decision, third round; ENG-1301): nothing had written a plan
// template since 2026-07-18, so the read could only return nothing or a stale
// pre-July plan. Learned routines are ADR-0139 D13 skills.
type Planner struct {
	client         Generator
	provider       AgentProvider
	WorkspaceStage domain.WorkspaceStage // ADR-0016: may be nil; nil disables enrichment
	advisor        TokenUsageAdvisor     // nil means no adaptive energy tuning
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
	// effects is the effect-checked-dispatch source (effects.go): the vocabulary and
	// each agent's tool-derived effects, shown to the planner under the capability
	// contract arm. nil ⇒ no effect block and no schema enum.
	effects domain.AgentEffectSource
	// toolMenu lists each agent's CALLABLE tools (tool_menu.go), so the capability
	// block shows the live tool surface with one-line summaries instead of the
	// declared allowlist globs. nil ⇒ the declared globs (the pre-fix rendering).
	toolMenu ToolMenuSource
	// calendar is the kernel's one "today" (domain.Calendar), shown in planner_input's
	// <today> region so relative asks resolve against the organisation's date, not an
	// assumed one (T-20261107-planner-and-chat-agent-are-not-told-today-s). nil ⇒ the
	// wall clock in UTC, so a bare Planner still tells the truth.
	calendar *domain.Calendar
}

// SetCalendar wires the kernel's calendar (nil ⇒ the wall clock in UTC).
func (p *Planner) SetCalendar(c *domain.Calendar) { p.calendar = c }

// Calendar is the calendar the planner dates its prompts with; the replanner shows
// the same one.
func (p *Planner) Calendar() *domain.Calendar { return p.calendar }

// hostGOOS is the OS the planner should be told about. Never hardcoded: it is
// the live host unless a test pinned it.
func (p *Planner) hostGOOS() string {
	if p.goos != "" {
		return p.goos
	}
	return runtime.GOOS
}

// NewPlanner creates a Planner.
func NewPlanner(client Generator, provider AgentProvider) *Planner {
	return &Planner{client: client, provider: provider}
}

// SetAdvisor wires an adaptive token usage advisor (nil clears it).
func (p *Planner) SetAdvisor(a TokenUsageAdvisor) {
	p.advisor = a
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

// The C1 surface (ADR-0139 D1) of the Planner's client, forwarded with its
// report: the replan handler re-plans through the Planner (via the plan-call
// budget), and a Planner that hid its client's RequestGenerator would silently
// put every replan back on the legacy single-string path. RequestsEnabled is
// false when the client is not a RequestGenerator, so AsRequestGenerator(planner)
// tells the truth about the client.

// GenerateRequest serves req on the planner's client.
func (p *Planner) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	return domain.GenerateVia(ctx, p.client, req)
}

// StreamRequest streams req on the planner's client, when it can.
func (p *Planner) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(p.client)
	if !ok {
		return nil, fmt.Errorf("planner: client %T does not implement StreamRequest", p.client)
	}
	return rg.StreamRequest(ctx, req)
}

// Capabilities reports the client's declared capabilities.
func (p *Planner) Capabilities() domain.Capabilities {
	if rg, ok := domain.AsRequestGenerator(p.client); ok {
		return rg.Capabilities()
	}
	return domain.Capabilities{}
}

// RequestsEnabled reports whether the client is a RequestGenerator.
func (p *Planner) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(p.client)
	return ok
}

var _ domain.RequestGenerator = (*Planner)(nil)

func (p *Planner) GetExecutionPlan(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
	// The in-band "[SYSTEM_REASONING_SIGNAL: JIT_LOGIC_SYNTHESIS]" fast-path was
	// deleted 2026-11-04: nothing in the workspace emitted the marker, so its only
	// reachable trigger was user text, which let any request skip planning.
	agents, err := p.provider.GetAllAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch agent list: %v", err)
	}
	checklist := domain.RequirementChecklistFrom(ctx)
	// ADR-0145 D11: the staged arm. After the first stage each call's context is
	// short and fresh — the checklist with statuses, the ledger index, the earlier
	// stages by reference — so remembered context (LTM, prior plans) is not
	// gathered again; it informed the first stage already.
	staged := domain.StagePlanningFrom(ctx)
	if checklist == nil {
		staged = nil // the staged arm plans against a checklist; without one it is the legacy planner
	}
	fresh := staged != nil && staged.Number > 1

	// ADR-0016: Enrich Planner with cross-session LTM facts.
	// ADR-0025: Retrieve typed LTM enrichment and prior plan for XML-tag injection.
	var ltmEnrichment domain.LTMEnrichment
	if p.WorkspaceStage != nil && !fresh {
		if enriched, err := p.WorkspaceStage.PrimeForPlanning(ctx, userInput); err == nil {
			ltmEnrichment = enriched
		}
	}

	// The stable system block is the arm's library prompt; everything per call —
	// the capability clusters, the host, remembered context and, last, the request —
	// goes into the user message as tagged data (ADR-0139 D2), and the reply shape
	// is the response schema (C1), validated and repaired once by the provider layer.
	//
	// ROUTE-03: under the capability_contract arm the capability prompt is used and
	// the clusters are the manifest-declared vocabulary.
	pair := plannerPrompts
	clusterBlock := buildCapabilityCluster(agents)
	promptVersion := plannerPromptHash
	// The capability vocabulary the planner was actually shown. nil when the contract
	// arm is off, which disables the guard below along with the feature itself.
	var capVocab map[string]struct{}
	if p.capabilityContract || checklist != nil {
		// manifest-derived vocabulary so the emitted required_capabilities
		// match what L1 Declaration enforces (NOT the clusterer's labels).
		clusterBlock, capVocab = buildCapabilityClusterFromManifests(ctx, agents, p.provider, p.canonicalVocab, p.toolMenu)
		// Effect-checked dispatch: the vocabulary and each agent's tool-derived
		// effects, so the planner can declare required_effects (planner_cap v3).
		clusterBlock = appendBlock(clusterBlock, buildEffectBlock(ctx, p.effects, agents))
		pair = plannerCapPrompts
		promptVersion = plannerPromptHashCap
	}
	values := map[string]string{
		"capability_clusters": clusterBlock,
		"host":                buildHostBlock(p.hostGOOS()),
		"today":               p.calendar.Block(),
		"documents":           buildLTMBlock(ltmEnrichment),
		"request":             userInput,
	}
	// ADR-0145 D5/D6: the checklist arm. The checklist rides ctx from the server
	// (it is derived once per request, before planning) and is shown to the
	// planner in <requirements>; the coverage stage's open items ride the same block.
	switch {
	case staged != nil:
		pair = plannerStagedPrompts
		promptVersion = plannerPromptHashStaged
		values["requirements"] = RenderStagedRequirements(checklist, domain.CoverageStageFrom(ctx))
		values["stage"] = RenderStageBlock(staged)
		values["ledger_reads"] = ""
		if fresh {
			values["documents"] = ""
		}
	case checklist != nil:
		pair = plannerChecklistPrompts
		promptVersion = plannerPromptHashChecklist
		stage := domain.CoverageStageFrom(ctx)
		values["requirements"] = RenderRequirementsBlock(checklist, stage)
		if stage != nil && stage.Earlier != "" {
			values["documents"] = appendBlock(values["documents"], stage.Earlier)
		}
	}
	var effectVocab *domain.StepEffectVocabulary
	if (p.capabilityContract || checklist != nil) && p.effects != nil {
		effectVocab = p.effects.Vocabulary()
	}
	buildRequest := func() (domain.GenerateRequest, error) {
		req, err := planRequest(pair, plannerDataTags, values, plannerCacheTTL)
		if err != nil {
			return req, err
		}
		if effectVocab != nil {
			if err := applyEffectSchema(&req, effectVocab); err != nil {
				return req, fmt.Errorf("planner: %w", err)
			}
		}
		return req, nil
	}
	req, err := buildRequest()
	if err != nil {
		return nil, err
	}

	var reply planReply
	for readRound := 0; ; readRound++ {
		if reply, err = p.planCall(ctx, req, promptVersion); err != nil {
			return nil, err
		}
		if staged == nil {
			break
		}
		// The staged arm (ADR-0145 D11): exactly one outcome per call.
		outcome := strings.TrimSpace(reply.Outcome)
		if outcome == domain.StageOutcomeRead {
			if readRound >= staged.MaxLookupRounds {
				return nil, fmt.Errorf("planner: still asking for ledger reads after %d rounds (execution.plan.stage_ledger_read_rounds)", readRound)
			}
			values["ledger_reads"] = appendBlock(values["ledger_reads"], serveLedgerReads(ctx, staged, reply.LedgerReads))
			if req, err = buildRequest(); err != nil {
				return nil, err
			}
			continue
		}
		staged.AddAssumptions(reply.Assumptions)
		switch outcome {
		case domain.StageOutcomeInputRequired:
			qs := normalizeQuestions(reply.Questions)
			if len(qs) == 0 {
				return nil, fmt.Errorf("planner: input_required without a question")
			}
			slog.Info("ADR-0145 D11: planner asks the person", "stage", staged.Number, "questions", len(qs), "can_ask", staged.CanAsk)
			return nil, &domain.InputRequiredError{Questions: qs, Reason: reply.Reasoning}
		case domain.StageOutcomeNothingMore:
			slog.Info("ADR-0145 D11: planner has nothing more to add", "stage", staged.Number, "reasoning", reply.Reasoning)
			return nil, domain.ErrNoFurtherStage
		}
		// "stage" (or an unstated outcome): the next stage. An empty stage adds
		// nothing.
		if len(reply.Steps) == 0 {
			return nil, domain.ErrNoFurtherStage
		}
		break
	}
	plan := reply.ExecutionPlan
	if staged != nil {
		// Thought steps are retired on the staged arm (ADR-0145 D6): a step that
		// only reasons runs on an agent with the ledger and calculation tools.
		for i := range plan.Steps {
			if plan.Steps[i].IsThought {
				slog.Warn("ADR-0145 D11: thought step retired; it runs on an agent", "step", i)
				plan.Steps[i].IsThought = false
			}
		}
	}
	return p.finishPlan(ctx, plan, ltmEnrichment, promptVersion, capVocab, effectVocab)
}

// planCall is one planner model call and its decode.
func (p *Planner) planCall(ctx context.Context, req domain.GenerateRequest, promptVersion string) (planReply, error) {
	var reply planReply
	resp, err := domain.GenerateVia(ctx, p.client, req)
	if err != nil {
		// Log it HERE. A bare `return nil, err` made a provider refusal invisible
		// server-side: on 2026-08-07 a chat turn sent its prompt and then logged
		// nothing at all, for four minutes, while the provider had already answered
		// "Weekly usage limit reached" in half a second. The operator's only evidence
		// was silence, which reads as "the kernel is broken" rather than "the model
		// provider said no". The error still propagates unchanged — this only stops
		// it leaving the kernel without a trace.
		slog.Error("planner: LLM call failed — no plan produced",
			"err", err,
			"prompt_chars", len(domain.FlattenRequest(req)),
			"prompt_version", promptVersion)
		return reply, err
	}
	if err := decodePlanReply(resp, &reply); err != nil {
		return reply, fmt.Errorf("planner: %w", err)
	}
	if reply.Reasoning != "" {
		slog.Debug("planner reasoning", "reasoning", reply.Reasoning, "prompt_version", promptVersion)
	}
	return reply, nil
}

// finishPlan applies the post-processing every arm shares to a decoded plan.
func (p *Planner) finishPlan(ctx context.Context, plan domain.ExecutionPlan, ltmEnrichment domain.LTMEnrichment,
	promptVersion string, capVocab map[string]struct{}, effectVocab *domain.StepEffectVocabulary) (*domain.ExecutionPlan, error) {

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
	// Same shape as the cache_policy validation directly below: an LLM-emitted name is
	// checked against the configured set and silently normalised when unknown.
	dropInventedCapabilities(ctx, &plan, capVocab, p.canonicalVocab)
	// A step that names itself in depends_on / fan_out_over meant the step before it
	// (one-based numbering slipped in): repaired here, deterministically, before the
	// validator would refuse the plan as a cycle
	// (T-20261108-planner-writes-depends-on-1-based-step-n).
	for _, r := range domain.RepairSelfDependencies(&plan) {
		slog.Warn("planner: repaired a self-dependency (one-based step number)",
			"step", r.Step, "field", r.Field, "from", r.From, "to", r.To)
	}
	// Effect contract: normalised and checked, but never dropped (fail closed).
	normalizeDeclaredEffects(ctx, &plan, effectVocab)

	// plan.CachePolicy (the prompt's cache_policy label) is no longer validated: the
	// ADR-0027 policy table it was checked against was retired with the procedural
	// plan memory it tuned (ENG-1301). It is a descriptive label carried on plan
	// events only; nothing routes on it.

	for i, step := range plan.Steps {
		fanOut := -1
		if step.FanOutOver != nil {
			fanOut = *step.FanOutOver
		}
		// preferred_agent / agent_pin are logged so a pinned plan is observable without the
		// prompt-exchange capture, which covers the agent plane only (E2E 20261105T000624Z E12).
		slog.Info("planner_step_generated", "index", i, "query", step.Query, "is_thought", step.IsThought, "depends_on", step.DependsOn, "fan_out_over", fanOut, "subject", plan.Subject, "preferred_agent", step.PreferredAgent, "agent_pin", step.AgentPin, "required_effects", step.RequiredEffects)
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

// dropInventedCapabilities is the ROUTE-03 vocabulary guard: it drops emitted
// capabilities the model was not shown. Shared by the planner and the replan
// handler so both producers of capability-contract plans apply one rule.
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
// Guarded on NON-EMPTY: an empty vocabulary means "no manifest declared any
// capability" — no knowledge — not "every emitted tag is invented". Filtering on
// an absence of information would silently strip every requirement the moment the
// registry were empty or a manifest read failed, which is the same silent-failure
// shape this guard exists to prevent.
func dropInventedCapabilities(ctx context.Context, plan *domain.ExecutionPlan, vocab map[string]struct{}, canonical bool) {
	if len(vocab) == 0 {
		return
	}
	for i := range plan.Steps {
		kept := plan.Steps[i].RequiredCapabilities[:0]
		for _, c := range plan.Steps[i].RequiredCapabilities {
			lookup := c
			if canonical {
				lookup = domain.NormalizeCapability(c)
			}
			if _, ok := vocab[lookup]; ok {
				kept = append(kept, c)
				continue
			}
			slog.WarnContext(ctx, "planner: dropping invented capability tag",
				"capability", c, "step", i)
		}
		plan.Steps[i].RequiredCapabilities = kept
	}
}

// CapabilityContract reports whether the ROUTE-03 capability_contract arm is on
// and, when it is, renders the vocabulary block and set the model must choose
// from. It exists for the replan handler, which must produce plans under the
// same contract: one builder (buildCapabilityClusterFromManifests) serves both,
// so the planner and the replanner cannot show the model different vocabularies.
// Off: ("", nil, false, false).
func (p *Planner) CapabilityContract(ctx context.Context) (block string, vocab map[string]struct{}, canonical bool, on bool) {
	if !p.capabilityContract {
		return "", nil, false, false
	}
	agents, err := p.provider.GetAllAgents(ctx)
	if err != nil {
		// An empty vocabulary disables the guard (the documented "no knowledge"
		// behaviour); the error is logged rather than swallowed.
		slog.WarnContext(ctx, "planner: capability vocabulary unavailable for replan", "err", err)
		return "", nil, p.canonicalVocab, true
	}
	block, vocab = buildCapabilityClusterFromManifests(ctx, agents, p.provider, p.canonicalVocab, p.toolMenu)
	return block, vocab, p.canonicalVocab, true
}

// buildCapabilityCluster groups agents by their Capabilities field and returns
// a formatted string for the planner prompt. TraitModel agents are excluded.
// Agents with no capabilities fall under "(uncategorized)" with their description.
// Cluster keys are sorted alphabetically for deterministic output.
// buildLTMBlock produces the typed XML-tag LTM prompt section. ADR-0025.
// REQ-DEDUP-1: deduplicates facts by content hash before injection.
// The <PlanLTM> prior-plan section went with the ADR-0027 procedural plan memory
// (ENG-1301).
func buildLTMBlock(enrichment domain.LTMEnrichment) string {
	var sb strings.Builder
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
	// The ADR-0029 <EpisodicMemory> block used to be rendered here. Removed with the
	// rest of the episodic lane: nothing has written a DocTypeEpisodicMemory row since
	// the extractor was deleted on 2026-07-18, so the block could only ever be empty.
	// The ADR-0049 D11 <PrecedentLTM> block was removed 2026-11-05 with scenes
	// (decisions/scenes-cancelled.md).
	// ADR-0094 D5: stored routines for this situation — how this kind of work has
	// gone here before.
	if block := buildProcedureBlock(enrichment.Procedures); block != "" {
		sb.WriteString(block)
	}
	return sb.String()
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
//
// menu, when set, renders the tool lines from each agent's live callable tools
// (tool_menu.go) — the fix for T-20261107-world-system-asks-are-planned-as-local-file.
// nil keeps the declared-glob rendering of agentToolLine.
func buildCapabilityClusterFromManifests(ctx context.Context, agents []domain.AgentDefinition, provider AgentProvider, canonical bool, menu ToolMenuSource) (string, map[string]struct{}) {
	clusters := make(map[string][]string)
	var uncategorized []domain.AgentDefinition
	var toolLines []string
	live := newLiveToolMenu()
	for _, a := range agents {
		if a.Trait == domain.TraitModel {
			continue
		}
		var caps []string
		if m, err := provider.GetManifest(ctx, a.ID); err == nil && m != nil {
			caps = m.Capabilities
			switch {
			case len(m.ToolAllowlist) == 0:
				// No allowlist: no tool line.
			case menu != nil:
				live.add(a.ID, menu.AvailableTools(ctx, a.ID))
			default:
				toolLines = append(toolLines, agentToolLine(a.ID, m.ToolAllowlist))
			}
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
	if len(toolLines) > 0 {
		sort.Strings(toolLines)
		sb.WriteString("\nTools each agent can call (agents not listed declare no tool allowlist):\n")
		for _, l := range toolLines {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	if block := live.render(); block != "" {
		sb.WriteString("\n")
		sb.WriteString(block)
	}
	return strings.TrimRight(sb.String(), "\n"), vocab
}

// agentToolLine renders one agent's tool surface for the planner — its declared
// tool allowlist (a profile's `tools`, ADR-0139 / agent stack v2 D3), grouped by
// MCP server — or "" when it declares none.
//
// Why the planner sees it: a capability tag says what an agent is FOR, not what it
// can DO. Shown only tags, the planner tagged a "rename and move each receipt" step
// with nothing a mover alone declared, and the step reached an agent whose
// allowlist had no move tool, which then offered to rewrite the PDFs as
// placeholder text (T-20261106-planner-routes-a-file-move-step-to-an-agent). The
// allowlist is the right source: it is the ceiling the kernel enforces on that
// agent (domain.ProfileToolCeiling), so the planner is shown exactly what the agent
// could ever call — never a self-asserted list.
//
// This is the FALLBACK, used only when the planner has no ToolMenuSource. The
// globs are rendered as declared ("read_*"); a server's "*" reads "all tools".
// That was the production rendering until 2026-11-07 and it is what planned an
// accounts-receivable ask as a local file search: a glob naming a server that is
// not connected still showed its tools by name, while the one live server read
// "vantis-emp-904 (all tools)" with nothing saying it held finance and mail
// (T-20261107-world-system-asks-are-planned-as-local-file). With a source wired the
// lines come from the live callable set instead (tool_menu.go).
func agentToolLine(agentID string, allowlist []string) string {
	if len(allowlist) == 0 {
		return ""
	}
	var order []string
	byServer := make(map[string][]string)
	for _, g := range allowlist {
		server, tool := "native", g
		if rest, ok := strings.CutPrefix(g, "mcp:"); ok {
			server, tool, _ = strings.Cut(rest, "/")
		}
		if _, seen := byServer[server]; !seen {
			order = append(order, server)
		}
		if tool == "*" {
			tool = "all tools"
		}
		byServer[server] = append(byServer[server], tool)
	}
	parts := make([]string, 0, len(order))
	for _, s := range order {
		parts = append(parts, fmt.Sprintf("%s (%s)", s, strings.Join(byServer[s], ", ")))
	}
	return fmt.Sprintf("- %s can call: %s", agentID, strings.Join(parts, "; "))
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
//   - Plans are STORED (run store, plan events) and were once replayed into later
//     plans as <PlanLTM> structural references (removed 2026-11-14, ENG-1301). An absolute "C:\Users\<someone>\Desktop\..." baked into a step
//     is wrong the moment it is recalled under a different user or host, and it
//     leaks a username into memory. The OS is a property of the deployment; a
//     home directory is a property of one account.
//
// It is the content of planner_input's <host> slot, never static text: the hash
// gates plan-cache reuse (PlannerPromptVersion), so folding a per-host value into
// it would give each host a different hash and silently invalidate cached plans
// across a fleet.
func buildHostBlock(goos string) string {
	if goos == "" {
		return ""
	}
	return "os: " + goos
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
	return strings.TrimRight(sb.String(), "\n")
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

// extractStepType derives a stable step-type label from a step query by taking
// the first word of the query as a cheap classifier.
func extractStepType(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return "unknown"
	}
	return strings.ToLower(fields[0])
}
