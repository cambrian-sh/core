package network

import (
	"context"
	"strings"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ADR-0142 D12 — private by default, on the agent plane's chat-turn write paths
// (closes T-20261106-agent-writes-during-a-conversation-owner-s-turn). The cross-user
// property under test: what Alice's assistant writes while working Alice's
// conversation is readable by Alice (and her assistant), never by Bob.

// captureStore is a VectorStore that keeps what the write chokepoint stored.
type captureStore struct {
	domain.VectorStore
	mu   sync.Mutex
	docs []*domain.Document
}

func (c *captureStore) Save(_ context.Context, d *domain.Document) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.docs = append(c.docs, d)
	return nil
}

// writingProcessor stands in for the chunking pipeline: it writes the document
// through the REAL write chokepoint with the context IngestMemory handed it.
type writingProcessor struct{ w *authz.EnforcingStoreWriter }

func (p writingProcessor) ProcessSync(ctx context.Context, d domain.ExternalDocument) (string, error) {
	doc := &domain.Document{ID: "doc-1", Text: d.Body, Metadata: map[string]interface{}{"tags": d.Tags}}
	return "source_doc:" + d.SourceURI, p.w.Save(ctx, doc)
}

func privateServer(t *testing.T, on bool) (*Server, *SubstrateLLMGateway, *captureStore) {
	t.Helper()
	s, gw := newLeaseServer(t)
	s.ConversationOwner = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	s.PrivateByDefault = on
	store := &captureStore{}
	s.IngestionProcessor = writingProcessor{w: authz.NewEnforcingStoreWriter(store, domain.AllowAllAuthorizer{}, nil)}
	s.ArtifactBytes = newFakeVault()
	s.ArtifactMeta = newFakeMeta()
	return s, gw, store
}

// readerAllows asks the OSS decision point what a reader may see, the way the
// read boundary does: a person reads as themself; an agent working a person's
// conversation reads as that person.
func readerAllows(t *testing.T, reader domain.PrincipalRef, conversationOwner string, tags []string) bool {
	t.Helper()
	ctx := domain.WithConversationOwner(context.Background(), conversationOwner)
	pred, _ := domain.AllowAllAuthorizer{}.ReadFilter(ctx, reader, domain.SurfaceRef{})
	return pred.Allows(tags)
}

func TestPrivateByDefault_IngestMemoryInAlicesConversationIsAlicesOnly(t *testing.T) {
	t.Parallel()
	s, gw, store := privateServer(t, true)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-alice"})
	ctx := agentPlaneCtx("chat_agent", leaseHeader, string(lease), "x-agent-id", "chat_agent")
	if _, err := s.IngestMemory(ctx, &pb.IngestMemoryRequest{Text: "alice's salary is 90k (from her payslip)", Tags: []string{"team_kb"}}); err != nil {
		t.Fatalf("IngestMemory: %v", err)
	}
	if len(store.docs) != 1 {
		t.Fatalf("stored %d docs", len(store.docs))
	}
	tags := authz.DocTags(store.docs[0])
	if !hasTag(tags, "owner:alice") || !hasTag(tags, "team_kb") {
		t.Fatalf("a chat-turn write must carry owner:alice beside its own tags, got %v", tags)
	}
	if readerAllows(t, domain.UserPrincipal("bob"), "", tags) {
		t.Fatal("Bob can read what Alice's assistant wrote in Alice's conversation")
	}
	if readerAllows(t, domain.AgentPrincipal("chat_agent"), "bob", tags) {
		t.Fatal("Bob's assistant can read what Alice's assistant wrote")
	}
	if !readerAllows(t, domain.UserPrincipal("alice"), "", tags) {
		t.Fatal("Alice cannot read her own assistant's note")
	}
	if !readerAllows(t, domain.AgentPrincipal("chat_agent"), "alice", tags) {
		t.Fatal("Alice's assistant cannot read back its own note in her conversation")
	}
}

func TestPrivateByDefault_UploadArtifactInAlicesConversation(t *testing.T) {
	t.Parallel()
	s, gw, _ := privateServer(t, true)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-alice"})
	ctx := agentPlaneCtx("chat_agent", leaseHeader, string(lease), "x-agent-id", "chat_agent")
	resp, err := s.UploadArtifact(ctx, &pb.UploadArtifactRequest{Content: []byte("report"), Tags: []string{"team_kb"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasTag(resp.GetTags(), "owner:alice") {
		t.Fatalf("artifact tags = %v, want owner:alice", resp.GetTags())
	}
	if readerAllows(t, domain.UserPrincipal("bob"), "", resp.GetTags()) {
		t.Fatal("Bob can read Alice's assistant's artifact")
	}
}

// Work no conversation ordered (no lease conversation) is not stamped; and the
// switch off restores the pre-D12 behaviour.
func TestPrivateByDefault_OnlyForConversationsAndOnlyWhenOn(t *testing.T) {
	t.Parallel()
	s, gw, store := privateServer(t, true)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "worker"})
	ctx := agentPlaneCtx("worker", leaseHeader, string(lease), "x-agent-id", "worker")
	if _, err := s.IngestMemory(ctx, &pb.IngestMemoryRequest{Text: "org fact", Tags: []string{"team_kb"}}); err != nil {
		t.Fatal(err)
	}
	if tags := authz.DocTags(store.docs[0]); len(domain.OwnersOf(tags)) != 0 {
		t.Fatalf("a write no conversation ordered was owner-stamped: %v", tags)
	}

	off, gw2, store2 := privateServer(t, false)
	lease2 := bindNotesLease(t, gw2, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-alice"})
	ctx2 := agentPlaneCtx("chat_agent", leaseHeader, string(lease2), "x-agent-id", "chat_agent")
	if _, err := off.IngestMemory(ctx2, &pb.IngestMemoryRequest{Text: "x", Tags: []string{"team_kb"}}); err != nil {
		t.Fatal(err)
	}
	if tags := authz.DocTags(store2.docs[0]); len(domain.OwnersOf(tags)) != 0 {
		t.Fatalf("privacy.private_by_default=false still stamped: %v", tags)
	}
}

// A tool call made in Alice's conversation (ExecuteTool → withCallerSession)
// carries the marker too, so files it writes are promoted as Alice's.
func TestPrivateByDefault_ToolCallsInheritTheMarker(t *testing.T) {
	t.Parallel()
	s, gw, _ := privateServer(t, true)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-alice"})
	ctx := s.withCallerSession(agentPlaneCtx("chat_agent", leaseHeader, string(lease)))
	if got := domain.PrivateWriteOwnerFromContext(ctx); got != "alice" {
		t.Fatalf("tool-call context private owner = %q, want alice", got)
	}
	if got := domain.StampPrivateOwner(ctx, []string{"provenance:source=chat_agent"}); !hasTag(got, "owner:alice") {
		t.Fatalf("promoted artifact tags = %v", got)
	}
}

func hasTag(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
