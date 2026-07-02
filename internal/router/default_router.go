package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// layer2Keywords maps each DecisionType to its precompiled word-boundary regex.
// Precompiled at init time — never per-call.
// Ingestion is intentionally absent: storing content into LTM is an automatic
// memory-subsystem function (IngestionManager / /v1/ingest / SynapticWatcher),
// not a user-input routing outcome. Keywords like "import"/"upload" therefore
// fall through to Layer 3 and are judged as chat/plan/watch on their actual
// intent, rather than being force-routed to the (agents-can't-write-LTM) ingest path.
var layer2Keywords = map[domain.DecisionType]*regexp.Regexp{
	domain.DecisionWatch: regexp.MustCompile(`(?i)\b(watch|monitor|track|alert)\b`),
	domain.DecisionPlan:  regexp.MustCompile(`(?i)\b(plan|execute|run)\b`),
}

// classifySchema is the static JSON Schema for the Layer 3 classification prompt.
// Registered in PromptRegistry at init() time.
const classifySchema = `{
  "decision": "chat|plan|watch",
  "confidence": 0.0,
  "alternatives": [{"decision": "chat|plan|watch", "confidence": 0.0}],
  "reason": "string"
}`

// classifyRole is the persona line of the Layer 3 prompt.
const classifyRole = `You are Cambrian's intent classifier. Decide what the user wants Cambrian to DO with their input and choose exactly one decision type.`

// classifyConstraints defines each decision type behaviorally with a decisive
// discriminator. Without these definitions the model only sees bare enum values
// and conflates "produce a summary file" (a task → plan) with answering directly
// (chat). Ingestion is deliberately NOT a decision type here: storing content into
// long-term memory is an automatic memory-subsystem function (IngestionManager /
// the /v1/ingest webhook / the SynapticWatcher), never a user-routed request that
// would (incorrectly) plan agent tasks to write LTM.
const classifyConstraints = `Decision types:
- chat: Answer a question directly from existing knowledge/memory. No external action, no artifact produced. Examples: "What is X?", "Explain how LLMs work."
- plan: DO a task or PRODUCE an artifact — anything needing action or tools: creating or writing a file, researching, fetching a URL, computing, transforming, building. Examples: "Create a file cambrian.txt that summarizes how LLMs work", "Research X and write a report", "Scrape this page and extract the prices."
- watch: Register a persistent reactive rule that fires on FUTURE events. Examples: "Monitor X and alert me when Y", "Track this page for changes."

Decisive rule: if the user asks to CREATE, WRITE, GENERATE, PRODUCE, or otherwise carry out a task — even when the requested output is a summary or explanation — classify as 'plan'. Use 'chat' only for a question to be answered directly with no action or artifact.`

// classifyPromptTemplate is the static (non-dynamic) portion of the prompt whose
// hash is written to PromptRegistry (role + constraints; the task body is dynamic).
const classifyPromptTemplate = classifyRole + "\n" + classifyConstraints

var classifyPromptHash string

func init() {
	classifyPromptHash = domain.PromptHashOf(classifyPromptTemplate)
	domain.PromptRegistry[classifyPromptHash] = domain.PromptEntry{
		ID:      "router.classify",
		Version: "1.1.0",
		Hash:    classifyPromptHash,
		Schema:  classifySchema,
	}
}

// DefaultRouter implements domain.InputRouter using four ordered layers:
//
//	Layer 0 — RouterInput.Intent pre-classification (Gateway shortcut)
//	Layer 1 — Slash-prefix commands (/watch, /plan, /ingest)
//	Layer 2 — Word-boundary keyword heuristics
//	Layer 3 — LLM classification with structured output
//
// Safe for concurrent use from multiple goroutines.
type DefaultRouter struct {
	gen           domain.Generator // may be nil; required only for Layer 3
	minConfidence float64
	bodyChars     int
}

// New constructs a DefaultRouter with default Layer 3 configuration.
// gen is the LLM generator; it may be nil when only Layers 0-2 are needed.
func New(gen domain.Generator) *DefaultRouter {
	return NewWithConfig(gen, 0.5, 500)
}

// NewWithConfig constructs a DefaultRouter with explicit Layer 3 tuning.
// minConfidence is the confidence gate below which DecisionClarification is returned.
// bodyChars is the maximum characters of Body passed to the Layer 3 prompt.
func NewWithConfig(gen domain.Generator, minConfidence float64, bodyChars int) *DefaultRouter {
	return &DefaultRouter{gen: gen, minConfidence: minConfidence, bodyChars: bodyChars}
}

