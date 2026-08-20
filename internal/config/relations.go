package config

import (
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// The identity plane's verb vocabulary, as a DEPLOYMENT declares it
// (five-planes step 2, FIVE-PLANES-BUILD.md supervisor decision D-W5-1).
//
// A link verb is data — `subsidiary_of`, `assigned_to`, `supplied_by` are
// facts about a corpus, not about the kernel — but the RelationRegistry is
// immutable after boot, so somebody has to say the words before the store will
// accept a link that uses them. Until now the only sayer was a plugin
// (Registry.AddRelationSpecs), which is right for a verb the plugin ITSELF
// emits and wrong for every verb a deployment's sources happen to carry: a
// corpus with `parent_account_id` in it needed a Go change.
//
// So the vocabulary also comes from config. Deliberately NOT auto-declaration
// from whatever a mapping happens to name: a registry that grew a verb because
// a mapping used one would make "undeclared verb" unreachable, and the refusal
// is the whole point — it is what makes the vocabulary a decision somebody
// reviewed rather than a side effect of an author's typing. Confirming a
// mapping that names an undeclared verb refuses, and names this section.

// RelationConfig declares one link verb in `relations`. It is the config
// mirror of domain.RelationSpec, field for field, and carries no validation of
// its own: malformed entries — an unknown family, a redeclared built-in seed,
// the same verb twice — refuse the BOOT in domain.NewRelationRegistry, which
// is the one place that decides what a verb may be.
type RelationConfig struct {
	// Name is the verb as links spell it ("subsidiary_of"). Required.
	Name string `json:"name"`
	// Family is identity | relation | lineage — the family every link using
	// this verb must carry. Required.
	Family string `json:"family"`
	// Symmetric makes the verb hold in both directions.
	Symmetric bool `json:"symmetric,omitempty"`
	// Closure is "identity" | "rollup" | "": whether an entity-scoped read may
	// expand through this verb. Empty = never expand, which is the answer for
	// every verb that is not an equivalence.
	Closure string `json:"closure,omitempty"`
	// MaxPerEntity bounds the fan-out; 0 = unlimited.
	MaxPerEntity int `json:"max_per_entity,omitempty"`
}

// RelationSpecs renders the configured verbs for Options.RelationSpecs.
//
// Order is preserved so a boot refusal names the entry an operator wrote, in
// the position they wrote it. Whitespace is trimmed because a JSON list is
// hand-edited and a trailing space in a verb would otherwise produce a link
// nothing else could ever match; nothing else is normalised, because every
// other malformation is a decision the registry must make out loud.
func (c *Config) RelationSpecs() []domain.RelationSpec {
	if c == nil || len(c.Relations) == 0 {
		return nil
	}
	out := make([]domain.RelationSpec, 0, len(c.Relations))
	for _, r := range c.Relations {
		out = append(out, domain.RelationSpec{
			Name:         strings.TrimSpace(r.Name),
			Family:       strings.TrimSpace(r.Family),
			Symmetric:    r.Symmetric,
			Closure:      strings.TrimSpace(r.Closure),
			MaxPerEntity: r.MaxPerEntity,
		})
	}
	return out
}
