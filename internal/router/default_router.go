package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
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

// classifyPrompts is the Layer 3 prompt pair from the library (ADR-0139 C4/D2):
// prompts/router, the stable system block (no slots, cached), and
// prompts/router_input, the user message carrying the input in an <input> data
// region. The reply shape is prompts/router/classification.schema.json, sent as
// the response schema and validated by the provider layer (C1).
var classifyPrompts = prompts.MustPair("router", "router_input")

// classifyPromptHash is the pair's static hash: the identity of the static text
// actually sent, registered under "router.classify".
var classifyPromptHash = classifyPrompts.StaticHash()

// classifyDataTags are the data regions of router_input.
var classifyDataTags = []string{"input"}

// classifyCacheTTL: routing classifies a human's input, so it is human-paced and
// takes the long breakpoint lifetime (ADR-0139 D1). The system block is small;
// below a provider's minimum cacheable prefix the mark is simply not used.
const classifyCacheTTL = domain.CacheTTLLong

// DefaultRouter implements domain.InputRouter using four ordered layers:
//
//	Layer 0 — RouterInput.Intent pre-classification (Gateway shortcut)
//	Layer 1 — Slash-prefix commands (/watch, /plan)
//	Layer 2 — Word-boundary keyword heuristics
//	Layer 3 — LLM classification with structured output
//
// Safe for concurrent use from multiple goroutines.
type DefaultRouter struct {
	gen           domain.Generator // may be nil; required only for Layer 3
	minConfidence float64
	bodyChars     int
}

// Default Layer 3 tuning. The kernel never uses these: it wires
// execution.router.router_min_classification_confidence and
// router_classification_body_chars through NewWithConfig. They mirror the config
// defaults for callers (tests, tools) that need only Layers 0-2 or a default router.
const (
	DefaultMinConfidence = 0.5
	DefaultBodyChars     = 500
)

// New constructs a DefaultRouter with the default Layer 3 tuning.
// gen is the LLM generator; it may be nil when only Layers 0-2 are needed.
func New(gen domain.Generator) *DefaultRouter {
	return NewWithConfig(gen, DefaultMinConfidence, DefaultBodyChars)
}

// NewWithConfig constructs a DefaultRouter with explicit Layer 3 tuning.
// minConfidence is the confidence gate below which DecisionClarification is returned.
// bodyChars is the maximum characters (runes, not bytes) of Body passed to the
// Layer 3 prompt; 0 disables the cut.
func NewWithConfig(gen domain.Generator, minConfidence float64, bodyChars int) *DefaultRouter {
	return &DefaultRouter{gen: gen, minConfidence: minConfidence, bodyChars: bodyChars}
}

// Resolve classifies the input through four ordered layers and returns a decision.
func (r *DefaultRouter) Resolve(ctx context.Context, input domain.RouterInput) (*domain.RouterDecision, error) {
	// Layer 0: Gateway pre-classification.
	if domain.IsKnownDecision(input.Intent) {
		return &domain.RouterDecision{
			Type: input.Intent,
			Why:  "layer 0: the caller pre-classified this input",
		}, nil
	}

	// Layer 1: Slash-prefix commands.
	if dec, ok := layer1(input.Body); ok {
		return &domain.RouterDecision{
			Type: dec,
			Why:  "layer 1: an explicit /" + string(dec) + " prefix",
		}, nil
	}

	// Layer 2: Word-boundary keyword heuristics.
	if dec, ok := layer2(input.Body); ok {
		return &domain.RouterDecision{
			Type: dec,
			Why:  "layer 2: a single keyword category matched (" + string(dec) + ")",
		}, nil
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
	Decision     string                `json:"decision"`
	Confidence   float64               `json:"confidence"`
	Alternatives []classifyAlternative `json:"alternatives"`
	Reason       string                `json:"reason"`
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

	req, err := r.classifyRequest(input.Body)
	if err != nil {
		return nil, err
	}
	out, err := domain.GenerateVia(ctx, r.gen, req)
	if err != nil {
		return nil, fmt.Errorf("router: Layer 3 generator error: %w", err)
	}

	// The provider layer validated the reply against the classification schema
	// (and repaired it once), so it is decoded as is. The ExtractJSONObject scrape
	// that used to sit here — added 2026-07-28 when fenced replies killed 5 of 21
	// benchmark tasks at the front door — is gone: a fence is now the only
	// tolerance, and only on a reply whose generator ignored the schema.
	var resp classifyResponse
	if err := decodeClassification(out, &resp); err != nil {
		return nil, fmt.Errorf("router: Layer 3 invalid JSON response: %w", err)
	}

	topDecision := domain.DecisionType(resp.Decision)
	if !domain.IsKnownDecision(topDecision) {
		return nil, fmt.Errorf("router: Layer 3 unknown decision %q in LLM response", resp.Decision)
	}

	if resp.Confidence >= r.minConfidence {
		why := resp.Reason
		if why == "" {
			why = "layer 3: classified by the LLM"
		}
		return &domain.RouterDecision{
			Type:       topDecision,
			Why:        why,
			Confidence: resp.Confidence,
		}, nil
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
		Type: domain.DecisionClarification,
		// The confidence that FAILED the threshold is carried through, not the
		// clarification's own. It is the number that explains why we are asking.
		Confidence:            resp.Confidence,
		Why:                   fmt.Sprintf("layer 3: best guess %q scored %.2f, below the %.2f threshold", topDecision, resp.Confidence, r.minConfidence),
		ClarificationQuestion: "I'm not sure what you'd like me to do. Please choose:",
		ClarificationOptions:  options,
	}, nil
}

// classifyRequest renders the Layer 3 call. The body is clipped to bodyChars
// RUNES: a byte cut could split a multi-byte character and send invalid UTF-8.
func (r *DefaultRouter) classifyRequest(body string) (domain.GenerateRequest, error) {
	if r.bodyChars > 0 {
		body = truncateRunes(body, r.bodyChars)
	}
	msg, err := classifyPrompts.Message(map[string]string{
		"input": prompts.NeutraliseClosingTags(body, classifyDataTags...),
	})
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("router: prompt %s: %w", classifyPrompts.Input.ID, err)
	}
	return domain.GenerateRequest{
		System:         []domain.SystemBlock{{Text: classifyPrompts.System.Body, Cache: true}},
		Messages:       []domain.ModelMessage{domain.UserMessage(msg)},
		ResponseSchema: &domain.ResponseSchema{Name: "route_classification", Schema: classifyPrompts.Schema()},
		CacheTTL:       classifyCacheTTL,
	}, nil
}

// decodeClassification decodes a Layer 3 reply. A validated reply is decoded as
// is; an unchecked one (resp.SchemaUnchecked: the generator ignored the schema)
// may carry one markdown code fence, nothing more.
func decodeClassification(resp domain.GenerateResponse, v *classifyResponse) error {
	text := strings.TrimSpace(resp.Text)
	if resp.SchemaUnchecked() {
		text = domain.StripCodeFence(text) // the shared one-fence tolerance
	}
	return json.Unmarshal([]byte(text), v)
}

// truncateRunes cuts s to at most n runes, never inside a UTF-8 sequence.
func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
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
