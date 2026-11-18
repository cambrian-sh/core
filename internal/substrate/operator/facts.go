package operator

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// Read-only inspection of the ADR-0136 fact layer (contract 0103; owner decision
// 2026-11-14 on T-20261103-operator-plane-cannot-inspect-facts-no-rpc). The fact layer
// is ON by default (ENG-1496), so an operator must be able to see what was extracted,
// how a value's supersession chain was derived and how much of the corpus has been
// processed. No override, backfill or reset controls: those stay with cmd/facts-*.
//
// Every content read runs at domain.ScopeSystem — the operator plane's read scope,
// as for SearchSystem — through the fail-closed authz.EnforcingFactInspector, which
// renders it IN SQL. ScopeSystem keeps the owner fence (ADR-0141): another person's
// private facts answer as absent. All three are reads by name, so the role gate opens
// them to Operators and Viewers; Members reach none of them.

func (s *Service) factInspector() (domain.FactInspector, error) {
	if s.facts == nil {
		return nil, status.Error(codes.Unimplemented, "fact inspection is not available on this kernel (capability facts-inspect)")
	}
	return s.facts, nil
}

// factStatus maps a store error: a cursor the reader did not issue is the caller's
// error (InvalidArgument); anything else is the store's (Unavailable), never an empty
// answer.
func factStatus(op string, err error) error {
	if errors.Is(err, domain.ErrFactCursor) {
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	}
	return status.Errorf(codes.Unavailable, "%s: %v", op, err)
}

// ListFacts lists stored memory_fact items, newest assertion first, paged.
func (s *Service) ListFacts(ctx context.Context, req *pb.ListFactsOpRequest) (*pb.ListFactsOpResponse, error) {
	fi, err := s.factInspector()
	if err != nil {
		return nil, err
	}
	facts, next, err := fi.ListFacts(ctx, domain.FactListFilter{
		Subject: req.GetSubject(), Attribute: req.GetAttribute(), Actor: req.GetActor(),
		Limit: int(req.GetLimit()), After: req.GetPageToken(),
	}, domain.ScopeSystem)
	if err != nil {
		return nil, factStatus("list facts", err)
	}
	out := &pb.ListFactsOpResponse{NextPageToken: next}
	for _, f := range facts {
		out.Facts = append(out.Facts, factToOp(f, false))
	}
	return out, nil
}

// GetFactChain returns the derived supersession chain of one fact's resolution key.
// A fact the caller may not read answers exactly as an absent one: an empty chain.
func (s *Service) GetFactChain(ctx context.Context, req *pb.GetFactChainOpRequest) (*pb.GetFactChainOpResponse, error) {
	fi, err := s.factInspector()
	if err != nil {
		return nil, err
	}
	if req.GetFactId() == "" {
		return nil, status.Error(codes.InvalidArgument, "fact_id is required")
	}
	found, err := fi.FactsByID(ctx, []domain.KnowledgeItemID{domain.KnowledgeItemID(req.GetFactId())}, domain.ScopeSystem)
	if err != nil {
		return nil, factStatus("fact chain", err)
	}
	out := &pb.GetFactChainOpResponse{}
	if len(found) == 0 {
		return out, nil
	}
	chain, err := fi.FactChain(ctx, found[0], domain.ScopeSystem)
	if err != nil {
		return nil, factStatus("fact chain", err)
	}
	for _, f := range chain {
		out.Chain = append(out.Chain, factToOp(f, true))
	}
	return out, nil
}

// GetFactCoverage reports extraction coverage (counts only).
func (s *Service) GetFactCoverage(ctx context.Context, _ *pb.GetFactCoverageOpRequest) (*pb.GetFactCoverageOpResponse, error) {
	fi, err := s.factInspector()
	if err != nil {
		return nil, err
	}
	c, err := fi.FactCoverage(ctx)
	if err != nil {
		return nil, factStatus("fact coverage", err)
	}
	out := &pb.GetFactCoverageOpResponse{
		DocumentsWithChunks: c.DocumentsWithChunks, DocumentsExtracted: c.DocumentsExtracted,
		FactItems: c.FactItems, LastExtractedAt: rfc3339(c.LastExtractedAt),
	}
	for _, v := range c.Versions {
		out.Versions = append(out.Versions, &pb.FactVersionCoverageOp{
			ExtractorVersion: v.Version, DocumentsComplete: v.DocumentsComplete,
			DocumentsPartial: v.DocumentsPartial, LastExtractedAt: rfc3339(v.LastExtractedAt),
		})
	}
	return out, nil
}

// factToOp renders a fact. derived=false (ListFacts): the stored, stated bounds —
// current and superseded_by are left unset because nothing was derived.
func factToOp(f domain.Fact, derived bool) *pb.FactOp {
	op := &pb.FactOp{
		Id: f.ID, EntityId: f.EntityID, Subject: f.Subject, Attribute: f.Attribute, Value: f.Value,
		Actor: f.Actor, AssertedAt: rfc3339(f.AssertedAt), ValidFrom: rfc3339Ptr(f.ValidFrom),
		ValidTo: rfc3339Ptr(f.ValidTo), ValidEmpty: f.ValidEmpty, Polarity: string(f.Polarity),
		Cardinality: string(f.Cardinality), SourceChunkId: f.SourceChunkID,
		SourceDocumentId: f.SourceDocumentID, Quote: f.Quote, Speaker: f.Speaker,
		Tags: append([]string(nil), f.Tags...),
	}
	if derived {
		op.Current, op.SupersededBy = f.Current, f.SupersededBy
	}
	return op
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func rfc3339Ptr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return rfc3339(*t)
}

// FactStatsSource is the fact extractor's counters (memory.FactExtractor).
type FactStatsSource interface {
	Stats() memory.FactExtractorStats
}

// factExtractorStatsOp is memory.FactExtractorStats on the wire.
func factExtractorStatsOp(st memory.FactExtractorStats) *pb.FactExtractorStatsOp {
	return &pb.FactExtractorStatsOp{
		Enqueued: st.Enqueued, DroppedQueueFull: st.Dropped, Skipped: st.Skipped, Extracted: st.Extracted,
		FailedDocuments: st.FailedDocs, PartialDocuments: st.PartialDocs, Stored: st.Stored, LlmCalls: st.LLMCalls,
		Timeouts: st.Timeouts, ParseFailures: st.ParseFailures,
		RefusedQuoteNotFound: st.RefusedQuoteNotFound, RefusedBadShape: st.RefusedBadShape,
		RefusedDuplicate: st.RefusedDuplicate, RefusedOverCap: st.RefusedOverCap,
	}
}
