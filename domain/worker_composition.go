package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// ADR-0145 D12: workers composed by the kernel.
//
// A plan step is no longer run by a pre-built agent chosen by capability. The
// kernel composes a worker for the step: ONE ReAct runtime, the skills of the
// professions that cover the step, the tools those professions may use that the
// act-for person's scope also allows (ADR-0145 D14: not narrowed by the step's effects), the step's
// brief, and the ledger references of its inputs. The composition is bound to the
// step's lease, where the kernel enforces it (tools: a per-lease ceiling held over
// the unrestricted bypass and the skill overlay; skills: ListSkills serves nothing
// outside the set and never opens an access request for one), so the model may
// choose among what it was given and narrow it, never widen it. Every composition
// is recorded in the run ledger (kind "composition").
//
// The system prompt stays developer-written (the runtime's own prompt); the brief
// travels as data (HandoffKeyComposition, and the task text).
// ─────────────────────────────────────────────────────────────────────────────

// HandoffKeyComposition carries the step's WorkerComposition (JSON) to the
// composed runtime. Kernel-authored and DATA: the runtime renders it as its menu
// and its brief, never as instructions; the kernel enforces it independently.
const HandoffKeyComposition = "_composition"

// LedgerComposition is the ledger kind of a recorded composition.
const LedgerComposition LedgerKind = "composition"

// WorkerComposition is what the kernel composed for one plan step.
type WorkerComposition struct {
	// Runtime is the agent id of the runtime that runs the worker.
	Runtime string `json:"runtime"`
	// Professions are the declarative profiles (agents/profiles) whose skills and
	// tool allowlists the worker was composed from, sorted.
	Professions []string `json:"professions"`
	// Skills is the worker's skill index (names, sorted): the procedures it may
	// load on demand with use_skill. Nothing outside it is ever served.
	Skills []string `json:"skills"`
	// Tools are the tool names the worker may call (sorted): the professions'
	// allowlists ∩ the act-for person's scope ∩ the live catalogue, minus tools
	// with resource arguments (compose.Composer.tools; the step's effects no longer narrow it — ADR-0145
	// D14 removed the per-step effect menu). Kernel tools (ledger, calculation)
	// are always available besides.
	Tools []string `json:"tools"`
	// Brief is the step's task text (data).
	Brief string `json:"brief"`
	// LedgerRefs are the ledger ids of the step's inputs (its dependencies' and
	// bound inputs' entries), sorted.
	LedgerRefs []string `json:"ledger_refs,omitempty"`
	// Model is the runtime's model preference, "" for the gateway's allocation.
	Model string `json:"model,omitempty"`
	// SystemPrompt names the developer-written system prompt the runtime uses.
	SystemPrompt string `json:"system_prompt"`
	// RunID and StepIndex locate the step.
	RunID     string `json:"run_id,omitempty"`
	StepIndex int    `json:"step"`
}

// Normalize sorts and de-duplicates the set fields, so equal compositions encode
// to equal bytes (deterministic ids, AGENTS.md §2.6).
func (c WorkerComposition) Normalize() WorkerComposition {
	c.Professions = sortedUnique(c.Professions)
	c.Skills = sortedUnique(c.Skills)
	c.Tools = sortedUnique(c.Tools)
	c.LedgerRefs = sortedUnique(c.LedgerRefs)
	return c
}

// ID is the composition's content address: cmp-<hex of sha256(normalized JSON)>[:24].
func (c WorkerComposition) ID() string {
	b, _ := json.Marshal(c.Normalize())
	h := sha256.Sum256(b)
	return "cmp-" + hex.EncodeToString(h[:])[:24]
}

// Encode renders the normalized composition for HandoffKeyComposition.
func (c WorkerComposition) Encode() string {
	b, _ := json.Marshal(c.Normalize())
	return string(b)
}

// DecodeWorkerComposition reads HandoffKeyComposition; ok=false when absent or malformed.
func DecodeWorkerComposition(s string) (WorkerComposition, bool) {
	if s == "" {
		return WorkerComposition{}, false
	}
	var c WorkerComposition
	if json.Unmarshal([]byte(s), &c) != nil || c.Runtime == "" {
		return WorkerComposition{}, false
	}
	return c, true
}

// HasTool reports whether tool is inside the composition.
func (c WorkerComposition) HasTool(tool string) bool { return containsString(c.Tools, tool) }

// HasSkill reports whether skill is inside the composition.
func (c WorkerComposition) HasSkill(skill string) bool { return containsString(c.Skills, skill) }

func sortedUnique(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x == "" {
			continue
		}
		if _, dup := seen[x]; dup {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ComposedLeases holds the composition bound to each live step lease. It is the
// kernel's enforcement point for D12: a LeaseToolCeiling for the tool executor
// and the skill bound ListSkills applies. Entries go when the lease completes
// (Forget, from the gateway's completion hook). Safe for concurrent use.
type ComposedLeases struct {
	mu      sync.RWMutex
	byLease map[string]WorkerComposition
}

// NewComposedLeases builds an empty registry.
func NewComposedLeases() *ComposedLeases {
	return &ComposedLeases{byLease: map[string]WorkerComposition{}}
}

var _ LeaseToolCeiling = (*ComposedLeases)(nil)

// Bind attaches c to lease, replacing any earlier composition of that lease.
func (r *ComposedLeases) Bind(lease LeaseID, c WorkerComposition) {
	if r == nil || lease == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byLease[string(lease)] = c.Normalize()
}

// Forget drops the lease's composition.
func (r *ComposedLeases) Forget(lease LeaseID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byLease, string(lease))
}

// For returns the lease's composition.
func (r *ComposedLeases) For(lease string) (WorkerComposition, bool) {
	if r == nil || lease == "" {
		return WorkerComposition{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byLease[lease]
	return c, ok
}

// AllowsLease implements LeaseToolCeiling: a composed lease may use only its
// composition's tools; any other lease is not bound here.
func (r *ComposedLeases) AllowsLease(lease, tool string) (inside, bound bool) {
	c, ok := r.For(lease)
	if !ok {
		return false, false
	}
	return c.HasTool(tool), true
}

// AllowsSkill reports, for a composed lease, whether skill is in its index.
func (r *ComposedLeases) AllowsSkill(lease, skill string) (inside, bound bool) {
	c, ok := r.For(lease)
	if !ok {
		return false, false
	}
	return c.HasSkill(skill), true
}

// leaseCeilings composes several per-lease ceilings: a lease is bound when any
// of them binds it, and a tool is inside only when every binding ceiling admits it.
type leaseCeilings []LeaseToolCeiling

func (cs leaseCeilings) AllowsLease(lease, tool string) (inside, bound bool) {
	inside = true
	for _, c := range cs {
		in, b := c.AllowsLease(lease, tool)
		if b {
			bound = true
			inside = inside && in
		}
	}
	if !bound {
		return false, false
	}
	return inside, true
}

// ChainLeaseCeilings returns one ceiling enforcing all of cs (nil entries are
// skipped); nil when none remain. Used to hold the D12 subagent toolsets and the
// D12 compositions in the executor's single LeaseCeiling slot.
func ChainLeaseCeilings(cs ...LeaseToolCeiling) LeaseToolCeiling {
	var out leaseCeilings
	for _, c := range cs {
		if c == nil {
			continue
		}
		if p, ok := c.(*ComposedLeases); ok && p == nil {
			continue
		}
		out = append(out, c)
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}
