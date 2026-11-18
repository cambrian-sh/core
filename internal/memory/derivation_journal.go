package memory

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// DerivationJournaler makes the post-ingest derivations durable (ENG-2122): ADR-0136 D4
// fact extraction (FactExtractor) and ADR-0053 KG chunk-triplet extraction
// (ChunkTripletsBatcher) run from in-memory queues after the ingest is acknowledged,
// and a kernel restart, a failed LLM call or a full queue used to lose the job for good
// — the chunks were stored, the facts and triplets never appeared, and nothing
// re-queued them (readiness run ENG-1995 B7: 61/67 documents in a crash window got no
// facts; after an ordinary restart 84/120 never did; a document whose extraction met an
// LLM outage was tried once).
//
// The ingest records one domain.DerivationJob per kind in the journal BEFORE it saves
// the chunks (so no crash window loses the job; a failed save deletes it again). The
// in-memory queues are unchanged — this adds no second extraction pathway (AGENTS.md
// §4.3) — but every journaled job carries a completion hook:
//
//   - done (the derivation finished, or the erasure / source-version fence refused its
//     writes for good): the row is deleted, for exactly the version it names;
//   - failed (an LLM or store failure): the row is rescheduled with exponential
//     backoff and one more attempt, and marked dead after MaxAttempts (still counted
//     in the backlog; facts-backfill repairs a dead fact job by hand);
//   - deferred (the in-memory queue was full): rescheduled after RetryBase without
//     counting an attempt;
//   - released (the queue stopped at shutdown): left as it is, for the next process.
//
// The recovery loop (Start/Stop, one goroutine) claims the rows a previous process
// left behind (claimed before this one started) and the rows whose retry is due, reads
// their chunks back through the ENFORCING read store, and re-feeds the same extractor
// and batcher, never claiming more than their queues can take. KG jobs, live and
// recovered, are fed to the batcher in slices, round-robin across documents, with the
// row's progress written after each slice (ENG-2111, derivation_kg_feed.go).
//
// Version fence (ENG-501). A replayed job carries the document id and the content
// digest it was journaled with (domain.WithSourceVersion) and its ingest origin
// (domain.WithIngestOrigin), so a document revised or erased since is refused by the
// store exactly as a late live job is. Completion deletes only the row of its own
// version, never a newer ingest's.
//
// Access (AGENTS.md §2.4). A replay serves no principal: like facts-backfill it reads
// the chunks under the explicit domain.ScopeMaintenance bypass and writes the facts as
// domain.SystemPrincipal, so each fact is classified by its SOURCE CHUNK's tags exactly
// as the backfill's are (see FactBackfill for why the system principal keeps a chunk's
// classification verbatim). The facts' actor is the journaled writer (or the chunks'
// recorded source_agent_id) and their evidence id the journaled one, so a replayed
// window writes the same deterministic fact ids the live job would have (AGENTS.md §2.6)
// — a window that was stored before the crash is an idempotent re-write.
type DerivationJournaler struct {
	journal domain.DerivationJournal
	reader  domain.VectorReader
	facts   *FactExtractor
	kg      *ChunkTripletsBatcher
	cfg     DerivationJournalConfig

	// since is when this journaler was built (before the ingest path can record
	// anything): a row claimed before it belongs to a previous process and is orphaned.
	// It is the kernel's clock compared with the database's claimed_at; a skew between
	// the two only delays an orphan until its lease runs out, or makes the first pass
	// claim a fresh row the in-flight set then skips.
	since time.Time

	// inflight is the job versions queued in THIS process's memory, so the recovery
	// loop never feeds a job twice when its claim lease runs out while it waits.
	mu       sync.Mutex
	inflight map[domain.DerivationKey]struct{}

	// ENG-2111: the KG jobs being fed slice by slice (derivation_kg_feed.go): feeds by
	// job version, the round-robin ring of feeds waiting for their next slice, and the
	// room reserved by slices taken but not yet enqueued. Guarded by feedMu.
	feedMu   sync.Mutex
	feeds    map[domain.DerivationKey]*kgFeed
	ring     []*kgFeed
	reserved int
	// kick wakes the loop to feed the ring (a slice settled, a document arrived).
	kick chan struct{}
	// halt is set by Stop: nothing is fed any more.
	halt atomic.Bool

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	started  atomic.Bool

	kgSlices, kgAdvanced atomic.Uint64

	recorded, recordFailed, completed, failed, deferred, died, recovered atomic.Uint64
	backlog                                                              atomic.Pointer[map[domain.DerivationKind]domain.DerivationBacklog]
}

