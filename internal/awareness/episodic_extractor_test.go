package awareness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── test doubles ─────────────────────────────────────────────────────────────

// episodicSaverSpy is a minimal EpisodicSaver that records Save calls.
type episodicSaverSpy struct {
	saved []*domain.Document
	err   error
}

func (s *episodicSaverSpy) Save(_ context.Context, doc *domain.Document) error {
	if s.err != nil {
		return s.err
	}
	s.saved = append(s.saved, doc)
	return nil
}

// episodicGenSpy captures LLM prompts and returns a scripted response.
type episodicGenSpy struct {
	response        string
	err             error
	capturedPrompts []string
}

func (g *episodicGenSpy) Generate(_ context.Context, prompt string) (string, error) {
	g.capturedPrompts = append(g.capturedPrompts, prompt)
	return g.response, g.err
}

// noopMasker passes text through unchanged.
type noopMasker struct{}

func (n *noopMasker) Mask(text string) string { return text }

// recordingMasker records every Mask call and delegates to RegexPIIMasker.
type recordingMasker struct {
	calls []string
	inner domain.PIIMasker
}

func (r *recordingMasker) Mask(text string) string {
	r.calls = append(r.calls, text)
	return r.inner.Mask(text)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func episodicLLMResponse(decisions []string) string {
	type decisionJSON struct {
		Text            string `json:"text"`
		MadeAt          string `json:"made_at"`
		SourceEventType string `json:"source_event_type"`
	}
	type actionJSON struct {
		Text string `json:"text"`
	}
	type episodicJSON struct {
		Decisions   []decisionJSON `json:"decisions"`
		ActionItems []actionJSON   `json:"action_items"`
	}
	d := make([]decisionJSON, len(decisions))
	for i, txt := range decisions {
		d[i] = decisionJSON{
			Text:            txt,
			MadeAt:          time.Now().Format(time.RFC3339),
			SourceEventType: "user_message",
		}
	}
	b, _ := json.Marshal(episodicJSON{
		Decisions:   d,
		ActionItems: []actionJSON{{Text: "follow up on implementation"}},
	})
	return string(b)
}

func makeSession(id, goal string) domain.Session {
	now := time.Now()
	return domain.Session{
		ID:          id,
		Goal:        goal,
		Status:      domain.SessionCompleted,
		CreatedAt:   now.Add(-2 * time.Hour),
		CompletedAt: now,
		UpdatedAt:   now,
	}
}

func singleEvent(sessionID string) []domain.SessionEvent {
	return []domain.SessionEvent{
		{SessionID: sessionID, Type: domain.EventUserMessage, Payload: "we discussed the design"},
	}
}

// ── Cycle 1: Go-owned fields come from Session, not the LLM ─────────────────

func TestEpisodicExtractor_GoOwnedFields_FromSession(t *testing.T) {
	gen := &episodicGenSpy{response: episodicLLMResponse([]string{"use JWT"})}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, &noopMasker{})

	sess := makeSession("sess-001", "Implement authentication")
	mem, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: sess,
		Events:  singleEvent("sess-001"),
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if mem.SessionID != "sess-001" {
		t.Errorf("SessionID: want %q got %q", "sess-001", mem.SessionID)
	}
	if mem.Goal != "Implement authentication" {
		t.Errorf("Goal: want %q got %q", "Implement authentication", mem.Goal)
	}
	if mem.StartedAt.IsZero() {
		t.Error("StartedAt must not be zero")
	}
	if mem.CompletedAt.IsZero() {
		t.Error("CompletedAt must not be zero")
	}
}

// ── Cycle 2: Decisions extracted from LLM response ───────────────────────────

