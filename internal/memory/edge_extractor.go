package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// streamingGenerator is the optional streaming capability a Generator may
// implement. The EdgeExtractor prefers it over Generate for batched calls
// because streaming responses are NOT subject to http.Client.Timeout
// (the streaming client omits the timeout so a slow reasoning model can
// stream its body without being killed mid-response). The fallback
// Generate is fine for test fakes and local Ollama.
type streamingGenerator interface {
	GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error)
}

// EntityMetaKind is the small closed set of meta-kinds the extractor assigns. Not
// a constraint on the entity set — the entity NAME is open; the meta-kind is a
// routing hint for the recall path.
type EntityMetaKind string

const (
	MetaKindNamed   EntityMetaKind = "named"   // proper nouns: people, places, orgs, products, characters
	MetaKindLocated EntityMetaKind = "located" // paths, URLs, endpoints, table names, file system entries
	MetaKindValued  EntityMetaKind = "valued"  // versions, dates, IDs, counts, ticket numbers
	MetaKindConcept EntityMetaKind = "concept" // abstract: topics, methods, ideas, categories, skills
)

// ValidMetaKinds is the closed set the recall path trusts; anything else is dropped.
var ValidMetaKinds = map[EntityMetaKind]bool{
	MetaKindNamed: true, MetaKindLocated: true, MetaKindValued: true, MetaKindConcept: true,
}

// ExtractedEntity is one entity the LLM emitted.
type ExtractedEntity struct {
	Kind       EntityMetaKind
	Name       string
	Confidence float64 // [0,1]; below costGate the entity is dropped
}

// ExtractedRelation is one typed edge the LLM emitted. The Label is FREE-FORM
// (the LLM emits whatever verb phrase fits — "imports", "is_friend_of", "caused").
// The recall path does NOT branch on Label; it traverses every edge weighted by
// confidence.
type ExtractedRelation struct {
	Source     string  // canonical key: "{kind}:{lowercased-name}"
	Target     string  // canonical key
	Label      string  // free-form verb phrase
	Confidence float64
}

// Extraction is what the LLM emits for one fact. The LLM is free to emit any
// number of entities and relations; the writer normalizes + gates + writes.
type Extraction struct {
	Entities  []ExtractedEntity
	Relations []ExtractedRelation
}

// EdgeExtractor is the LLM-based entity+relation extractor. It runs once per
// IngestMemory call. The LLM is the same model Tier-2 uses (cheap, fast).
type EdgeExtractor struct {
	gen       domain.Generator
	costGate  float64 // entities / relations below this confidence are dropped (default 0.5)
	maxOutput int     // output token cap; default 1024
}

// NewEdgeExtractor builds the extractor. costGate below 0 disables the gate.
func NewEdgeExtractor(gen domain.Generator) *EdgeExtractor {
	return &EdgeExtractor{gen: gen, costGate: 0.5, maxOutput: 1024}
}

// SetCostGate overrides the per-entity / per-relation confidence gate.
func (e *EdgeExtractor) SetCostGate(g float64) { e.costGate = g }

// SetMaxOutput overrides the output token cap.
func (e *EdgeExtractor) SetMaxOutput(n int) { e.maxOutput = n }

// extractorOutput is the raw LLM response shape.
type extractorOutput struct {
	Entities []struct {
		Kind       string  `json:"kind"`
		Name       string  `json:"name"`
		Confidence float64 `json:"confidence"`
	} `json:"entities"`
	Relations []struct {
		Source     string  `json:"source"`
		Target     string  `json:"target"`
		Label      string  `json:"label"`
		Confidence float64 `json:"confidence"`
	} `json:"relations"`
}

const edgeExtractionRole = "You are an entity and relationship extractor for a long-term memory system. " +
	"For the input fact, emit ONLY a JSON object with 'entities' and 'relations' lists."

