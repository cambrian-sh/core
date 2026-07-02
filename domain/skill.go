package domain

import (
	"context"
	"strings"
	"sync"
)

// Skill is an authored procedural capability (ADR-0046 D1): instructions an agent
// loads to steer its execution, plus a bundle of tool grants the load activates
// for the run. A tool is an executable leaf; a Skill is a context + grant
// mutation. Distinct from A2ASkill (the A2A AgentCard declaration). Authored and
// instruction-only in v1 — no executable scripts (D8).
type Skill struct {
	Name         string   // identity
	Description  string   // Tier-1 one-liner source (the shared deriveSummary)
	Instructions string   // SKILL.md body — Tier-2, injected on use_skill
	ToolGrants   []string // bundled tools, activated run-scoped on load (D6)
	// ScopeTags are the skill's classification labels (ADR-0046 D9, option A):
	// stored as the skill document's metadata.tags, so an agent retrieves the
	// skill only when its ADR-0034 EffectiveScope.Allows these tags — the same
	// read path as memory, no new permission logic.
	ScopeTags []string
}

// SkillRegistry is the kernel-owned catalog of system skills (ADR-0046 D2).
// Agent skills are NOT here — they are SDK-local (the @tools analog).
type SkillRegistry interface {
	Register(s Skill)
	Get(name string) (Skill, bool)
	All() []Skill
}

// InMemorySkillRegistry is the system-skill catalog; the file-based discovery
// (internal/skill/discovery) loads SKILL.md files into one of these at startup.
type InMemorySkillRegistry struct {
	mu     sync.RWMutex
	skills map[string]Skill
}

// NewInMemorySkillRegistry constructs an empty registry.
func NewInMemorySkillRegistry() *InMemorySkillRegistry {
	return &InMemorySkillRegistry{skills: map[string]Skill{}}
}

func (r *InMemorySkillRegistry) Register(s Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[s.Name] = s
}

func (r *InMemorySkillRegistry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// All returns every registered system skill (unordered).
func (r *InMemorySkillRegistry) All() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	return out
}

// BuildSkillDoc assembles the deterministic embedding document for a system skill
// (ADR-0046 D4): name + the Tier-1 short summary, document-prefixed for the
// asymmetric embedder side. Reuses the ADR-0045 deriver (deriveSummary) so the
// embedded vector matches the served Tier-1 menu line. Pure — no LLM.
func BuildSkillDoc(s Skill) string {
	var b strings.Builder
	b.WriteString("search_document: ")
	b.WriteString(s.Name)
	if sum := deriveSummary(s.Name, s.Description); sum != "" {
		b.WriteString(". ")
		b.WriteString(sum)
	}
	return b.String()
}

// SkillDisclosure renders a skill's served fields for ListSkills (ADR-0046 D4),
// the skill analog of ToolDisclosure. full=false (Tier-1, the always-served menu):
// the short summary only. full=true (Tier-2, use_skill): summary + the full
// instructions + the bundled tool grants. Pure; reuses the ADR-0045 deriver so
// the menu line matches the embedded vector.
func SkillDisclosure(s Skill, full bool) (description, instructions string, toolGrants []string) {
	description = deriveSummary(s.Name, s.Description)
	if full {
		instructions = s.Instructions
		toolGrants = s.ToolGrants
	}
	return description, instructions, toolGrants
}

// SkillVisible reports whether an agent's effective scope permits loading a skill
// (ADR-0046 D9, option A): the skill's ScopeTags are treated as the document's
// classification labels, gated by EffectiveScope.Allows — the same relation
// memory uses. A nil scope is fail-closed (deny); ScopeSystem permits all. Used
// for the registry-based (named / full-menu) paths; the ranked path delegates
// the identical check to the store via SearchOptions.Scope.
func SkillVisible(eff *EffectiveScope, s Skill) bool {
	if eff == nil {
		return false
	}
	if eff.System {
		return true
	}
	return eff.Allows(s.ScopeTags)
}

// SkillIndexer embeds a system skill's document and upserts it as a DocTypeSkill
// keyed by name (ADR-0046 D2/D4) — the SkillRetriever ranks over these. Mirrors
// ToolIndexer. Called at boot (discovery) and on re-index.
type SkillIndexer struct {
	Store    VectorStore
	Embedder Embedder
}

// Index embeds and upserts one skill's document (keyed by skill name).
func (ix *SkillIndexer) Index(ctx context.Context, s Skill) error {
	doc := BuildSkillDoc(s)
	vec, err := ix.Embedder.Embed(ctx, doc)
	if err != nil {
		return err
	}
	return ix.Store.Save(ctx, &Document{
		ID:           s.Name,
		DocumentType: DocTypeSkill,
		Text:         doc,
		Embedding:    Embedding{Vector: vec},
		// ADR-0046 D9: the skill's scope tags are its classification labels under
		// metadata.tags, which the ADR-0034 scope predicate filters on (the same
		// key memory uses). An unscoped skill carries an empty tag set.
		Metadata: map[string]any{"tags": skillTags(s.ScopeTags)},
	})
}

// skillTags normalizes a nil tag slice to an empty slice so the indexed
// metadata.tags is always a well-defined JSON array for the scope predicate.
func skillTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// IndexAll indexes a batch of skills best-effort, returning the first error.
func (ix *SkillIndexer) IndexAll(ctx context.Context, skills []Skill) error {
	var firstErr error
	for _, s := range skills {
		if err := ix.Index(ctx, s); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Remove drops a skill's document from the index.
func (ix *SkillIndexer) Remove(ctx context.Context, name string) error {
	return ix.Store.Delete(ctx, name)
}
