package network

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/metadata"
)

// mockMemorySearcher is a test double for domain.MemorySearcher.
type mockMemorySearcher struct {
	results          []domain.SearchResult
	actionResults    []domain.SearchResult
	sceneResults     []domain.SearchResult
	entityResults    []domain.SearchResult
	precedentResults []domain.SearchResult
	err              error
	lastLane         string // "facts" | "actions" | "scenes" | "entity" | "precedents"
}

func (m *mockMemorySearcher) Search(_ context.Context, _, _ string) ([]domain.SearchResult, error) {
	m.lastLane = "facts"
	return m.results, m.err
}

func (m *mockMemorySearcher) SearchActions(_ context.Context, _, _ string) ([]domain.SearchResult, error) {
	m.lastLane = "actions"
	return m.actionResults, m.err
}

func (m *mockMemorySearcher) SearchScenes(_ context.Context, _, _ string) ([]domain.SearchResult, error) {
	m.lastLane = "scenes"
	return m.sceneResults, m.err
}

func (m *mockMemorySearcher) SearchEntities(_ context.Context, _, _ string) ([]domain.SearchResult, error) {
	m.lastLane = "entity"
	return m.entityResults, m.err
}

func (m *mockMemorySearcher) SearchPrecedents(_ context.Context, _, _ string) ([]domain.SearchResult, error) {
	m.lastLane = "precedents"
	return m.precedentResults, m.err
}

func ctxWithAgentID(agentID string) context.Context {
	md := metadata.Pairs("x-agent-id", agentID)
	return metadata.NewIncomingContext(context.Background(), md)
}

func ctxWithLane(agentID, lane string) context.Context {
	md := metadata.Pairs("x-agent-id", agentID, "x-lane", lane)
	return metadata.NewIncomingContext(context.Background(), md)
}

func makeQueryMemoryServer(searcher domain.MemorySearcher) *Server {
	return &Server{MemorySearcher: searcher}
}

func TestQueryMemory_ReturnsResults(t *testing.T) {
	searcher := &mockMemorySearcher{
		results: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:       "doc-a",
					Text:     "agent-a knowledge",
					Metadata: map[string]interface{}{"source_agent_id": "agent-a"},
				},
				Score: 0.9,
			},
		},
	}

	srv := makeQueryMemoryServer(searcher)
	resp, err := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "test", TopK: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].Text != "agent-a knowledge" {
		t.Errorf("expected agent-a document, got: %q", resp.Results[0].Text)
	}
}

// The handler honors req.TopK: the MemorySearcher returns the full server-side
// window (config recall_top_k), and the transport truncates the blend-ranked
// prefix to what the caller asked for. TopK==0 (unset) returns the full window.
func TestQueryMemory_HonorsTopK(t *testing.T) {
	full := make([]domain.SearchResult, 5)
	for i := range full {
		full[i] = domain.SearchResult{
			Document: domain.Document{ID: fmt.Sprintf("doc-%d", i), Text: fmt.Sprintf("fact %d", i)},
			Score:    float64(5 - i), // already ranked best-first
		}
	}
	srv := makeQueryMemoryServer(&mockMemorySearcher{results: full})

	resp, err := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q", TopK: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("TopK=2 must truncate to 2, got %d", len(resp.Results))
	}
	if resp.Results[0].Text != "fact 0" || resp.Results[1].Text != "fact 1" {
		t.Fatalf("truncation must keep the ranked prefix, got %q,%q", resp.Results[0].Text, resp.Results[1].Text)
	}

	// TopK unset (0) ⇒ full server window, unchanged.
	respAll, err := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q", TopK: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(respAll.Results) != 5 {
		t.Fatalf("TopK=0 must return the full window of 5, got %d", len(respAll.Results))
	}

	// TopK larger than the window can't fabricate — caps at what's available.
	respBig, _ := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q", TopK: 50})
	if len(respBig.Results) != 5 {
		t.Fatalf("TopK>window must return all 5, got %d", len(respBig.Results))
	}
}