const edgeExtractionRules = `ENTITIES — every noun phrase that is a distinct referent the memory is about:
  kind = "named"    for proper nouns: people, places, orgs, products, characters
  kind = "located"  for paths, URLs, endpoints, table names, file system entries
  kind = "valued"   for versions, dates, IDs, counts, ticket numbers
  kind = "concept"  for abstract topics, methods, ideas, categories, skills
  name = the literal entity string the input text uses (preserve case for paths/URLs, lowercase proper nouns)
  confidence = [0,1] how confident you are this is a real entity

RELATIONS — every observed arc between two entities (or between a fact and an entity):
  source, target = the canonical key: "{kind}:{lowercase-name}" or "{kind}:{value}"
  label = a short verb phrase describing the relationship: "researched", "imports", "is_friend_of", "caused"
  confidence = [0,1] how confident you are this arc is real

EXAMPLES:
  Input: "Caroline chose an LGBTQ-friendly adoption agency for her future family."
  Output: {
    "entities": [
      {"kind":"named","name":"Caroline","confidence":0.95},
      {"kind":"named","name":"LGBTQ-friendly adoption agency","confidence":0.80},
      {"kind":"concept","name":"adoption","confidence":0.85}
    ],
    "relations": [
      {"source":"named:caroline","target":"concept:adoption","label":"researched","confidence":0.70}
    ]
  }

  Input: "src/auth/login.ts imports lib/oauth.ts and calls PostgresAdapter.query()."
  Output: {
    "entities": [
      {"kind":"located","name":"src/auth/login.ts","confidence":0.99},
      {"kind":"located","name":"lib/oauth.ts","confidence":0.99},
      {"kind":"concept","name":"PostgresAdapter","confidence":0.90}
    ],
    "relations": [
      {"source":"located:src/auth/login.ts","target":"located:lib/oauth.ts","label":"imports","confidence":0.95},
      {"source":"located:src/auth/login.ts","target":"concept:postgresadapter","label":"calls","confidence":0.85}
    ]
  }

RULES:
- Extract EVERY observed entity and relation. Don't summarize; don't drop "obvious" ones.
- If you're not sure whether something is a real entity or relation, emit it with lower confidence rather than dropping it.
- Don't invent entities or relations that aren't in the input text.
- Output ONLY the JSON object. No prose, no markdown, no explanation.`

const edgeExtractionSchema = `{
  "type": "object",
  "properties": {
    "entities": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind":       {"type": "string", "enum": ["named", "located", "valued", "concept"]},
          "name":       {"type": "string"},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        },
        "required": ["kind", "name", "confidence"]
      }
    },
    "relations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "source":     {"type": "string"},
          "target":     {"type": "string"},
          "label":      {"type": "string"},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        },
        "required": ["source", "target", "label", "confidence"]
      }
    }
  },
  "required": ["entities", "relations"]
}`

// Extract runs one LLM call and returns the parsed extraction. Best-effort: a
// failed LLM call or an unparseable response returns an empty Extraction (the
// caller decides whether to log + continue, or treat as a write failure).
func (e *EdgeExtractor) Extract(ctx context.Context, fact string) (Extraction, error) {
	if e.gen == nil || fact == "" {
		return Extraction{}, nil
	}
	prompt := e.buildPrompt(fact)
	resp, err := e.gen.Generate(ctx, prompt)
	if err != nil {
		return Extraction{}, fmt.Errorf("edge extractor: LLM call failed: %w", err)
	}
	var raw extractorOutput
	if err := json.Unmarshal([]byte(extractFirstJSON(resp)), &raw); err != nil {
		slog.Warn("EdgeExtractor: unparseable response, dropping extraction",
			"err", err, "fact_prefix", firstN(fact, 80))
		return Extraction{}, nil
	}
	return e.normalize(raw), nil
}

