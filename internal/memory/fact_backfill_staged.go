package memory

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// The TWO-PHASE ordered backfill (T-20261103-fact-backfill-processes-each-principal-s;
// ADR-0136 A2 "Backfill concurrency"). With Ordered and GroupConcurrency > 1, one
// classification group runs in three phases instead of one document at a time, and ends
// in EXACTLY the state the serial ordered run (GroupConcurrency 1) produces for the same
// model answers: the same knowledge_items, knowledge_assessments (ids and verdicts),
// current resolutions, predicate_profiles and extraction markers.
//
//  1. Extract — every pending document of the group concurrently (GroupConcurrency at
//     once): windows, items and embeddings exactly as process() writes them, but the
//     assessment pass is deferred and the extraction marker does not advance (a marker
//     means "extracted AND assessed"). Each window is recorded in the extraction journal
//     (domain.ExtractionWindow) instead, so a stop here resumes with no second
//     extraction call.
//  2. Speculate — every window's judge call concurrently, against the stored corpus with
//     the items the serial run had NOT written yet hidden (see factVisibility). Nothing is
//     stored; the answers are kept by their keyed assessment id.
//  3. Commit — the windows in serial (event-time, then window) order, one at a time: the
//     window's candidate set is planned AGAIN against the live store, where every earlier
//     window is already committed, so profiles, assessments and items are exactly what
//     the serial run saw at this point. Its keyed id then either names a stored assessment
//     (a replay, as in the serial run), or a speculated answer (used as the call), or —
//     when an earlier commit changed the plan, e.g. a predicate's learned cardinality
//     flipped after its third vote — neither: then the remaining windows are speculated
//     again (a wave) under the live state. The commit is PutAssessment + the profile vote
//     + the marker, the serial path's own code (commitAssessment, vote, mark).
//
// Why that is the serial run's state. The serial run's assessment of window w reads (a)
// the profile of each fact's predicate, (b) the chain of each fact's key, (c) the k most
// similar same-subject facts — (b) and (c) over the items stored by then — and keys the
// call on the candidate set. Phase 3 reads (a) live, after committing exactly the earlier
// windows' verdicts and votes in the same order. For (b) and (c) the group's later items
// are hidden: a chain's membership and each member's own start (all the prompt shows)
// do not depend on the other members, so (b) is filtered after the read; (c) is not —
// its top-k and its "derived interval empty" filter depend on which items exist — so it
// is read with SimilarFactsHiding, which excludes them from the ranking and from every
// derivation. The (asserted_at, id) "before" rule still applies on top, unchanged.
// Resolutions are a pure function of items + assessments + profile (the authority is
// order-independent); each is re-derived on the writes that change its inputs
// (PutAssessment, a cardinality change's RederivePredicate), so the final current rows
// equal the serial run's. Profiles receive the same votes in the same order.
//
// Not guaranteed (and not guaranteed between two serial runs either): a document with no
// session time (its facts' asserted_at is the wall clock); a group whose facts land in
// another group's classification (chunk tags differing from the document's), or
// concurrent writers of the same predicates (other groups with Concurrency > 1, a live
// ingest) — their votes interleave differently; an embedder that is not deterministic,
// for windows reloaded from the journal (their vectors are re-embedded when the
// projection has none).

// stagedWindow is one extracted, not yet committed window.
type stagedWindow struct {
	start, end int
	facts      []domain.Fact // as stored: the window's assessment batch
	fresh      []bool        // parallel to facts: inserted by this window's write
}

// stagedDoc is one document after phase 1.
type stagedDoc struct {
	c          domain.FactBackfillCandidate
	hash       string
	version    string
	passages   int
	priorItems int // items the marker already counted when the run started
	empty      bool
	windows    []stagedWindow
	// replayedWindows were reloaded from the journal (no extraction call).
	replayedWindows int
	res             FactDocumentResult
	err             error // extraction stopped by this error after len(windows) windows
}