// DerivationJournalConfig is execution.ingestion's derivation_* keys
// (kernel.derivationJournalConfigFrom). Zero values take the defaults below.
type DerivationJournalConfig struct {
	// RecoveryInterval is the period of the recovery pass (default 30 s).
	RecoveryInterval time.Duration
	// RecoveryBatch caps the jobs one pass claims per kind (default 256).
	RecoveryBatch int
	// RetryBase is the backoff after a job's first failure, doubled per further
	// attempt up to RetryMax (defaults 30 s and 30 min). A queue-full deferral waits
	// RetryBase.
	RetryBase, RetryMax time.Duration
	// MaxAttempts is the failed attempts after which a job is dead (default 8).
	MaxAttempts int
	// Lease is how long a claim (an ingest's, or a recovery pass's) keeps the row from
	// the recovery loop of another process (default 10 min). Within one process the
	// in-flight set guards instead, so a lease that runs out only renews.
	Lease time.Duration
	// StoreTimeout bounds one journal write (default 10 s).
	StoreTimeout time.Duration
	// KGSliceChunks is the most chunks of one document fed to the KG batcher at once
	// (default 256; never more than the batcher queue holds), and KGSlicesInFlight how
	// many such slices of one document may be queued at the same time (default 2).
	// Documents take turns slice by slice, so one large ingest cannot hold the queue
	// (ENG-2111).
	KGSliceChunks, KGSlicesInFlight int
}

