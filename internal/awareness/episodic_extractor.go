package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// EpisodicSaver is the narrow persistence port used by EpisodicExtractor.
// Satisfied by domain.VectorStore — only Save is needed. ADR-0029.
type EpisodicSaver interface {
	Save(ctx context.Context, doc *domain.Document) error
}

// EpisodicExtractionInput carries all data needed for one session's episodic extraction.
type EpisodicExtractionInput struct {
	Session           domain.Session
	Events            []domain.SessionEvent     // narrative events; PII-masked before LLM
	PlanEvents        []domain.PlanEvent        // structured plan outcomes for context
	TaskEvents        []domain.TaskEvent        // per-task records for Participants derivation
	RetrievalSessions []domain.RetrievalSession // referenced fact doc IDs for KeyFacts
	CreatedFactIDs    []string                  // doc IDs from Tier-2 tagged with session_id
}

// EpisodicExtractor produces an EpisodicMemory document from a completed session.
// It is a second output of the ConsolidatorAgent's consolidation pass — same LLM
// call, zero extra cost. ADR-0029.
type EpisodicExtractor struct {
	gen     domain.Generator
	saver   EpisodicSaver
	masker  domain.PIIMasker
}

// NewEpisodicExtractor constructs an EpisodicExtractor with injected dependencies.
func NewEpisodicExtractor(gen domain.Generator, saver EpisodicSaver, masker domain.PIIMasker) *EpisodicExtractor {
	return &EpisodicExtractor{gen: gen, saver: saver, masker: masker}
}

// episodicLLMOutput mirrors the "episodic_memory" key in the ConsolidatorAgent LLM response.
type episodicLLMOutput struct {
	Decisions   []episodicDecisionLLM `json:"decisions"`
	ActionItems []episodicActionLLM   `json:"action_items"`
}

type episodicDecisionLLM struct {
	Text            string `json:"text"`
	MadeAt          string `json:"made_at"`
	SourceEventType string `json:"source_event_type"`
}

type episodicActionLLM struct {
	Text string `json:"text"`
}

const episodicExtractionRole = "You are Cambrian's episodic memory indexer. Extract structured decisions and action items from the session narrative below."
const episodicExtractionRules = `- decisions: explicit choices made by the user or system ("decided to", "agreed that", "rejected", "we will", "let's use")
- action_items: follow-up tasks mentioned or implied
- Output ONLY a JSON object matching the schema. No explanation. No markdown.`
const episodicExtractionSchema = `{
  "type": "object",
  "properties": {
    "decisions":    { "type": "array", "items": { "type": "object", "properties": { "text": {"type":"string"}, "made_at": {"type":"string"}, "source_event_type": {"type":"string"} }, "required": ["text","made_at","source_event_type"] } },
    "action_items": { "type": "array", "items": { "type": "object", "properties": { "text": {"type":"string"} }, "required": ["text"] } }
  },
  "required": ["decisions","action_items"]
}`

var episodicExtractionPromptHash = domain.PromptHashOf(episodicExtractionRole + episodicExtractionRules + episodicExtractionSchema)

func init() {
	domain.PromptRegistry[episodicExtractionPromptHash] = domain.PromptEntry{
		ID:      "memory.episodic_extraction",
		Version: "1.0.0",
		Hash:    episodicExtractionPromptHash,
		Schema:  episodicExtractionSchema,
	}
}

// Extract produces an EpisodicMemory from the given input without saving it.
// Go-owned fields (SessionID, Goal, StartedAt, CompletedAt, Participants, KeyFacts)
// are populated deterministically; only Decisions and ActionItems come from the LLM.
func (e *EpisodicExtractor) Extract(ctx context.Context, in EpisodicExtractionInput) (*domain.EpisodicMemory, error) {
	prompt := e.buildPrompt(in)

	resp, err := e.gen.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("episodic extractor: LLM call failed: %w", err)
	}

	llmOut, err := parseEpisodicLLMOutput(resp)
	if err != nil {
		slog.Warn("EpisodicExtractor: failed to parse LLM response, using empty decisions", "err", err)
		llmOut = &episodicLLMOutput{}
	}

	decisions := e.toDecisions(llmOut.Decisions)
	actionItems := e.toActionItems(llmOut.ActionItems)

	mem := &domain.EpisodicMemory{
		SessionID:    in.Session.ID,
		StartedAt:    in.Session.CreatedAt,
		CompletedAt:  in.Session.CompletedAt,
		Goal:         in.Session.Goal,
		Decisions:    decisions,
		ActionItems:  actionItems,
		Participants: collectParticipants(in.TaskEvents),
		KeyFacts:     collectKeyFacts(in.RetrievalSessions, in.CreatedFactIDs),
	}
	return mem, nil
}