// stagedSink is what the two-phase backfill needs of the extractor. *FactExtractor
// implements it; a sink that does not falls back to the serial ordered path.
type stagedSink interface {
	stagedReady() error
	stageDocument(ctx context.Context, c domain.FactBackfillCandidate, chunks []*domain.Document) *stagedDoc
	assessing() bool
	planAssessment(ctx context.Context, facts []domain.Fact, vis *factVisibility) *assessPlan
	assessmentStored(ctx context.Context, id string, countReplay bool) bool
	askJudge(ctx context.Context, p *assessPlan) (domain.GenerateResponse, error)
	commitAssessment(ctx context.Context, p *assessPlan, resp domain.GenerateResponse, callErr error)
	markStagedWindow(ctx context.Context, d *stagedDoc, upto int)
	finishStagedDoc(ctx context.Context, d *stagedDoc)
}

var _ stagedSink = (*FactExtractor)(nil)

// stagedReady reports whether the read side supports the staged reads (journal reload,
// hidden similar-fact reads); without them the backfill runs the serial ordered path.
func (e *FactExtractor) stagedReady() error {
	if e == nil {
		return ErrFactBackfillUnavailable
	}
	sr, ok := e.reader.(domain.FactStagedReader)
	if !ok {
		return errors.New("fact backfill: the fact reader has no staged reads (domain.FactStagedReader)")
	}
	if h, ok := sr.(interface{ HasStagedReads() bool }); ok && !h.HasStagedReads() {
		return errors.New("fact backfill: the enforcing fact reader's inner reader has no staged reads")
	}
	return nil
}

// stageDocument is phase 1 for one document: process() without the assessment pass and
// without the marker, journaling every window. It resumes exactly as process() does
// (from the marker's coverage) and, beyond it, from the journal of an earlier run that
// stopped before committing (a journaled window costs no extraction call and counts as
// one of this run's max_calls_per_document windows, so a stopped-and-resumed run covers
// what an uninterrupted one would).
func (e *FactExtractor) stageDocument(ctx context.Context, c domain.FactBackfillCandidate, chunks []*domain.Document) *stagedDoc {
	j := factJob{ctx: ctx, documentID: c.DocumentID, actor: c.Author, source: c.SourceMeta(), chunks: chunks}
	d := &stagedDoc{c: c, hash: factContentHash(chunks), version: e.ExtractorVersion()}
	marker, err := e.sink.ExtractionMarker(ctx, e.cfg.Namespace, domain.KindMemoryFact, c.DocumentID, d.hash)
	if err != nil {
		slog.WarnContext(ctx, "FactExtractor: extraction marker unreadable; extracting the whole source", "document_id", c.DocumentID, "err", err)
		marker = nil
	}
	start, items, done := e.resumeFrom(marker)
	if done {
		e.skipped.Add(1)
		d.res.Skipped = true
		return d
	}
	d.priorItems = items
	passages := factPassages(chunks)
	d.passages = len(passages)
	if len(passages) == 0 {
		d.empty = true
		return d
	}
	texts := make([]string, len(passages))
	speakers := make([]string, len(passages))
	for i, p := range passages {
		texts[i], speakers[i] = p.Text, p.Speaker
	}
	windows := packFactWindows(texts, speakers, j.source, e.canonical, e.cfg.MaxPromptChars)
	first := 0
	for first < len(windows) && windows[first].End <= start {
		first++
	}
	journal := map[int]domain.ExtractionWindow{}
	if rows, err := e.sink.ExtractionWindows(ctx, e.cfg.Namespace, domain.KindMemoryFact, c.DocumentID, d.hash, d.version); err != nil {
		slog.WarnContext(ctx, "FactExtractor: extraction journal unreadable; extracting the uncommitted windows again", "document_id", c.DocumentID, "err", err)
	} else {
		for _, r := range rows {
			journal[r.PassageStart] = r
		}
	}
	ran := 0
	for w := first; w < len(windows); w++ {
		if e.cfg.MaxCallsPerDocument > 0 && ran >= e.cfg.MaxCallsPerDocument {
			d.res.Partial = true
			slog.WarnContext(ctx, "FactExtractor: source longer than max_calls_per_document windows; partial coverage recorded for facts-backfill to resume",
				"document_id", c.DocumentID, "passages", len(passages), "windows", len(windows), "max_calls_per_document", e.cfg.MaxCallsPerDocument)
			break
		}
		budget := e.cfg.MaxFactsPerSource - items
		if budget <= 0 {
			d.res.Partial = true
			slog.WarnContext(ctx, "FactExtractor: max_facts_per_source reached; the rest of the source is not extracted",
				"document_id", c.DocumentID, "passages", len(passages), "max_facts_per_source", e.cfg.MaxFactsPerSource)
			break
		}
		if row, ok := journal[windows[w].Start]; ok && row.PassageEnd == windows[w].End {
			if sw, ok := e.reloadWindow(ctx, row); ok {
				d.windows = append(d.windows, sw)
				d.replayedWindows++
				items += len(sw.facts)
				ran++
				continue
			}
		}
		win := passages[windows[w].Start:windows[w].End]
		st, n, stored, fresh, err := e.extractWindow(j, win, w, len(windows), min(e.cfg.MaxFactsPerDocument, budget), &d.res)
		if err != nil {
			d.err = err
			break
		}
		ran++
		d.res.Facts += n
		items += st.Inserted + st.Existing
		d.res.Stats.Inserted += st.Inserted
		d.res.Stats.Existing += st.Existing
		d.res.Stats.Failed += st.Failed
		sw := stagedWindow{start: windows[w].Start, end: windows[w].End, facts: stored, fresh: fresh}
		d.windows = append(d.windows, sw)
		e.journalWindow(ctx, d, sw)
	}
	return d
}

