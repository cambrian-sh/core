package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2143: background embedding of a large accepted document.
//
// A 5 MB text (5295 chunks) held a synchronous IngestMemory for ~190 s, all of it
// embedding and saving the chunks (readiness run ENG-1995 B3). Owner decision
// 2026-11-19: embed in the BACKGROUND, with VISIBLE progress. A document of at least
// execution.ingestion.background_embed_min_chunks chunks is accepted at once —
// extraction and the ENG-2110 refusal, evidence, the ADR-0138 revise, the document row
// and the source-doc entity are written synchronously as before — and its prepared
// chunks are staged in an 'embed' row of the ENG-2122 derivation journal. The call
// returns the document id with state accepted.
//
// One pathway (AGENTS.md §4.3): the embed row is claimed, leased, retried with backoff,
// marked dead and recovered after a restart by the SAME journal and recovery loop as
// the fact and KG jobs; a bounded set of workers (DerivationJournalConfig.EmbedWorkers)
// embeds and saves each claimed document slice by slice through the SAME embedder and
// tier-2 writer the synchronous ingest uses. After every slice the row's chunk ids
// shrink to the chunks still to embed and its claim is renewed (RenewDerivation), so a
// restart resumes where the previous process stopped and the progress is readable
// (IngestStatus: accepted → embedding n/total → searchable, or failed with a reason).
// When the last slice is stored the structure graph is persisted, the document's fact
// and KG jobs are journaled due-now for the recovery loop, the operator feed event is
// published and the embed row is deleted.
//
// Erasure and the ENG-501 version fence apply during the window without new code:
// every slice is written under the ingest's origin and the accepted version
// (WithIngestOrigin, WithSourceVersion), so the store refuses a slice once the
// document was erased (its tombstone postdates the origin) or revised (its digest moved
// on); erasure deletes the embed row with the document's other derivation rows, and a
// revise replaces it. A worker that meets either — a refused slice, or a renewal that
// finds its row gone — stops and leaves nothing of the old version behind: a slice
// committed before the erase or revise is deleted by it.
//
// Classification. The chunks are classified by the tier-2 writer exactly as at a
// synchronous ingest: as the journaled writer, with the document row's stored tags (the
// result of the same decision on the ingest's context) as the hint, and with the
// classification-relevant context the ingest carried (the private-write owner, the
// conversation owner, the principal's roles and the surface) restored from the stage. A
// decision point that would answer differently now refuses the slice (ENG-2115 strict
// classification), and the document ends failed with that reason — never stored under a
// classification it was not accepted with.

// embedStage is the staged state of an accepted document (DerivationJob.Staged). Field
// names are a stored contract: never rename one.
type embedStage struct {
	Doc    embedStageDoc   `json:"doc"`
	Write  embedWriteCtx   `json:"write"`
	Chunks []stagedChunk   `json:"chunks"`
	Struct *embedStructure `json:"structure,omitempty"`
}

