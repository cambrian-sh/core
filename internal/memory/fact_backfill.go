package memory

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/cambrian-sh/core/domain"
)

// FactBackfill feeds documents stored before (or without) ADR-0136 D4 fact extraction
// through the SAME FactExtractor the ingest path uses
// (T-20261103-no-backfill-path-populates-memory-facts-for). It adds scheduling only:
// which documents (domain.FactBackfillLister — those with no extraction marker), in
// what order, how many at once, how fast. Prompt, grounding, classification,
// supersession and the marker are the extractor's (ExtractDocument → process), so there
// is no second extraction pathway (AGENTS.md §4.3).
//
// Composition: kernel.NewMemoryStack (MemoryStack.FactBackfill), over the ENFORCING
// read store, the substrate lister (postgres.PgKnowledgeStore, documents without a
// memory_fact marker in knowledge_extractions) and the FactExtractor, whose writes pass
// authz.EnforcingKnowledgeWriter. Its caller is the maintenance command
// cmd/facts-backfill (ADR-0136 A1.7).
//
// Access (AGENTS.md §2.4). The backfill serves no principal: it is an operator-run
// maintenance pass that must read every selected document whatever its
// classification, so chunk reads carry the explicit, greppable bypass
// domain.WithScope(ctx, &domain.TagPredicate{Bypass: true}) — through the enforcing
// read store, which honours it — and fact writes run as domain.SystemPrincipal. The
// facts are then classified by the tags of their SOURCE CHUNK, copied by the extractor
// and passed through EnforcingKnowledgeWriter.PutClassifiedItem, exactly as at ingest.
// The ACTOR of each item (the resolution key's, A1-Q2) is still the document's writing
// principal (FactBackfillCandidate.Author), not the system principal, so backfilled and
// later-ingested facts of one principal resolve together. The system
// principal is chosen over the document's own principal on purpose: every decision
// point (the OSS AllowAll and the premium policy) keeps a system write's hint verbatim,
// so the fact carries precisely the classification its chunk was stored under, while
// re-deriving against the author's CURRENT write ceiling could narrow it to something
// the chunk never had.
//
// Order (D5). Supersession decides direction by dates, so any order is correct; but an
// out-of-order arrival costs a close of the incoming fact and a later re-plan, and the
// fuzzy pass sees fewer candidates. With Ordered, documents are grouped by
// classification (supersession never relates two scopes) and each group runs strictly
// in event-time order on one worker; groups run in parallel up to Concurrency.
//
// Resumable. A document is done exactly when the extractor wrote a COMPLETE marker of
// its running version (knowledge_extractions, A2.4), which happens only after its items
// are stored. The extractor marks coverage after every window, so a run that is
// cancelled, crashes, fails part-way or stops at max_calls_per_document leaves a partial
// (or no) marker, and the next run resumes exactly there; a marker of another extractor
// version (a changed prompt or backbone) makes the document pending again. A complete
// marker of the running version costs no LLM call.
type FactBackfill struct {
	lister    domain.FactBackfillLister
	reader    domain.VectorStore
	extractor FactBackfillSink
}

// FactBackfillSink is the extraction target a backfill feeds: it reports whether a
// document's current content (chunk ids in ingest order) needs no further extraction by
// its version (A2.4: a partial or other-version marker does), extracts one document
// synchronously on the caller's ctx, names its version (the lister's SQL pre-filter),
// and states its per-document LLM-call bound and estimate. *FactExtractor implements it.
type FactBackfillSink interface {
	AlreadyExtracted(ctx context.Context, documentID string, chunkIDs []string) (bool, error)
	ExtractDocument(ctx context.Context, documentID, actor string, src domain.SourceMeta, chunks []*domain.Document) (FactDocumentResult, error)
	ExtractorVersion() string
	MaxLLMCallsPerDocument() int
	EstimateLLMCalls(textChars int) int
}

var _ FactBackfillSink = (*FactExtractor)(nil)

// NewFactBackfill builds the backfill. reader must be the enforcing read store
// (MemoryStack.ReadStore) and sink must write through the enforcing tier.
func NewFactBackfill(lister domain.FactBackfillLister, reader domain.VectorStore, sink FactBackfillSink) *FactBackfill {
	return &FactBackfill{lister: lister, reader: reader, extractor: sink}
}