// journalWindow records a stored, unassessed window (best-effort: without the row a
// stop before the commit re-extracts the window; ids are deterministic, so that is an
// idempotent write, but a second extraction call).
func (e *FactExtractor) journalWindow(ctx context.Context, d *stagedDoc, sw stagedWindow) {
	row := domain.ExtractionWindow{
		NamespaceID: e.cfg.Namespace, Extractor: domain.KindMemoryFact, DocumentID: d.c.DocumentID,
		ContentHash: d.hash, ExtractorVersion: d.version, PassageStart: sw.start, PassageEnd: sw.end,
		ItemIDs: make([]domain.KnowledgeItemID, 0, len(sw.facts)),
	}
	for i, f := range sw.facts {
		row.ItemIDs = append(row.ItemIDs, domain.KnowledgeItemID(f.ID))
		if sw.fresh[i] {
			row.NewItemIDs = append(row.NewItemIDs, domain.KnowledgeItemID(f.ID))
		}
	}
	if err := e.sink.PutExtractionWindow(ctx, row); err != nil {
		slog.WarnContext(ctx, "FactExtractor: extraction journal row not written; a stop before the commit extracts this window again",
			"document_id", d.c.DocumentID, "err", err)
	}
}

// reloadWindow rebuilds a journaled window from its stored items (through the enforcing
// reader, with the explicit kernel bypass). Every item must still exist, else the window
// is extracted again. Facts with no projection embedding are re-embedded.
func (e *FactExtractor) reloadWindow(ctx context.Context, row domain.ExtractionWindow) (stagedWindow, bool) {
	sr, ok := e.reader.(domain.FactStagedReader)
	if !ok {
		return stagedWindow{}, false
	}
	sys := domain.WithScope(ctx, domain.ScopeSystem)
	got, err := sr.FactsByID(sys, row.ItemIDs, domain.ScopeSystem)
	if err != nil {
		slog.WarnContext(ctx, "FactExtractor: journaled window unreadable; extracting it again", "document_id", row.DocumentID, "err", err)
		return stagedWindow{}, false
	}
	byID := make(map[string]domain.Fact, len(got))
	for _, f := range got {
		byID[f.ID] = f
	}
	fresh := map[domain.KnowledgeItemID]bool{}
	for _, id := range row.NewItemIDs {
		fresh[id] = true
	}
	sw := stagedWindow{start: row.PassageStart, end: row.PassageEnd}
	var noVec []int
	for _, id := range row.ItemIDs {
		f, ok := byID[string(id)]
		if !ok {
			slog.WarnContext(ctx, "FactExtractor: a journaled item is gone; extracting the window again", "document_id", row.DocumentID, "item", id)
			return stagedWindow{}, false
		}
		if len(f.Embedding) == 0 {
			noVec = append(noVec, len(sw.facts))
		}
		sw.facts = append(sw.facts, f)
		sw.fresh = append(sw.fresh, fresh[id])
	}
	if len(noVec) > 0 && e.cfg.FuzzyCandidates > 0 {
		re := make([]domain.Fact, len(noVec))
		for i, k := range noVec {
			re[i] = sw.facts[k]
		}
		if err := e.embedFacts(ctx, re); err != nil {
			slog.WarnContext(ctx, "FactExtractor: journaled window not re-embedded; extracting it again", "document_id", row.DocumentID, "err", err)
			return stagedWindow{}, false
		}
		for i, k := range noVec {
			sw.facts[k].Embedding = re[i].Embedding
		}
	}
	return sw, true
}