func (c DerivationJournalConfig) withDefaults() DerivationJournalConfig {
	if c.RecoveryInterval <= 0 {
		c.RecoveryInterval = 30 * time.Second
	}
	if c.RecoveryBatch <= 0 {
		c.RecoveryBatch = 256
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 30 * time.Second
	}
	if c.RetryMax < c.RetryBase {
		c.RetryMax = max(c.RetryBase, 30*time.Minute)
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.Lease <= 0 {
		c.Lease = 10 * time.Minute
	}
	if c.StoreTimeout <= 0 {
		c.StoreTimeout = 10 * time.Second
	}
	if c.KGSliceChunks <= 0 {
		c.KGSliceChunks = 256
	}
	if c.KGSlicesInFlight <= 0 {
		c.KGSlicesInFlight = 2
	}
	return c
}

// NewDerivationJournaler builds the journaler without starting it. journal is the
// derivation_jobs store, reader the ENFORCING read store (MemoryStack.Reader()); facts
// and kg are the queues it journals (either may be nil, not both). It returns nil when
// there is nothing to journal or nowhere to journal it, and a nil journaler is a no-op
// everywhere. kernel.NewMemoryStack is the only production caller.
func NewDerivationJournaler(journal domain.DerivationJournal, reader domain.VectorReader, facts *FactExtractor, kg *ChunkTripletsBatcher, cfg DerivationJournalConfig) *DerivationJournaler {
	if journal == nil || reader == nil || (facts == nil && kg == nil) {
		return nil
	}
	return &DerivationJournaler{
		journal: journal, reader: reader, facts: facts, kg: kg, cfg: cfg.withDefaults(),
		since:    time.Now(),
		inflight: map[domain.DerivationKey]struct{}{},
		feeds:    map[domain.DerivationKey]*kgFeed{},
		kick:     make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// derivationOutcome is how one journaled job ended in memory.
type derivationOutcome int

const (
	derivationDone     derivationOutcome = iota // finished, or refused for good by a fence
	derivationDeferred                          // dropped by a full queue
	derivationFailed                            // extraction or save failed
	derivationReleased                          // released by Stop; the next process recovers it
)

// derivationHook receives a journaled job's outcome. nil = not journaled.
type derivationHook func(derivationOutcome, error)

func (h derivationHook) fire(o derivationOutcome, err error) {
	if h != nil {
		h(o, err)
	}
}

// derivationTracker folds the per-chunk outcomes of one journaled document into one
// outcome (the KG batcher settles chunk by chunk, across documents): when the last
// chunk settles it fires the hook with the most significant outcome seen — released
// over failed over deferred over done. It holds no goroutine. nil-safe.
type derivationTracker struct {
	remaining atomic.Int64
	mu        sync.Mutex
	worst     derivationOutcome
	err       error
	hook      derivationHook
	once      sync.Once
}

func newDerivationTracker(n int, hook derivationHook) *derivationTracker {
	if hook == nil {
		return nil
	}
	t := &derivationTracker{hook: hook}
	t.remaining.Store(int64(n))
	return t
}

func (t *derivationTracker) settle(o derivationOutcome, err error) { t.settleAll(1, o, err) }

// settleAll settles n chunks with one outcome; n == 0 fires a tracker of a document
// with no chunk (nothing to extract).
func (t *derivationTracker) settleAll(n int, o derivationOutcome, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if o > t.worst || (o == t.worst && t.err == nil) {
		t.worst, t.err = o, err
	}
	t.mu.Unlock()
	if t.remaining.Add(-int64(max(n, 0))) > 0 {
		return
	}
	t.once.Do(func() {
		t.mu.Lock()
		o, err := t.worst, t.err
		t.mu.Unlock()
		t.hook(o, err)
	})
}

// errTripletsQueueFull is the deferral cause a journaled chunk records on a full queue.
var errTripletsQueueFull = errors.New("chunk triplets queue full")

// derivationSubmission is one ingest's journaled jobs, between RecordIngest and the
// enqueue. nil-safe: a nil submission journals nothing.
type derivationSubmission struct {
	d     *DerivationJournaler
	ctx   context.Context
	jobs  map[domain.DerivationKind]domain.DerivationJob
	saved bool
}

// recordIngest journals the derivations an ingest is about to queue, BEFORE the chunks
// are saved. kg / facts say which queues the ingest feeds; a fact job is journaled only
// when the source passes execution.memory.fact_extraction_sources. ctx is the job
// context (it carries the principal, the evidence id, the ingest origin and the
// version). A journal write that fails is logged and counted, and the ingest goes on
// without durability for this document — never refused for it.
func (d *DerivationJournaler) recordIngest(ctx context.Context, documentID, digest string, chunkIDs []string, src domain.SourceMeta, kg, facts bool) *derivationSubmission {
	if d == nil || documentID == "" || len(chunkIDs) == 0 {
		return nil
	}
	base := domain.DerivationJob{
		DocumentID: documentID, ContentDigest: digest, ChunkIDs: append([]string(nil), chunkIDs...),
		Writer: domain.PrincipalFromContext(ctx), EvidenceID: ingestEvidenceFrom(ctx), Source: src,
	}
	if t, ok := domain.IngestOriginFrom(ctx); ok {
		base.IngestOrigin = t
	}
	s := &derivationSubmission{d: d, ctx: context.WithoutCancel(ctx), jobs: map[domain.DerivationKind]domain.DerivationJob{}}
	var jobs []domain.DerivationJob
	if kg && d.kg != nil {
		j := base
		j.Kind = domain.DerivationKGTriplets
		jobs = append(jobs, j)
	}
	if facts && d.facts != nil && d.facts.eligible(ctx) {
		j := base
		j.Kind = domain.DerivationFacts
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return nil
	}
	for _, j := range jobs {
		d.markInflight(j.Key())
	}
	wctx, cancel := context.WithTimeout(s.ctx, d.cfg.StoreTimeout)
	err := d.journal.RecordDerivations(wctx, jobs, d.cfg.Lease)
	cancel()
	if err != nil {
		for _, j := range jobs {
			d.clearInflight(j.Key())
		}
		d.recordFailed.Add(1)
		slog.WarnContext(ctx, "DerivationJournal: derivations not journaled; this document's facts/triplets are queued in memory only",
			"document_id", documentID, "err", err,
			"effect", "a restart or failure before they finish loses them (facts-backfill repairs facts)")
		return nil
	}
	d.recorded.Add(uint64(len(jobs)))
	for _, j := range jobs {
		s.jobs[j.Kind] = j
	}
	return s
}

// abandon deletes the submission's jobs: the chunk save failed, so there is nothing to
// derive (the ingest itself fails and its retry journals again).
func (s *derivationSubmission) abandon() {
	if s == nil {
		return
	}
	for _, j := range s.jobs {
		s.d.complete(s.ctx, j.Key())
	}
}

// hook returns the completion hook of the submission's job of kind, nil when that kind
// is not journaled.
func (s *derivationSubmission) hook(kind domain.DerivationKind) derivationHook {
	if s == nil {
		return nil
	}
	j, ok := s.jobs[kind]
	if !ok {
		return nil
	}
	return s.d.hookFor(s.ctx, j)
}

// enqueueKG feeds the submission's KG job to the batcher slice by slice (ENG-2111) and
// returns the sync ingest's wait (track). ok is false when no KG job is journaled: the
// caller then enqueues unjournaled, as before.
func (s *derivationSubmission) enqueueKG(docs []*domain.Document, track bool) (w *TripletsWait, ok bool) {
	if s == nil || s.d.kg == nil {
		return nil, false
	}
	j, ok := s.jobs[domain.DerivationKGTriplets]
	if !ok {
		return nil, false
	}
	return s.d.startKG(s.ctx, j, docs, track), true
}

// hookFor is the completion hook of one journaled job.
func (d *DerivationJournaler) hookFor(ctx context.Context, j domain.DerivationJob) derivationHook {
	k := j.Key()
	return func(o derivationOutcome, err error) {
		switch o {
		case derivationDone:
			d.complete(ctx, k)
		case derivationFailed:
			d.reschedule(ctx, j, err, true)
		case derivationDeferred:
			d.reschedule(ctx, j, err, false)
		case derivationReleased:
			// Shutdown: the row stays claimed by this (ending) process, so the next
			// process's first pass takes it. Nothing to write.
			d.clearInflight(k)
		}
	}
}

func (d *DerivationJournaler) complete(ctx context.Context, k domain.DerivationKey) {
	defer d.clearInflight(k)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.StoreTimeout)
	defer cancel()
	if err := d.journal.CompleteDerivation(wctx, k); err != nil {
		// The row stays; a later pass re-feeds the job, whose marker / idempotent writes
		// make it a cheap no-op.
		slog.WarnContext(ctx, "DerivationJournal: finished job not removed from the journal; it will be replayed (idempotently)",
			"kind", string(k.Kind), "document_id", k.DocumentID, "err", err)
		return
	}
	d.completed.Add(1)
}

// backoff is the retry delay after the given failed attempt (1-based).
func (d *DerivationJournaler) backoff(attempt int) time.Duration {
	delay := d.cfg.RetryBase
	for i := 1; i < attempt && delay < d.cfg.RetryMax; i++ {
		delay *= 2
	}
	return min(delay, d.cfg.RetryMax)
}

func (d *DerivationJournaler) reschedule(ctx context.Context, j domain.DerivationJob, cause error, countAttempt bool) {
	k := j.Key()
	defer d.clearInflight(k)
	delay := d.cfg.RetryBase
	if countAttempt {
		delay = d.backoff(j.Attempts + 1)
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.StoreTimeout)
	defer cancel()
	dead, err := d.journal.RescheduleDerivation(wctx, k, time.Now().Add(delay), msg, countAttempt, d.cfg.MaxAttempts)
	if err != nil {
		// The row keeps its claim lease; the recovery loop takes it when the lease runs out.
		slog.WarnContext(ctx, "DerivationJournal: retry not recorded; the job is retried when its claim lease runs out",
			"kind", string(k.Kind), "document_id", k.DocumentID, "err", err)
		return
	}
	switch {
	case dead:
		d.died.Add(1)
		slog.WarnContext(ctx, "DerivationJournal: derivation failed its last allowed attempt; not retried automatically",
			"kind", string(k.Kind), "document_id", k.DocumentID, "attempts", j.Attempts+1, "cause", msg,
			"effect", "the document's chunks are stored; this derivation stays missing (counted as dead in the backlog; facts-backfill repairs facts)")
	case countAttempt:
		d.failed.Add(1)
		slog.InfoContext(ctx, "DerivationJournal: derivation failed; retry scheduled",
			"kind", string(k.Kind), "document_id", k.DocumentID, "attempt", j.Attempts+1, "retry_in", delay.String(), "cause", msg)
	default:
		d.deferred.Add(1)
	}
}

func (d *DerivationJournaler) markInflight(k domain.DerivationKey) {
	d.mu.Lock()
	d.inflight[k] = struct{}{}
	d.mu.Unlock()
}

func (d *DerivationJournaler) clearInflight(k domain.DerivationKey) {
	d.mu.Lock()
	delete(d.inflight, k)
	d.mu.Unlock()
}

// claimInflight marks k in flight and reports whether it already was.
func (d *DerivationJournaler) claimInflight(k domain.DerivationKey) (already bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.inflight[k]; ok {
		return true
	}
	d.inflight[k] = struct{}{}
	return false
}

// Start launches the recovery loop: a first pass at once (the jobs a previous process
// left), then one every RecoveryInterval. Start it after the extractor and the batcher
// (MemoryStack.Start). Idempotent; nil-safe.
func (d *DerivationJournaler) Start(ctx context.Context) {
	if d == nil || !d.started.CompareAndSwap(false, true) {
		return
	}
	go d.loop(ctx)
}

// Stop ends the recovery loop and waits for it. Call it before stopping the extractor
// and the batcher, so no pass feeds a stopping queue. Safe without Start; nil-safe.
func (d *DerivationJournaler) Stop() {
	if d == nil {
		return
	}
	d.stopOnce.Do(func() {
		d.halt.Store(true)
		close(d.stopCh)
		if d.started.CompareAndSwap(false, true) {
			close(d.doneCh) // never started: no loop to wait for
		}
	})
	<-d.doneCh
	d.releaseWaitingFeeds()
}

func (d *DerivationJournaler) loop(ctx context.Context) {
	defer close(d.doneCh)
	t := time.NewTicker(d.cfg.RecoveryInterval)
	defer t.Stop()
	d.RecoverOnce(ctx)
	for {
		select {
		case <-d.stopCh:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			d.RecoverOnce(ctx)
		case <-d.kick:
			d.pump() // ENG-2111: the next slices, as room frees
		}
	}
}

// RecoverOnce runs one recovery pass over both kinds and refreshes the backlog
// snapshot. It returns the jobs it re-fed. Exported for the loop's tests and an
// operator trigger; the loop is its production caller.
func (d *DerivationJournaler) RecoverOnce(ctx context.Context) int {
	if d == nil {
		return 0
	}
	n := 0
	if d.facts != nil {
		n += d.recoverKind(ctx, domain.DerivationFacts)
	}
	if d.kg != nil {
		n += d.recoverKind(ctx, domain.DerivationKGTriplets)
	}
	if b, err := d.journal.DerivationBacklog(ctx); err == nil {
		d.backlog.Store(&b)
		if n > 0 {
			attrs := []any{"refed", n}
			for k, v := range b {
				attrs = append(attrs, string(k)+"_pending", v.Pending, string(k)+"_retrying", v.Retrying, string(k)+"_dead", v.Dead)
			}
			slog.InfoContext(ctx, "DerivationJournal: recovery pass re-queued journaled derivations", attrs...)
		}
	} else if ctx.Err() == nil {
		slog.WarnContext(ctx, "DerivationJournal: backlog unreadable", "err", err)
	}
	return n
}

func (d *DerivationJournaler) recoverKind(ctx context.Context, kind domain.DerivationKind) int {
	if kind == domain.DerivationKGTriplets {
		return d.recoverKG(ctx)
	}
	room := d.facts.headroom()
	if room <= 0 {
		return 0
	}
	limit := min(d.cfg.RecoveryBatch, room) // one queue slot per document
	jobs, err := d.journal.ClaimDerivations(ctx, kind, d.since, limit, d.cfg.Lease)
	if err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "DerivationJournal: journal unreadable; recovery retries next pass", "kind", string(kind), "err", err)
		}
		return 0
	}
	n := 0
	for _, j := range jobs {
		if d.claimInflight(j.Key()) {
			continue // still queued here: the claim only renewed its lease
		}
		if d.replay(ctx, j) {
			n++
		}
	}
	d.recovered.Add(uint64(n))
	return n
}

