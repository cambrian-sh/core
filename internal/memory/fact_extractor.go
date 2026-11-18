package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// FactExtractor is ADR-0136 D4's asynchronous, per-source fact extraction, writing to
// the KNOWLEDGE SUBSTRATE (Amendment A1): one LLM call per WINDOW of an ingested source
// (A2.3 — a source that fits max_prompt_chars is one call; a longer one is covered in
// order, window by window, up to max_calls_per_document), after its chunks are stored,
// off the ingest RPC's critical path (the scene-gen-on-ingest incident — a synchronous
// 10 s/item LLM stall — is why it is never inline; failure archaeology §4 #10). What
// differs per source reaches the prompt as data (domain.SourceMeta, A2.1).
//
// Each extracted fact becomes one append-only `memory_fact` knowledge item, written
// through the classification chokepoint (authz.EnforcingKnowledgeWriter), with its
// embedding in the item projection. The substrate derives each key's chain on write
// (the valid_time_supersession authority), so there is no supersession code here: the
// extractor supplies INPUTS — the supersedes_prior cue on each item, and with
// assessments on, at most ONE batched judge call per window whose verdicts are stored as
// a keyed Assessment BEFORE they have any effect (A1.3). A replay of the same candidate
// set reads the stored verdict and makes no call.
//
// Queue discipline is the ChunkTripletsBatcher's (chunk_triplets_batcher.go): Enqueue
// never blocks; a full queue drops the enrichment with a WARN and a counter (the chunks
// are already stored); a tracked enqueue returns a FactsWait a sync ingest may wait on
// (execution.ingestion.sync_ingest_wait_for_facts); Start launches a fixed pool of
// workers, Stop drains them and releases whatever is still queued.
//
// Each job carries its ingest context through context.WithoutCancel (AGENTS.md §2.7):
// the principal and trace survive (the write chokepoint classifies the items as that
// principal) while the client's cancellation does not cancel the extraction.
type FactExtractor struct {
	gen    domain.Generator
	embed  domain.Embedder
	sink   domain.FactSubstrateWriter
	reader domain.FactItemReader
	cfg    FactExtractorConfig
	// declared is execution.facts.single_attributes / multi_attributes, keyed by
	// domain.NormalizeFactAttribute → cardinality. Seeded as declared profiles at Start.
	declared  map[string]string
	canonical []string // the declared predicate names, offered to the prompt

	queue chan factJob

	closeMu sync.RWMutex
	closed  bool

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	started  atomic.Bool

	enqueued        atomic.Uint64
	sourceFiltered  atomic.Uint64 // ingests the fact_extraction_sources guard did not extract
	dropped         atomic.Uint64
	skipped         atomic.Uint64 // content already extracted (idempotent re-ingest)
	extracted       atomic.Uint64 // documents run through the model
	failedDocs      atomic.Uint64
	llmCalls        atomic.Uint64 // extraction + assessment calls
	assessmentCalls atomic.Uint64
	assessReplayed  atomic.Uint64 // candidate sets whose stored verdict was reused (no call)
	stored          atomic.Uint64 // items inserted
	learned         atomic.Uint64 // profile cardinality changes from votes
	parseFailures   atomic.Uint64 // extraction answers that could not be read
	parseRetries    atomic.Uint64 // re-asks after an unreadable answer (fact_llm_call.go)
	parseRepaired   atomic.Uint64 // windows whose re-ask was read
	timeouts        atomic.Uint64 // extraction calls that ran out of time
	// ENG-431: what was only in the log — partially covered documents and the facts
	// validation refused, by reason — so the operator Snapshot can show it.
	partialDocs    atomic.Uint64
	refusedQuote   atomic.Uint64
	refusedShape   atomic.Uint64
	refusedDup     atomic.Uint64
	refusedOverCap atomic.Uint64
	// staleVersion counts documents whose run stopped because the document was revised
	// after the job was queued (domain.ErrSourceVersionRetired, ENG-501).
	staleVersion atomic.Uint64
}

// FactExtractorConfig is execution.facts, mapped by kernel.factExtractorConfigFrom.
type FactExtractorConfig struct {
	QueueSize int
	Workers   int
	// LLMTimeout is the base bound of one call; LLMTimeoutPerKChar is added per 1000
	// prompt characters, and the sum is capped at LLMTimeoutMax (callTimeout). A cap
	// below LLMTimeout is raised to it, so a zero cap means a flat LLMTimeout.
	LLMTimeout         time.Duration
	LLMTimeoutPerKChar time.Duration
	LLMTimeoutMax      time.Duration
	// ParseRetries is the re-asks of the same prompt after an unreadable extraction
	// answer (0 = none; negative reads as 0).
	ParseRetries int
	// MaxFactsPerDocument caps the facts kept from ONE window (A2.3: per call).
	MaxFactsPerDocument int
	MaxPromptChars      int
	// MaxCallsPerDocument caps the extraction windows of one source per run (A2.3);
	// the rest is recorded as a PARTIAL marker that facts-backfill resumes. 0 = no cap.
	MaxCallsPerDocument int
	// MaxFactsPerSource bounds the facts kept from one source across all its windows
	// and runs (A2.3). <= 0 means 512 (64 per window x 8 windows).
	MaxFactsPerSource int
	SingleAttributes  []string
	MultiAttributes   []string
	// Supersession (D5): cues set a session-time lower bound, declared profiles are
	// seeded, and the lane reads chains. Without Assessments this is F2a — no extra call.
	Supersession bool
	// Assessments (F2b): at most one batched judge call per session over
	// unknown-cardinality exact-key pairs and FuzzyCandidates same-subject candidates.
	Assessments bool
	// FuzzyCandidates is k of A1.2 rule 3; 0 = exact-key pairs only.
	FuzzyCandidates int
	// CardinalityMinVotes is the learning threshold (A1.4). <= 0 means 3.
	CardinalityMinVotes int
	// Namespace of the items (substrate namespace_id). "" = "default".
	Namespace string
	// EmbedModelID identifies the embedder in the item projection (knowledge_item_embeddings.model_id).
	EmbedModelID string
	// AssessorModel identifies the judge model (the facts role's generator) in the
	// assessment authority_version: a new model is a new judge, never a replay.
	AssessorModel string
	// GeneratorModel is the facts generator's model, the second half of the extractor
	// version on every marker (A2.4: "<prompt id>/<generator model>"). "" = "default".
	GeneratorModel string
	// ExtractionSources is execution.memory.fact_extraction_sources: the writer kinds
	// ("agent", "user", "system", "source", …) or connectors ("source:<connector>")
	// whose ingests Enqueue accepts. Empty = every ingest (ENG-494 cost guard).
	ExtractionSources []string
}

