package network

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

type forgetRawFake struct {
	records []domain.ErasureCandidate
	erased  []domain.ErasureRequest
}

func (f *forgetRawFake) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, r := range f.records {
		if slices.Contains(sel.DocumentIDs, r.ID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *forgetRawFake) Erase(_ context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	f.erased = append(f.erased, req)
	return domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, PrincipalKind: req.PrincipalKind,
		Reason: req.Reason, Status: domain.ErasureStatusComplete, Counts: map[string]int{"documents": 1}}, nil
}

func (f *forgetRawFake) Receipt(context.Context, string) (domain.ErasureReceipt, bool, error) {
	return domain.ErasureReceipt{}, false, nil
}

func (f *forgetRawFake) ListReceipts(_ context.Context, flt domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	return []domain.ErasureReceipt{{RequestID: "erase:mine", Principal: flt.Principal}}, nil
}

func agentCall(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", id))
}

// The agent plane: the caller is the authenticated agent (never a request field), its
// read predicate rides ctx, and the chokepoint keeps the owner fence and the writer-only
// rule — another person's private row is ABSENT (NotFound), a visible row someone else
// wrote is DENIED, its own row is erased after a preview.
func TestForgetRPC_AgentWriterOnlyOwnerFenceKept(t *testing.T) {
	raw := &forgetRawFake{records: []domain.ErasureCandidate{
		{Kind: domain.ErasureTargetDocument, ID: "mine", Via: "mine", DocumentID: "mine", WrittenBy: "alice"},
		{Kind: domain.ErasureTargetDocument, ID: "bobs", Via: "bobs", DocumentID: "bobs", WrittenBy: "bob"},
		{Kind: domain.ErasureTargetDocument, ID: "private", Via: "private", DocumentID: "private", WrittenBy: "alice",
			Tags: []string{domain.OwnerTag("carol")}},
	}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	s := &Server{Forgetter: forget.New(e, forget.Config{PolicyFingerprint: "t"})}
	ctx := agentCall("alice")

	if _, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"private"}}}); status.Code(err) != codes.NotFound {
		t.Fatalf("owner-fenced row of another person must be absent: %v", err)
	}
	if _, err := s.Forget(ctx, &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"bobs"}}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("someone else's row must be denied: %v", err)
	}
	p, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}})
	if err != nil || len(p.GetCandidates()) != 1 || p.GetPreviewToken() == "" {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	resp, err := s.Forget(ctx, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken(), ReasonCode: "correction"})
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.GetReceipt(); r.GetPrincipal() != "alice" || r.GetPrincipalKind() != "agent" || r.GetReason() != "correction" {
		t.Fatalf("receipt = %+v", r)
	}
	if len(raw.erased) != 1 || !slices.Equal(raw.erased[0].Selector.DocumentIDs, []string{"mine"}) {
		t.Fatalf("erased = %+v", raw.erased)
	}
	if _, err := s.Forget(ctx, &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}, ReasonCode: "because"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown reason code: %v", err)
	}
	list, err := s.ListMyErasures(ctx, &pb.ListMyErasuresRequest{})
	if err != nil || len(list.GetReceipts()) != 1 || list.GetReceipts()[0].GetPrincipal() != "alice" {
		t.Fatalf("ListMyErasures = %+v, %v (must be forced to the caller)", list, err)
	}
}

// No agent identity is refused; erasure off answers Unimplemented; agent erasure off
// (the default) is a denial, never a silent success.
func TestForgetRPC_UnknownCallerDisabledAndAgentEraseOff(t *testing.T) {
	raw := &forgetRawFake{records: []domain.ErasureCandidate{{Kind: domain.ErasureTargetDocument, ID: "mine", Via: "mine", DocumentID: "mine", WrittenBy: "alice"}}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{}, nil)
	s := &Server{Forgetter: forget.New(e, forget.Config{})}
	if _, err := s.Forget(context.Background(), &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no agent identity: %v", err)
	}
	if _, err := s.Forget(agentCall("alice"), &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}}); status.Code(err) != codes.PermissionDenied || len(raw.erased) != 0 {
		t.Fatalf("agent erase off by default must deny: %v", err)
	}
	for _, off := range []*Server{{}, {Forgetter: forget.New(nil, forget.Config{})}} {
		if _, err := off.PreviewForget(agentCall("alice"), &pb.PreviewForgetRequest{}); status.Code(err) != codes.Unimplemented {
			t.Fatalf("disabled: %v", err)
		}
	}
}