// assessmentStored reports whether the keyed assessment is already on record (the
// serial path's replay check). countReplay counts it as a replay.
func (e *FactExtractor) assessmentStored(ctx context.Context, id string, countReplay bool) bool {
	prior, err := e.sink.GetAssessment(ctx, id)
	if err != nil || prior == nil {
		return false
	}
	if countReplay {
		e.assessReplayed.Add(1)
	}
	return true
}

// markStagedWindow writes the marker as process() does after window upto: coverage to
// its end, the items counted so far.
func (e *FactExtractor) markStagedWindow(ctx context.Context, d *stagedDoc, upto int) {
	items := d.priorItems
	covered := 0
	for i := 0; i <= upto && i < len(d.windows); i++ {
		items += len(d.windows[i].facts)
		covered = d.windows[i].end
	}
	if d.empty {
		covered = 0
	}
	if err := e.sink.MarkExtracted(ctx, domain.ExtractionMarker{
		NamespaceID: e.cfg.Namespace, Extractor: domain.KindMemoryFact, DocumentID: d.c.DocumentID,
		ContentHash: d.hash, Items: items, ExtractorVersion: d.version,
		PassagesCovered: covered, PassagesTotal: d.passages,
	}); err != nil {
		slog.WarnContext(ctx, "FactExtractor: extraction marker not written; a re-run will extract these passages again",
			"document_id", d.c.DocumentID, "err", err)
	}
}

// finishStagedDoc closes a committed document: its journal rows are obsolete (the marker
// covers them), and the counters and log line are process()'s.
func (e *FactExtractor) finishStagedDoc(ctx context.Context, d *stagedDoc) {
	if d.err != nil {
		// Keep the journal: a window an earlier run journaled AFTER the one that failed
		// now is still reusable; the committed ones are skipped by the marker's coverage.
		return
	}
	if err := e.sink.DeleteExtractionWindows(ctx, e.cfg.Namespace, domain.KindMemoryFact, d.c.DocumentID, d.hash); err != nil {
		slog.WarnContext(ctx, "FactExtractor: extraction journal rows not deleted (harmless: the marker covers them)",
			"document_id", d.c.DocumentID, "err", err)
	}
	e.extracted.Add(1)
	e.stored.Add(uint64(d.res.Stats.Inserted))
	slog.InfoContext(ctx, "FactExtractor: document extracted (two-phase)", "document_id", d.c.DocumentID,
		"windows", len(d.windows), "journal_windows", d.replayedWindows, "passages", d.passages, "partial", d.res.Partial,
		"facts", d.res.Facts, "inserted", d.res.Stats.Inserted, "existing", d.res.Stats.Existing, "failed", d.res.Stats.Failed,
		"assessment_calls", d.res.AssessmentCalls, "assessment_replayed", d.res.AssessmentReplayed, "extractor_version", d.version)
}