// FactExtractorStats is a snapshot of the counters.
type FactExtractorStats struct {
	Enqueued, Dropped, Skipped, Extracted, FailedDocs, LLMCalls, AssessmentCalls, AssessmentsReplayed, Stored, ProfilesLearned uint64
	// ParseFailures counts unreadable extraction answers, ParseRetries the re-asks they
	// caused, ParseRepaired the windows a re-ask recovered, Timeouts the calls that ran
	// out of time.
	ParseFailures, ParseRetries, ParseRepaired, Timeouts uint64
	// PartialDocs counts documents cut at max_calls_per_document (facts-backfill
	// resumes them); Refused* count facts validation refused, by reason (ENG-431).
	PartialDocs, RefusedQuoteNotFound, RefusedBadShape, RefusedDuplicate, RefusedOverCap uint64
	// SourceFiltered counts ingests execution.memory.fact_extraction_sources kept out
	// of extraction (ENG-494).
	SourceFiltered uint64
	// StaleVersion counts documents whose extraction stopped because the document was
	// revised after the job was queued: its writes were refused by the version fence
	// (domain.ErrSourceVersionRetired, ENG-501).
	StaleVersion uint64
}

type factJob struct {
	ctx        context.Context
	documentID string
	evidenceID domain.EvidenceID
	actor      string // "" = derive from the chunks / ctx principal
	// source is the A2.1 metadata: its ReferenceTime is the "now" of the text (and the
	// facts' asserted_at); its basis decides whether that time dates "now" facts.
	source domain.SourceMeta
	chunks []*domain.Document
	wait   *FactsWait
}

// factAssessmentPromptVersion is part of every assessment's authority_version, so a
// changed prompt is a new judge. It is derived from the library prompts' static hashes
// (prompts/fact_assess + prompts/fact_assess_input, ADR-0139 C4); the inline prompt it
// replaced was "assess-v1".
var factAssessmentPromptVersion = factAssessPrompts.version

// NewFactExtractor builds the extractor without starting it. sink must be the ENFORCING
// writer (authz.EnforcingKnowledgeWriter) and reader the ENFORCING fact reader
// (authz.EnforcingFactItemReader); kernel.NewMemoryStack is the only production caller.
func NewFactExtractor(gen domain.Generator, embed domain.Embedder, sink domain.FactSubstrateWriter, reader domain.FactItemReader, cfg FactExtractorConfig) *FactExtractor {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.LLMTimeout <= 0 {
		cfg.LLMTimeout = 2 * time.Minute
	}
	if cfg.LLMTimeoutPerKChar < 0 {
		cfg.LLMTimeoutPerKChar = 0
	}
	if cfg.LLMTimeoutMax < cfg.LLMTimeout {
		cfg.LLMTimeoutMax = cfg.LLMTimeout
	}
	if cfg.ParseRetries < 0 {
		cfg.ParseRetries = 0
	}
	if cfg.MaxFactsPerDocument <= 0 {
		cfg.MaxFactsPerDocument = 64
	}
	if cfg.MaxPromptChars <= 0 {
		cfg.MaxPromptChars = 48000
	}
	if cfg.MaxCallsPerDocument < 0 {
		cfg.MaxCallsPerDocument = 0
	}
	if cfg.MaxFactsPerSource <= 0 {
		cfg.MaxFactsPerSource = 512
	}
	if cfg.GeneratorModel == "" {
		cfg.GeneratorModel = "default"
	}
	if cfg.FuzzyCandidates < 0 {
		cfg.FuzzyCandidates = 0
	}
	if cfg.CardinalityMinVotes <= 0 {
		cfg.CardinalityMinVotes = 3
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	declared := map[string]string{}
	var canonical []string
	add := func(list []string, card string) {
		for _, a := range list {
			if k := domain.NormalizeFactAttribute(a); k != "" {
				if _, dup := declared[k]; !dup {
					canonical = append(canonical, k)
				}
				declared[k] = card // multi_attributes win a name listed in both
			}
		}
	}
	add(cfg.SingleAttributes, domain.CardinalitySingle)
	add(cfg.MultiAttributes, domain.CardinalityMulti)
	return &FactExtractor{
		gen: gen, embed: embed, sink: sink, reader: reader, cfg: cfg,
		declared: declared, canonical: canonical,
		queue:  make(chan factJob, cfg.QueueSize),
		stopCh: make(chan struct{}),
	}
}

// Enqueue queues one stored document's chunks for extraction, with the source's A2.1
// metadata. Non-blocking and nil-safe. track=true returns a FactsWait that completes
// when the document's facts are stored (or extraction definitively failed); track=false
// returns nil. The ADR-0105 evidence id rides ctx (withIngestEvidence) and links every
// item to its evidence.
func (e *FactExtractor) Enqueue(ctx context.Context, documentID string, src domain.SourceMeta, chunks []*domain.Document, track bool) *FactsWait {
	var w *FactsWait
	if track {
		w = newFactsWait()
	}
	if e == nil || len(chunks) == 0 {
		w.settle(true)
		return w
	}
	if writer := domain.PrincipalFromContext(ctx); !factSourceEligible(e.cfg.ExtractionSources, writer) {
		// The operator's cost guard: this source is not extracted. Not a failure —
		// the chunks are stored and retrievable; a sync wait is settled at once.
		e.sourceFiltered.Add(1)
		slog.DebugContext(ctx, "FactExtractor: source not in execution.memory.fact_extraction_sources; not extracted",
			"document_id", documentID, "writer", writer.String())
		w.settle(true)
		return w
	}
	job := factJob{
		ctx:        context.WithoutCancel(ctx),
		documentID: documentID,
		evidenceID: ingestEvidenceFrom(ctx),
		source:     src,
		chunks:     slimFactChunks(chunks),
		wait:       w,
	}
	e.closeMu.RLock()
	defer e.closeMu.RUnlock()
	if e.closed {
		e.dropped.Add(1)
		w.stop()
		return w
	}
	select {
	case e.queue <- job:
		e.enqueued.Add(1)
	default:
		e.dropped.Add(1)
		w.settle(false)
		slog.WarnContext(ctx, "FactExtractor: queue full, dropping fact extraction",
			"document_id", documentID, "queue_size", cap(e.queue),
			"effect", "the chunks are stored; this document has no facts until it is re-ingested")
	}
	return w
}

// factSourceEligible is the execution.memory.fact_extraction_sources match: an
// empty list admits every writer; otherwise an entry must name the writer's kind
// ("agent", "user", "system", "source", …) or, for a source service principal, its
// connector ("source:<connector>"). A write with no principal matches no entry.
func factSourceEligible(sources []string, writer domain.PrincipalRef) bool {
	if len(sources) == 0 {
		return true
	}
	if writer.IsZero() {
		return false
	}
	connector := writer.SourceConnector()
	for _, s := range sources {
		if s == string(writer.Kind) || (connector != "" && s == string(domain.PrincipalSourceKind)+":"+connector) {
			return true
		}
	}
	return false
}

// slimFactChunks copies the chunks a fact job keeps WITHOUT their embedding vectors:
// extraction reads id, text, metadata and event time only (facts are embedded on
// their own), and a full queue of stored *Documents held each chunk's 4 KB vector
// (~100-400 MB at 1024 queued jobs; T-20261103-factextractor-queue-jobs-retain-full-chunk).
// A shallow copy per chunk, so the caller's documents are never mutated.
func slimFactChunks(chunks []*domain.Document) []*domain.Document {
	out := make([]*domain.Document, 0, len(chunks))
	for _, c := range chunks {
		if c == nil {
			continue
		}
		d := *c
		d.Embedding = domain.Embedding{}
		out = append(out, &d)
	}
	return out
}

// Start seeds the declared predicate profiles (supersession on), then launches the
// worker pool. Idempotent. The seed runs BEFORE any worker, so no document derives
// under a profile the config has already changed.
func (e *FactExtractor) Start(ctx context.Context) {
	if e == nil || !e.started.CompareAndSwap(false, true) {
		return
	}
	if e.cfg.Supersession {
		e.seedDeclared(ctx)
	}
	// Workers run each job on the job's own (WithoutCancel) ingest context; ctx only
	// bounds the pool's lifetime, alongside Stop.
	for range e.cfg.Workers {
		e.wg.Add(1)
		go e.worker(ctx)
	}
}

// seedDeclaredLimit bounds the keys one changed profile re-derives at boot.
const seedDeclaredLimit = 10000

func (e *FactExtractor) seedDeclared(ctx context.Context) {
	changed, err := e.sink.SeedDeclaredProfiles(ctx, e.cfg.Namespace, domain.KindMemoryFact, e.declared, e.cfg.CardinalityMinVotes, "config:execution.facts")
	if err != nil {
		slog.WarnContext(ctx, "FactExtractor: declared predicate profiles not seeded; facts derive under their stored profiles",
			"err", err, "declared", len(e.declared))
		return
	}
	for _, p := range changed {
		n, err := e.sink.RederivePredicate(ctx, e.cfg.Namespace, domain.KindMemoryFact, p, seedDeclaredLimit)
		slog.InfoContext(ctx, "FactExtractor: predicate profile changed by config; re-derived its keys",
			"predicate", p, "cardinality", e.declared[p], "keys", n, "err", err)
	}
}

// Stop signals the workers, waits for the jobs in flight, and releases every queued job
// (its waiter gets ErrFactExtractorStopped). Safe to call without Start.
func (e *FactExtractor) Stop() {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() { close(e.stopCh) })
	e.wg.Wait()
	e.closeMu.Lock()
	e.closed = true
	e.closeMu.Unlock()
	for {
		select {
		case j := <-e.queue:
			j.wait.stop()
		default:
			return
		}
	}
}

