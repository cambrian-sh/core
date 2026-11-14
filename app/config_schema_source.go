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
// 3: + per-property "x-param" / "x-runtime" annotations (2026-11-14, ENG-1456).
// 4: + the ADR-0136 fact layer (execution.facts.*) and boolean fields
// (2026-11-15, T-20261103-execution-facts-is-absent-from-the-operator).
const ConfigSchemaVersion = "4" // 2: + execution.tools.tool_menu_k (2026-08-11)

// tunable is one kernel setting the console can show (and, with Param, edit).
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
	// Bool marks an on/off switch. Its schema type is "boolean" and its current
	// value is reported as 1 (on) or 0 (off), because current_values is a map of
	// numbers on the wire. Min/Max are ignored for it.
	Bool bool
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

	// The ADR-0136 fact layer (execution.facts.*). Read-only: the extractor, the
	// lane and the supersession seeding are wired at boot, and SetRuntimeConfig
	// applies none of them, so each one is a restart.
	{
		Key: "execution.facts.extract_enabled", Bool: true,
		Title:       "Fact extraction",
		Description: "Extract facts from every ingested document with the facts generator, after its chunks are stored.",
		Consequence: "On: every ingested document costs LLM calls (bounded by the budgets below) and feeds the fact lane. Off: no new facts; existing ones stay. Restart required.",
	},
	{
		Key: "execution.facts.lane_enabled", Bool: true,
		Title:       "Fact lane in retrieval",
		Description: "Search the stored facts at query time and add their source chunks as hits.",
		Consequence: "On: questions phrased differently from the source text still reach it through its facts. Off: retrieval ignores facts entirely. Restart required.",
	},
	{
		Key: "execution.facts.supersession_enabled", Bool: true,
		Title:       "Fact supersession",
		Description: "Close an older fact value by valid time when a newer one arrives, and rank current/latest questions by the current value.",
		Consequence: "On: \"what is X now\" prefers the latest value; history is kept, never deleted. Off: every value ranks alike. Restart required.",
	},
	{
		Key: "execution.facts.assessments_enabled", Bool: true,
		Title:       "Fact supersession judge",
		Description: "Ask the model whether two facts about the same attribute conflict, and learn attribute cardinality. Needs supersession.",
		Consequence: "On: catches conflicts the cue words miss, at one batched LLM call per new fact group. Off: only exact keys and change-language cues supersede. Restart required.",
	},
	{
		Key: "execution.facts.max_calls_per_document", Min: 0, Max: 1000,
		Title:       "Extraction calls per document",
		Description: "How many extraction windows one ingest may spend. 0 = no cap.",
		Consequence: "Higher covers more of a long document and costs more LLM calls per ingest; the rest of a capped document is marked partially covered. Restart required.",
	},
	{
		Key: "execution.facts.max_facts_per_document", Min: 1, Max: 10000,
		Title:       "Facts per extraction window",
		Description: "The most facts kept from one extraction window.",
		Consequence: "Lower drops the tail of a dense window; higher keeps more, including weaker facts. Restart required.",
	},
	{
		Key: "execution.facts.max_facts_per_source", Min: 1, Max: 100000,
		Title:       "Facts per source",
		Description: "The most facts kept for one source document across all its windows.",
		Consequence: "Once reached, the rest of the source is not extracted. Raise it for long reference documents. Restart required.",
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
	// overrides lists the durable store's overrides against the config files as they
	// are on disk now (config.StoreOverrideReport). nil ⇒ no store: no overrides.
	overrides func() []config.StoreOverride
}

// FileOverrides implements operator.ConfigSchemaReporter (ADR-0101 D8 read side,
// contract 0104): for each catalogued tunable the store overrides and a config file
// also states, the file layer and the file's value — what the console needs to say
// "stored value; overrides tuning.json (file value 0.3)" without a write ack.
func (c configSchemaSource) FileOverrides() (files, values map[string]string) {
	if c.overrides == nil {
		return nil, nil
	}
	known := make(map[string]bool, len(tunables))
	for _, t := range tunables {
		known[t.Key] = true
	}
	for _, o := range c.overrides() {
		if !known[o.Key] || o.File == "" {
			continue
		}
		if files == nil {
			files, values = map[string]string{}, map[string]string{}
		}
		files[o.Key] = o.File
		if !o.Secret {
			values[o.Key] = config.DisplayValue(o.FileValue, false)
		}
	}
	return files, values
}

func (c configSchemaSource) SchemaJSON() (schema, version, hash string) {
	props := map[string]any{}
	for _, t := range tunables {
		prop := map[string]any{
			"type":        "number",
			"title":       t.Title,
			"description": t.Description,
			"minimum":     t.Min,
			"maximum":     t.Max,
			// Not a JSON Schema keyword — carried as an annotation because it is
			// the part of the field an operator actually needs.
			"x-consequence": t.Consequence,
			"x-writable":    t.Param != "",
			// The name SetRuntimeConfig takes for this field ("" when not
			// writable) and whether it is a live-only runtime value. Published so
			// a client (the benchmark harness's operator sweep, ENG-1456) reads the
			// runtime-settable params from the kernel instead of keeping a copy or
			// re-deriving the name from the config key. Annotations, not a proto
			// field: schema_json is already the per-field description.
			"x-param":   t.Param,
			"x-runtime": t.Runtime,
		}
		if t.Bool {
			// current_values reports it as 1/0 (a numeric map on the wire).
			prop["type"] = "boolean"
			delete(prop, "minimum")
			delete(prop, "maximum")
		}
		props[t.Key] = prop
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
	case "execution.facts.extract_enabled":
		return boolNum(e.Facts.ExtractEnabled), true
	case "execution.facts.lane_enabled":
		return boolNum(e.Facts.LaneEnabled), true
	case "execution.facts.supersession_enabled":
		return boolNum(e.Facts.SupersessionEnabled), true
	case "execution.facts.assessments_enabled":
		return boolNum(e.Facts.AssessmentsEnabled), true
	case "execution.facts.max_calls_per_document":
		return float64(e.Facts.MaxCallsPerDocument), true
	case "execution.facts.max_facts_per_document":
		return float64(e.Facts.MaxFactsPerDocument), true
	case "execution.facts.max_facts_per_source":
		return float64(e.Facts.MaxFactsPerSource), true
	}
	return 0, false
}

// boolNum reports a switch in current_values' numeric form: 1 on, 0 off.
func boolNum(b bool) float64 {
	if b {
		return 1
	}
	return 0
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
