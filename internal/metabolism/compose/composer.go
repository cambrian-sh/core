// Package compose builds the worker the kernel runs a plan step with (ADR-0145 D12):
// one ReAct runtime, the skills of the professions that cover the step, the tools
// those professions may use that the act-for person's scope allows, the step's
// brief and the ledger references of its inputs. A step's declared effects do not
// narrow its tools (ADR-0145 D14 removed per-step permissions).
//
// It runs AFTER the gatekeeper's eligibility check (capabilities + effects, ADR-0100)
// and replaces only the last move of agent-per-step dispatch, "run the winner's
// pre-built agent". Eligibility stays the kernel's typed decision of which
// professions cover the step; a profession is a declarative profile
// (agents/profiles) — skills plus a tool allowlist — not a long-lived agent.
//
// Code and service agents (class b in the CONTEXT.md inventory: a selected agent
// with no profile) are never composed: the step goes to that agent as before.
package compose

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// HandoffLeaseKey is the handoff context key carrying the step's lease (the wire
// name predates the lease rename, see dispatch.handoffLeaseKey).
const HandoffLeaseKey = "_session_token_id"

// HandoffKeyCompositionID carries the composition's id beside its body, so a
// trace or a step output can name the composition without decoding it.
const HandoffKeyCompositionID = "_composition_id"

// DefaultSystemPrompt names the runtime's developer-written system prompt
// (cambrian-core/prompts/composed_worker).
const DefaultSystemPrompt = "composed_worker"

// AgentLookup reads one registered agent definition.
type AgentLookup interface {
	GetAgentByName(ctx context.Context, name string) (*domain.AgentDefinition, error)
}

// ManifestReader reads a registered agent's manifest.
type ManifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// ToolCatalogue lists the live tool registry (*domain.ToolExecutor).
type ToolCatalogue interface {
	AllTools() []domain.SystemTool
}

// AgentLister lists every registered agent (domain.AgentRegistry).
type AgentLister interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
}

// SkillCatalogue resolves a skill by name (domain.SkillRegistry).
type SkillCatalogue interface {
	Get(name string) (domain.Skill, bool)
}

// SkillGrants returns an agent's effective skill set (profile skills plus operator
// grants, decisions/skill-access-grants.md). *access.Service satisfies it.
type SkillGrants interface {
	Effective(ctx context.Context, agentID string) (set map[string]string, bound bool)
}

// ScopeCheck reports whether the runtime, acting for the person on ctx, may use a
// tool (the executor's data-store regime under the conversation owner's
// intersection). nil admits every tool; the executor still authorizes every call.
type ScopeCheck func(ctx context.Context, agentID string, tool domain.SystemTool) bool

// Composer composes workers. Every dependency but Agents, Manifests, Tools and
// Leases is optional and documented on its field.
type Composer struct {
	// RuntimeID is the composed runtime's agent id (execution.workers.runtime_agent_id).
	RuntimeID string
	Agents    AgentLookup
	Manifests ManifestReader
	Tools     ToolCatalogue
	// Scope is the act-for person's scope check. nil admits every tool.
	Scope ScopeCheck
	// Skills drops index entries the kernel does not serve. nil keeps every name.
	Skills SkillCatalogue
	// Grants adds each profession's operator-granted skills. nil: profile skills only.
	Grants SkillGrants
	// MaxSkills caps the index (sorted by name). <= 0: no cap.
	MaxSkills int
	// Leases is where the composition is bound and enforced.
	Leases *domain.ComposedLeases
	// Overlay receives the composed tools as run-scoped grants of the step lease,
	// so the runtime (which holds no standing grants) may call exactly them; the
	// gateway's completion hook clears them with the lease (ToolExecutor.EndRun).
	Overlay *domain.RunGrantOverlay
	// Ledger records every composition (kind "composition"). nil: not recorded,
	// logged (configuration refuses composed workers without the ledger).
	Ledger domain.LedgerWriter
	// SystemPrompt names the runtime's system prompt. "" ⇒ DefaultSystemPrompt.
	SystemPrompt string
	// Fleet lists the registered agents, whose profiles' tool allowlists bound
	// what any composition could hold (OutsideStep). nil ⇒ OutsideStep reports
	// nothing.
	Fleet AgentLister
}

