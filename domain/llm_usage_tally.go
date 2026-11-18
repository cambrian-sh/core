package domain

import (
	"context"
	"sort"
	"sync"
)

// LLMUsageTally counts the LLM calls made on behalf of ONE request (one
// IngestMemory, one QueryMemory) across layers that share no return path: the
// fact extractor's worker, the agentic retrieval chain's retrieval_agent stream,
// a memory organ. It rides the context, like StageClock, so no port signature
// changes for an accounting figure.
//
// It is FED ONLY by the ADR-0131 SpendMeter (internal/infrastructure/llm), the
// kernel's one metering point: the meter adds every call it books to the tally
// found on the call's ctx (Provider.Acquire door) or captured on the call's lease
// (gateway door — the lease remembers the tally of the ctx it was acquired in).
// No caller counts its own calls, so a new organ is counted by construction.
//
// A tally is a request's view, not a ledger: the session totals stay in the
// SpendMeter. Work that outlives the request (an async fact job still running
// when the response is sent) is added after the response was written and is
// therefore not in what the response reported.
//
// Every method is nil-safe. Safe for concurrent use. A tally opened inside a ctx
// that already carries one forwards every call to its parent too, so an outer
// request still sees the calls of an inner one.
type LLMUsageTally struct {
	mu        sync.Mutex
	byPurpose map[string]LLMUsage
	// byGenerator counts calls per generator id that ANSWERED them (ENG-453:
	// every result records the answering model).
	byGenerator map[string]int
	parent      *LLMUsageTally
}

// LLMUsage is the usage of a set of LLM calls. Token counts are provider-reported
// where the provider returned usage; EstimatedCalls counts the calls whose usage
// came from the len/4 fallback (ADR-0131 D2).
type LLMUsage struct {
	Calls           int
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	// CacheWriteTokens is the input written to the provider's prompt cache
	// (part of InputTokens; zero when the provider reports no writes).
	CacheWriteTokens int
	EstimatedCalls   int
	CostUSD          float64
}

// Add returns u + o, field-wise.
func (u LLMUsage) Add(o LLMUsage) LLMUsage {
	return LLMUsage{
		Calls:            u.Calls + o.Calls,
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		EstimatedCalls:   u.EstimatedCalls + o.EstimatedCalls,
		CostUSD:          u.CostUSD + o.CostUSD,
	}
}

type llmUsageTallyKey struct{}

// WithLLMUsageTally returns ctx carrying a fresh tally, and the tally. When ctx
// already carries one, the new tally forwards to it.
func WithLLMUsageTally(ctx context.Context) (context.Context, *LLMUsageTally) {
	t := &LLMUsageTally{byPurpose: map[string]LLMUsage{}, byGenerator: map[string]int{}, parent: LLMUsageTallyFrom(ctx)}
	return context.WithValue(ctx, llmUsageTallyKey{}, t), t
}

// LLMUsageTallyFrom returns the tally on ctx, or nil.
func LLMUsageTallyFrom(ctx context.Context) *LLMUsageTally {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(llmUsageTallyKey{}).(*LLMUsageTally)
	return t
}

// Add books one call (or a batch of them) under purpose ("" = "model").
func (t *LLMUsageTally) Add(purpose string, u LLMUsage) {
	if purpose == "" {
		purpose = "model"
	}
	for ; t != nil; t = t.parent {
		t.mu.Lock()
		t.byPurpose[purpose] = t.byPurpose[purpose].Add(u)
		t.mu.Unlock()
	}
}

// AddGenerator books n calls answered by the generator id ("" is ignored).
func (t *LLMUsageTally) AddGenerator(id string, n int) {
	if id == "" || n == 0 {
		return
	}
	for ; t != nil; t = t.parent {
		t.mu.Lock()
		if t.byGenerator == nil {
			t.byGenerator = map[string]int{}
		}
		t.byGenerator[id] += n
		t.mu.Unlock()
	}
}

// ByGenerator returns a copy of the per-generator call counts.
func (t *LLMUsageTally) ByGenerator() map[string]int {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.byGenerator))
	for id, n := range t.byGenerator {
		out[id] = n
	}
	return out
}

// Total is the sum over every purpose.
func (t *LLMUsageTally) Total() LLMUsage {
	var sum LLMUsage
	if t == nil {
		return sum
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, u := range t.byPurpose {
		sum = sum.Add(u)
	}
	return sum
}

// PurposeUsage is one purpose's share of a tally.
type PurposeUsage struct {
	Purpose string
	Usage   LLMUsage
}

// ByPurpose returns a copy of the per-purpose usage, sorted by purpose.
func (t *LLMUsageTally) ByPurpose() []PurposeUsage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PurposeUsage, 0, len(t.byPurpose))
	for p, u := range t.byPurpose {
		out = append(out, PurposeUsage{Purpose: p, Usage: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Purpose < out[j].Purpose })
	return out
}

type llmCallLabelKey struct{}

// WithLLMCallLabel returns ctx whose LLM calls the SpendMeter books under label
// instead of the generator's purpose — a sub-label for one organ's distinct kind
// of call on a shared purpose handle. The fact extractor's extraction and its
// assessment judge both use GeneratorFor(PurposeFacts); the judge's calls carry
// LLMCallLabelFactsAssess so a per-request report (the cambrian-llm-usage trailer)
// and the session ledger split them
// (T-20261104-fact-extraction-and-fact-assessment-share-llm). Admission and the
// spend ceiling are unchanged: only the label a booked call is counted under moves.
// "" leaves ctx as it is.
func WithLLMCallLabel(ctx context.Context, label string) context.Context {
	if label == "" {
		return ctx
	}
	return context.WithValue(ctx, llmCallLabelKey{}, label)
}

// LLMCallLabelFrom returns the call label on ctx, or "".
func LLMCallLabelFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	l, _ := ctx.Value(llmCallLabelKey{}).(string)
	return l
}

// LLMCallLabelFactsAssess labels the ADR-0136 A1.2 fact-assessment judge's calls.
// No '.', ',' or '=' — the trailer grammar's separators.
const LLMCallLabelFactsAssess = "facts_assess"
