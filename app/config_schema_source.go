package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memory"
)

// ConfigSchemaVersion versions the tunable catalogue below. Bump it when a field
// is added, removed or changes meaning — a console caches the schema by hash and
// this is how it learns to refetch.
const ConfigSchemaVersion = "2" // 2: + execution.tools.tool_menu_k (2026-08-11)

// tunable is one numerically-editable kernel setting.
type tunable struct {
	// Key is the flat Koanf path, which is ALSO the provenance key. They must
	// match exactly or value_source silently reports nothing for the field.
	Key string
	// Param is the name SetRuntimeConfig accepts. Empty ⇒ the field is readable
	// but not writable at runtime, and lands in kernel_only_keys.
	Param       string
	Title       string
	Description string
	Min, Max    float64
	// Consequence is what actually changes if you move this. It is the product of
	// the settings screen: a number with a range tells an operator what is
	// permitted, not what it will do to them.
	Consequence string
	// Runtime marks a tunable with NO config-file key — it exists only as a live
	// value set through SetRuntimeConfig (the Stage-A blend weights, ADR-0054).
	//
	// Such a key is absent from every config layer, so the provenance tracker has
	// nothing to say about it. Reporting the tracker's silence as "default" would
	// be wrong in the one direction that matters: it would tell an operator a
	// hot-applied weight is untouched.
	Runtime bool
}