// Resolve classifies the input through four ordered layers and returns a decision.
func (r *DefaultRouter) Resolve(ctx context.Context, input domain.RouterInput) (*domain.RouterDecision, error) {
	// Layer 0: Gateway pre-classification.
	if domain.IsKnownDecision(input.Intent) {
		return &domain.RouterDecision{Type: input.Intent}, nil
	}

	// Layer 1: Slash-prefix commands.
	if dec, ok := layer1(input.Body); ok {
		return &domain.RouterDecision{Type: dec}, nil
	}

	// Layer 2: Word-boundary keyword heuristics.
	if dec, ok := layer2(input.Body); ok {
		return &domain.RouterDecision{Type: dec}, nil
	}

	// Layer 3: LLM classification.
	return r.layer3(ctx, input)
}

// layer2 applies word-boundary keyword matching. Returns ("", false) if no
// category matches or if more than one category matches (multi-match falls
// through to Layer 3 so the LLM can arbitrate).
func layer2(body string) (domain.DecisionType, bool) {
	var matched []domain.DecisionType
	for dec, re := range layer2Keywords {
		if re.MatchString(body) {
			matched = append(matched, dec)
		}
	}
	if len(matched) == 1 {
		return matched[0], true
	}
	return "", false
}

// layer1 checks for an unambiguous slash-prefix command at the start of body.
// Returns (decision, true) on match; ("", false) otherwise.
func layer1(body string) (domain.DecisionType, bool) {
	lower := strings.ToLower(strings.TrimSpace(body))
	switch {
	case strings.HasPrefix(lower, "/watch"):
		return domain.DecisionWatch, true
	case strings.HasPrefix(lower, "/plan"):
		return domain.DecisionPlan, true
	}
	return "", false
}

// classifyResponse is the JSON structure returned by the Layer 3 LLM.
type classifyResponse struct {
	Decision     string              `json:"decision"`
	Confidence   float64             `json:"confidence"`
	Alternatives []classifyAlternative `json:"alternatives"`
	Reason       string              `json:"reason"`
}

type classifyAlternative struct {
	Decision   string  `json:"decision"`
	Confidence float64 `json:"confidence"`
}

const altConfidenceThreshold = 0.1

// layer3 calls the LLM generator to classify the input. Returns an error when
// the Generator is nil or when classification fails — never a silent fallback.
func (r *DefaultRouter) layer3(ctx context.Context, input domain.RouterInput) (*domain.RouterDecision, error) {
	if r.gen == nil {
		return nil, errors.New("router: Layer 3 classification requires a Generator; none configured")
	}

	body := input.Body
	if r.bodyChars > 0 && len(body) > r.bodyChars {
		body = body[:r.bodyChars]
	}

	prompt := domain.PromptBuild(
		domain.PromptSystem(classifyRole, classifyConstraints),
		domain.PromptTask(fmt.Sprintf("Classify this input: %q", body)),
		domain.PromptOutputSchemaJSON(classifySchema),
	)

	raw, err := r.gen.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("router: Layer 3 generator error: %w", err)
	}

	var resp classifyResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("router: Layer 3 invalid JSON response: %w", err)
	}

	topDecision := domain.DecisionType(resp.Decision)
	if !domain.IsKnownDecision(topDecision) {
		return nil, fmt.Errorf("router: Layer 3 unknown decision %q in LLM response", resp.Decision)
	}

	if resp.Confidence >= r.minConfidence {
		return &domain.RouterDecision{Type: topDecision}, nil
	}

	// Below confidence threshold → return structured clarification.
	options := []domain.ClarificationOption{
		{
			Label:       labelFor(topDecision),
			Decision:    topDecision,
			Recommended: true,
		},
	}
	for _, alt := range resp.Alternatives {
		altDec := domain.DecisionType(alt.Decision)
		if alt.Confidence > altConfidenceThreshold && domain.IsKnownDecision(altDec) {
			options = append(options, domain.ClarificationOption{
				Label:    labelFor(altDec),
				Decision: altDec,
			})
		}
	}

	return &domain.RouterDecision{
		Type:                  domain.DecisionClarification,
		ClarificationQuestion: "I'm not sure what you'd like me to do. Please choose:",
		ClarificationOptions:  options,
	}, nil
}

// labelFor returns a human-readable label for a decision type.
func labelFor(d domain.DecisionType) string {
	switch d {
	case domain.DecisionChat:
		return "Answer directly from memory"
	case domain.DecisionPlan:
		return "Build and execute a multi-step plan"
	case domain.DecisionWatch:
		return "Register a persistent reactive rule"
	default:
		return string(d)
	}
}