// recoverKG claims due and orphaned KG jobs into the feed ring (ENG-2111) and feeds
// it. It never holds more than RecoveryBatch feeds: a feed costs only its chunk ids
// until its slices are fed, which happens as the batcher frees room.
func (d *DerivationJournaler) recoverKG(ctx context.Context) int {
	n := 0
	if limit := d.cfg.RecoveryBatch - d.activeFeeds(); limit > 0 && !d.halt.Load() {
		jobs, err := d.journal.ClaimDerivations(ctx, domain.DerivationKGTriplets, d.since, limit, d.cfg.Lease)
		if err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "DerivationJournal: journal unreadable; recovery retries next pass", "kind", string(domain.DerivationKGTriplets), "err", err)
			}
		}
		for _, j := range jobs {
			if d.claimInflight(j.Key()) {
				continue // still being fed here: the claim only renewed its lease
			}
			d.queueFeed(ctx, j)
			n++
		}
	}
	d.recovered.Add(uint64(n))
	d.pump()
	return n
}

// replay re-feeds one claimed fact job (KG jobs go through the feed ring,
// recoverKG). It reports whether the job was queued.
func (d *DerivationJournaler) replay(ctx context.Context, j domain.DerivationJob) bool {
	// See the type comment (Access): the replay serves no principal.
	rctx := journalReplayCtx(ctx, j)
	chunks, err := readChunksInOrder(rctx, d.reader, j.ChunkIDs)
	if err != nil {
		slog.WarnContext(ctx, "DerivationJournal: chunks of a journaled job unreadable; retried later",
			"kind", string(j.Kind), "document_id", j.DocumentID, "err", err)
		d.reschedule(ctx, j, err, false)
		return false
	}
	if len(chunks) == 0 {
		// Erased, or revised to a version without these chunks: nothing left to derive.
		slog.InfoContext(ctx, "DerivationJournal: journaled document has no stored chunks; job dropped",
			"kind", string(j.Kind), "document_id", j.DocumentID)
		d.complete(ctx, j.Key())
		return false
	}
	hook := d.hookFor(ctx, j)
	switch j.Kind {
	case domain.DerivationFacts:
		d.facts.submit(factJob{
			ctx: rctx, documentID: j.DocumentID, evidenceID: j.EvidenceID,
			actor:  replayFactActor(chunks, j.Writer),
			source: j.Source, chunks: slimFactChunks(chunks), done: hook,
		})
	default:
		d.complete(ctx, j.Key())
		return false
	}
	return true
}