func TestEpisodicExtractor_DecisionsFromLLM(t *testing.T) {
	gen := &episodicGenSpy{response: episodicLLMResponse([]string{"use JWT", "avoid OAuth for now"})}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, &noopMasker{})

	mem, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-002", "auth design"),
		Events:  singleEvent("sess-002"),
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(mem.Decisions) != 2 {
		t.Fatalf("Decisions: want 2 got %d", len(mem.Decisions))
	}
	if mem.Decisions[0].Text != "use JWT" {
		t.Errorf("Decisions[0].Text: want %q got %q", "use JWT", mem.Decisions[0].Text)
	}
	if mem.Decisions[1].Text != "avoid OAuth for now" {
		t.Errorf("Decisions[1].Text: want %q got %q", "avoid OAuth for now", mem.Decisions[1].Text)
	}
}

// ── Cycle 3: PIIMasker applied to SessionEvent payloads before LLM ──────────

func TestEpisodicExtractor_PIIMaskedBeforeLLM(t *testing.T) {
	gen := &episodicGenSpy{response: episodicLLMResponse([]string{"some decision"})}
	masker := &recordingMasker{inner: domain.NewRegexPIIMasker()}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, masker)

	_, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-003", "auth"),
		Events: []domain.SessionEvent{
			{SessionID: "sess-003", Type: domain.EventUserMessage, Payload: "contact alice@example.com for auth spec"},
		},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, prompt := range gen.capturedPrompts {
		if strings.Contains(prompt, "alice@example.com") {
			t.Errorf("LLM prompt must not contain raw email; prompt contained alice@example.com")
		}
	}
	if len(masker.calls) == 0 {
		t.Error("PIIMasker.Mask was never called")
	}
}

// ── Cycle 4: PIIMasker applied to LLM-returned Decision.Text ────────────────

func TestEpisodicExtractor_PIIMaskedAfterLLM(t *testing.T) {
	gen := &episodicGenSpy{
		response: episodicLLMResponse([]string{"assign bob@corp.com as owner"}),
	}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, domain.NewRegexPIIMasker())

	mem, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-004", "ownership"),
		Events:  singleEvent("sess-004"),
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, d := range mem.Decisions {
		if strings.Contains(d.Text, "bob@corp.com") {
			t.Errorf("Decision.Text must not contain raw email; got %q", d.Text)
		}
	}
}

// ── Cycle 5: Participants derived from distinct TaskEvent.AgentID values ─────

func TestEpisodicExtractor_ParticipantsFromTaskEvents(t *testing.T) {
	gen := &episodicGenSpy{response: episodicLLMResponse([]string{"use pgvector"})}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, &noopMasker{})

	mem, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-005", "db choice"),
		Events:  singleEvent("sess-005"),
		TaskEvents: []domain.TaskEvent{
			{AgentID: "analyst_agent"},
			{AgentID: "code_generator_agent"},
			{AgentID: "analyst_agent"}, // duplicate — must appear once
		},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(mem.Participants) != 2 {
		t.Errorf("Participants: want 2 distinct got %d: %v", len(mem.Participants), mem.Participants)
	}
	seen := map[string]bool{}
	for _, p := range mem.Participants {
		if seen[p] {
			t.Errorf("duplicate participant %q", p)
		}
		seen[p] = true
	}
}

// ── Cycle 6: Go-owned fields set even when LLM returns empty decisions ───────

func TestEpisodicExtractor_GoOwnedFields_WhenLLMReturnsEmpty(t *testing.T) {
	gen := &episodicGenSpy{response: `{"decisions":[],"action_items":[]}`}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, &noopMasker{})

	mem, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-006", "exploration"),
		Events:  singleEvent("sess-006"),
	})
	if err != nil {
		t.Fatalf("Extract returned error on empty LLM response: %v", err)
	}
	if mem.SessionID != "sess-006" {
		t.Errorf("SessionID: want %q got %q", "sess-006", mem.SessionID)
	}
	if mem.Goal != "exploration" {
		t.Errorf("Goal: want %q got %q", "exploration", mem.Goal)
	}
	if mem.StartedAt.IsZero() || mem.CompletedAt.IsZero() {
		t.Error("StartedAt/CompletedAt must be set from Session even when LLM returns empty")
	}
}