// ADR-0048 #1: when a fact has a Summary, recall serves the summary (the gist) as
// the agent-facing text and exposes the full body's cid via metadata, rather than
// shipping the full Text into the agent's context.
func TestQueryMemory_ServesSummaryAndContentCID(t *testing.T) {
	searcher := &mockMemorySearcher{
		results: []domain.SearchResult{{
			Document: domain.Document{
				ID:      "doc-big",
				Text:    "…2913 chars of full body…",
				Summary: "Web search results listing the world's longest rivers.",
				Metadata: map[string]interface{}{
					"source_agent_id": "agent-a",
					"content_cid":     "cid-xyz",
				},
			},
			Score: 0.8,
		}},
	}
	srv := makeQueryMemoryServer(searcher)
	resp, err := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "rivers", TopK: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Results[0].Text != "Web search results listing the world's longest rivers." {
		t.Errorf("recall must serve the summary, not the full body; got %q", resp.Results[0].Text)
	}
	if !strings.Contains(resp.Results[0].Metadata, "cid-xyz") {
		t.Errorf("content_cid must ride in metadata for drill-down; got %q", resp.Results[0].Metadata)
	}
}

// A fact with no Summary falls back to its full Text (short facts are their own gist).
// ADR-0049 D4: x-lane="actions" routes to the actions lane; default is facts.
func TestQueryMemory_RoutesActionsLane(t *testing.T) {
	searcher := &mockMemorySearcher{
		results:       []domain.SearchResult{{Document: domain.Document{ID: "f", Text: "a fact"}, Score: 0.9}},
		actionResults: []domain.SearchResult{{Document: domain.Document{ID: "a", Text: "write_file → ok"}, Score: 0.9}},
	}
	srv := makeQueryMemoryServer(searcher)

	resp, _ := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q"})
	if searcher.lastLane != "facts" || resp.Results[0].Text != "a fact" {
		t.Errorf("default lane must be facts; got lane=%q text=%q", searcher.lastLane, resp.Results[0].Text)
	}

	resp, _ = srv.QueryMemory(ctxWithLane("agent-a", "actions"), &pb.MemoryRequest{Query: "q"})
	if searcher.lastLane != "actions" || resp.Results[0].Text != "write_file → ok" {
		t.Errorf("x-lane=actions must route to the actions lane; got lane=%q text=%q", searcher.lastLane, resp.Results[0].Text)
	}
}

// ADR-0049 Issue 012/014: x-lane="entity" routes to exact entity lookup and
// x-lane="precedents" routes to the world-model transition lane.
func TestQueryMemory_RoutesEntityAndPrecedentLanes(t *testing.T) {
	searcher := &mockMemorySearcher{
		entityResults:    []domain.SearchResult{{Document: domain.Document{ID: "e", Text: "file:a.md exists=false"}, Score: 1}},
		precedentResults: []domain.SearchResult{{Document: domain.Document{ID: "p", Text: "SITUATION: x | OUTCOME: failure"}, Score: 0.8}},
	}
	srv := makeQueryMemoryServer(searcher)

	resp, _ := srv.QueryMemory(ctxWithLane("agent-a", "entity"), &pb.MemoryRequest{Query: "file:a.md"})
	if searcher.lastLane != "entity" || resp.Results[0].Text != "file:a.md exists=false" {
		t.Errorf("x-lane=entity must route to entity lookup; got lane=%q text=%q", searcher.lastLane, resp.Results[0].Text)
	}

	resp, _ = srv.QueryMemory(ctxWithLane("agent-a", "precedents"), &pb.MemoryRequest{Query: "ship"})
	if searcher.lastLane != "precedents" || !strings.Contains(resp.Results[0].Text, "OUTCOME: failure") {
		t.Errorf("x-lane=precedents must route to the precedent lane; got lane=%q text=%q", searcher.lastLane, resp.Results[0].Text)
	}
}

func TestQueryMemory_NoSummaryFallsBackToText(t *testing.T) {
	searcher := &mockMemorySearcher{
		results: []domain.SearchResult{{
			Document: domain.Document{ID: "doc-s", Text: "a short fact"},
			Score:    0.9,
		}},
	}
	srv := makeQueryMemoryServer(searcher)
	resp, _ := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q", TopK: 10})
	if resp.Results[0].Text != "a short fact" {
		t.Errorf("no-summary fact must return its Text; got %q", resp.Results[0].Text)
	}
}

func TestQueryMemory_NoMatchesReturnsEmptyResponse(t *testing.T) {
	srv := makeQueryMemoryServer(&mockMemorySearcher{results: []domain.SearchResult{}})
	resp, err := srv.QueryMemory(ctxWithAgentID("agent-x"), &pb.MemoryRequest{Query: "test", TopK: 10})
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(resp.Results))
	}
}