// ExtractAndSave calls Extract then saves the resulting EpisodicMemory to pgvector.
// Returns an error (and does not save) if the LLM call fails.
func (e *EpisodicExtractor) ExtractAndSave(ctx context.Context, in EpisodicExtractionInput) error {
	mem, err := e.Extract(ctx, in)
	if err != nil {
		return err
	}

	decisionTexts := make([]string, len(mem.Decisions))
	for i, d := range mem.Decisions {
		decisionTexts[i] = d.Text
	}
	docText := mem.Goal
	if len(decisionTexts) > 0 {
		docText += ": " + strings.Join(decisionTexts, "; ")
	}

	doc := &domain.Document{
		DocumentType:       domain.DocTypeEpisodicMemory,
		Text:               docText,
		ActivationStrength: 0.1,
		Metadata:           map[string]interface{}{"episodic": mem},
	}

	if err := e.saver.Save(ctx, doc); err != nil {
		return fmt.Errorf("episodic extractor: save failed: %w", err)
	}
	slog.Info("EpisodicExtractor: episodic memory saved",
		"session_id", mem.SessionID,
		"decisions", len(mem.Decisions),
		"participants", len(mem.Participants))
	return nil
}

// buildPrompt assembles the LLM prompt with PII-masked event payloads.
func (e *EpisodicExtractor) buildPrompt(in EpisodicExtractionInput) string {
	var eventLog strings.Builder
	for _, ev := range in.Events {
		masked := e.masker.Mask(ev.Payload)
		fmt.Fprintf(&eventLog, "[%s] %s\n", ev.Type, masked)
	}

	return domain.PromptBuild(
		domain.PromptSystem(episodicExtractionRole, episodicExtractionRules),
		domain.PromptContext(fmt.Sprintf(
			"Session Goal: %s\n\nEvent Log:\n%s",
			in.Session.Goal, eventLog.String(),
		)),
		domain.PromptTask("Extract decisions and action items from the session above."),
		domain.PromptOutputSchemaJSON(episodicExtractionSchema),
	)
}

func (e *EpisodicExtractor) toDecisions(raw []episodicDecisionLLM) []domain.Decision {
	out := make([]domain.Decision, 0, len(raw))
	for _, d := range raw {
		maskedText := e.masker.Mask(d.Text)
		out = append(out, domain.Decision{
			Text:            maskedText,
			SourceEventType: domain.SessionEventType(d.SourceEventType),
		})
	}
	return out
}

func (e *EpisodicExtractor) toActionItems(raw []episodicActionLLM) []domain.ActionItem {
	out := make([]domain.ActionItem, 0, len(raw))
	for _, a := range raw {
		out = append(out, domain.ActionItem{Text: e.masker.Mask(a.Text)})
	}
	return out
}

// collectParticipants returns a deduplicated slice of agent IDs from TaskEvents.
func collectParticipants(events []domain.TaskEvent) []string {
	seen := map[string]bool{}
	var out []string
	for _, ev := range events {
		if ev.AgentID != "" && !seen[ev.AgentID] {
			seen[ev.AgentID] = true
			out = append(out, ev.AgentID)
		}
	}
	return out
}

// collectKeyFacts unions referenced fact IDs (from RetrievalSessions) with created fact IDs.
func collectKeyFacts(sessions []domain.RetrievalSession, created []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, rs := range sessions {
		for _, doc := range rs.RetrievedDocs {
			if doc.DocID != "" && !seen[doc.DocID] {
				seen[doc.DocID] = true
				out = append(out, doc.DocID)
			}
		}
	}
	for _, id := range created {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// parseEpisodicLLMOutput extracts the episodic JSON from the LLM response string.
func parseEpisodicLLMOutput(resp string) (*episodicLLMOutput, error) {
	// Try to find a JSON object in the response
	start := strings.IndexByte(resp, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found in response")
	}
	var out episodicLLMOutput
	if err := json.Unmarshal([]byte(resp[start:]), &out); err != nil {
		// Try the full string
		if err2 := json.Unmarshal([]byte(resp), &out); err2 != nil {
			return nil, fmt.Errorf("unmarshal episodic LLM output: %w", err)
		}
	}
	return &out, nil
}