// stagedPos addresses one window of a group in serial order.
type stagedPos struct{ doc, win int }

// stagedGroupRun is one group's phase 2/3 state.
type stagedGroupRun struct {
	ss    stagedSink
	docs  []*stagedDoc
	seq   []stagedPos
	first map[string]int // new item id → serial position of its first producing window
	// newBySubject lists the new items per subject reference with their first position.
	newBySubject map[string][]stagedItem

	mu        sync.Mutex
	answers   map[string]*judgeAnswer
	calls     int
	waves     int
	discarded int
}

type stagedItem struct {
	id  domain.KnowledgeItemID
	pos int
}

type judgeAnswer struct {
	resp domain.GenerateResponse
	err  error
	used bool
}

func subjectRefOf(f domain.Fact) string { return f.ScopeKey + "\x00" + f.SubjectKey }

// visibility is the serial run's view at window position p: every item new in this run
// whose first producing window comes after p is hidden.
func (g *stagedGroupRun) visibility(p int) *factVisibility {
	return &factVisibility{
		hidden: func(id string) bool {
			fp, ok := g.first[id]
			return ok && fp > p
		},
		bySubject: func(f domain.Fact) []domain.KnowledgeItemID {
			var out []domain.KnowledgeItemID
			for _, it := range g.newBySubject[subjectRefOf(f)] {
				if it.pos > p {
					out = append(out, it.id)
				}
			}
			return out
		},
	}
}

// stagedGroup runs one ordered classification group in the three phases. Every outcome
// is counted on the report; it owns every goroutine it starts and returns after they
// exit.
func (r *backfillRun) stagedGroup(ctx context.Context, ss stagedSink, g []domain.FactBackfillCandidate, n int) {
	n = max(1, min(n, len(g)))
	docs := make([]*stagedDoc, len(g))

	// Phase 1: extract concurrently.
	idx := make(chan int)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				docs[i] = r.stageOne(ctx, ss, g[i])
			}
		}()
	}
	fed := 0