// Compose decides whether the step is run by a composed worker and, if so, binds
// the composition to the step's lease, stamps it on the handoff, records it and
// returns the runtime as the candidate to call. ok=false leaves the dispatch to
// the selected winner (a code or service agent, no lease, no runtime). A non-nil
// error refuses the dispatch: a yielded sub-goal on a composed lease whose
// capability hint that composition does not cover (subgoalGap).
func (c *Composer) Compose(ctx context.Context, task *domain.DispatchTask, winner domain.ScoredCandidate,
	scored []domain.ScoredCandidate, in *domain.Handoff) (domain.ScoredCandidate, domain.WorkerComposition, bool, error) {
	if c == nil || task == nil || in == nil {
		return domain.ScoredCandidate{}, domain.WorkerComposition{}, false, nil
	}
	if task.Subgoal && in.Context != nil {
		// Checked before the winner's class: a sub-goal on a composed lease runs
		// under that composition whoever ranked first.
		if prior, bound := c.leaseComposition(in); bound {
			if gap := c.subgoalGap(ctx, task, prior); gap != nil {
				slog.InfoContext(ctx, "ADR-0145 D12: yielded sub-goal needs a capability its step's composition lacks; the step goes to the re-planner",
					"task", task.ID, "composition", prior.ID(), "needed", gap.Needed)
				return domain.ScoredCandidate{}, domain.WorkerComposition{}, false, gap
			}
		}
	}
	if winner.Agent.Profile == "" {
		// Class (b): a code or service agent was selected; it stays callable.
		return domain.ScoredCandidate{}, domain.WorkerComposition{}, false, nil
	}
	lease := ""
	if in.Context != nil {
		lease = in.Context[HandoffLeaseKey]
	}
	if lease == "" || c.Leases == nil {
		// Without a lease there is nothing to bind the bounds to: composing would
		// hand the runtime an unenforced menu. The pre-built agent runs instead.
		slog.WarnContext(ctx, "ADR-0145 D12: step not composed (no step lease to bind the composition to)",
			"task", task.ID, "winner", winner.Agent.ID)
		return domain.ScoredCandidate{}, domain.WorkerComposition{}, false, nil
	}
	runtime, err := c.Agents.GetAgentByName(ctx, c.RuntimeID)
	if err != nil || runtime == nil {
		slog.ErrorContext(ctx, "ADR-0145 D12: composed runtime agent is not registered; step runs on the selected agent",
			"runtime", c.RuntimeID, "task", task.ID, "winner", winner.Agent.ID, "err", err)
		return domain.ScoredCandidate{}, domain.WorkerComposition{}, false, nil
	}
	if prior, bound := c.Leases.For(lease); bound {
		// The lease already carries the step's composition: a retry of the step (the
		// self-healer re-dispatches on the same lease) or a yield sub-goal dispatched
		// on its parent's step lease (yield.subHandoff). It runs on the runtime under
		// that SAME composition: re-composing would replace the step's bounds
		// mid-step, and the composed path never falls back to a pre-built agent
		// (T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent: a heal retry
		// that fell through to the selected profile agent took the lease from the
		// worker still running the step). Nothing new is recorded.
		in.Context[domain.HandoffKeyComposition] = prior.Encode()
		in.Context[HandoffKeyCompositionID] = prior.ID()
		slog.InfoContext(ctx, "ADR-0145 D12: step re-dispatched under its lease's existing composition",
			"task", task.ID, "composition", prior.ID(), "runtime", runtime.ID)
		return domain.ScoredCandidate{Agent: *runtime, Score: winner.Score}, prior, true, nil
	}

	professions := c.professions(ctx, task, winner, scored)
	var skills, allow []string
	for _, p := range professions {
		m, mErr := c.Manifests.GetManifest(ctx, p.ID)
		if mErr != nil || m == nil {
			continue
		}
		skills = append(skills, m.Skills...)
		allow = append(allow, m.ToolAllowlist...)
		if c.Grants != nil {
			if set, bound := c.Grants.Effective(ctx, p.ID); bound {
				for name, acc := range set {
					if acc == domain.SkillAccessGranted || acc == domain.SkillAccessProfile {
						skills = append(skills, name)
					}
				}
			}
		}
	}

	comp := domain.WorkerComposition{
		Runtime:      runtime.ID,
		Skills:       c.indexSkills(skills),
		Tools:        c.tools(ctx, runtime.ID, domain.ToolAllowlist(allow), task),
		Brief:        task.Description,
		LedgerRefs:   ledgerRefs(in),
		Model:        runtime.ModelPreference,
		SystemPrompt: c.systemPrompt(),
		StepIndex:    stepIndex(in),
	}
	for _, p := range professions {
		comp.Professions = append(comp.Professions, p.ID)
	}
	prov := domain.LedgerProvenanceFromContext(ctx)
	comp.RunID = prov.RunID
	comp = comp.Normalize()

	c.Leases.Bind(domain.LeaseID(lease), comp)
	if c.Overlay != nil && len(comp.Tools) > 0 {
		c.Overlay.Activate(lease, comp.Tools)
	}
	in.Context[domain.HandoffKeyComposition] = comp.Encode()
	in.Context[HandoffKeyCompositionID] = comp.ID()
	c.record(ctx, prov, comp)

	slog.InfoContext(ctx, "ADR-0145 D12: step composed",
		"task", task.ID, "composition", comp.ID(), "runtime", comp.Runtime,
		"professions", comp.Professions, "skills", len(comp.Skills), "tools", len(comp.Tools),
		"selected", winner.Agent.ID)
	return domain.ScoredCandidate{Agent: *runtime, Score: winner.Score}, comp, true, nil
}