// chunkResolvingFake resolves a chunk id to its document the way PgErasureStore does
// since ENG-1947 (Via = the chunk id).
type chunkResolvingFake struct {
	forgetRawFake
	chunkDoc map[string]domain.ErasureCandidate
}

func (f *chunkResolvingFake) Resolve(ctx context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	out, _ := f.forgetRawFake.Resolve(ctx, sel)
	for _, id := range sel.ChunkIDs {
		if c, ok := f.chunkDoc[id]; ok {
			c.Via = id
			out = append(out, c)
		}
	}
	return out, nil
}

type scopeRecordingSearcher struct {
	hits     []domain.SearchResult
	callerID string
	scoped   bool
}

func (s *scopeRecordingSearcher) Search(ctx context.Context, _, callerID string) ([]domain.SearchResult, error) {
	s.callerID = callerID
	p, ok := domain.ScopeFromContext(ctx)
	s.scoped = ok && p != nil && domain.PrincipalFromContext(ctx).ID == callerID
	return s.hits, nil
}

// ENG-1948 (contract 0106): PreviewForget's query form runs the agent's own search under
// the principal and predicate forgetCaller seeded, lists only whole documents the agent
// wrote, each with a snippet; selector + query is refused; no searcher is Unimplemented.
func TestForgetRPC_QueryForm(t *testing.T) {
	mine := domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: "mine", DocumentID: "mine", WrittenBy: "alice"}
	bobs := domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: "bobs", DocumentID: "bobs", WrittenBy: "bob"}
	raw := &chunkResolvingFake{
		forgetRawFake: forgetRawFake{records: []domain.ErasureCandidate{mine, bobs}},
		chunkDoc:      map[string]domain.ErasureCandidate{"m1": mine, "m2": mine, "b1": bobs},
	}
	srch := &scopeRecordingSearcher{hits: []domain.SearchResult{
		{Document: domain.Document{ID: "b1", Text: "bob wrote this"}},
		{Document: domain.Document{ID: "m2", Text: "alice wrote this"}},
		{Document: domain.Document{ID: "m1", Text: "alice wrote that too"}},
	}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	s := &Server{Forgetter: forget.New(e, forget.Config{PolicyFingerprint: "t", Searcher: srch})}
	ctx := agentCall("alice")

	p, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Query: "what I wrote"})
	if err != nil {
		t.Fatal(err)
	}
	if !srch.scoped || srch.callerID != "alice" {
		t.Fatalf("search must run as alice with her predicate on ctx: %+v", srch)
	}
	c := p.GetCandidates()
	if len(c) != 1 || c[0].GetId() != "mine" || c[0].GetKind() != "document" || c[0].GetVia() != "m2" || c[0].GetSnippet() != "alice wrote this" {
		t.Fatalf("candidates = %+v", c)
	}
	if _, err := s.Forget(ctx, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken()}); err != nil {
		t.Fatal(err)
	}
	if len(raw.erased) != 1 || !slices.Equal(raw.erased[0].Selector.DocumentIDs, []string{"mine"}) {
		t.Fatalf("erased = %+v", raw.erased)
	}
	// A selector preview stays content-free.
	sp, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}})
	if err != nil || len(sp.GetCandidates()) != 1 || sp.GetCandidates()[0].GetSnippet() != "" {
		t.Fatalf("selector preview = %+v, %v", sp, err)
	}
	if _, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Query: "x", Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("selector + query: %v", err)
	}
	srch.hits = srch.hits[:1] // only bob's
	if _, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Query: "bob"}); status.Code(err) != codes.NotFound {
		t.Fatalf("only another writer's hits must be NotFound: %v", err)
	}
	noSearch := &Server{Forgetter: forget.New(e, forget.Config{})}
	if _, err := noSearch.PreviewForget(ctx, &pb.PreviewForgetRequest{Query: "x"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no searcher: %v", err)
	}
}