// FactBackfillOptions controls one run.
type FactBackfillOptions struct {
	Filter domain.FactBackfillFilter
	// DryRun only lists and counts: no chunk text is read and no model is called.
	DryRun bool
	// Concurrency is the number of documents (Ordered: classification groups) in
	// flight at once. <= 0 means 1.
	Concurrency int
	// DocsPerMinute caps how many documents start extraction per minute, across all
	// workers (each costs MaxLLMCallsPerDocument calls at most). <= 0 = no cap.
	DocsPerMinute float64
	// Ordered runs each classification group in event-time order on one worker.
	Ordered bool
	// ProgressEvery logs a progress line every N finished documents (<= 0 means 25).
	ProgressEvery int
	Logger        *slog.Logger
}

// FactBackfillReport is what a run found and did.
type FactBackfillReport struct {
	// Selection.
	Candidates  int // documents the lister returned
	AlreadyDone int // of those, already extracted for their current content
	Pending     int // documents to extract
	Chunks      int // content chunks of the pending documents
	Groups      int // work units (classification groups when Ordered, else Pending)
	// Cost: MaxLLMCallsPerDocument is the per-document bound of one run
	// (max_calls_per_document windows x (1 + an assessment call); 0 = no window cap);
	// EstimatedLLMCalls sums each pending document's windows from its text length
	// (A2.3), counting an assessment call per window although one is made only when a
	// window has candidate pairs with no stored verdict.
	MaxLLMCallsPerDocument int
	EstimatedLLMCalls      int

	// Execution (zero on a dry run).
	Extracted           int // documents run through the model and marked
	Skipped             int // found marked at run time (a concurrent run or ingest did it)
	Failed              int // extraction failed; left unmarked for the next run
	Missing             int // no chunk text could be read; left unmarked
	Cancelled           int // not finished because the run was cancelled
	Partial             int // extracted, but stopped before the end of the source (a later run resumes it)
	LLMCalls            int
	AssessmentCalls     int
	AssessmentsReplayed int
	FactsInserted       int
	Elapsed             time.Duration
}

// ErrFactBackfillUnavailable is returned when the stack has no fact extractor or no
// lister (execution.facts.extract_enabled off, or no knowledge substrate).
var ErrFactBackfillUnavailable = errors.New("fact backfill: no fact extractor or candidate lister in this memory stack")

// Run executes one backfill. It owns every goroutine it starts and returns only after
// all of them have exited. The returned error is ctx's when the run was cancelled, a
// listing error, or nil; per-document failures are counted, logged and left for the
// next run.
func (b *FactBackfill) Run(ctx context.Context, opts FactBackfillOptions) (FactBackfillReport, error) {
	var rep FactBackfillReport
	if b == nil || b.lister == nil || b.extractor == nil || b.reader == nil {
		return rep, ErrFactBackfillUnavailable
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	start := time.Now()
	// The explicit kernel bypass and the system principal — see the type comment for
	// why this pass reads as no principal and writes as the kernel.
	ctx = domain.WithScope(ctx, &domain.TagPredicate{Bypass: true})
	ctx = domain.WithPrincipal(ctx, domain.SystemPrincipal)

	filter := opts.Filter
	if filter.ExtractorVersion == "" {
		// A2.4: only a complete marker of the RUNNING version excludes a document.
		filter.ExtractorVersion = b.extractor.ExtractorVersion()
	}
	cands, err := b.lister.ListFactBackfillCandidates(ctx, filter)
	if err != nil {
		return rep, err
	}
	rep.Candidates = len(cands)
	pending := make([]domain.FactBackfillCandidate, 0, len(cands))
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		done, err := b.extractor.AlreadyExtracted(ctx, c.DocumentID, c.ChunkIDs)
		if err != nil {
			// Counted as pending: the extractor checks the marker again before it
			// spends, so an unreadable marker here only inflates the estimate.
			log.WarnContext(ctx, "facts-backfill: extraction marker unreadable; counting the document as pending",
				"document_id", c.DocumentID, "err", err)
		}
		if done {
			rep.AlreadyDone++
			continue
		}
		pending = append(pending, c)
		rep.Chunks += len(c.ChunkIDs)
		rep.EstimatedLLMCalls += b.extractor.EstimateLLMCalls(c.TextChars)
	}
	rep.Pending = len(pending)
	groups := backfillGroups(pending, opts.Ordered)
	rep.Groups = len(groups)
	rep.MaxLLMCallsPerDocument = b.extractor.MaxLLMCallsPerDocument()
	if opts.DryRun || rep.Pending == 0 {
		rep.Elapsed = time.Since(start)
		return rep, nil
	}

	workers := opts.Concurrency
	if workers <= 0 {
		workers = 1
	}
	workers = min(workers, len(groups))
	every := opts.ProgressEvery
	if every <= 0 {
		every = 25
	}
	limit := rate.Inf
	if opts.DocsPerMinute > 0 {
		limit = rate.Limit(opts.DocsPerMinute / 60)
	}
	r := &backfillRun{b: b, log: log, limiter: rate.NewLimiter(limit, 1), every: every, total: rep.Pending, start: start, rep: &rep}

	work := make(chan []domain.FactBackfillCandidate)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range work {
				for i, c := range g {
					if ctx.Err() != nil {
						r.cancelled(len(g) - i)
						break
					}
					r.one(ctx, c)
				}
			}
		}()
	}
	fed := 0