feed:
	for i := range g {
		select {
		case idx <- i:
			fed++
		case <-ctx.Done():
			break feed
		}
	}
	close(idx)
	wg.Wait()
	r.cancelled(len(g) - fed)
	if ctx.Err() != nil {
		// Nothing is committed: every staged document resumes from its journal.
		for _, d := range docs {
			if d != nil {
				r.mu.Lock()
				r.rep.LLMCalls += d.res.LLMCalls
				r.rep.Cancelled++
				r.mu.Unlock()
			}
		}
		return
	}

	gr := &stagedGroupRun{ss: ss, docs: docs, first: map[string]int{}, newBySubject: map[string][]stagedItem{}, answers: map[string]*judgeAnswer{}}
	newIDs := map[string]bool{}
	for di, d := range docs {
		if d == nil {
			continue
		}
		for wi, w := range d.windows {
			p := len(gr.seq)
			gr.seq = append(gr.seq, stagedPos{di, wi})
			for k, f := range w.facts {
				if w.fresh[k] {
					newIDs[f.ID] = true
				}
				if fp, ok := gr.first[f.ID]; !ok || p < fp {
					gr.first[f.ID] = p
				}
			}
		}
	}
	// Only items NEW in this run are ever hidden; an item that existed before the run
	// (another version's identical fact, an earlier partial run) was visible to all.
	for id := range gr.first {
		if !newIDs[id] {
			delete(gr.first, id)
		}
	}
	seenNew := map[string]bool{}
	for _, pos := range gr.seq {
		for _, f := range docs[pos.doc].windows[pos.win].facts {
			if fp, ok := gr.first[f.ID]; ok && !seenNew[f.ID] {
				seenNew[f.ID] = true
				gr.newBySubject[subjectRefOf(f)] = append(gr.newBySubject[subjectRefOf(f)], stagedItem{domain.KnowledgeItemID(f.ID), fp})
			}
		}
	}

	// Phase 2: speculate every window's judge call.
	if ss.assessing() {
		gr.speculate(ctx, 0, n)
	}

	// Phase 3: commit in serial order.
	p := 0
	stopped := false
	for _, d := range docs {
		if d == nil {
			continue // counted in stageOne
		}
		if stopped || ctx.Err() != nil {
			stopped = true
			r.mu.Lock()
			r.rep.LLMCalls += d.res.LLMCalls
			r.rep.Cancelled++
			r.mu.Unlock()
			continue
		}
		if d.empty {
			ss.markStagedWindow(ctx, d, -1)
		}
		for k := range d.windows {
			if ctx.Err() != nil {
				stopped = true
				break
			}
			gr.commitWindow(ctx, d, k, p, n)
			ss.markStagedWindow(ctx, d, k)
			p++
		}
		if stopped {
			// Committed up to the last marked window; the rest resumes from the journal.
			r.mu.Lock()
			r.rep.LLMCalls += d.res.LLMCalls
			r.rep.Cancelled++
			r.mu.Unlock()
			continue
		}
		ss.finishStagedDoc(ctx, d)
		if d.err != nil {
			r.log.WarnContext(ctx, "facts-backfill: extraction failed; left unmarked for the next run",
				"document_id", d.c.DocumentID, "err_kind", FactErrorKind(d.err), "err", d.err)
		}
		dd := d
		r.finish(ctx, func(rep *FactBackfillReport) {
			rep.LLMCalls += dd.res.LLMCalls
			rep.countParse(dd.res, dd.err)
			rep.JournalWindowsReplayed += dd.replayedWindows
			if dd.res.AssessmentReplayed {
				rep.AssessmentsReplayed++
			}
			switch {
			case dd.err != nil:
				rep.Failed++
			default:
				rep.Extracted++
				rep.FactsInserted += dd.res.Stats.Inserted
				if dd.res.Partial {
					rep.Partial++
				}
			}
		})
	}
	gr.mu.Lock()
	for _, a := range gr.answers {
		if !a.used {
			gr.discarded++
		}
	}
	r.mu.Lock()
	r.rep.LLMCalls += gr.calls
	r.rep.AssessmentCalls += gr.calls
	r.rep.AssessmentCallsDiscarded += gr.discarded
	r.rep.SpeculationWaves += gr.waves
	r.mu.Unlock()
	gr.mu.Unlock()
}

// stageOne is phase 1 for one document, with one()'s checks. nil = counted and done
// (skipped, missing, cancelled); otherwise the staged document (possibly with an error
// after some windows, or skipped by its marker).
func (r *backfillRun) stageOne(ctx context.Context, ss stagedSink, c domain.FactBackfillCandidate) *stagedDoc {
	if done, err := r.b.extractor.AlreadyExtracted(ctx, c.DocumentID, c.ChunkIDs); err == nil && done {
		r.finish(ctx, func(rep *FactBackfillReport) { rep.Skipped++ })
		return nil
	}
	if err := r.limiter.Wait(ctx); err != nil {
		r.cancelled(1)
		return nil
	}
	chunks, err := r.b.readChunks(ctx, c.ChunkIDs)
	if err != nil || len(chunks) == 0 {
		if ctx.Err() != nil {
			r.cancelled(1)
			return nil
		}
		r.log.WarnContext(ctx, "facts-backfill: document chunks unreadable; left for the next run",
			"document_id", c.DocumentID, "chunks", len(c.ChunkIDs), "err", err)
		r.finish(ctx, func(rep *FactBackfillReport) { rep.Missing++ })
		return nil
	}
	if len(chunks) < len(c.ChunkIDs) {
		r.log.WarnContext(ctx, "facts-backfill: some chunks of the document were not readable; extracting from the rest",
			"document_id", c.DocumentID, "listed", len(c.ChunkIDs), "read", len(chunks))
	}
	d := ss.stageDocument(ctx, c, chunks)
	if d.res.Skipped {
		r.finish(ctx, func(rep *FactBackfillReport) { rep.Skipped++ })
		return nil
	}
	return d
}