// ExtractBatch runs ONE LLM call and returns one Extraction per input fact
// (parallel slice). Empty facts are skipped and produce an empty Extraction
// at the matching index. The prompt is a batched variant of buildPrompt:
// facts are joined with --- separators in a <Context> section, and the
// output schema is a JSON array.
//
// Prefers streaming when the underlying Generator implements it: the
// streaming path is not subject to the LLM client's http.Client.Timeout
// (a critical win for hosted reasoning models that can take 2-4 minutes
// to stream a body). The non-streaming fallback is used otherwise (test
// fakes, local Ollama with short prompts).
//
// Failure modes (best-effort, never fatal):
//   - LLM call fails: every input gets an empty Extraction + a single
//     WARN log (no per-fact log spam).
//   - Response unparseable: same — every input gets empty Extraction.
//   - Response has fewer items than input: tail is filled with empty
//     Extraction (so the caller can still index the leading docs).
//   - Response has MORE items than input: tail is dropped (defensive).
//
// Cost gate and per-fact normalization are applied identically to the
// single-fact path, so the output is interchangeable.
func (e *EdgeExtractor) ExtractBatch(ctx context.Context, facts []string) []Extraction {
	out := make([]Extraction, len(facts))
	if e.gen == nil || len(facts) == 0 {
		return out
	}
	// Build the batched prompt: filter blanks, remember the index map.
	type indexEntry struct{ originalIdx int }
	var nonEmpty []string
	var indexMap []indexEntry
	for i, f := range facts {
		if strings.TrimSpace(f) == "" {
			continue
		}
		nonEmpty = append(nonEmpty, f)
		indexMap = append(indexMap, indexEntry{originalIdx: i})
	}
	if len(nonEmpty) == 0 {
		return out
	}
	prompt := e.buildBatchPrompt(nonEmpty)
	resp, err := e.callLLM(ctx, prompt)
	if err != nil {
		slog.Warn("EdgeExtractor: batch LLM call failed, dropping entire batch",
			"err", err, "batch_size", len(nonEmpty))
		return out
	}
	var rawList []extractorOutput
	if err := json.Unmarshal([]byte(extractFirstJSONArray(resp)), &rawList); err != nil {
		slog.Warn("EdgeExtractor: unparseable batch response, dropping entire batch",
			"err", err, "batch_size", len(nonEmpty))
		return out
	}
	// Map each parsed extraction back to its original input index.
	for j, raw := range rawList {
		if j >= len(indexMap) {
			break // defensive: extra items dropped
		}
		out[indexMap[j].originalIdx] = e.normalize(raw)
	}
	return out
}

// callLLM invokes the LLM, preferring streaming when the Generator supports
// it. The streaming path concatenates chunks and returns the full text; it
// is NOT subject to http.Client.Timeout, so a slow reasoning model can
// stream its body without being killed mid-response. The non-streaming
// fallback is bounded by the Generator's http.Client.Timeout (typical
// 30-180s), which is plenty for test fakes and local Ollama.
func (e *EdgeExtractor) callLLM(ctx context.Context, prompt string) (string, error) {
	if sg, ok := e.gen.(streamingGenerator); ok {
		return e.callLLMStream(ctx, sg, prompt)
	}
	return e.gen.Generate(ctx, prompt)
}

// callLLMStream consumes a streaming response and returns the concatenated
// text. A non-final chunk that contains a stream_error is propagated as
// the error.
func (e *EdgeExtractor) callLLMStream(ctx context.Context, sg streamingGenerator, prompt string) (string, error) {
	ch, err := sg.GenerateStream(ctx, prompt)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for c := range ch {
		if c.Text != "" {
			// The streaming client may emit a "stream_error: ..." line on
			// failure; surface it as a real error so the batcher's WARN
			// log includes the underlying cause.
			if strings.HasPrefix(c.Text, "stream_error: ") {
				return "", fmt.Errorf("%s", strings.TrimPrefix(c.Text, "stream_error: "))
			}
			sb.WriteString(c.Text)
		}
		if c.IsFinal {
			break
		}
	}
	return sb.String(), nil
}