feed:
	for _, g := range groups {
		select {
		case work <- g:
			fed++
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	for _, g := range groups[fed:] {
		r.cancelled(len(g))
	}
	rep.Elapsed = time.Since(start)
	log.InfoContext(ctx, "facts-backfill: finished", reportAttrs(rep)...)
	return rep, ctx.Err()
}

// backfillGroups splits the (event-time ordered) pending list into work units. Ordered:
// one unit per classification, each keeping event-time order, units ordered by their
// first document. Unordered: one unit per document.
func backfillGroups(pending []domain.FactBackfillCandidate, ordered bool) [][]domain.FactBackfillCandidate {
	// The lister orders by session time; re-sort stably so a caller-supplied list
	// (or a lister that cannot order) still yields event-time order.
	sorted := append([]domain.FactBackfillCandidate(nil), pending...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SessionTime.Before(sorted[j].SessionTime) })
	if !ordered {
		out := make([][]domain.FactBackfillCandidate, len(sorted))
		for i, c := range sorted {
			out[i] = []domain.FactBackfillCandidate{c}
		}
		return out
	}
	idx := map[string]int{}
	var out [][]domain.FactBackfillCandidate
	for _, c := range sorted {
		k := domain.FactScopeKey(c.Tags)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], c)
	}
	return out
}

// backfillRun is one Run's shared state across its workers.
type backfillRun struct {
	b       *FactBackfill
	log     *slog.Logger
	limiter *rate.Limiter
	every   int
	total   int
	start   time.Time

	mu       sync.Mutex
	rep      *FactBackfillReport
	finished int
}

func (r *backfillRun) cancelled(n int) {
	r.mu.Lock()
	r.rep.Cancelled += n
	r.mu.Unlock()
}

