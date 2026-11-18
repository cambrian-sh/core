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

func (s *scopeRecordingSearcher) SearchMatches(ctx context.Context, _, callerID string) ([]domain.SearchResult, error) {
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
		{Document: domain.Document{ID: "b1", Text: "bob wrote this"}, RawScore: 0.9},
		{Document: domain.Document{ID: "m2", Text: "alice wrote this"}, RawScore: 0.8},
		{Document: domain.Document{ID: "m1", Text: "alice wrote that too"}, RawScore: 0.7},
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

// ENG-1953 (contract 0107): a query preview the candidate cap cut says so in
// PreviewForgetResponse.truncated, and hits below the relevance floor name nothing.
func TestForgetRPC_QueryPreviewTruncatedAndFloored(t *testing.T) {
	one := domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: "one", DocumentID: "one", WrittenBy: "alice"}
	two := domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: "two", DocumentID: "two", WrittenBy: "alice"}
	low := domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: "low", DocumentID: "low", WrittenBy: "alice"}
	raw := &chunkResolvingFake{
		forgetRawFake: forgetRawFake{records: []domain.ErasureCandidate{one, two, low}},
		chunkDoc:      map[string]domain.ErasureCandidate{"o1": one, "t1": two, "l1": low},
	}
	srch := &scopeRecordingSearcher{hits: []domain.SearchResult{
		{Document: domain.Document{ID: "o1", Text: "one"}, RawScore: 0.9},
		{Document: domain.Document{ID: "l1", Text: "boilerplate"}, RawScore: 0.3},
		{Document: domain.Document{ID: "t1", Text: "two"}, RawScore: 0.8},
	}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	ctx := agentCall("alice")

	capped := &Server{Forgetter: forget.New(e, forget.Config{Searcher: srch, MaxCandidates: 1})}
	p, err := capped.PreviewForget(ctx, &pb.PreviewForgetRequest{Query: "q"})
	if err != nil || len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetId() != "one" || !p.GetTruncated() {
		t.Fatalf("capped preview = %+v, %v; want only \"one\", truncated", p, err)
	}
	full := &Server{Forgetter: forget.New(e, forget.Config{Searcher: srch})}
	p, err = full.PreviewForget(ctx, &pb.PreviewForgetRequest{Query: "q"})
	if err != nil || len(p.GetCandidates()) != 2 || p.GetTruncated() {
		t.Fatalf("default preview = %+v, %v; want one+two (low is below the floor), not truncated", p, err)
	}
	for _, c := range p.GetCandidates() {
		if c.GetId() == "low" {
			t.Fatal("a hit below the relevance floor named its document")
		}
	}
}

// ENG-2018 (contract 0108): the agent-plane receipt names the ADR-0137 D7 async steps
// that could not erase from an external service, as the Go receipt does.
func TestReceiptToPB_CarriesNotErasableExternal(t *testing.T) {
	got := receiptToPB(domain.ErasureReceipt{RequestID: "erase:x", Status: domain.ErasureStatusComplete,
		NotErasableExternal: []string{"langfuse_traces"}})
	if !slices.Equal(got.GetNotErasableExternal(), []string{"langfuse_traces"}) {
		t.Fatalf("not_erasable_external = %v, want [langfuse_traces]", got.GetNotErasableExternal())
	}
	if empty := receiptToPB(domain.ErasureReceipt{RequestID: "erase:y"}); len(empty.GetNotErasableExternal()) != 0 {
		t.Fatalf("a receipt with nothing left behind reports %v", empty.GetNotErasableExternal())
	}
}

// ENG-2147/ENG-2154 (contract 0110): the agent-plane selector names memory-note lines
// (whose authority the chokepoint decides on the run principal; the end-to-end test
// drives it with a lease), and the receipt lists the replies an erase redacted.
func TestForgetRPC_MemoryNoteIDsAndRedactedMessagesOnTheWire(t *testing.T) {
	id := domain.MemoryNoteEntryID("personal", "alice", "Locker code: 1")
	sel := selectorFromPB(&pb.ErasureSelector{MemoryNoteIds: []string{id}, DocumentIds: []string{"d"}})
	if !slices.Equal(sel.MemoryNoteIDs, []string{id}) || !slices.Equal(sel.DocumentIDs, []string{"d"}) || len(sel.MemoryNoteOwners) != 0 {
		t.Fatalf("selector = %+v", sel)
	}
	got := receiptToPB(domain.ErasureReceipt{RequestID: "erase:x", RedactedMessages: []string{"m1", "m2"}})
	if !slices.Equal(got.GetRedactedMessages(), []string{"m1", "m2"}) {
		t.Fatalf("redacted_messages = %v", got.GetRedactedMessages())
	}
}
