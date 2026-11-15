// Package llmusage renders the per-request LLM usage tally (domain.LLMUsageTally,
// fed by the ADR-0131 SpendMeter) as the cambrian-llm-usage gRPC trailer. It is
// shared by the agent plane (internal/substrate/network) and the operator plane
// (internal/substrate/operator) so both memory surfaces report the same thing
// in the same grammar (ENG-534); neither plane imports the other.
package llmusage

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Trailer is the gRPC trailer key carrying the kernel's own LLM calls made on
// behalf of one memory call — agent plane IngestMemory/QueryMemory, operator plane
// IngestMemory/QueryMemory/AnswerMemory (ENG-534): the
// calls the ADR-0131 SpendMeter booked to the request's domain.LLMUsageTally.
// Diagnostic, not a contract (like IngestTimingTrailer): keys may change, and
// adding it needed no proto change.
//
// Format, the same "key=value,..." grammar as the ingest-timing trailer, counts
// suffixed '#':
//
//	calls#=3,input#=1200,output#=310,cache_read#=0,estimated#=0,cost_usd=0.000412,
//	facts.calls#=2,facts.input#=...,agent_step:retrieval_agent.calls#=1,...,
//	generator:haiku-local.calls#=3
//
// The unprefixed keys are the request's total; "<purpose>." keys split it by the
// purpose the meter booked (the providers.json role — facts, memory, router … —
// or agent_step[:<agent id>] for calls an agent made under a lease the request
// minted). "generator:<id>.calls#" keys count the calls each generator ANSWERED
// (the id served after any fallback; ENG-453). The trailer is ALWAYS sent, so "calls#=0" is an observed zero, while a
// missing trailer means a kernel that does not report.
//
// Completeness: a call is in the trailer only when it finished before the
// response was written. With execution.ingestion.sync_ingest_wait_for_facts the
// fact lane does; async lanes that finish later (the KG lane without
// sync_ingest_wait_for_kg, tier-2, verification) are booked to the session but
// not to this response.
const Trailer = "cambrian-llm-usage"

// WithTally opens a per-request tally on ctx and returns the function that
// writes it as the Trailer. Call the function once, after the work
// and before returning (grpc.SetTrailer is best-effort: it fails only outside a
// gRPC call, i.e. in unit tests that call the handler directly).
func WithTally(ctx context.Context) (context.Context, func()) {
	tctx, tally := domain.WithLLMUsageTally(ctx)
	return tctx, func() {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(Trailer, Format(tally))) // best-effort diagnostic, see above
	}
}

// Format renders a tally in the Trailer format.
func Format(t *domain.LLMUsageTally) string {
	var b strings.Builder
	writeLLMUsage(&b, "", t.Total())
	for _, p := range t.ByPurpose() {
		writeLLMUsage(&b, sanitizePurpose(p.Purpose)+".", p.Usage)
	}
	gens := t.ByGenerator()
	ids := make([]string, 0, len(gens))
	for id := range gens {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b.WriteString(",generator:")
		b.WriteString(sanitizePurpose(id))
		b.WriteString(".calls#=")
		b.WriteString(strconv.Itoa(gens[id]))
	}
	return b.String()
}

func writeLLMUsage(b *strings.Builder, prefix string, u domain.LLMUsage) {
	for _, kv := range []struct {
		k string
		v int
	}{
		{"calls", u.Calls}, {"input", u.InputTokens}, {"output", u.OutputTokens},
		{"cache_read", u.CacheReadTokens}, {"estimated", u.EstimatedCalls},
	} {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(prefix)
		b.WriteString(kv.k)
		b.WriteString("#=")
		b.WriteString(strconv.Itoa(kv.v))
	}
	b.WriteByte(',')
	b.WriteString(prefix)
	b.WriteString("cost_usd=")
	b.WriteString(strconv.FormatFloat(u.CostUSD, 'f', -1, 64))
}

// sanitizePurpose keeps a purpose label from breaking the trailer grammar: the
// separators ',', '=' and '.' (and whitespace) become '_'.
func sanitizePurpose(p string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ',', '=', '.', ' ', '\t', '\n', '\r':
			return '_'
		}
		return r
	}, p)
}