// normalize maps the LLM output into the canonical Extraction shape: enforces
// the meta-kind enum, lowercases proper-noun names, drops below-gate items.
func (x *EdgeExtractor) normalize(raw extractorOutput) Extraction {
	out := Extraction{Entities: make([]ExtractedEntity, 0, len(raw.Entities)),
		Relations: make([]ExtractedRelation, 0, len(raw.Relations))}
	for _, e := range raw.Entities {
		kind := EntityMetaKind(strings.ToLower(strings.TrimSpace(e.Kind)))
		if !ValidMetaKinds[kind] {
			continue
		}
		if x.costGate > 0 && e.Confidence < x.costGate {
			continue
		}
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		// Preserve case for paths/URLs (the canonical form is the literal text);
		// lowercase proper-noun names so "Caroline" and "caroline" hit the same key.
		if kind == MetaKindNamed || kind == MetaKindConcept {
			name = strings.ToLower(name)
		}
		out.Entities = append(out.Entities, ExtractedEntity{
			Kind: kind, Name: name, Confidence: clamp01(e.Confidence),
		})
	}
	for _, r := range raw.Relations {
		if x.costGate > 0 && r.Confidence < x.costGate {
			continue
		}
		src := strings.TrimSpace(r.Source)
		tgt := strings.TrimSpace(r.Target)
		lbl := strings.TrimSpace(r.Label)
		if src == "" || tgt == "" {
			continue
		}
		if src == tgt {
			continue // self-loops add no signal
		}
		// The LLM sometimes emits "{kind}:{name}" in the wrong case; lowercase
		// the prefix so a recall-key lookup matches.
		src = normalizeKey(src)
		tgt = normalizeKey(tgt)
		out.Relations = append(out.Relations, ExtractedRelation{
			Source: src, Target: tgt, Label: lbl, Confidence: clamp01(r.Confidence),
		})
	}
	return out
}

// normalizeKey lowercases the meta-kind prefix of a canonical key, preserving
// the rest of the string. "Named:Caroline" → "named:Caroline".
func normalizeKey(k string) string {
	if i := strings.IndexByte(k, ':'); i > 0 {
		return strings.ToLower(k[:i]) + k[i:]
	}
	return k
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// extractFirstJSON pulls the first balanced JSON object from the LLM response
// (the model sometimes wraps the object in prose despite the prompt).
func extractFirstJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return s
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:] // unbalanced; let json.Unmarshal surface the error
}

// extractFirstJSONArray pulls the first balanced JSON array from the LLM
// response. Returns "" if no leading '[' is found. The batch extractor
// uses this to parse responses like [{...}, {...}, ...] that the model
// sometimes wraps in prose. Mirrors extractFirstJSON's brace-tracking with
// array tracking.
func extractFirstJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:] // unbalanced; let json.Unmarshal surface the error
}

func (e *EdgeExtractor) buildPrompt(fact string) string {
	return domain.PromptBuild(
		domain.PromptSystem(edgeExtractionRole, edgeExtractionRules),
		domain.PromptContext(fact),
		domain.PromptTask("Extract entities and relations from the fact above."),
		domain.PromptOutputSchemaJSON(edgeExtractionSchema),
	)
}

// buildBatchPrompt produces the batched variant: a <Context> block holding
// N facts separated by "---", and an output schema that is a JSON array of
// the single-fact shape. Indexing is positional: position i of the output
// array corresponds to position i of the input list.
func (e *EdgeExtractor) buildBatchPrompt(facts []string) string {
	var ctx strings.Builder
	for i, f := range facts {
		if i > 0 {
			ctx.WriteString("\n\n---\n\n")
		}
		ctx.WriteString(f)
	}
	batchSchema := `{
  "type": "array",
  "items": ` + edgeExtractionSchema + `
}`
	return domain.PromptBuild(
		domain.PromptSystem(edgeExtractionRole, edgeExtractionRules),
		domain.PromptContext(ctx.String()),
		domain.PromptTask("For each fact above (in order, separated by ---), emit one extraction object in the array. Output ONLY the JSON array."),
		domain.PromptOutputSchemaJSON(batchSchema),
	)
}

// Register the prompt at init so it shows up in the prompt registry (ADR-0019).
func init() {
	h := domain.PromptHashOf(edgeExtractionRole + edgeExtractionRules + edgeExtractionSchema)
	domain.PromptRegistry[h] = domain.PromptEntry{
		ID:      "memory.edge_extraction",
		Version: "1.0.0",
		Hash:    h,
		Schema:  edgeExtractionSchema,
	}
}
