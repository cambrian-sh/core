package network

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// withTaintScope puts the caller's Rule-of-Two taint scope on ctx (ENG-531,
// domain/rule_of_two.go): the identities its provenance is tracked under, all
// read from the kernel-held lease binding — the task session, the conversation
// (a plan step's conversation through its session, the second hop
// conversationOfBinding takes), the run — and the lease itself only when the
// lease is known, held by this caller and bound to none of them. A lease the
// caller does not hold, or no lease at all, contributes nothing, so a caller
// cannot shed its taint by presenting a fresh or foreign lease: with no identity
// left the scope is zero, which the gate treats as tainted (fail closed).
func (s *Server) withTaintScope(ctx context.Context, lease domain.LeaseID) context.Context {
	var b domain.LeaseBinding
	known := false
	if lease != "" {
		b, known = s.resolveLeaseFor(ctx, lease)
	}
	conv := domain.ConversationIDFromContext(ctx)
	if conv == "" && known {
		conv = s.conversationOfBinding(ctx, b)
	}
	sid, _ := domain.SessionIDFromContext(ctx)
	scopeLease := domain.LeaseID("")
	if known {
		scopeLease = lease
	}
	return domain.WithTaintScope(ctx, domain.NewTaintScope(scopeLease, b, sid, conv))
}

// observeRecall reports a recall's results to the Rule-of-Two gate as content the
// session consumed, each under its record's source class and tags, and returns
// the class of each result (index-aligned) for the result metadata label. A no-op
// for the gate when no tool executor or gate is wired; the labels are computed
// either way (they are provenance facts, not policy).
func (s *Server) observeRecall(ctx context.Context, results []domain.SearchResult) []domain.SourceClass {
	classes := make([]domain.SourceClass, len(results))
	for i, r := range results {
		tags := domain.RecordTagsOf(r.Document.Metadata)
		classes[i] = domain.RecordSourceClass(r.Document.Metadata, tags)
		if s.ToolExecutor != nil {
			s.ToolExecutor.ObserveContent(ctx, classes[i], tags)
		}
	}
	return classes
}