// Stats returns the counters.
func (e *FactExtractor) Stats() FactExtractorStats {
	return FactExtractorStats{
		Enqueued: e.enqueued.Load(), Dropped: e.dropped.Load(), Skipped: e.skipped.Load(),
		Extracted: e.extracted.Load(), FailedDocs: e.failedDocs.Load(), LLMCalls: e.llmCalls.Load(),
		AssessmentCalls: e.assessmentCalls.Load(), AssessmentsReplayed: e.assessReplayed.Load(),
		Stored: e.stored.Load(), ProfilesLearned: e.learned.Load(),
		ParseFailures: e.parseFailures.Load(), ParseRetries: e.parseRetries.Load(),
		ParseRepaired: e.parseRepaired.Load(), Timeouts: e.timeouts.Load(),
		PartialDocs: e.partialDocs.Load(), RefusedQuoteNotFound: e.refusedQuote.Load(),
		RefusedBadShape: e.refusedShape.Load(), RefusedDuplicate: e.refusedDup.Load(), RefusedOverCap: e.refusedOverCap.Load(),
		SourceFiltered: e.sourceFiltered.Load(), StaleVersion: e.staleVersion.Load(),
	}
}

func (e *FactExtractor) worker(ctx context.Context) {
	defer e.wg.Done()
	for {
		// Stop wins over a non-empty queue: Stop releases what is left.
		select {
		case <-e.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-e.stopCh:
			return
		case <-ctx.Done():
			return
		case j := <-e.queue:
			_, err := e.process(j)
			if err != nil {
				e.failedDocs.Add(1)
				slog.WarnContext(j.ctx, "FactExtractor: fact extraction failed for a document",
					"document_id", j.documentID, "chunks", len(j.chunks), "cause", err,
					"effect", "the chunks are stored and retrievable; facts of the windows before the failure are stored, and the marker lets facts-backfill resume the rest")
			}
			j.wait.settle(err == nil)
		}
	}
}

// factContentHash digests the document's chunk ids (themselves content-derived), so the
// same content re-ingested is recognised and not re-extracted.
func factContentHash(chunks []*domain.Document) string {
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ID
	}
	return factContentHashOfIDs(ids)
}