// one extracts one document. Every outcome is counted; none aborts the run.
func (r *backfillRun) one(ctx context.Context, c domain.FactBackfillCandidate) {
	// Re-checked at run time: another backfill or an ingest may have extracted it
	// since the listing, and a marked document must not spend a limiter token.
	if done, err := r.b.extractor.AlreadyExtracted(ctx, c.DocumentID, c.ChunkIDs); err == nil && done {
		r.finish(ctx, func(rep *FactBackfillReport) { rep.Skipped++ })
		return
	}
	if err := r.limiter.Wait(ctx); err != nil {
		r.cancelled(1)
		return
	}
	chunks, err := r.b.readChunks(ctx, c.ChunkIDs)
	if err != nil || len(chunks) == 0 {
		if ctx.Err() != nil {
			r.cancelled(1)
			return
		}
		r.log.WarnContext(ctx, "facts-backfill: document chunks unreadable; left for the next run",
			"document_id", c.DocumentID, "chunks", len(c.ChunkIDs), "err", err)
		r.finish(ctx, func(rep *FactBackfillReport) { rep.Missing++ })
		return
	}
	if len(chunks) < len(c.ChunkIDs) {
		r.log.WarnContext(ctx, "facts-backfill: some chunks of the document were not readable; extracting from the rest",
			"document_id", c.DocumentID, "listed", len(c.ChunkIDs), "read", len(chunks))
	}
	res, err := r.b.extractor.ExtractDocument(ctx, c.DocumentID, c.Author, c.SourceMeta(), chunks)
	if err != nil && ctx.Err() != nil {
		r.mu.Lock()
		r.rep.LLMCalls += res.LLMCalls
		r.rep.Cancelled++
		r.mu.Unlock()
		return
	}
	if err != nil {
		r.log.WarnContext(ctx, "facts-backfill: extraction failed; left unmarked for the next run",
			"document_id", c.DocumentID, "err", err)
	}
	r.finish(ctx, func(rep *FactBackfillReport) {
		rep.LLMCalls += res.LLMCalls
		rep.AssessmentCalls += res.AssessmentCalls
		if res.AssessmentReplayed {
			rep.AssessmentsReplayed++
		}
		switch {
		case err != nil:
			rep.Failed++
		case res.Skipped:
			rep.Skipped++
		default:
			rep.Extracted++
			rep.FactsInserted += res.Stats.Inserted
			if res.Partial {
				rep.Partial++
			}
		}
	})
}

// finish applies one document's outcome and logs progress every r.every documents.
func (r *backfillRun) finish(ctx context.Context, apply func(*FactBackfillReport)) {
	r.mu.Lock()
	apply(r.rep)
	r.finished++
	n := r.finished
	var snap FactBackfillReport
	if n%r.every == 0 || n == r.total {
		snap = *r.rep
	}
	r.mu.Unlock()
	if n%r.every != 0 && n != r.total {
		return
	}
	elapsed := time.Since(r.start)
	attrs := []any{"done", n, "of", r.total, "elapsed", elapsed.Round(time.Second).String()}
	if n > 0 && n < r.total {
		eta := time.Duration(float64(elapsed) / float64(n) * float64(r.total-n))
		attrs = append(attrs, "eta", eta.Round(time.Second).String())
	}
	attrs = append(attrs, "extracted", snap.Extracted, "skipped", snap.Skipped, "failed", snap.Failed,
		"missing", snap.Missing, "llm_calls", snap.LLMCalls, "facts_inserted", snap.FactsInserted)
	r.log.InfoContext(ctx, "facts-backfill: progress", attrs...)
}

// readChunks reads the documents' chunks through the enforcing read store (the bypass
// is on ctx) and returns them in the listed order — the order the extractor's turn
// indexes and content hash use.
func (b *FactBackfill) readChunks(ctx context.Context, ids []string) ([]*domain.Document, error) {
	var (
		docs []domain.Document
		err  error
	)
	if cb, ok := b.reader.(interface {
		ChunksByIDs(context.Context, []string) ([]domain.Document, error)
	}); ok {
		docs, err = cb.ChunksByIDs(ctx, ids)
	} else {
		docs, err = b.reader.GetBatch(ctx, ids)
	}
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*domain.Document, len(docs))
	for i := range docs {
		byID[docs[i].ID] = &docs[i]
	}
	out := make([]*domain.Document, 0, len(ids))
	for _, id := range ids {
		if d, ok := byID[id]; ok && d.Text != "" {
			out = append(out, d)
		}
	}
	return out, nil
}

func reportAttrs(r FactBackfillReport) []any {
	return []any{
		"candidates", r.Candidates, "already_done", r.AlreadyDone, "pending", r.Pending,
		"extracted", r.Extracted, "skipped", r.Skipped, "failed", r.Failed, "missing", r.Missing,
		"cancelled", r.Cancelled, "partial", r.Partial, "llm_calls", r.LLMCalls, "assessment_calls", r.AssessmentCalls,
		"assessments_replayed", r.AssessmentsReplayed, "facts_inserted", r.FactsInserted, "elapsed", r.Elapsed.Round(time.Second).String(),
	}
}