// ── Cycle 7: LLM failure → Extract returns error, no pgvector save ──────────

func TestEpisodicExtractor_LLMFailure_ReturnsErrorNoSave(t *testing.T) {
	gen := &episodicGenSpy{err: errors.New("llm timeout")}
	spy := &episodicSaverSpy{}
	ex := NewEpisodicExtractor(gen, spy, &noopMasker{})

	err := ex.ExtractAndSave(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-007", "failed"),
		Events:  singleEvent("sess-007"),
	})
	if err == nil {
		t.Fatal("expected error when LLM fails")
	}
	if len(spy.saved) != 0 {
		t.Errorf("expected 0 saves on LLM failure; got %d", len(spy.saved))
	}
}

// ── Cycle 8: ExtractAndSave stores DocTypeEpisodicMemory with correct Document fields ──

func TestEpisodicExtractor_ExtractAndSave_Document(t *testing.T) {
	gen := &episodicGenSpy{response: episodicLLMResponse([]string{"use JWT", "skip refresh tokens"})}
	spy := &episodicSaverSpy{}
	ex := NewEpisodicExtractor(gen, spy, &noopMasker{})

	err := ex.ExtractAndSave(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-008", "auth design"),
		Events:  singleEvent("sess-008"),
	})
	if err != nil {
		t.Fatalf("ExtractAndSave: %v", err)
	}
	if len(spy.saved) != 1 {
		t.Fatalf("expected 1 document saved, got %d", len(spy.saved))
	}
	doc := spy.saved[0]
	if doc.DocumentType != domain.DocTypeEpisodicMemory {
		t.Errorf("DocumentType: want %q got %q", domain.DocTypeEpisodicMemory, doc.DocumentType)
	}
	if !strings.Contains(doc.Text, "auth design") {
		t.Errorf("Document.Text should contain goal; got %q", doc.Text)
	}
	if !strings.Contains(doc.Text, "use JWT") {
		t.Errorf("Document.Text should contain decision text; got %q", doc.Text)
	}
	// Metadata must round-trip to EpisodicMemory
	rawBytes, err := json.Marshal(doc.Metadata["episodic"])
	if err != nil {
		t.Fatalf("marshal metadata.episodic: %v", err)
	}
	var em domain.EpisodicMemory
	if err := json.Unmarshal(rawBytes, &em); err != nil {
		t.Fatalf("unmarshal EpisodicMemory: %v", err)
	}
	if em.SessionID != "sess-008" {
		t.Errorf("metadata EpisodicMemory.SessionID: want %q got %q", "sess-008", em.SessionID)
	}
}

// ── Cycle 9: KeyFacts = union of RetrievalSession doc IDs + CreatedFactIDs ──

func TestEpisodicExtractor_KeyFacts_Union(t *testing.T) {
	gen := &episodicGenSpy{response: episodicLLMResponse([]string{"some decision"})}
	ex := NewEpisodicExtractor(gen, &episodicSaverSpy{}, &noopMasker{})

	mem, err := ex.Extract(context.Background(), EpisodicExtractionInput{
		Session: makeSession("sess-009", "facts test"),
		Events:  singleEvent("sess-009"),
		RetrievalSessions: []domain.RetrievalSession{
			{RetrievedDocs: []domain.RetrievedDoc{{DocID: "ref-1"}, {DocID: "ref-2"}}},
		},
		CreatedFactIDs: []string{"created-1"},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]bool{"ref-1": true, "ref-2": true, "created-1": true}
	if len(mem.KeyFacts) != 3 {
		t.Errorf("KeyFacts: want 3 got %d: %v", len(mem.KeyFacts), mem.KeyFacts)
	}
	for _, id := range mem.KeyFacts {
		if !want[id] {
			t.Errorf("unexpected KeyFact %q", id)
		}
	}
}
