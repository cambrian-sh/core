package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync/atomic"
)

// The query correlation handle (ADR-0126 D6).
//
// A retrieval mints its own QueryID deep inside the memory subsystem and hands it
// only to in-process observers, so nothing a CALLER holds can later name the
// receipt its question produced. That is the one piece the week-6 exit test
// ("verifies the receipt chain") cannot be wired around: `get_receipt` would have
// nothing to look up.
//
// The fix is a handle the boundary seeds and the retrieval path adopts, carried on
// the context beside the principal and the surface (authz_context.go) for the same
// reason those are: the intermediate helpers between a transport and the emit site
// must not each grow a parameter.
//
// IT IS A PREFIX, NOT AN ID. Agentic retrieval emits ONE DECISION PER HOP, so a
// single tool call legitimately produces several receipts. A 1:1 id would make
// hop 2 land on hop 1's handle — one row wins, the rest are unfindable, and the
// chain a caller asks for reads incomplete without ever saying so. Each hop
// therefore extends the prefix (`<corr>-h1`, `<corr>-h2`, …) and the read side
// filters on the prefix to recover the whole call as one chain.

// queryCorrelationCtxKey is the private context key carrying the handle.
type queryCorrelationCtxKey struct{}

// queryCorrelation is the prefix plus the hop counter that extends it.
//
// The counter lives HERE, next to the prefix, rather than in whichever subsystem
// happens to loop: there is more than one hop loop in the retrieval path (the
// greedy bridge loop and the up-front decomposition loop, plus that loop's
// original-question pass), and a numbering rule defined once cannot be half
// applied by a loop somebody adds later.
//
// It is a pointer value in the context so the counter is shared by every hop of
// one call — the standard shape for per-request accounting — and atomic because
// nothing forbids a future loop from retrieving two hops concurrently.
type queryCorrelation struct {
	prefix string
	hops   atomic.Uint64
}

// WithQueryCorrelation returns a child context carrying the correlation prefix a
// caller can later use to fetch this call's receipts. An empty id is a no-op, so a
// transport with no handle to offer does not have to branch.
//
// Each call installs a FRESH hop counter: two tool invocations that (legally)
// chose the same prefix still number their own hops from 1 rather than continuing
// each other's.
func WithQueryCorrelation(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, queryCorrelationCtxKey{}, &queryCorrelation{prefix: id})
}

// QueryCorrelationFromContext returns the correlation PREFIX carried by ctx. The
// boolean reports presence; absence is the ordinary case (an in-process read, an
// agent call, any transport that seeded no handle) and never an error.
func QueryCorrelationFromContext(ctx context.Context) (string, bool) {
	c, ok := ctx.Value(queryCorrelationCtxKey{}).(*queryCorrelation)
	if !ok || c == nil || c.prefix == "" {
		return "", false
	}
	return c.prefix, true
}

// NextQueryCorrelationID returns the handle for the NEXT hop of this call —
// `<prefix>-h1`, then `<prefix>-h2`, and so on — advancing the counter.
//
// Call it exactly once per emitted decision. It returns false when ctx carries no
// correlation; the retrieval entry points seed one first (EnsureQueryCorrelation),
// so that is a wiring gap, not a normal path.
func NextQueryCorrelationID(ctx context.Context) (string, bool) {
	c, ok := ctx.Value(queryCorrelationCtxKey{}).(*queryCorrelation)
	if !ok || c == nil || c.prefix == "" {
		return "", false
	}
	return c.prefix + "-h" + strconv.FormatUint(c.hops.Add(1), 10), true
}

// DerivedQueryCorrelation is the correlation prefix of a retrieval whose
// boundary seeded none (ENG-2068, owner decision 2026-11-18): derived from what
// the request already carries, never from a clock or a process-local counter
// (AGENTS.md §2.6), so a retried request names the same receipts again.
//
// The anchor is the most specific request handle on ctx: the plan step the call
// serves (run id + step index, set from the caller's lease), else the
// conversation/session id; then the principal and the question text. Two
// different questions, principals, sessions or plan steps get different
// prefixes; the same request asked twice gets the same one — that is the point:
// a retry is the same retrieval, and its receipts must say so. Hops extend the
// prefix as for a seeded handle (`<prefix>-h1`, `-h2`, …).
func DerivedQueryCorrelation(ctx context.Context, principal, query string) string {
	anchor := ""
	if s, ok := StepCallFrom(ctx); ok {
		anchor = "run:" + string(s.RunID) + "/" + strconv.Itoa(s.StepIndex)
	} else if sid, ok := SessionIDFromContext(ctx); ok {
		anchor = "session:" + string(sid)
	}
	h := sha256.New()
	for _, part := range []string{anchor, principal, query} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "q-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// EnsureQueryCorrelation returns ctx unchanged when it carries a correlation
// (the boundary's handle wins), else a child carrying DerivedQueryCorrelation
// with a fresh hop counter. The retrieval entry points call it once per call,
// before any hop runs, so every hop of the call shares one counter.
func EnsureQueryCorrelation(ctx context.Context, principal, query string) context.Context {
	if _, ok := QueryCorrelationFromContext(ctx); ok {
		return ctx
	}
	return WithQueryCorrelation(ctx, DerivedQueryCorrelation(ctx, principal, query))
}
