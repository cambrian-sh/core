package memory

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// ── Synthesis opt-in (T-20261102-agentic-synthesis-llm-call-is-computed-for) ──

// ctxKeyAgenticSynthesis marks a request whose caller reads the agentic control
// row (its typed status, synthesized text and trace).
type ctxKeyAgenticSynthesis struct{}

// WithAgenticSynthesis opts the request in to the agentic loop's closing
// synthesis and its _agentic_control row. Set by QueryMemory when the caller
// sends x-agentic-control (the benchmark harness), and by the answer lane, whose
// abstention rule reads the row's status. Without it the loop returns retrieved
// evidence only and makes no synthesis LLM call — every other caller has the row
// stripped anyway, so computing it was pure cost.
//
// A context value, not a parameter on MemorySearcher.Search: the opt-in is a
// property of the request (like scope and session), it must survive the hops
// through agenticSearch/decompSearch unchanged, and the port stays stable.
func WithAgenticSynthesis(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyAgenticSynthesis{}, true)
}

// AgenticSynthesisWanted reports whether the request opted in (WithAgenticSynthesis).
func AgenticSynthesisWanted(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyAgenticSynthesis{}).(bool)
	return v
}

// ── Echo plans (T-20261102-decomposition-re-runs-the-original-question) ──

// placeholderRE matches a decomposition {n} placeholder.
var placeholderRE = regexp.MustCompile(`\{(\d+)\}`)

// isEchoPlan reports whether a decomposition plan is the question itself: ONE
// sub-question, no placeholder, and equal to the question after
// normalizeQuestion. Deliberately exact after normalisation, not a similarity
// threshold: a one-word change ("gave" → "give", "I" → "the user", a tense or an
// entity) is a rewrite the planner chose, and a fuzzy rule tight enough to
// accept the typo fixes would also accept a swapped entity. Measured on the
// 2026-11-02 LongMemEval arm1 log: 15 of 29 single-sub-question plans are exact
// echoes under this rule; the other 14 are rewrites and keep their plan.
func isEchoPlan(query string, subqs []string) bool {
	if len(subqs) != 1 || placeholderRE.MatchString(subqs[0]) {
		return false
	}
	nq := normalizeQuestion(query)
	return nq != "" && nq == normalizeQuestion(subqs[0])
}

// normalizeQuestion lowercases, turns every rune that is not a letter or digit
// (punctuation, quotes, symbols) into a space and collapses whitespace — so case,
// trailing "?", curly quotes and spacing differences do not hide an echo.
func normalizeQuestion(s string) string {
	mapped := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(mapped), " ")
}

// ── Pass scheduling (T-20261102-independent-decomposition-sub-questions-are) ──

// subqOutcome is one sub-question's pass: the query after placeholder
// substitution, its hits, and its grounded answer ("" when none).
type subqOutcome struct {
	resolved string
	hits     []domain.SearchResult
	ans      string
	err      error
}

// decompRun is the result of runDecompPasses, in PLAN order whatever the
// execution order was. subs[i] is meaningful only for i < firstErr; firstErr ==
// len(subs) when no sub-question pass failed.
type decompRun struct {
	orig     []domain.SearchResult
	origErr  error
	subs     []subqOutcome
	firstErr int
}

// subqDeps returns, per sub-question, the earlier sub-questions whose answers it
// needs: every {n} in its text or its ref with n-1 < i. A placeholder pointing
// at itself or a later sub-question is not a dependency — the sequential loop
// never had that answer either, and substituteAnswers strips it.
func subqDeps(subqs, refs []string) [][]int {
	deps := make([][]int, len(subqs))
	for i, sq := range subqs {
		text := sq
		if i < len(refs) {
			text += " " + refs[i]
		}
		seen := map[int]bool{}
		for _, m := range placeholderRE.FindAllStringSubmatch(text, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 1 || n-1 >= i || seen[n-1] {
				continue
			}
			seen[n-1] = true
			deps[i] = append(deps[i], n-1)
		}
	}
	return deps
}