// tunables is the catalogue the operator console renders.
//
// Keys and defaults come from the kernel itself (internal/config and
// configs/tuning.json), not from a hand-maintained parallel list, so what the
// console writes back is what the kernel reads.
//
// Only the blend weights are runtime-writable today, because they are the only
// keys SetRuntimeConfig's effect handler applies (ADR-0054). Everything else is
// reported read-only rather than hidden: a field an operator cannot find is
// indistinguishable from one that does not exist, and "you must restart to change
// this" is a useful answer.
var tunables = []tunable{
	{
		Key: "execution.blend_weight_cosine", Param: "blend_weight_cosine", Runtime: true,
		Title: "Blend weight — cosine", Min: 0, Max: 1,
		Description: "Weight of embedding similarity in the Stage-A blend.",
		Consequence: "Raise it and retrieval follows semantic similarity more closely; lower it and lexical and graph signals decide more of the ranking.",
	},
	{
		Key: "execution.blend_weight_lexical", Param: "blend_weight_lexical", Runtime: true,
		Title: "Blend weight — lexical", Min: 0, Max: 1,
		Description: "Weight of BM25 lexical overlap in the Stage-A blend.",
		Consequence: "Raise it when exact terms matter (identifiers, error codes); it will not help a query whose gold document shares no vocabulary with it.",
	},
	{
		Key: "execution.blend_weight_coherence", Param: "blend_weight_coherence", Runtime: true,
		Title: "Blend weight — coherence", Min: 0, Max: 1,
		Description: "Weight of inter-chunk coherence in the Stage-A blend.",
		Consequence: "Favours chunks that agree with their neighbours, which suppresses isolated but correct answers as well as isolated noise.",
	},
	{
		Key: "execution.blend_weight_confidence", Param: "blend_weight_confidence", Runtime: true,
		Title: "Blend weight — confidence", Min: 0, Max: 1,
		Description: "Weight of stored confidence in the Stage-A blend.",
		Consequence: "Trusts memories the kernel already rated highly, which compounds an earlier mis-rating rather than correcting it.",
	},
	{
		Key: "execution.blend_weight_pagerank", Param: "blend_weight_pagerank", Runtime: true,
		Title: "Blend weight — PageRank", Min: 0, Max: 1,
		Description: "Weight of graph centrality in the Stage-A blend.",
		Consequence: "Promotes well-connected chunks. On a sparse or freshly-ingested corpus the graph is thin and this mostly adds noise.",
	},
	{
		Key: "execution.blend_weight_recency", Param: "blend_weight_recency", Runtime: true,
		Title: "Blend weight — recency", Min: 0, Max: 1,
		Description: "Weight of recency in the Stage-A blend.",
		Consequence: "Newer memories win ties. Raise it for changing operational facts; lower it for reference material, where the oldest write is often the right one.",
	},
	{
		Key: "execution.blend_weight_activation", Param: "blend_weight_activation", Runtime: true,
		Title: "Blend weight — activation", Min: 0, Max: 1,
		Description: "Weight of activation (recent access) in the Stage-A blend.",
		Consequence: "Reinforces what has been read lately, which makes retrieval feel responsive and can also lock it into a loop around the same documents.",
	},

	// Read-only below: real kernel keys, but SetRuntimeConfig does not apply them
	// live, so they are reported with their source and marked kernel-only.
	{
		Key: "execution.ewma_alpha", Min: 0, Max: 1,
		Title:       "EWMA alpha",
		Description: "Smoothing factor for the exponentially weighted moving averages behind agent merit.",
		Consequence: "Higher reacts faster to an agent's latest outcome and forgets its track record sooner. Restart required.",
	},
	{
		Key: "execution.gatekeeper_max_candidates", Min: 1, Max: 50,
		Title:       "Gatekeeper max candidates",
		Description: "How many agents survive the capability gate into merit scoring.",
		Consequence: "Wider considers more agents per step and costs more selection time. Restart required.",
	},
	{
		Key: "execution.memory_relevance_threshold", Min: 0, Max: 1,
		Title:       "Memory relevance threshold",
		Description: "Minimum score for a retrieved chunk to reach an agent.",
		Consequence: "Raise it and answers cite less but drop borderline evidence; lower it and agents reason over more near-misses. Restart required.",
	},
	{
		Key: "execution.tool_retrieval_floor", Min: 0, Max: 1,
		Title:       "Tool retrieval floor",
		Description: "Minimum similarity for a tool to be offered to an agent.",
		Consequence: "Too high and a capable tool is never offered; too low and agents are handed tools that do not fit the step. Restart required.",
	},
	{
		// NESTED path, unlike the flat v1 names above: this key postdates the
		// flat schema, so no legacy alias exists to migrate a flat spelling.
		Key: "execution.tools.tool_menu_k", Min: 0, Max: 20,
		Title:       "Tool menu size",
		Description: "How many tools/skills the agent SDK lists per menu query. 0 uses the SDK default (3).",
		Consequence: "Injected into every agent's environment at spawn, so a change reaches agents on their next boot. Larger menus ground better but cost prompt tokens on every step. Restart required.",
	},
	{
		Key: "execution.step_timeout_multiplier", Min: 0.1, Max: 20,
		Title:       "Step timeout multiplier",
		Description: "Scales the estimated latency to produce each step's timeout.",
		Consequence: "Too low kills slow-but-correct steps; too high lets a hung step hold the plan open. Restart required.",
	},
}

// configSchemaSource implements operator.ConfigSchemaReporter.
type configSchemaSource struct {
	cfg  *config.Config
	prov config.Provenance
	// live reads the CURRENT value of a runtime-applied tunable. Without it the
	// form would report the booted value for a key that has since been hot-applied
	// — showing a stale number as if it were live, which is the exact failure
	// GetConfigSchema exists to remove.
	live func(param string) (float64, bool)
}

func (c configSchemaSource) SchemaJSON() (schema, version, hash string) {
	props := map[string]any{}
	for _, t := range tunables {
		props[t.Key] = map[string]any{
			"type":        "number",
			"title":       t.Title,
			"description": t.Description,
			"minimum":     t.Min,
			"maximum":     t.Max,
			// Not a JSON Schema keyword — carried as an annotation because it is
			// the part of the field an operator actually needs.
			"x-consequence": t.Consequence,
			"x-writable":    t.Param != "",
		}
	}
	doc := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"type":       "object",
		"properties": props,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", ConfigSchemaVersion, ""
	}
	sum := sha256.Sum256(b)
	return string(b), ConfigSchemaVersion, hex.EncodeToString(sum[:8])
}

func (c configSchemaSource) EditableKeys() []string {
	var out []string
	for _, t := range tunables {
		if t.Param != "" {
			out = append(out, t.Key)
		}
	}
	sort.Strings(out)
	return out
}