// replayFactActor is factActor for a replay, whose ctx principal is the system
// principal: the chunks' recorded writer, else the journaled one.
func replayFactActor(chunks []*domain.Document, writer domain.PrincipalRef) string {
	for _, c := range chunks {
		if a, ok := c.Metadata["source_agent_id"].(string); ok && a != "" {
			return a
		}
	}
	return writer.ID
}

// DerivationJournalStats is the journaler's counters and the last backlog read.
type DerivationJournalStats struct {
	// Recorded jobs journaled at ingest; RecordFailed ingests whose journal write
	// failed (not durable); Completed rows removed; Failed retries scheduled after a
	// failure; Deferred queue-full deferrals; Died jobs past their last attempt;
	// Recovered jobs re-fed by the recovery loop.
	Recorded, RecordFailed, Completed, Failed, Deferred, Died, Recovered uint64
	// KGSlices KG slices fed to the batcher; KGAdvanced KG progress writes (ENG-2111).
	KGSlices, KGAdvanced uint64
	// Backlog is the journal's rows per kind at the last recovery pass.
	Backlog map[domain.DerivationKind]domain.DerivationBacklog
}

// Stats returns the counters. nil-safe.
func (d *DerivationJournaler) Stats() DerivationJournalStats {
	if d == nil {
		return DerivationJournalStats{}
	}
	st := DerivationJournalStats{
		Recorded: d.recorded.Load(), RecordFailed: d.recordFailed.Load(), Completed: d.completed.Load(),
		Failed: d.failed.Load(), Deferred: d.deferred.Load(), Died: d.died.Load(), Recovered: d.recovered.Load(),
		KGSlices: d.kgSlices.Load(), KGAdvanced: d.kgAdvanced.Load(),
	}
	if b := d.backlog.Load(); b != nil {
		st.Backlog = *b
	}
	return st
}

// readChunksInOrder reads chunks through the enforcing read store (the caller's scope
// is on ctx) and returns the readable ones in the listed order — the order the fact
// extractor's turn indexes and content hash use. Shared by FactBackfill and the
// derivation journal's replay.
func readChunksInOrder(ctx context.Context, reader domain.VectorReader, ids []string) ([]*domain.Document, error) {
	var (
		docs []domain.Document
		err  error
	)
	if cb, ok := reader.(interface {
		ChunksByIDs(context.Context, []string) ([]domain.Document, error)
	}); ok {
		docs, err = cb.ChunksByIDs(ctx, ids)
	} else {
		docs, err = reader.GetBatch(ctx, ids)
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
