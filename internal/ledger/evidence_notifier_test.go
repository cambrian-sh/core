package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type stubWriter struct {
	err error
	n   int
}

func (s *stubWriter) Record(_ context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	s.n++
	if s.err != nil {
		return domain.LedgerEntry{}, s.err
	}
	return domain.LedgerEntry{ID: "led_" + rec.ToolName, Kind: rec.Kind}, nil
}

func capture(bus *domain.InMemoryEventBus) *[]domain.ToolEvidenceRecordedEvent {
	var got []domain.ToolEvidenceRecordedEvent
	bus.Subscribe(domain.EventTypeToolEvidenceRecorded, func(e domain.DomainEvent) {
		got = append(got, e.(domain.ToolEvidenceRecordedEvent))
	})
	return &got
}

// ENG-715: an external MCP tool result becomes a digest + ledger-pointer event;
// the body itself never travels.
func TestEvidenceNotifier_PublishesDigestAndPointerForExternalMCP(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	got := capture(bus)
	inner := &stubWriter{}
	w := NewEvidenceNotifier(inner, bus)

	body := []byte(`{"case":"K-1","status":"open"}`)
	ent, err := w.Record(context.Background(), domain.LedgerRecord{
		Kind: domain.LedgerToolResult, ToolName: "mcp:registry/cases_get",
		ArgsJSON: []byte(`{"id": "K-1"}`), Body: body,
		Writer:     domain.AgentPrincipal("chat_agent"),
		Provenance: domain.LedgerProvenance{ConversationID: "c1", TurnID: "m1", RunID: "r1", StepIndex: -1, Owner: "ayse"},
	})
	if err != nil || ent.ID == "" {
		t.Fatalf("record: %v %+v", err, ent)
	}
	if len(*got) != 1 {
		t.Fatalf("events = %d, want 1", len(*got))
	}
	e := (*got)[0]
	sum := sha256.Sum256(body)
	if e.LedgerID != ent.ID || e.ServerID != "registry" || e.ContentDigest != hex.EncodeToString(sum[:]) {
		t.Errorf("event = %+v", e)
	}
	if e.ArgsDigest == "" || e.ConversationID != "c1" || e.Owner != "ayse" || e.Writer != "chat_agent" {
		t.Errorf("provenance lost: %+v", e)
	}
}

func TestEvidenceNotifier_SilentForKernelToolsOtherKindsAndFailures(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	got := capture(bus)
	w := NewEvidenceNotifier(&stubWriter{}, bus)
	ctx := context.Background()
	_, _ = w.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, ToolName: "ledger_get"})
	_, _ = w.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerStepOutput, ToolName: "mcp:x/y"})

	failing := NewEvidenceNotifier(&stubWriter{err: errors.New("db down")}, bus)
	if _, err := failing.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, ToolName: "mcp:x/y"}); err == nil {
		t.Fatal("the inner error must pass through")
	}
	if len(*got) != 0 {
		t.Fatalf("no event expected, got %+v", *got)
	}
	inner := &stubWriter{}
	if NewEvidenceNotifier(inner, nil) != domain.LedgerWriter(inner) {
		t.Error("a nil bus must return the writer unchanged")
	}
}

func TestExternalMCPServer(t *testing.T) {
	for name, want := range map[string]string{"mcp:registry/cases_get": "registry", "mcp:x": "", "ledger_get": "", "mcp:/t": ""} {
		got, ok := domain.ExternalMCPServer(name)
		if got != want || ok != (want != "") {
			t.Errorf("%s → %q %v", name, got, ok)
		}
	}
}
