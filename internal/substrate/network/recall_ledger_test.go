package network

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/metadata"
)

type capturingLedgerWriter struct {
	mu   sync.Mutex
	recs []domain.LedgerRecord
}

func (w *capturingLedgerWriter) Record(_ context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recs = append(w.recs, rec)
	return domain.LedgerEntry{ID: "led_recall_1"}, nil
}

// T-20261109-chat-agent-answers-a-published-rule: the chat turn's seed recall of
// the council's FOI procedure was not a ledger entry, so the reply check counted
// the correct "20 working days" as uncited. A recall by a lease-holding caller is
// recorded under the turn (one entry, each chunk's own tags as a source
// classification), and every hit carries the entry id plus its document/section.
func TestQueryMemory_RecordsTheRecallAsATurnLedgerEntry(t *testing.T) {
	chunk := domain.Document{ID: "chunk-4", Text: "**4. Time for compliance**\nThe council responds within 20 working days (s.10 FOIA).",
		SectionPath: "Freedom of Information procedure > 4. Time for compliance",
		Metadata: map[string]interface{}{"tags": []string{"public", "org:harrowmere"},
			"chunk_relations": json.RawMessage(`{"parent_entity_id":"source_doc:foi.md","sibling_context":{"parent_title":"Freedom of Information procedure"}}`)}}
	other := domain.Document{ID: "chunk-9", Text: "Stage 2 is answered within 20 working days.", Metadata: map[string]interface{}{"tags": []string{"internal"}}}
	w := &capturingLedgerWriter{}
	s := &Server{
		MemorySearcher: &mockMemorySearcher{results: []domain.SearchResult{{Document: chunk, Score: 0.9}, {Document: other, Score: 0.5}}},
		Ledger:         &LedgerAccess{Writer: w, Reader: memLedgerReader{}},
		LLMGateway: &stubLeaseResolver{leaseID: "lease-turn",
			binding: domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-1", OriginMessageID: "msg-7"}},
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "chat_agent", "x-lease-id", "lease-turn"))
	resp, err := s.QueryMemory(ctx, &pb.MemoryRequest{Query: "FOI response deadline"})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.recs) != 1 {
		t.Fatalf("want one recall entry, got %d", len(w.recs))
	}
	rec := w.recs[0]
	if rec.ToolName != domain.RecallToolName || rec.Kind != domain.LedgerToolResult ||
		rec.Provenance.ConversationID != "conv-1" || rec.Provenance.TurnID != "msg-7" {
		t.Fatalf("record = %+v", rec)
	}
	if !reflect.DeepEqual(rec.SourceClassifications, [][]string{{"public", "org:harrowmere"}, {"internal"}}) {
		t.Fatalf("each recalled chunk's tags must travel as a source classification: %v", rec.SourceClassifications)
	}
	var body recallEntryBody
	if err := json.Unmarshal(rec.Body, &body); err != nil || len(body.Hits) != 2 ||
		body.Hits[0].Document != "Freedom of Information procedure" || body.Hits[0].Section != chunk.SectionPath {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(resp.Results[0].Metadata), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["_ledger_ref"] != "led_recall_1" || meta["_source_document"] != "Freedom of Information procedure" || meta["_section_path"] != chunk.SectionPath {
		t.Fatalf("hit metadata = %v", meta)
	}

	// A caller with no lease the kernel issued places no entry.
	w.recs = nil
	anon := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "chat_agent", "x-lease-id", "lease-forged"))
	if _, err := s.QueryMemory(anon, &pb.MemoryRequest{Query: "q"}); err != nil || len(w.recs) != 0 {
		t.Fatalf("an unknown lease recorded %d entries (err %v)", len(w.recs), err)
	}
}