// runDecompPasses runs the coverage-floor pass on the original question and
// every sub-question pass (retrieve, then extract the grounded answer).
//
// subqConcurrency <= 1: the original sequential loop — floor pass, then each
// sub-question in order, stopping at the first failed one.
//
// subqConcurrency > 1: waves. Each wave is every unfinished sub-question whose
// dependencies (subqDeps) are resolved; the floor pass joins the first wave; at
// most subqConcurrency passes run at once. A dependent sub-question still waits
// for the answers it substitutes. Outcomes are written to per-index slots, so the
// caller assembles hopResults and the trace in plan order — the same lists, in
// the same order, as the sequential loop, hence the same interleave and ranking.
// A failure at index e stops scheduling anything at or after e, mirroring the
// sequential break (sub-questions already running finish and are discarded).
//
// Each worker gets the request's own ctx/sctx: scope, session, event-time window,
// caller lease (spend metering, ADR-0131) and cancellation all travel with it
// (AGENTS.md §2.4, §2.7). LLM calls go through the same planner, so the global
// LLM semaphore still bounds them. Every goroutine is joined before return.
func (q *QueryService) runDecompPasses(ctx, sctx context.Context, dp DecomposePlanner, query string, subqs, refs []string, callerID, docType string, spread bool) decompRun {
	n := len(subqs)
	run := decompRun{subs: make([]subqOutcome, n), firstErr: n}
	// subst[j] is what placeholder {j+1} resolves to downstream: the grounded
	// answer when found, else the sub-question's descriptive ref (so an unfound
	// sub-answer does NOT blank out later queries — the don't-break-on-empty fix).
	subst := make([]string, n)
	resolve := func(i int) {
		resolution := run.subs[i].ans
		if resolution == "" && i < len(refs) {
			resolution = substituteAnswers(refs[i], subst[:i])
		}
		subst[i] = resolution
	}

	if q.subqConcurrency <= 1 {
		run.orig, run.origErr = q.searchByType(sctx, query, callerID, docType, spread)
		for i := range subqs {
			run.subs[i] = q.runSubQuestion(ctx, sctx, dp, substituteAnswers(subqs[i], subst[:i]), callerID, docType, spread)
			if run.subs[i].err != nil {
				run.firstErr = i
				break
			}
			resolve(i)
		}
		return run
	}

	sem := make(chan struct{}, q.subqConcurrency)
	var origWG sync.WaitGroup
	origWG.Add(1)
	go func() {
		defer origWG.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		defer func() {
			if r := recover(); r != nil {
				run.orig, run.origErr = nil, fmt.Errorf("decomp: coverage-floor pass panicked: %v", r)
			}
		}()
		run.orig, run.origErr = q.searchByType(sctx, query, callerID, docType, spread)
	}()

	deps := subqDeps(subqs, refs)
	done := make([]bool, n)
	for {
		var ready []int
		for i := 0; i < n && i < run.firstErr; i++ {
			if done[i] {
				continue
			}
			ok := true
			for _, d := range deps[i] {
				if !done[d] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			break
		}
		var wg sync.WaitGroup
		for _, i := range ready {
			// Resolved on this goroutine, before launch: subst is written only
			// between waves, so workers never touch it.
			resolved := substituteAnswers(subqs[i], subst[:i])
			wg.Add(1)
			go func(i int, resolved string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				run.subs[i] = q.runSubQuestion(ctx, sctx, dp, resolved, callerID, docType, spread)
			}(i, resolved)
		}
		wg.Wait()
		for _, i := range ready { // ascending: resolve(i) reads only subst[:i] deps, all resolved
			done[i] = true
			if run.subs[i].err != nil {
				if i < run.firstErr {
					run.firstErr = i
				}
				continue
			}
			resolve(i)
		}
	}
	origWG.Wait()
	return run
}

// runSubQuestion retrieves one resolved sub-question and extracts its grounded
// answer from ITS OWN top chunks (never parametric). A panic in a parallel worker
// would take the process down, so it is recovered into the pass's error, which
// the caller treats like any failed pass (fail-open to what came before).
func (q *QueryService) runSubQuestion(ctx, sctx context.Context, dp DecomposePlanner, resolved, callerID, docType string, spread bool) (out subqOutcome) {
	out.resolved = resolved
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "decomp: sub-question pass panicked; treated as a failed pass", "panic", r)
			out = subqOutcome{resolved: resolved, err: fmt.Errorf("decomp: sub-question pass panicked: %v", r)}
		}
	}()
	hits, err := q.searchByType(sctx, resolved, callerID, docType, spread)
	if err != nil {
		out.err = err
		return out
	}
	out.hits = hits
	const maxContextChunks = 16
	texts := make([]string, 0, maxContextChunks)
	for j, h := range hits {
		if j >= maxContextChunks {
			break
		}
		texts = append(texts, h.Document.Text)
	}
	ans, aerr := dp.AnswerSubQuestion(ctx, resolved, texts)
	if aerr != nil {
		ans = ""
	}
	out.ans = strings.TrimSpace(ans)
	return out
}