// embedStageDoc is what completion needs of the document (the feed event, logs).
type embedStageDoc struct {
	SourceURI string `json:"source_uri,omitempty"`
	Title     string `json:"title,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	EntityID  string `json:"entity_id,omitempty"`
}

// embedWriteCtx is the classification-relevant context of the ingest (see above).
type embedWriteCtx struct {
	PrivateOwner      string   `json:"private_owner,omitempty"`
	ConversationOwner string   `json:"conversation_owner,omitempty"`
	Roles             []string `json:"roles,omitempty"`
	SurfaceKind       string   `json:"surface_kind,omitempty"`
	SurfaceID         string   `json:"surface_id,omitempty"`
}

// stagedChunk is one prepared chunk: everything SaveBatch stores except the vector.
type stagedChunk struct {
	ID         string            `json:"id"`
	Text       string            `json:"text"`
	Metadata   json.RawMessage   `json:"metadata,omitempty"`
	Activation float64           `json:"activation"`
	CreatedAt  time.Time         `json:"created_at"`
	Stamp      domain.EventStamp `json:"stamp"`
}

// embedStructure is the ADR-0060 structure graph assembled at acceptance
// (BuildStructureGraph is pure); persisted once every chunk is stored.
type embedStructure struct {
	Sections []SectionRow     `json:"sections,omitempty"`
	Stamps   []ChunkStamp     `json:"stamps,omitempty"`
	Edges    []StructuralEdge `json:"edges,omitempty"`
}

func writeCtxOf(ctx context.Context) embedWriteCtx {
	s := domain.SurfaceFromContext(ctx)
	return embedWriteCtx{
		PrivateOwner:      domain.PrivateWriteOwnerFromContext(ctx),
		ConversationOwner: domain.ConversationOwnerFromContext(ctx),
		Roles:             domain.PrincipalRolesFromContext(ctx),
		SurfaceKind:       s.Kind, SurfaceID: s.ID,
	}
}

// stageChunks encodes prepared documents (no embedding) as staged chunks.
func stageChunks(docs []*domain.Document) ([]stagedChunk, error) {
	out := make([]stagedChunk, len(docs))
	for i, d := range docs {
		meta, err := json.Marshal(d.Metadata)
		if err != nil {
			return nil, fmt.Errorf("chunk %s metadata: %w", d.ID, err)
		}
		out[i] = stagedChunk{ID: d.ID, Text: d.Text, Metadata: meta, Activation: d.ActivationStrength,
			CreatedAt: d.CreatedAt, Stamp: d.EventStamp}
	}
	return out, nil
}

// document rebuilds the chunk's store row (without its vector). A list of strings in
// the metadata comes back as []string, the shape the writer produced it in.
func (c stagedChunk) document() (*domain.Document, error) {
	meta := map[string]any{}
	if len(c.Metadata) > 0 {
		if err := json.Unmarshal(c.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("chunk %s metadata: %w", c.ID, err)
		}
	}
	for k, v := range meta {
		if list, ok := v.([]any); ok {
			if ss, ok := allStrings(list); ok {
				meta[k] = ss
			}
		}
	}
	return &domain.Document{
		ID: c.ID, DocumentType: domain.DocTypeMnemonicFact, Text: c.Text,
		ActivationStrength: c.Activation, Metadata: meta, CreatedAt: c.CreatedAt, EventStamp: c.Stamp,
	}, nil
}

func allStrings(list []any) ([]string, bool) {
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// backgroundEmbedRunner embeds and stores one claimed embed job. *IngestionManager
// implements it (runEmbedJob); the journaler owns the goroutines.
type backgroundEmbedRunner interface {
	runEmbedJob(ctx context.Context, d *DerivationJournaler, j domain.DerivationJob) (derivationOutcome, error)
}

// bindEmbedder makes the journaler run embed jobs through r. Called by
// IngestionManager.SetDerivationJournal, before Start.
func (d *DerivationJournaler) bindEmbedder(r backgroundEmbedRunner) {
	if d != nil && d.cfg.BackgroundEmbed {
		d.embedRun = r
	}
}

// backgroundEmbedReady reports whether an ingest may be accepted for background
// embedding: the feature is on and a runner is bound.
func (d *DerivationJournaler) backgroundEmbedReady() bool {
	return d != nil && d.cfg.BackgroundEmbed && d.embedRun != nil && !d.halt.Load()
}

// acceptEmbed journals an accepted document's embed job, due at once, and wakes the
// loop to claim it. An error leaves nothing journaled (the caller embeds synchronously).
func (d *DerivationJournaler) acceptEmbed(ctx context.Context, j domain.DerivationJob) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.StoreTimeout)
	defer cancel()
	if err := d.journal.RecordDerivations(wctx, []domain.DerivationJob{j}, 0); err != nil {
		d.recordFailed.Add(1)
		return err
	}
	d.recorded.Add(1)
	d.embedAccepted.Add(1)
	d.wakeEmbed()
	return nil
}

func (d *DerivationJournaler) wakeEmbed() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// recoverEmbed claims due and orphaned embed jobs up to the free worker slots and
// starts a worker for each. Runs on the loop goroutine (and RecoverOnce).
func (d *DerivationJournaler) recoverEmbed(ctx context.Context) int {
	if d.embedRun == nil || d.halt.Load() {
		return 0
	}
	free := cap(d.embedSlots) - len(d.embedSlots)
	if free <= 0 {
		return 0
	}
	jobs, err := d.journal.ClaimDerivations(ctx, domain.DerivationEmbed, d.since, free, d.cfg.Lease)
	if err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "DerivationJournal: journal unreadable; background embeds resume next pass", "err", err)
		}
		return 0
	}
	n := 0
	for _, j := range jobs {
		if d.claimInflight(j.Key()) {
			continue // being embedded here: the claim only renewed its lease
		}
		select {
		case d.embedSlots <- struct{}{}:
		default:
			d.clearInflight(j.Key()) // no slot after all; due again when its lease runs out
			continue
		}
		d.embedWG.Add(1)
		go d.embedJob(d.workerCtx(ctx), j)
		n++
	}
	return n
}

// workerCtx is the context every embed worker runs under: derived once from the first
// launching pass's ctx without its cancellation (a worker outlives the pass), and
// cancelled by Stop or when the loop's kernel context ends (cancelEmbeds).
func (d *DerivationJournaler) workerCtx(ctx context.Context) context.Context {
	d.embedMu.Lock()
	defer d.embedMu.Unlock()
	if d.embedCtx == nil {
		d.embedCtx, d.embedCancel = context.WithCancel(context.WithoutCancel(ctx))
	}
	return d.embedCtx
}

// cancelEmbeds stops the running embed workers (each leaves its row, with its
// progress, for the next pass or process). Safe before any worker ran.
func (d *DerivationJournaler) cancelEmbeds() {
	d.embedMu.Lock()
	defer d.embedMu.Unlock()
	if d.embedCancel != nil {
		d.embedCancel()
	}
}

// embedJob runs one embed job on its own goroutine (owned by the journaler: Stop
// cancels embedCtx and waits for embedWG).
func (d *DerivationJournaler) embedJob(ctx context.Context, j domain.DerivationJob) {
	defer d.embedWG.Done()
	defer func() { <-d.embedSlots }()
	o, err := d.embedRun.runEmbedJob(ctx, d, j)
	if o == derivationReleased && ctx.Err() == nil {
		o = derivationDeferred // not a shutdown: try again after RetryBase
	}
	switch o {
	case derivationDone:
		d.embedDone.Add(1)
	case derivationFailed:
		d.embedFailed.Add(1)
	}
	d.hookFor(context.WithoutCancel(ctx), j)(o, err)
}

// renewEmbed records an embed job's progress and renews its claim; live is false when
// the job's row is gone (erased or replaced). A write error counts as live: the slice
// is stored, and only the progress is not recorded (a resume redoes it idempotently).
func (d *DerivationJournaler) renewEmbed(ctx context.Context, k domain.DerivationKey, remaining []string) bool {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.StoreTimeout)
	defer cancel()
	live, err := d.journal.RenewDerivation(wctx, k, remaining, d.cfg.Lease)
	if err != nil {
		slog.WarnContext(ctx, "DerivationJournal: background embed progress not recorded; a resume repeats the slice",
			"document_id", k.DocumentID, "err", err)
		return true
	}
	return live
}

// recordDerivationsDue journals the fact and KG jobs of a document whose chunks the
// background embed has just stored, due at once: the recovery loop feeds them exactly
// as it feeds a recovered job (journalReplayCtx). Woken immediately.
func (d *DerivationJournaler) recordDerivationsDue(ctx context.Context, j domain.DerivationJob, chunkIDs []string) {
	base := domain.DerivationJob{
		DocumentID: j.DocumentID, ContentDigest: j.ContentDigest, ChunkIDs: chunkIDs,
		Writer: j.Writer, EvidenceID: j.EvidenceID, IngestOrigin: j.IngestOrigin, Source: j.Source,
	}
	var jobs []domain.DerivationJob
	if d.kg != nil {
		k := base
		k.Kind = domain.DerivationKGTriplets
		jobs = append(jobs, k)
	}
	if d.facts != nil && d.facts.eligible(domain.WithPrincipal(ctx, j.Writer)) {
		f := base
		f.Kind = domain.DerivationFacts
		jobs = append(jobs, f)
	}
	if len(jobs) == 0 || len(chunkIDs) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.StoreTimeout)
	defer cancel()
	if err := d.journal.RecordDerivations(wctx, jobs, 0); err != nil {
		d.recordFailed.Add(1)
		slog.WarnContext(ctx, "DerivationJournal: facts/triplets of a background-embedded document not journaled",
			"document_id", j.DocumentID, "err", err, "effect", "its chunks are searchable; facts-backfill repairs facts")
		return
	}
	d.recorded.Add(uint64(len(jobs)))
	d.wakeEmbed()
}

// backgroundWriteCtx is the context one embed job writes under: the journaled writer
// and the ingest's classification context (see the file comment), and the fences.
func backgroundWriteCtx(ctx context.Context, j domain.DerivationJob, w embedWriteCtx) context.Context {
	c := ctx
	if !j.Writer.IsZero() {
		c = domain.WithPrincipal(c, j.Writer)
	}
	c = domain.WithPrivateWrites(c, w.PrivateOwner)
	c = domain.WithConversationOwner(c, w.ConversationOwner)
	if len(w.Roles) > 0 {
		c = domain.WithPrincipalRoles(c, w.Roles)
	}
	if w.SurfaceKind != "" || w.SurfaceID != "" {
		c = domain.WithSurface(c, domain.SurfaceRef{Kind: w.SurfaceKind, ID: w.SurfaceID})
	}
	c = domain.WithSourceVersion(c, domain.SourceVersion{DocumentID: j.DocumentID, ContentDigest: j.ContentDigest})
	c = domain.WithIngestOrigin(c, j.IngestOrigin)
	return withIngestEvidence(c, j.EvidenceID)
}

// runEmbedJob implements backgroundEmbedRunner: embed and store the job's remaining
// chunks slice by slice, then complete the document (see the file comment).
func (im *IngestionManager) runEmbedJob(ctx context.Context, d *DerivationJournaler, j domain.DerivationJob) (derivationOutcome, error) {
	started := time.Now()
	var st embedStage
	if err := json.Unmarshal(j.Staged, &st); err != nil {
		return derivationFailed, fmt.Errorf("staged chunks unreadable: %w", err)
	}
	remaining := make(map[string]struct{}, len(j.ChunkIDs))
	for _, id := range j.ChunkIDs {
		remaining[id] = struct{}{}
	}
	todo := make([]stagedChunk, 0, len(j.ChunkIDs))
	allIDs := make([]string, 0, len(st.Chunks))
	for _, c := range st.Chunks {
		allIDs = append(allIDs, c.ID)
		if _, ok := remaining[c.ID]; ok {
			todo = append(todo, c)
		}
	}
	total := j.ChunksTotal
	if total <= 0 {
		total = len(st.Chunks)
	}
	wctx := backgroundWriteCtx(ctx, j, st.Write)
	key := j.Key()
	slog.InfoContext(ctx, "IngestionManager: background embed started",
		"document_id", j.DocumentID, "source_uri", st.Doc.SourceURI, "chunks_total", total,
		"chunks_embedded", total-len(todo), "attempt", j.Attempts+1)
	slice := max(d.cfg.EmbedSliceChunks, 1)
	dropped := 0
	for len(todo) > 0 {
		if ctx.Err() != nil {
			return derivationReleased, ctx.Err()
		}
		part := todo[:min(slice, len(todo))]
		docs := make([]*domain.Document, 0, len(part))
		chunks := make([]domain.Chunk, len(part))
		for i, c := range part {
			doc, err := c.document()
			if err != nil {
				return derivationFailed, err
			}
			docs = append(docs, doc)
			chunks[i] = domain.Chunk{Body: c.Text}
		}
		vectors, err := im.embedChunkBodies(wctx, domain.ExternalDocument{SourceURI: st.Doc.SourceURI}, chunks)
		if ctx.Err() != nil {
			return derivationReleased, ctx.Err()
		}
		if err != nil {
			return derivationFailed, fmt.Errorf("embed: %w", err)
		}
		kept := docs[:0]
		for i, doc := range docs {
			if i < len(vectors) && len(vectors[i]) > 0 {
				doc.Embedding = domain.Embedding{Vector: vectors[i], Model: "dynamic", Size: len(vectors[i])}
				kept = append(kept, doc)
			}
		}
		if len(kept) == 0 {
			// The embedder returned nothing for a whole slice: an outage, not a bad
			// chunk. Retried with backoff; dead (failed) after the last attempt.
			return derivationFailed, fmt.Errorf("embedding returned no vector for %d chunk(s)", len(part))
		}
		if n := len(part) - len(kept); n > 0 {
			dropped += n
			slog.WarnContext(ctx, "IngestionManager: PARTIAL chunk loss in background embed — chunks with no embedding were not stored",
				"document_id", j.DocumentID, "dropped", n, "slice", len(part),
				"effect", "part of this document's content is not retrievable")
		}
		if err := im.agent.Manager.SaveBatch(wctx, kept); err != nil {
			if errors.Is(err, domain.ErrSourceErased) || errors.Is(err, domain.ErrSourceVersionRetired) {
				slog.InfoContext(ctx, "IngestionManager: background embed stopped: the document was erased or revised",
					"document_id", j.DocumentID, "cause", err)
				return derivationDone, nil
			}
			if ctx.Err() != nil {
				return derivationReleased, ctx.Err()
			}
			return derivationFailed, fmt.Errorf("save chunk slice: %w", err)
		}
		todo = todo[len(part):]
		left := make([]string, len(todo))
		for i, c := range todo {
			left[i] = c.ID
		}
		if !d.renewEmbed(ctx, key, left) {
			slog.InfoContext(ctx, "IngestionManager: background embed stopped: the document was erased or revised",
				"document_id", j.DocumentID)
			return derivationDone, nil
		}
		d.embedSlices.Add(1)
		slog.InfoContext(ctx, "IngestionManager: background embed progress", "document_id", j.DocumentID,
			"chunks_embedded", total-len(todo), "chunks_total", total)
	}
	if s := st.Struct; s != nil && im.structureStore != nil {
		im.persistStructureRows(wctx, j.DocumentID, s.Sections, s.Stamps, s.Edges, "staged")
	}
	d.recordDerivationsDue(ctx, j, allIDs)
	im.publishWritten(wctx, domain.ExternalDocument{SourceURI: st.Doc.SourceURI, Title: st.Doc.Title, ThreadID: st.Doc.ThreadID},
		st.Doc.EntityID, total-dropped)
	slog.InfoContext(ctx, "IngestionManager: background embed complete; document searchable",
		"document_id", j.DocumentID, "source_uri", st.Doc.SourceURI, "chunks_total", total, "dropped", dropped,
		"elapsed_ms", time.Since(started).Milliseconds())
	return derivationDone, nil
}

// backgroundEmbedEligible reports whether a document of n chunks is accepted for
// background embedding: the journal can run it and n reaches
// execution.ingestion.background_embed_min_chunks. Smaller documents stay synchronous.
func (im *IngestionManager) backgroundEmbedEligible(n int) bool {
	return im.derivations.backgroundEmbedReady() && im.cfg.BackgroundEmbedMinChunks > 0 && n >= im.cfg.BackgroundEmbedMinChunks
}

// acceptBackground stages the prepared chunks of an accepted document in an embed job
// of the derivation journal (see the file comment). An error stages nothing.
func (im *IngestionManager) acceptBackground(ctx context.Context, doc domain.ExternalDocument, ident docIdentity, entityID string,
	src domain.SourceMeta, prepared []*domain.Document, structure *embedStructure) error {
	chunks, err := stageChunks(prepared)
	if err != nil {
		return err
	}
	staged, err := json.Marshal(embedStage{
		Doc:   embedStageDoc{SourceURI: doc.SourceURI, Title: doc.Title, ThreadID: doc.ThreadID, EntityID: entityID},
		Write: writeCtxOf(ctx), Chunks: chunks, Struct: structure,
	})
	if err != nil {
		return fmt.Errorf("stage chunks: %w", err)
	}
	ids := make([]string, len(prepared))
	for i, d := range prepared {
		ids[i] = d.ID
	}
	j := domain.DerivationJob{
		Kind: domain.DerivationEmbed, DocumentID: ident.ID, ContentDigest: ident.ContentDigest, ChunkIDs: ids,
		Writer: domain.PrincipalFromContext(ctx), EvidenceID: ingestEvidenceFrom(ctx), Source: src,
		Staged: staged, ChunksTotal: len(ids),
	}
	if t, ok := domain.IngestOriginFrom(ctx); ok {
		j.IngestOrigin = t
	}
	if err := im.derivations.acceptEmbed(ctx, j); err != nil {
		return fmt.Errorf("journal embed job: %w", err)
	}
	slog.InfoContext(ctx, "IngestionManager: document accepted; embedding continues in the background",
		"document_id", ident.ID, "source_uri", doc.SourceURI, "chunks_total", len(ids), "staged_bytes", len(staged))
	return nil
}

// IngestStatus reads one document's ingest state (ENG-2143) for the agent and operator
// planes. ok is false when no document carries the id or no journal is wired; a
// document stored synchronously reads searchable.
func (im *IngestionManager) IngestStatus(ctx context.Context, documentID string) (domain.IngestStatus, bool, error) {
	if im == nil || im.derivations == nil {
		return domain.IngestStatus{}, false, nil
	}
	return im.derivations.journal.IngestStatus(ctx, documentID)
}
