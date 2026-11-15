package memory

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// AnswerEvidence narrows the evidence pool an ADR-0081 cited answer is
// synthesized from (ENG-223). The operator plane's AnswerMemory carries
// source / session / min_importance filters and a top_k "evidence-pool cap"
// (operator.proto AnswerMemoryRequest); they must apply BEFORE synthesis, or
// the answer can cite text the caller filtered out and the handler's citation
// markers drift from the synthesizer's [n] labels once a filter drops a row.
//
// Keep (nil = keep all) is the caller's row filter; Max (≤ 0 = no cap) caps the
// pool after filtering. These are request filters, not access control: scope is
// still enforced by the read store underneath (ADR-0085).
type AnswerEvidence struct {
	Keep func(domain.SearchResult) bool
	Max  int
}

type ctxKeyAnswerEvidence struct{}

// WithAnswerEvidence attaches the pool narrowing to the request. A context value,
// like WithAgenticSynthesis: it is a property of one request and must survive the
// agentic hops unchanged while the MemoryAnswerer port stays stable.
func WithAnswerEvidence(ctx context.Context, opts AnswerEvidence) context.Context {
	return context.WithValue(ctx, ctxKeyAnswerEvidence{}, opts)
}

// AnswerEvidenceFrom returns the request's pool narrowing, if one was attached.
func AnswerEvidenceFrom(ctx context.Context) (AnswerEvidence, bool) {
	opts, ok := ctx.Value(ctxKeyAnswerEvidence{}).(AnswerEvidence)
	return opts, ok
}

// narrowAnswerEvidence applies the request's AnswerEvidence (if any) to rows,
// preserving order so evidence[n-1] still resolves the synthesizer's [n].
func narrowAnswerEvidence(ctx context.Context, rows []domain.SearchResult) []domain.SearchResult {
	opts, ok := AnswerEvidenceFrom(ctx)
	if !ok {
		return rows
	}
	out := rows
	if opts.Keep != nil {
		out = make([]domain.SearchResult, 0, len(rows))
		for _, r := range rows {
			if opts.Keep(r) {
				out = append(out, r)
			}
		}
	}
	if opts.Max > 0 && len(out) > opts.Max {
		out = out[:opts.Max]
	}
	return out
}