func (c configSchemaSource) KernelOnlyKeys() []string {
	var out []string
	for _, t := range tunables {
		if t.Param == "" {
			out = append(out, t.Key)
		}
	}
	sort.Strings(out)
	return out
}

func (c configSchemaSource) CurrentValues() map[string]float64 {
	out := make(map[string]float64, len(tunables))
	for _, t := range tunables {
		// A hot-applied value wins over the booted one: it is what the kernel is
		// using right now, which is the question the form is asking.
		if t.Param != "" && c.live != nil {
			if v, ok := c.live(t.Param); ok {
				out[t.Key] = v
				continue
			}
		}
		if v, ok := c.bootValue(t.Key); ok {
			out[t.Key] = v
		}
	}
	return out
}

// bootValue reads a tunable from the merged config.
func (c configSchemaSource) bootValue(key string) (float64, bool) {
	if c.cfg == nil {
		return 0, false
	}
	e := c.cfg.Execution
	switch key {
	case "execution.ewma_alpha":
		return e.Supervision.EWMAAlpha, true
	case "execution.gatekeeper_max_candidates":
		return float64(e.Gatekeeper.GatekeeperMaxCandidates), true
	case "execution.memory_relevance_threshold":
		return e.Memory.MemoryRelevanceThreshold, true
	case "execution.tool_retrieval_floor":
		return e.Tools.ToolRetrievalFloor, true
	case "execution.tools.tool_menu_k":
		return float64(e.Tools.ToolMenuK), true
	case "execution.step_timeout_multiplier":
		return e.Plan.StepTimeoutMultiplier, true
	}
	return 0, false
}

// ValueSource reports which layer supplies each key.
//
// A key the provenance tracker does not know is reported as "default" rather than
// omitted: an absent entry would render as "unknown", and "unknown" for a key
// that is simply at its built-in default is a false alarm — the form would
// disclaim on every untouched field and the real pins would stop standing out.
func (c configSchemaSource) ValueSource() map[string]string {
	out := make(map[string]string, len(tunables))
	for _, t := range tunables {
		if t.Runtime {
			// No config layer holds these. "runtime" when a live value exists,
			// "default" when the blender has never been set.
			if c.live != nil {
				if _, ok := c.live(t.Param); ok {
					out[t.Key] = "runtime"
					continue
				}
			}
			out[t.Key] = config.SourceDefault
			continue
		}
		if src := c.prov.Source(t.Key); src != "" {
			out[t.Key] = src
			continue
		}
		// Absent from the tracker means no layer STATED the key. That happens for
		// a real config field whose Go tag carries `omitempty` and whose value is
		// the zero value (execution.tool_retrieval_floor is one): it vanishes from
		// the marshalled defaults layer. The kernel is genuinely using the Go
		// default, so "default" is the true answer, not "unknown".
		out[t.Key] = config.SourceDefault
	}
	return out
}

// liveBlendWeights adapts the hot-applied Stage-A weights to the schema's live
// lookup. Returns nil when no query service exists, in which case the form falls
// back to booted values — which is correct, because nothing can have hot-applied
// a weight on a kernel with no blender.
func liveBlendWeights(q blendReader) func(param string) (float64, bool) {
	if q == nil {
		return nil
	}
	return func(param string) (float64, bool) {
		w := q.CurrentBlendWeights()
		switch param {
		case "blend_weight_cosine":
			return w.Cosine, true
		case "blend_weight_lexical":
			return w.Lexical, true
		case "blend_weight_coherence":
			return w.GraphCoherence, true
		case "blend_weight_confidence":
			return w.Confidence, true
		case "blend_weight_pagerank":
			return w.PageRank, true
		case "blend_weight_recency":
			return w.Recency, true
		case "blend_weight_activation":
			return w.Activation, true
		}
		return 0, false
	}
}

// blendReader is the narrow read the schema needs from the memory stack.
type blendReader interface {
	CurrentBlendWeights() memory.BlendWeights
}