// speculate is phase 2 from serial position from: every window's plan under the live
// state with the not-yet-written items hidden, and its judge call unless the answer is
// already stored or held. n bounds the calls in flight.
func (g *stagedGroupRun) speculate(ctx context.Context, from, n int) {
	g.mu.Lock()
	g.waves++
	g.mu.Unlock()
	idx := make(chan int)
	var wg sync.WaitGroup
	for range max(1, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range idx {
				pos := g.seq[p]
				facts := g.docs[pos.doc].windows[pos.win].facts
				if len(facts) == 0 {
					continue
				}
				plan := g.ss.planAssessment(ctx, facts, g.visibility(p))
				if plan == nil || ctx.Err() != nil {
					continue
				}
				g.mu.Lock()
				_, held := g.answers[plan.id]
				if !held {
					g.answers[plan.id] = nil // claimed: no second call for the same id
				}
				g.mu.Unlock()
				if held {
					continue
				}
				if g.ss.assessmentStored(ctx, plan.id, false) {
					g.mu.Lock()
					delete(g.answers, plan.id)
					g.mu.Unlock()
					continue
				}
				resp, err := g.ss.askJudge(ctx, plan)
				g.mu.Lock()
				g.calls++
				if ctx.Err() != nil {
					delete(g.answers, plan.id) // a cancelled call is no answer
				} else {
					g.answers[plan.id] = &judgeAnswer{resp: resp, err: err}
				}
				g.mu.Unlock()
			}
		}()
	}
feed:
	for p := from; p < len(g.seq); p++ {
		select {
		case idx <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(idx)
	wg.Wait()
}

// commitWindow is phase 3 for the window at serial position p: plan it against the live
// store (exactly the serial run's state here), then replay, use the held answer, or
// speculate again from p and use that.
func (g *stagedGroupRun) commitWindow(ctx context.Context, d *stagedDoc, k, p, n int) {
	facts := d.windows[k].facts
	if !g.ss.assessing() || len(facts) == 0 {
		return
	}
	plan := g.ss.planAssessment(ctx, facts, g.visibility(p))
	if plan == nil {
		return
	}
	if g.ss.assessmentStored(ctx, plan.id, true) {
		d.res.AssessmentReplayed = true
		return
	}
	ans := g.answer(plan.id)
	if ans == nil {
		// An earlier commit changed this window's candidate set (a learned cardinality
		// flipped, say): speculate the rest again under the live state.
		g.speculate(ctx, p, n)
		if ctx.Err() != nil {
			return
		}
		if plan = g.ss.planAssessment(ctx, facts, g.visibility(p)); plan == nil {
			return
		}
		if g.ss.assessmentStored(ctx, plan.id, true) {
			d.res.AssessmentReplayed = true
			return
		}
		if ans = g.answer(plan.id); ans == nil {
			// The plan moved again between the wave and now (another writer of the same
			// predicate): ask for exactly this plan.
			resp, err := g.ss.askJudge(ctx, plan)
			ans = &judgeAnswer{resp: resp, err: err}
			g.mu.Lock()
			g.calls++
			g.answers[plan.id] = ans
			g.mu.Unlock()
		}
	}
	g.mu.Lock()
	ans.used = true
	g.mu.Unlock()
	d.res.AssessmentCalls++
	g.ss.commitAssessment(ctx, plan, ans.resp, ans.err)
}

func (g *stagedGroupRun) answer(id string) *judgeAnswer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.answers[id]
}