func TestQueryMemory_NilSearcher_ReturnsEmpty(t *testing.T) {
	srv := makeQueryMemoryServer(nil)
	resp, err := srv.QueryMemory(context.Background(), &pb.MemoryRequest{Query: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(resp.Results))
	}
}

func TestQueryMemory_SearcherError_Propagated(t *testing.T) {
	srv := makeQueryMemoryServer(&mockMemorySearcher{err: fmt.Errorf("db unavailable")})
	_, err := srv.QueryMemory(context.Background(), &pb.MemoryRequest{Query: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestQueryMemory_ScoreAndMetadataMapped(t *testing.T) {
	searcher := &mockMemorySearcher{
		results: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:       "doc-a",
					Text:     "knowledge",
					Metadata: map[string]interface{}{"topic": "security"},
				},
				Score: 0.95,
			},
		},
	}

	srv := makeQueryMemoryServer(searcher)
	resp, err := srv.QueryMemory(context.Background(), &pb.MemoryRequest{Query: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := resp.Results[0]
	if r.Score != float32(0.95) {
		t.Errorf("expected score 0.95, got %f", r.Score)
	}
	if r.Metadata == "" {
		t.Error("expected non-empty metadata JSON")
	}
}

// ADR-0048 A1 D10: the recall response folds the Document's struct-level temporal
// facts (created_at, last_accessed, activation) into the metadata payload — they do
// not live in Metadata, so without this they never reach the SDK that renders the
// freshness signal. The kernel-stamped author keys (source_agent/session_id, D9) must
// survive the fold unchanged.
func TestQueryMemory_FoldsProvenanceAndFreshness(t *testing.T) {
	created := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	accessed := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	searcher := &mockMemorySearcher{
		results: []domain.SearchResult{{
			Document: domain.Document{
				ID:                 "doc-prov",
				Text:               "a recalled fact",
				Metadata:           map[string]interface{}{"source_agent": "System", "session_id": "sess-9"},
				ActivationStrength: 0.42,
				CreatedAt:          created,
				LastAccessedAt:     accessed,
			},
			Score: 0.7,
		}},
	}
	srv := makeQueryMemoryServer(searcher)
	resp, err := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q", TopK: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var meta map[string]any
	if err := json.Unmarshal([]byte(resp.Results[0].Metadata), &meta); err != nil {
		t.Fatalf("metadata must be valid JSON: %v", err)
	}
	// D9 author keys survive the fold.
	if meta["source_agent"] != "System" || meta["session_id"] != "sess-9" {
		t.Errorf("kernel-stamped author keys must survive; got %v", meta)
	}
	// D10 temporal facts are folded in under reserved underscore keys.
	if got, ok := meta["_activation_strength"].(float64); !ok || got != 0.42 {
		t.Errorf("_activation_strength must be folded in; got %v", meta["_activation_strength"])
	}
	if meta["_created_at"] != created.Format(time.RFC3339) {
		t.Errorf("_created_at must be folded in as RFC3339; got %v", meta["_created_at"])
	}
	if meta["_last_accessed_at"] != accessed.Format(time.RFC3339) {
		t.Errorf("_last_accessed_at must be folded in as RFC3339; got %v", meta["_last_accessed_at"])
	}
}

// A zero CreatedAt/LastAccessedAt must NOT emit a bogus epoch timestamp — the SDK
// omits the attribute when the key is absent, so the kernel must omit the key.
func TestQueryMemory_OmitsZeroTimestamps(t *testing.T) {
	searcher := &mockMemorySearcher{
		results: []domain.SearchResult{{
			Document: domain.Document{ID: "doc-z", Text: "f", ActivationStrength: 0.1},
			Score:    0.5,
		}},
	}
	srv := makeQueryMemoryServer(searcher)
	resp, _ := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "q"})

	var meta map[string]any
	if err := json.Unmarshal([]byte(resp.Results[0].Metadata), &meta); err != nil {
		t.Fatalf("metadata must be valid JSON: %v", err)
	}
	if _, present := meta["_created_at"]; present {
		t.Error("zero CreatedAt must be omitted, not emitted as an epoch")
	}
	if _, present := meta["_last_accessed_at"]; present {
		t.Error("zero LastAccessedAt must be omitted, not emitted as an epoch")
	}
	if _, present := meta["_activation_strength"]; !present {
		t.Error("_activation_strength is always folded (a real 0.0 is meaningful)")
	}
}
