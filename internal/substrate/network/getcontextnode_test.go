package network

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"

	"google.golang.org/grpc/metadata"
)

// fakeCS is a ContentStore holding a fixed node set.
type fakeCS struct{ nodes map[domain.CID]*domain.ContextNode }

func (f *fakeCS) Put(context.Context, []byte, string, []string, string) (domain.CID, error) {
	return "", nil
}
func (f *fakeCS) Get(_ context.Context, cid domain.CID) (*domain.ContextNode, error) {
	if n, ok := f.nodes[cid]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("cid not found")
}
func (f *fakeCS) Has(_ context.Context, cid domain.CID) (bool, error) {
	_, ok := f.nodes[cid]
	return ok, nil
}
func (f *fakeCS) GC(context.Context, []domain.CID) error { return nil }

// leakyVS would happily return raw LTM document text by id — the old fallback.
type leakyVS struct{ *capturingVS }

func (l *leakyVS) GetByID(_ context.Context, id string) (*domain.Document, error) {
	return &domain.Document{ID: id, Text: "SECRET LTM TEXT"}, nil
}

// ADR-0048 D5: a cid absent from the ContentStore but present in the VectorStore
// must NOT leak the LTM document text — the scope-bypassing fallback is removed.
func TestGetContextNode_NoLTMFallback(t *testing.T) {
	s := &Server{
		ContentStore: &fakeCS{nodes: map[domain.CID]*domain.ContextNode{}},
		VectorStore:  &leakyVS{&capturingVS{saved: make(chan *domain.Document, 1)}},
	}
	resp, err := s.GetContextNode(context.Background(), &pb.ContextNodeRequest{Cid: "some-ltm-uuid"})
	if err != nil {
		t.Fatalf("GetContextNode: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Errorf("LTM fallback not removed — GetContextNode leaked %q", resp.Data)
	}
}

// A genuine ContentStore cid is still served unchanged.
func TestGetContextNode_ServesContentStoreCID(t *testing.T) {
	cid := domain.CID("abc")
	s := &Server{ContentStore: &fakeCS{nodes: map[domain.CID]*domain.ContextNode{
		cid: {CID: cid, Type: "tool_result", Data: []byte("blob")},
	}}}
	resp, err := s.GetContextNode(context.Background(), &pb.ContextNodeRequest{Cid: string(cid)})
	if err != nil {
		t.Fatalf("GetContextNode: %v", err)
	}
	if string(resp.Data) != "blob" {
		t.Errorf("expected content-store blob, got %q", resp.Data)
	}
}

// ADR-0048 D4: an owned ContentStore node is readable only by its owning session.
func TestGetContextNode_OwnedNode_ReadGated(t *testing.T) {
	cid := domain.CID("owned")
	s := &Server{ContentStore: &fakeCS{nodes: map[domain.CID]*domain.ContextNode{
		cid: {CID: cid, Data: []byte("private"), OwnerSession: "sessA"},
	}}}

	// A caller in a DIFFERENT session gets not-found (no data, no leak).
	other := domain.WithSessionID(context.Background(), "sessB")
	resp, _ := s.GetContextNode(other, &pb.ContextNodeRequest{Cid: string(cid)})
	if len(resp.Data) != 0 {
		t.Errorf("owned node leaked to a different session: %q", resp.Data)
	}

	// The owning session reads it.
	own := domain.WithSessionID(context.Background(), "sessA")
	resp2, _ := s.GetContextNode(own, &pb.ContextNodeRequest{Cid: string(cid)})
	if string(resp2.Data) != "private" {
		t.Errorf("owner could not read its own node: %q", resp2.Data)
	}
}

// capturingCS records the session ctx at Put and stamps the owner like the real store.
type capturingCS struct {
	nodes    map[domain.CID]*domain.ContextNode
	putOwner string
}

func (c *capturingCS) Put(ctx context.Context, data []byte, nodeType string, _ []string, _ string) (domain.CID, error) {
	sid, _ := domain.SessionIDFromContext(ctx)
	c.putOwner = sid
	if c.nodes == nil {
		c.nodes = map[domain.CID]*domain.ContextNode{}
	}
	cid := domain.CID("cid-" + nodeType)
	c.nodes[cid] = &domain.ContextNode{CID: cid, Data: data, Type: nodeType, OwnerSession: sid}
	return cid, nil
}
func (c *capturingCS) Get(_ context.Context, cid domain.CID) (*domain.ContextNode, error) {
	if n, ok := c.nodes[cid]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("not found")
}
func (c *capturingCS) Has(context.Context, domain.CID) (bool, error) { return false, nil }
func (c *capturingCS) GC(context.Context, []domain.CID) error        { return nil }

// ADR-0048 D4/R7: PutContextNode owner-stamps from the x-session-id metadata, and
// the resulting node is readable only by that session.
func TestPutContextNode_OwnerStampedAndGated(t *testing.T) {
	cs := &capturingCS{}
	s := &Server{ContentStore: cs}

	ctxA := metadata.NewIncomingContext(context.Background(),
		metadata.New(map[string]string{"x-session-id": "sessA"}))
	resp, err := s.PutContextNode(ctxA, &pb.PutContextNodeRequest{Data: []byte("blob"), NodeType: "agent_offload"})
	if err != nil {
		t.Fatalf("PutContextNode: %v", err)
	}
	if cs.putOwner != "sessA" {
		t.Errorf("owner stamped = %q, want sessA", cs.putOwner)
	}

	// Same session reads it.
	got, _ := s.GetContextNode(ctxA, &pb.ContextNodeRequest{Cid: resp.Cid})
	if string(got.Data) != "blob" {
		t.Errorf("owner could not read its offload: %q", got.Data)
	}

	// A different session is denied (no data).
	ctxB := metadata.NewIncomingContext(context.Background(),
		metadata.New(map[string]string{"x-session-id": "sessB"}))
	gotB, _ := s.GetContextNode(ctxB, &pb.ContextNodeRequest{Cid: resp.Cid})
	if len(gotB.Data) != 0 {
		t.Errorf("offload leaked across sessions: %q", gotB.Data)
	}
}