// leaseComposition is the composition already bound to the handoff's step lease.
func (c *Composer) leaseComposition(in *domain.Handoff) (domain.WorkerComposition, bool) {
	if c.Leases == nil || in == nil || in.Context == nil {
		return domain.WorkerComposition{}, false
	}
	lease := in.Context[HandoffLeaseKey]
	if lease == "" {
		return domain.WorkerComposition{}, false
	}
	return c.Leases.For(lease)
}

// subgoalGap decides whether a yielded sub-goal's capability hint is a need its
// step's composition cannot meet (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under,
// owner 2026-11-12: such a sub-goal is not dispatched to the same composition; it
// becomes the capability_missing / re-plan path). The sub-goal would run under
// that composition — the worker that just yielded it has exactly those tools — so:
//
//   - no hint: a decomposition sub-goal, dispatched as before (yield_subgoal is kept);
//   - a hint no registered agent declares (the gatekeeper's ADR-0100 D5
//     resolution left it unmatched and fell back to the generalist tier): a gap;
//   - a hint resolved to declared capabilities: a gap unless one of the
//     composition's professions declares each of them.
//
// The decision is typed (capabilities and the gatekeeper's resolution), never a
// reading of the intent's words.
func (c *Composer) subgoalGap(ctx context.Context, task *domain.DispatchTask, prior domain.WorkerComposition) *domain.CapabilityMissingError {
	if len(task.RequiredCapabilities) == 0 {
		return nil
	}
	gap := func(needed []string) *domain.CapabilityMissingError {
		return &domain.CapabilityMissingError{
			TaskID:      task.ID,
			Needed:      needed,
			Intent:      task.Description,
			Composition: prior.ID(),
			Source:      domain.CapabilitySourceSubgoal,
		}
	}
	need := task.RequiredCapabilities
	if r := task.CapabilityResolution; r != nil {
		if len(r.Unmatched) > 0 {
			return gap(r.Unmatched)
		}
		if len(r.Resolved) > 0 {
			need = r.Resolved
		}
	}
	covered := map[string]bool{}
	for _, p := range prior.Professions {
		m, err := c.Manifests.GetManifest(ctx, p)
		if err != nil || m == nil {
			continue
		}
		for _, cp := range domain.NormalizeCapabilities(m.Capabilities) {
			covered[cp] = true
		}
	}
	var missing []string
	for _, cp := range need {
		if n := domain.NormalizeCapability(cp); n != "" && !covered[n] {
			missing = append(missing, cp)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return gap(missing)
}

// professions are the profile agents among the eligible candidates that cover the
// step: those declaring one of its required capabilities, or — for a step that
// requires none — the selected winner alone (every agent is eligible then, and a
// union of all of them would be no selection at all). Sorted by id.
func (c *Composer) professions(ctx context.Context, task *domain.DispatchTask, winner domain.ScoredCandidate,
	scored []domain.ScoredCandidate) []domain.AgentDefinition {
	if len(task.RequiredCapabilities) == 0 {
		return []domain.AgentDefinition{winner.Agent}
	}
	want := map[string]bool{}
	for _, cp := range domain.NormalizeCapabilities(task.RequiredCapabilities) {
		want[cp] = true
	}
	seen := map[string]bool{}
	var out []domain.AgentDefinition
	for _, sc := range scored {
		a := sc.Agent
		if a.Profile == "" || seen[a.ID] {
			continue
		}
		m, err := c.Manifests.GetManifest(ctx, a.ID)
		if err != nil || m == nil {
			continue
		}
		for _, cp := range domain.NormalizeCapabilities(m.Capabilities) {
			if want[cp] {
				seen[a.ID] = true
				out = append(out, a)
				break
			}
		}
	}
	if !seen[winner.Agent.ID] {
		out = append(out, winner.Agent)
	}
	sortAgents(out)
	return out
}

func sortAgents(as []domain.AgentDefinition) {
	for i := 1; i < len(as); i++ {
		for j := i; j > 0 && as[j].ID < as[j-1].ID; j-- {
			as[j], as[j-1] = as[j-1], as[j]
		}
	}
}

// indexSkills keeps the names the kernel serves, sorted, capped at MaxSkills.
func (c *Composer) indexSkills(names []string) []string {
	var kept []string
	for _, n := range names {
		if c.Skills != nil {
			if _, ok := c.Skills.Get(n); !ok {
				continue
			}
		}
		kept = append(kept, n)
	}
	kept = domain.WorkerComposition{Skills: kept}.Normalize().Skills
	if c.MaxSkills > 0 && len(kept) > c.MaxSkills {
		kept = kept[:c.MaxSkills]
	}
	return kept
}

// tools are the catalogue tools inside the professions' allowlists and admitted
// by the person's scope.
func (c *Composer) tools(ctx context.Context, runtimeID string, allow domain.ToolAllowlist, task *domain.DispatchTask) []string {
	if c.Tools == nil || len(allow) == 0 {
		return nil
	}
	var out []string
	for _, t := range c.Tools.AllTools() {
		if !allow.Allows(t.Name) {
			continue
		}
		if c.Scope != nil && !c.Scope(ctx, runtimeID, t) {
			continue
		}
		if hasResourceArgs(t) {
			// A tool with resource arguments is bounded by its GRANT's resource
			// policy (allowed roots, domains, commands). The runtime receives its
			// tools as run-scoped grants without such a policy, so composing this
			// tool would widen it; it stays with the profession's own agent until
			// compositions carry grant policies
			// (T-20261108-composed-workers-carry-no-tool-resource-policy).
			slog.WarnContext(ctx, "ADR-0145 D12: tool with resource arguments left out of the composition",
				"task", task.ID, "tool", t.Name)
			continue
		}
		out = append(out, t.Name)
	}
	return out
}

// hasResourceArgs reports a tool bounded by its grant's resource policy (paths,
// URLs, commands), which a composition cannot carry yet.
func hasResourceArgs(t domain.SystemTool) bool {
	return len(t.PathArgs) > 0 || len(t.URLArgs) > 0 || len(t.CommandArgs) > 0
}

// OutsideStep is what a composed worker's find_tools may REPORT but not load
// (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under, owner 2026-11-12:
// "find_tools reports 'tool X exists but is not in this step' without loading
// it"): the tools some composition could hold for the act-for person on ctx —
// inside a registered profession's allowlist, in the live catalogue, admitted by
// the person's scope, and composable at all (no resource arguments, see tools) —
// that comp does not hold. Search widens what is known, never what is permitted
// (research note §1, Claude tool search): the re-planner, not the worker, decides
// whether a re-planned step gets one, and the kernel composes it from grants.
func (c *Composer) OutsideStep(ctx context.Context, comp domain.WorkerComposition) []domain.SystemTool {
	if c == nil || c.Fleet == nil || c.Tools == nil || c.Manifests == nil {
		return nil
	}
	agents, err := c.Fleet.GetAllAgents(ctx)
	if err != nil {
		return nil
	}
	var allow []string
	for _, a := range agents {
		if a.Profile == "" {
			continue
		}
		if m, mErr := c.Manifests.GetManifest(ctx, a.ID); mErr == nil && m != nil {
			allow = append(allow, m.ToolAllowlist...)
		}
	}
	if len(allow) == 0 {
		return nil
	}
	list := domain.ToolAllowlist(allow)
	in := map[string]bool{}
	for _, n := range comp.Tools {
		in[n] = true
	}
	var out []domain.SystemTool
	for _, t := range c.Tools.AllTools() {
		if in[t.Name] || !list.Allows(t.Name) || hasResourceArgs(t) {
			continue
		}
		if c.Scope != nil && !c.Scope(ctx, comp.Runtime, t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func (c *Composer) systemPrompt() string {
	if c.SystemPrompt != "" {
		return c.SystemPrompt
	}
	return DefaultSystemPrompt
}

// record writes the composition to the run ledger. Best-effort for the step, as a
// tool result's write is: a failed write is logged and the step still runs.
func (c *Composer) record(ctx context.Context, prov domain.LedgerProvenance, comp domain.WorkerComposition) {
	if c.Ledger == nil {
		slog.WarnContext(ctx, "ADR-0145 D12: composition not recorded (no run ledger)", "composition", comp.ID())
		return
	}
	prov.StepIndex = comp.StepIndex
	body := []byte(comp.Encode())
	if _, err := c.Ledger.Record(ctx, domain.LedgerRecord{
		Kind:        domain.LedgerComposition,
		Provenance:  prov,
		ToolName:    comp.Runtime,
		ArgsJSON:    []byte(`{"composition":"` + comp.ID() + `"}`),
		Writer:      domain.SystemPrincipal,
		DerivedFrom: comp.LedgerRefs,
		ContentType: "application/json",
		Body:        body,
	}); err != nil {
		slog.WarnContext(ctx, "ADR-0145 D12: composition not recorded to the ledger", "composition", comp.ID(), "err", err)
	}
}

// stepIndex reads the kernel-stamped step index; -1 when absent.
func stepIndex(in *domain.Handoff) int {
	if in == nil || in.Context == nil {
		return -1
	}
	if n, err := strconv.Atoi(in.Context["_step_index"]); err == nil {
		return n
	}
	return -1
}

// ledgerRefs collects the ledger ids of the step's inputs: its dependencies'
// entries (HandoffKeyStepDependencies) and its bound inputs (HandoffKeyStepInputs),
// as the kernel stamped them on the handoff context.
func ledgerRefs(in *domain.Handoff) []string {
	var refs []string
	for _, src := range []map[string]string{in.Context} {
		if src == nil {
			continue
		}
		if deps, err := domain.DecodeStepDependencies(src[domain.HandoffKeyStepDependencies]); err == nil {
			for _, d := range deps {
				if domain.IsLedgerID(d.CID) {
					refs = append(refs, d.CID)
				}
			}
		}
		if raw := strings.TrimSpace(src[domain.HandoffKeyStepInputs]); raw != "" {
			var vals []domain.StepInputValue
			if json.Unmarshal([]byte(raw), &vals) == nil {
				for _, v := range vals {
					if domain.IsLedgerID(v.Ref) {
						refs = append(refs, v.Ref)
					}
				}
			}
		}
	}
	return refs
}