// factContentHashOfIDs is factContentHash over the ids alone (the backfill knows a
// document's chunk ids before it reads their text).
func factContentHashOfIDs(ids []string) string {
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FactSaveStats reports what the item writes of one document did.
type FactSaveStats struct {
	Inserted int // new items
	Existing int // items already present (an idempotent re-ingest)
	Failed   int // items refused (denied by the chokepoint, refused by the kind declaration, store error)
	// SourceErased counts items refused because their source document was erased after
	// the ingest that queued this job (domain.ErrSourceErased, ADR-0137 D5). Final:
	// the window's remaining items are not attempted.
	SourceErased int
	// StaleVersion counts items refused because the document was revised after the job
	// was queued (domain.ErrSourceVersionRetired, ENG-501). Final: the run stops, writes
	// no marker and makes no further call — the new version's own job extracts it.
	StaleVersion int
}

// FactDocumentResult is what one document's extraction did (ExtractDocument).
type FactDocumentResult struct {
	// Skipped: the content was already extracted (marker present); no LLM call.
	Skipped bool
	// LLMCalls made for this document (0 when skipped; one per window, plus the
	// assessment calls).
	LLMCalls int
	// AssessmentCalls is the judge calls made (at most one per window);
	// AssessmentReplayed is true when a stored verdict for the same candidate set was
	// reused instead in some window.
	AssessmentCalls    int
	AssessmentReplayed bool
	// Windows is the extraction calls made; Partial is true when the run stopped before
	// the end of the source (max_calls_per_document or max_facts_per_source), recorded
	// on the marker for a later resume.
	Windows int
	Partial bool
	// ParseRetries is the re-asks made after an unreadable answer (counted in LLMCalls
	// too); ParseRepaired the windows such a re-ask recovered.
	ParseRetries  int
	ParseRepaired int
	// Facts is the number of facts built from the model's answer; Stats is what the
	// store did with them.
	Facts int
	Stats FactSaveStats
}

// ExtractDocument runs ONE document through the extractor synchronously, on the
// caller's ctx — the same process() the ingest workers run, so the prompt, windows,
// grounding, classification, assessments and marker are identical; only the scheduling
// differs. Its caller is the ADR-0136 backfill (memory.FactBackfill). actor names the
// writing principal recorded on the document ("" = the chunks' source_agent_id); src is
// its A2.1 metadata. Unlike Enqueue it does NOT detach ctx: cancelling it aborts the
// document after its last finished window, whose marker records the coverage, so a
// re-run resumes from the first uncovered window.
func (e *FactExtractor) ExtractDocument(ctx context.Context, documentID, actor string, src domain.SourceMeta, chunks []*domain.Document) (FactDocumentResult, error) {
	if e == nil || len(chunks) == 0 {
		return FactDocumentResult{Skipped: true}, nil
	}
	return e.process(factJob{ctx: ctx, documentID: documentID, actor: actor, source: src, chunks: chunks})
}

// ExtractorVersion is the version every marker this extractor writes carries (A2.4):
// "<prompt id>/<generator model>". A marker of another version is pending.
func (e *FactExtractor) ExtractorVersion() string {
	if e == nil {
		return ""
	}
	return factExtractPromptVersion + "/" + e.cfg.GeneratorModel
}

// resumeFrom reads a document's marker against this extractor (A2.4): done when it is a
// complete run of this version, or a partial one whose fact budget is spent (nothing
// more could be kept); otherwise the passage to resume from (0 for no marker, another
// version or a legacy row) and the items the marker already counts.
func (e *FactExtractor) resumeFrom(m *domain.ExtractionMarker) (start, priorItems int, done bool) {
	if m == nil || m.ExtractorVersion == "" || m.ExtractorVersion != e.ExtractorVersion() {
		return 0, 0, false
	}
	if m.Complete(m.ExtractorVersion) || m.Items >= e.cfg.MaxFactsPerSource {
		return 0, 0, true
	}
	return m.PassagesCovered, m.Items, false
}

// AlreadyExtracted reports whether the document's content — its chunk ids in ingest
// order, the digest process() keys the marker on — needs no further extraction by THIS
// extractor version: false for no marker, another version, a legacy marker or a partial
// one with fact budget left (A2.4).
func (e *FactExtractor) AlreadyExtracted(ctx context.Context, documentID string, chunkIDs []string) (bool, error) {
	if e == nil {
		return false, nil
	}
	m, err := e.sink.ExtractionMarker(ctx, e.cfg.Namespace, domain.KindMemoryFact, documentID, factContentHashOfIDs(chunkIDs))
	if err != nil {
		return false, err
	}
	_, _, done := e.resumeFrom(m)
	return done, nil
}

// MaxLLMCallsPerDocument is the upper bound of model calls one document costs in one
// run under this configuration: max_calls_per_document windows, each with at most one
// assessment call when assessments are on. 0 = unbounded (no window cap).
func (e *FactExtractor) MaxLLMCallsPerDocument() int {
	if e == nil {
		return 1
	}
	return e.cfg.MaxCallsPerDocument * e.callsPerWindow()
}

// callsPerWindow is the upper bound of one window's calls: the extraction call, its
// parse re-asks, and the judge call when assessing.
func (e *FactExtractor) callsPerWindow() int {
	n := 1 + e.cfg.ParseRetries
	if e.cfg.Supersession && e.cfg.Assessments {
		n++
	}
	return n
}

// EstimateLLMCalls estimates one document's calls from its text length (A2.3: about
// ceil(chars / window capacity) windows, capped), times the calls of a window whose
// answer parses (the extraction call, plus the judge call when assessing).
func (e *FactExtractor) EstimateLLMCalls(textChars int) int {
	if e == nil {
		return 1
	}
	capacity := e.cfg.MaxPromptChars - factWindowOverhead(domain.SourceMeta{}, e.canonical)
	if capacity < 1000 {
		capacity = 1000
	}
	windows := max(1, (textChars+capacity-1)/capacity)
	if e.cfg.MaxCallsPerDocument > 0 {
		windows = min(windows, e.cfg.MaxCallsPerDocument)
	}
	// Parse re-asks are rare (7 unreadable answers in the 2026-11-04 strat-100 v3
	// backfill) and not
	// estimated; MaxLLMCallsPerDocument is the bound that counts them.
	return windows * (e.callsPerWindow() - e.cfg.ParseRetries)
}

// process runs one document (A2.3): its chunks are split into passages, packed in order
// into windows of at most max_prompt_chars, and each window is one extraction call, one
// batch embed, one item write per fact and — assessments on — at most one judge call,
// in window order, so a later window's change language closes an earlier window's value
// by dates exactly as A1.2 does across sources. After every window the marker records
// the coverage, so a stop at max_calls_per_document, a fact budget, a failure or a
// cancellation leaves a PARTIAL marker the next run resumes from.
func (e *FactExtractor) process(j factJob) (FactDocumentResult, error) {
	var res FactDocumentResult
	ctx := j.ctx
	hash := factContentHash(j.chunks)
	version := e.ExtractorVersion()
	marker, err := e.sink.ExtractionMarker(ctx, e.cfg.Namespace, domain.KindMemoryFact, j.documentID, hash)
	if err != nil {
		// Unreadable marker: extract from the start; item ids are deterministic, so a
		// repeated fact is an idempotent write, not a duplicate.
		slog.WarnContext(ctx, "FactExtractor: extraction marker unreadable; extracting the whole source", "document_id", j.documentID, "err", err)
		marker = nil
	}
	start, items, done := e.resumeFrom(marker)
	if done {
		e.skipped.Add(1)
		res.Skipped = true
		return res, nil
	}
	passages := factPassages(j.chunks)
	texts := make([]string, len(passages))
	speakers := make([]string, len(passages))
	for i, p := range passages {
		texts[i], speakers[i] = p.Text, p.Speaker
	}
	windows := packFactWindows(texts, speakers, j.source, e.canonical, e.cfg.MaxPromptChars)
	// Resume at the window holding the first uncovered passage (a changed
	// max_prompt_chars can make that window start a little before it; the repeated
	// passages re-extract idempotently).
	first := 0
	for first < len(windows) && windows[first].End <= start {
		first++
	}
	covered := min(start, len(passages))
	// stale ends the run of a document revised after this job was queued (ENG-501): the
	// version fence refused a write, so nothing more of the retired version is written
	// or extracted, and no marker is left for the new version's job to misread.
	stale := func() (FactDocumentResult, error) {
		e.staleVersion.Add(1)
		slog.InfoContext(ctx, "FactExtractor: document revised after this job was queued; extraction of the retired version stopped",
			"document_id", j.documentID, "windows", res.Windows, "dropped", res.Stats.StaleVersion,
			"stale_version_docs", e.staleVersion.Load())
		return res, nil
	}
	// mark records the coverage; false when the version fence refused it.
	mark := func() bool {
		err := e.sink.MarkExtracted(ctx, domain.ExtractionMarker{
			NamespaceID: e.cfg.Namespace, Extractor: domain.KindMemoryFact, DocumentID: j.documentID,
			ContentHash: hash, Items: items, ExtractorVersion: version,
			PassagesCovered: covered, PassagesTotal: len(passages),
		})
		if errors.Is(err, domain.ErrSourceVersionRetired) {
			return false
		}
		if err != nil {
			slog.WarnContext(ctx, "FactExtractor: extraction marker not written; a re-run will extract these passages again",
				"document_id", j.documentID, "err", err)
		}
		return true
	}
	if len(passages) == 0 {
		if !mark() {
			return stale()
		}
		e.extracted.Add(1)
		return res, nil
	}
	facts := 0
	for w := first; w < len(windows); w++ {
		if e.cfg.MaxCallsPerDocument > 0 && res.Windows >= e.cfg.MaxCallsPerDocument {
			res.Partial = true
			slog.WarnContext(ctx, "FactExtractor: source longer than max_calls_per_document windows; partial coverage recorded for facts-backfill to resume",
				"document_id", j.documentID, "passages", len(passages), "covered", covered,
				"windows", len(windows), "max_calls_per_document", e.cfg.MaxCallsPerDocument)
			break
		}
		budget := e.cfg.MaxFactsPerSource - items
		if budget <= 0 {
			res.Partial = true
			slog.WarnContext(ctx, "FactExtractor: max_facts_per_source reached; the rest of the source is not extracted",
				"document_id", j.documentID, "passages", len(passages), "covered", covered, "max_facts_per_source", e.cfg.MaxFactsPerSource)
			break
		}
		win := passages[windows[w].Start:windows[w].End]
		st, n, err := e.processWindow(j, win, w, len(windows), min(e.cfg.MaxFactsPerDocument, budget), &res)
		if err != nil {
			return res, err
		}
		facts += n
		items += st.Inserted + st.Existing
		res.Stats.Inserted += st.Inserted
		res.Stats.Existing += st.Existing
		res.Stats.Failed += st.Failed
		if st.StaleVersion > 0 {
			res.Stats.StaleVersion += st.StaleVersion
			return stale()
		}
		covered = windows[w].End
		if !mark() {
			return stale()
		}
	}
	res.Facts = facts
	e.extracted.Add(1)
	if res.Partial {
		e.partialDocs.Add(1)
	}
	e.stored.Add(uint64(res.Stats.Inserted))
	slog.InfoContext(ctx, "FactExtractor: document extracted", "document_id", j.documentID,
		"windows", res.Windows, "passages", len(passages), "covered", covered, "partial", res.Partial,
		"facts", facts, "inserted", res.Stats.Inserted, "existing", res.Stats.Existing, "failed", res.Stats.Failed,
		"assessment_calls", res.AssessmentCalls, "assessment_replayed", res.AssessmentReplayed,
		"evidence_id", string(j.evidenceID), "extractor_version", version)
	return res, nil
}

// processWindow is one window: one extraction call, validation against the passages
// actually sent, embed, write, and the window's assessment. It returns the write stats
// and the number of facts built.
func (e *FactExtractor) processWindow(j factJob, win []factPassage, w, of, maxFacts int, res *FactDocumentResult) (FactSaveStats, int, error) {
	st, n, storedFacts, _, err := e.extractWindow(j, win, w, of, maxFacts, res)
	if err != nil {
		return st, n, err
	}
	if e.assessing() && len(storedFacts) > 0 && st.StaleVersion == 0 {
		called, replayed := e.assess(j.ctx, storedFacts)
		if called {
			res.LLMCalls++
			res.AssessmentCalls++
		}
		res.AssessmentReplayed = res.AssessmentReplayed || replayed
	}
	return st, n, nil
}

// assessing reports whether windows are assessed (A1.2 rules 2-4 need both switches).
func (e *FactExtractor) assessing() bool { return e.cfg.Supersession && e.cfg.Assessments }

// extractWindow is a window's extraction half: one extraction call, validation against
// the passages actually sent, embed and write. It returns the write stats, the number of
// facts built, the facts AS STORED (the window's assessment batch) and, parallel to it,
// whether each was inserted by this write. The serial path assesses right after it
// (processWindow); the two-phase backfill defers the assessment (fact_backfill_staged.go).
func (e *FactExtractor) extractWindow(j factJob, win []factPassage, w, of, maxFacts int, res *FactDocumentResult) (FactSaveStats, int, []domain.Fact, []bool, error) {
	ctx := j.ctx
	texts := make([]string, len(win))
	speakers := make([]string, len(win))
	for i, p := range win {
		texts[i], speakers[i] = p.Text, p.Speaker
	}
	req := buildFactRequest(factExtractionInput{
		Source: j.source, Passages: texts, Speakers: speakers, CanonicalPredicates: e.canonical, Section: w + 1, Sections: of,
	})
	res.Windows++
	parsed, pst, err := e.extractCall(ctx, j.documentID, w, req, texts, maxFacts, res)
	if err != nil {
		return FactSaveStats{}, 0, nil, nil, err
	}
	e.refusedQuote.Add(uint64(pst.QuoteNotFound))
	e.refusedShape.Add(uint64(pst.BadShape))
	e.refusedDup.Add(uint64(pst.Duplicate))
	e.refusedOverCap.Add(uint64(pst.OverCap))
	if pst.Returned > pst.Kept {
		slog.InfoContext(ctx, "FactExtractor: facts refused by validation", "document_id", j.documentID, "window", w+1,
			"returned", pst.Returned, "kept", pst.Kept, "bad_shape", pst.BadShape,
			"quote_not_found", pst.QuoteNotFound, "duplicate", pst.Duplicate, "over_cap", pst.OverCap)
	}
	facts := e.buildFacts(j, win, parsed)
	if len(facts) > 0 {
		if err := e.embedFacts(ctx, facts); err != nil {
			return FactSaveStats{}, 0, nil, nil, err
		}
	}
	storedFacts, inserted, st := e.writeItems(ctx, facts)
	if st.Failed > 0 && st.Inserted+st.Existing == 0 && len(facts) > 0 {
		return st, len(facts), nil, nil, errors.New("fact extraction: no item of the window could be stored")
	}
	return st, len(facts), storedFacts, inserted, nil
}

// writeItems writes each fact as a memory_fact item through the chokepoint, then their
// embeddings. It returns the facts AS STORED (final classification and identity), the
// only form later reads may key on, and parallel to them whether each write inserted.
func (e *FactExtractor) writeItems(ctx context.Context, facts []domain.Fact) ([]domain.Fact, []bool, FactSaveStats) {
	var st FactSaveStats
	stored := make([]domain.Fact, 0, len(facts))
	fresh := make([]bool, 0, len(facts))
	var embs []domain.ItemEmbedding
	for _, f := range facts {
		it, inserted, err := e.sink.PutClassifiedItem(ctx, f.ToItem(e.cfg.Namespace))
		if errors.Is(err, domain.ErrSourceVersionRetired) {
			st.StaleVersion += len(facts) - len(stored) - st.Failed
			slog.InfoContext(ctx, "FactExtractor: document revised while extracting; facts of the retired version dropped",
				"source_chunk", f.SourceChunkID, "dropped", st.StaleVersion)
			break
		}
		if errors.Is(err, domain.ErrSourceErased) {
			st.SourceErased += len(facts) - len(stored) - st.Failed
			slog.InfoContext(ctx, "FactExtractor: source erased while extracting; remaining facts dropped",
				"source_chunk", f.SourceChunkID, "dropped", st.SourceErased)
			break
		}
		if err != nil {
			st.Failed++
			slog.WarnContext(ctx, "FactExtractor: fact item not stored", "source_chunk", f.SourceChunkID,
				"predicate", f.AttributeKey, "err", err)
			continue
		}
		if inserted {
			st.Inserted++
		} else {
			st.Existing++
		}
		sf := domain.FactFromItem(it)
		sf.Embedding = f.Embedding
		stored = append(stored, sf)
		fresh = append(fresh, inserted)
		if len(f.Embedding) > 0 && e.cfg.EmbedModelID != "" {
			embs = append(embs, domain.ItemEmbedding{ItemID: it.ID, ModelID: e.cfg.EmbedModelID, Vector: f.Embedding})
		}
	}
	if err := e.sink.PutItemEmbeddings(ctx, embs); err != nil {
		slog.WarnContext(ctx, "FactExtractor: item embeddings not stored; these facts are invisible to the fact lane until re-embedded",
			"items", len(embs), "err", err)
	}
	return stored, fresh, st
}

// factActor is the writing principal of a document (A1-Q2): the principal recorded on
// its stored chunks (source_agent_id — the ingest path's doc.Author), else the ingest
// context's principal. The backfill passes the recorded one explicitly, so a backfilled
// fact and a later ingested one of the same principal share a resolution key.
func factActor(j factJob) string {
	if j.actor != "" {
		return j.actor
	}
	for _, c := range j.chunks {
		if a, ok := c.Metadata["source_agent_id"].(string); ok && a != "" {
			return a
		}
	}
	return domain.PrincipalFromContext(j.ctx).ID
}

// buildFacts turns validated extractions into fact views positioned on their source
// chunks. The classification HINT is the source chunk's stored tags; the chokepoint's
// ClassifyWrite decides the final tags (and so the resolution key's scope component).
//
// window holds the window's passages; a parsed fact's Passage indexes it, and that
// passage's Chunk is the fact's source chunk and turn index.
func (e *FactExtractor) buildFacts(j factJob, window []factPassage, parsed []extractedFact) []domain.Fact {
	out := make([]domain.Fact, 0, len(parsed))
	seen := map[string]bool{}
	actor := factActor(j)
	asserted := j.source.ReferenceTime.UTC()
	if j.source.ReferenceTime.IsZero() {
		asserted = time.Now().UTC() // the substrate orders by asserted_at; zero is refused
	}
	for _, p := range parsed {
		chunkIdx := window[p.Passage].Chunk
		chunk := j.chunks[chunkIdx]
		f := domain.Fact{
			Actor: actor, EvidenceID: string(j.evidenceID),
			Subject: p.Subject, Attribute: p.Attribute, Value: p.Value,
			SubjectKey:   domain.NormalizeFactText(p.Subject),
			AttributeKey: domain.NormalizeFactAttribute(p.Attribute),
			ValueNorm:    domain.NormalizeFactText(p.Value),
			ValidFrom:    p.ValidFrom, ValidTo: p.ValidTo,
			Polarity: p.Polarity, TurnIndex: chunkIdx, Quote: p.Quote,
			Speaker: p.Speaker, SupersedesPrior: p.SupersedesPrior,
			AssertedAt:    asserted,
			SourceChunkID: chunk.ID, SourceDocumentID: j.documentID,
			Tags: docTags(chunk.Metadata),
		}
		f.ID = domain.FactItemID(f.EvidenceID, f.SourceChunkID, f.SubjectKey, f.AttributeKey, f.ValueNorm)
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		switch {
		case f.ValidFrom != nil:
			f.ValidFromSource = domain.ValidFromStated
		case f.SupersedesPrior && e.cfg.Supersession && !j.source.ReferenceTime.IsZero() && j.source.ReferenceBasis.DatesWriting():
			// A1.2 rule 1: with a change-language cue, a fact with no stated start takes
			// the reference time as its lower bound, and the value says so. Without a cue
			// an unknown start stays unknown (the authority orders it by assertion time).
			// A2.2 rule 5: only a time of WRITING (basis event/authored) can bound it; an
			// ingest clock says nothing about when the change happened.
			t := j.source.ReferenceTime.UTC()
			f.ValidFrom = &t
			f.ValidFromSource = domain.ValidFromSession
		default:
			f.ValidFromSource = domain.ValidFromUnknown
		}
		f.ScopeKey = domain.FactScopeKey(f.Tags)
		out = append(out, f)
	}
	return out
}

// factEmbedText is what a fact's embedding encodes: the statement itself, so a query
// naming the attribute and the value ("what car do I drive") lands on it.
func factEmbedText(f domain.Fact) string {
	return f.Subject + " " + strings.ReplaceAll(f.Attribute, "_", " ") + ": " + f.Value
}

func (e *FactExtractor) embedFacts(ctx context.Context, facts []domain.Fact) error {
	texts := make([]string, len(facts))
	for i := range facts {
		texts[i] = factEmbedText(facts[i])
	}
	if be, ok := e.embed.(domain.BatchEmbedder); ok {
		vecs, err := be.EmbedBatch(ctx, texts)
		if err == nil && len(vecs) == len(texts) {
			for i := range facts {
				facts[i].Embedding = vecs[i]
			}
			return nil
		}
	}
	for i := range facts {
		v, err := e.embed.Embed(ctx, texts[i])
		if err != nil {
			return errors.Join(errors.New("fact extraction: embed facts"), err)
		}
		facts[i].Embedding = v
	}
	return nil
}

// cardinalityOf is the predicate's current profile cardinality (unknown on error).
func (e *FactExtractor) cardinalityOf(ctx context.Context, predicate string, cache map[string]string) string {
	if c, ok := cache[predicate]; ok {
		return c
	}
	c := domain.CardinalityUnknown
	if p, err := e.sink.PredicateProfile(ctx, e.cfg.Namespace, domain.KindMemoryFact, predicate); err == nil && p.Cardinality != "" {
		c = p.Cardinality
	}
	cache[predicate] = c
	return c
}

// assess is A1.2 rules 2-4 over ONE window's new facts (A2.3): collect the candidate pairs — exact-key pairs of a
// predicate whose cardinality is still unknown, and the FuzzyCandidates nearest
// same-subject facts under other predicates — into ONE batch, key it, and either reuse
// the stored verdict (replay: no call) or ask the judge once and store the answer as an
// Assessment before anything uses it. Storing it re-derives every cited key in the same
// transaction; exact-key verdicts then vote on the predicate's profile.
// Best-effort: any failure leaves the items and their cue/profile-driven chains as they
// are. It returns whether the judge was called and whether a stored verdict was reused.
func (e *FactExtractor) assess(ctx context.Context, facts []domain.Fact) (called, replayed bool) {
	p := e.planAssessment(ctx, facts, nil)
	if p == nil {
		return false, false
	}
	if prior, err := e.sink.GetAssessment(ctx, p.id); err == nil && prior != nil {
		e.assessReplayed.Add(1)
		return false, true
	}
	resp, err := e.askJudge(ctx, p)
	e.commitAssessment(ctx, p, resp, err)
	return true, false
}

// assessPlan is one window's batched judge call, fully determined before any call: the
// candidate pairs, the request and the keyed assessment id. prompt is the request
// flattened (domain.FlattenRequest): every byte the judge is sent, which is what the
// assessment's prompt hash certifies.
type assessPlan struct {
	items   []contradictionItem
	req     domain.GenerateRequest
	prompt  string
	inputs  []domain.KnowledgeItemID
	version string
	id      string
}

// factVisibility hides, from one window's assessment, the items the SERIAL ordered run
// had not stored yet when it assessed that window (the two-phase backfill stores a whole
// group first; fact_backfill_staged.go). nil hides nothing — the serial path.
type factVisibility struct {
	// hidden reports whether an item id is hidden; bySubject lists the hidden ids per
	// subject reference (scope key + subject key), the only ones a fuzzy read can return.
	hidden    func(id string) bool
	bySubject func(f domain.Fact) []domain.KnowledgeItemID
}

func (v *factVisibility) hides(id string) bool { return v != nil && v.hidden(id) }

// planAssessment collects one window's candidate pairs (A1.2 rules 2-4) and keys them.
// It only READS (profiles, chains, similar facts); nil when there is no pair.
func (e *FactExtractor) planAssessment(ctx context.Context, facts []domain.Fact, vis *factVisibility) *assessPlan {
	// Candidates are read with the explicit kernel bypass, but within each new fact's own
	// subject reference (scope key + subject): they are facts of exactly this
	// classification, which is what lets them into this prompt.
	// Each fact's candidates are read under the kernel bypass that may see the
	// owner-scoped rows of exactly that fact's owners (domain.ScopeSystemFor):
	// X's fact is judged against X's facts and the unowned corpus, never Y's — and
	// an unowned fact never against anyone's private ones (domain/owner_scope.go).
	sysFor := func(f domain.Fact) (context.Context, *domain.TagPredicate) {
		sc := domain.ScopeSystemFor(domain.OwnersOf(f.Tags)...)
		return domain.WithScope(ctx, sc), sc
	}
	cards := map[string]string{}
	sorted := append([]domain.Fact(nil), facts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	// A pair is judged ONCE, from the side of its later-asserted member, against what was
	// asserted before it: candidates are restricted to items ordered strictly before the
	// new fact by (asserted_at, id). This is what makes the candidate set — and so the
	// keyed assessment id — a function of the session and the corpus BEFORE it, so
	// re-ingesting a corpus in the same order replays every stored verdict with no call
	// (A1.10 falsification 1). The cost: a session ingested out of order (older than
	// facts already stored) is not judged against those newer facts; its exact-key
	// pairs still close by declared/learned cardinality and cues, by dates.
	before := func(c, f domain.Fact) bool {
		if !c.AssertedAt.Equal(f.AssertedAt) {
			return c.AssertedAt.Before(f.AssertedAt)
		}
		return c.ID < f.ID
	}
	var staged domain.FactStagedReader
	if vis != nil {
		staged, _ = e.reader.(domain.FactStagedReader)
	}
	var items []contradictionItem
	for _, f := range sorted {
		if f.Polarity == domain.FactNegative {
			continue
		}
		var cands []domain.Fact
		seen := map[string]bool{f.ID: true}
		if e.cardinalityOf(ctx, f.AttributeKey, cards) == domain.CardinalityUnknown {
			sys, sc := sysFor(f)
			chain, err := e.reader.FactChain(sys, f, sc)
			if err != nil {
				slog.WarnContext(ctx, "FactExtractor: exact-key candidates unavailable", "item", f.ID, "err", err)
			}
			for _, c := range chain {
				// A hidden item is simply absent: a chain's MEMBERSHIP and each member's
				// own start (all the prompt shows) do not depend on the other members.
				if seen[c.ID] || vis.hides(c.ID) || c.Polarity == domain.FactNegative || c.ValueNorm == f.ValueNorm || !before(c, f) {
					continue
				}
				seen[c.ID] = true
				cands = append(cands, c)
			}
		}
		if e.cfg.FuzzyCandidates > 0 && len(f.Embedding) > 0 {
			var (
				sim []domain.Fact
				err error
			)
			sys, sc := sysFor(f)
			if staged != nil {
				// The top-k and the derived-empty filter depend on which items exist, so
				// the hidden items are excluded inside the read, not after it.
				sim, err = staged.SimilarFactsHiding(sys, f, f.Embedding, e.cfg.FuzzyCandidates, sc, vis.bySubject(f))
			} else {
				sim, err = e.reader.SimilarFacts(sys, f, f.Embedding, e.cfg.FuzzyCandidates, sc)
			}
			if err != nil {
				slog.WarnContext(ctx, "FactExtractor: fuzzy candidates unavailable", "item", f.ID, "err", err)
			}
			for _, c := range sim {
				if !seen[c.ID] && !vis.hides(c.ID) && before(c, f) {
					seen[c.ID] = true
					cands = append(cands, c)
				}
			}
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
		items = append(items, contradictionItem{New: f, Candidates: cands})
	}
	if len(items) == 0 {
		return nil
	}
	req := buildAssessmentRequest(items)
	p := &assessPlan{items: items, req: req, prompt: domain.FlattenRequest(req), version: e.cfg.AssessorModel + "|" + factAssessmentPromptVersion}
	for _, it := range items {
		p.inputs = append(p.inputs, domain.KnowledgeItemID(it.New.ID))
		for _, c := range it.Candidates {
			p.inputs = append(p.inputs, domain.KnowledgeItemID(c.ID))
		}
	}
	p.id = domain.AssessmentID(domain.AssessmentAuthorityLLMContradiction, p.version, domain.PromptHash(p.prompt), p.inputs)
	return p
}

// askJudge makes the plan's one judge call. It stores nothing.
func (e *FactExtractor) askJudge(ctx context.Context, p *assessPlan) (domain.GenerateResponse, error) {
	// Booked as facts_assess, not facts: extraction and assessment share the
	// PurposeFacts handle, and a per-request report must tell them apart.
	llmCtx, cancel := context.WithTimeout(domain.WithLLMCallLabel(ctx, domain.LLMCallLabelFactsAssess), e.callTimeout(len(p.prompt)))
	resp, err := domain.GenerateVia(llmCtx, e.gen, p.req)
	cancel()
	e.llmCalls.Add(1)
	e.assessmentCalls.Add(1)
	return resp, err
}

// commitAssessment applies the judge's answer to the plan: stored as a keyed Assessment
// (which re-derives every cited key) and, when new, its exact-key verdicts vote.
func (e *FactExtractor) commitAssessment(ctx context.Context, p *assessPlan, resp domain.GenerateResponse, callErr error) {
	var verdicts []domain.AssessmentVerdict
	perr := callErr
	switch {
	case errors.Is(callErr, domain.ErrSchemaValidation):
		// The reply failed its schema even after the provider's repair turn: an
		// unreadable answer, handled below like any other (stored as refused).
	case callErr != nil:
		// A transport failure is not a verdict: nothing is stored, so a later session
		// over the same candidates asks again.
		slog.WarnContext(ctx, "FactExtractor: assessment call failed; no verdict stored", "err", callErr)
		return
	default:
		verdicts, perr = parseAssessmentReply(resp, p.items)
	}
	if perr != nil {
		// An answer that cannot be read IS a verdict — the judge refused — and it is
		// stored, so a replay is consistent with it.
		slog.WarnContext(ctx, "FactExtractor: assessment answer unreadable; stored as refused", "err", perr)
		verdicts = refusedVerdicts(p.items)
	}
	a := domain.Assessment{
		ID: p.id, NamespaceID: e.cfg.Namespace, Kind: domain.KindMemoryFact,
		Authority: domain.AssessmentAuthorityLLMContradiction, AuthorityVersion: p.version,
		PromptHash: domain.PromptHash(p.prompt), InputItemIDs: p.inputs, Verdicts: verdicts,
		RawOutput: resp.Text, Classification: p.items[0].New.Tags,
	}
	inserted, err := e.sink.PutAssessment(ctx, a)
	if err != nil {
		slog.WarnContext(ctx, "FactExtractor: assessment not stored; its verdicts have no effect", "err", err)
		return
	}
	if inserted {
		e.vote(ctx, a, p.items)
	}
}

// vote turns an assessment's EXACT-KEY verdicts into profile votes (A1.4): per predicate,
// one single vote when any exact pair was judged contradictory, one multi vote when any
// was judged consistent. A profile whose cardinality changed re-derives its keys.
func (e *FactExtractor) vote(ctx context.Context, a domain.Assessment, items []contradictionItem) {
	byID := map[domain.KnowledgeItemID]domain.Fact{}
	for _, it := range items {
		byID[domain.KnowledgeItemID(it.New.ID)] = it.New
		for _, c := range it.Candidates {
			byID[domain.KnowledgeItemID(c.ID)] = c
		}
	}
	votes := map[string]*domain.ProfileVote{}
	for _, v := range a.Verdicts {
		x, okA := byID[v.A]
		y, okB := byID[v.B]
		if !okA || !okB || x.EntityID != y.EntityID {
			continue
		}
		pv := votes[x.AttributeKey]
		if pv == nil {
			pv = &domain.ProfileVote{AssessmentID: a.ID}
			votes[x.AttributeKey] = pv
		}
		switch v.Verdict {
		case domain.VerdictContradictory:
			pv.Single = 1
		case domain.VerdictConsistent:
			pv.Multi = 1
		}
	}
	preds := make([]string, 0, len(votes))
	for p := range votes {
		preds = append(preds, p)
	}
	sort.Strings(preds)
	for _, p := range preds {
		changed, err := e.sink.VoteProfile(ctx, e.cfg.Namespace, domain.KindMemoryFact, p, *votes[p], e.cfg.CardinalityMinVotes)
		if err != nil {
			slog.WarnContext(ctx, "FactExtractor: profile vote not recorded", "predicate", p, "err", err)
			continue
		}
		if changed {
			e.learned.Add(1)
			n, err := e.sink.RederivePredicate(ctx, e.cfg.Namespace, domain.KindMemoryFact, p, seedDeclaredLimit)
			slog.InfoContext(ctx, "FactExtractor: predicate cardinality learned; re-derived its keys",
				"predicate", p, "keys", n, "err", err)
		}
	}
}

// ErrFactExtractorStopped is returned by FactsWait.Wait when the extractor stopped
// before the document was extracted. The chunks are stored; only the facts are missing.
var ErrFactExtractorStopped = errors.New("fact extractor stopped before extraction completed")

// FactsWait is the completion signal of one tracked document (sync_ingest_wait_for_facts).
// It holds no goroutine: the worker that processes the job settles it. nil-safe.
type FactsWait struct {
	done    chan struct{}
	once    sync.Once
	failed  atomic.Bool
	stopped atomic.Bool
}

func newFactsWait() *FactsWait { return &FactsWait{done: make(chan struct{})} }

func (w *FactsWait) settle(ok bool) {
	if w == nil {
		return
	}
	if !ok {
		w.failed.Store(true)
	}
	w.once.Do(func() { close(w.done) })
}

func (w *FactsWait) stop() {
	if w == nil {
		return
	}
	w.stopped.Store(true)
	w.settle(false)
}

// Wait blocks until the document settled or ctx is done. nil when it settled (see
// Failed), ErrFactExtractorStopped when the extractor shut down first, ctx.Err()
// otherwise. Cancelling ctx stops only the wait.
func (w *FactsWait) Wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	select {
	case <-w.done:
		if w.stopped.Load() {
			return ErrFactExtractorStopped
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Failed reports whether the document settled without its facts stored.
func (w *FactsWait) Failed() bool { return w != nil && w.failed.Load() }

// Done reports whether the document has settled.
func (w *FactsWait) Done() bool {
	if w == nil {
		return true
	}
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// ingestEvidenceKey carries the ADR-0105 evidence id of the document being ingested from
// ProcessSync's capture to the fact job (it was discarded at the capture site before A1).
type ingestEvidenceKey struct{}

func withIngestEvidence(ctx context.Context, id domain.EvidenceID) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ingestEvidenceKey{}, id)
}

func ingestEvidenceFrom(ctx context.Context) domain.EvidenceID {
	id, _ := ctx.Value(ingestEvidenceKey{}).(domain.EvidenceID)
	return id
}
