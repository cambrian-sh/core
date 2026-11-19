package kernel

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/forget"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
)

type recordingErasureObserver struct {
	mu   sync.Mutex
	seen []domain.ErasureReceipt
}

func (o *recordingErasureObserver) ObserveErasure(_ context.Context, r domain.ErasureReceipt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, r)
}

// ADR-0137 S6 end to end (ENG-411 stage C): the production composition — a real
// Postgres adapter, the raw PgErasureStore as MemoryBackend.Eraser, NewMemoryStack with
// execution.erasure on, the forget service over MemoryStack.Eraser() — driven through
// the real agent-plane and operator-plane RPC handlers, with the effect checked in
// Postgres:
//   - an agent cannot erase another agent's memory (PermissionDenied, row kept);
//   - an agent cannot even see a row owner-fenced to another person (NotFound);
//   - an agent previews its own memory and erases it with the token; the row is gone,
//     a tombstone and a receipt remain, the observer saw it once, a retry replays;
//   - an operator's compliance erase lifts the owner fence and removes the fenced row,
//     audited, idempotent on command_id.
//
// Requires PG_TEST_DSN pointing at a scratch database (the adapter auto-migrates it).
func TestPg_Forget_EndToEndThroughBothPlanes(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)

	obs := &recordingErasureObserver{}
	backend := NewMemoryBackend(vec)
	backend.Eraser = postgres.NewPgErasureStore(postgres.NewPgDocumentLifecycle(postgres.NewPgKnowledgeStore(vec.Pool(), nil)),
		postgres.WithErasureObserver(obs))
	mem := NewMemoryStack(backend, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	if mem.Eraser() == nil {
		t.Fatal("erasure enabled but MemoryStack.Eraser() is nil")
	}
	svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "e2e"})

	stamp := time.Now().UTC().Format("150405.000000")
	mine, bobs, fenced := "eng411c-mine-"+stamp, "eng411c-bobs-"+stamp, "eng411c-fenced-"+stamp
	maint := domain.WithScope(ctx, domain.ScopeMaintenance)
	t.Cleanup(func() {
		for _, id := range []string{mine, bobs, fenced} {
			_ = vec.Delete(maint, id)
		}
		_, _ = vec.Pool().Exec(context.Background(),
			`DELETE FROM memory_erasures WHERE principal IN ('eng411c-alice', 'eng411c-ops')`)
	})
	vector := make([]float32, cfg.Embedder.Dimensions)
	vector[0] = 1
	write := func(agent, id string) {
		t.Helper()
		d := &domain.Document{ID: id, DocumentType: domain.DocTypeMnemonicFact, Text: "eng411c secret " + id,
			Embedding: domain.Embedding{Vector: vector}, ActivationStrength: 0.5}
		if err := mem.Writer().Save(domain.WithPrincipal(ctx, domain.AgentPrincipal(agent)), d); err != nil {
			t.Fatalf("Writer().Save %s: %v", id, err)
		}
	}
	write("eng411c-alice", mine)
	write("eng411c-bob", bobs)
	write("eng411c-alice", fenced)
	// Owner-fence the third row to another person (as a personal ingress would).
	if _, err := vec.Pool().Exec(ctx, `UPDATE chunks SET metadata = jsonb_set(metadata, '{tags}', '["owner:eng411c-carol"]'::jsonb) WHERE id = $1`, fenced); err != nil {
		t.Fatalf("fence: %v", err)
	}
	rows := func(id string) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tombstones := func(id string) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM memory_erasure_targets WHERE target_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// ── Agent plane ──
	agentSrv := &subnetwork.Server{Forgetter: svc}
	alice := metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", "eng411c-alice"))
	sel := func(id string) *pb.ErasureSelector { return &pb.ErasureSelector{ChunkIds: []string{id}} }

	if _, err := agentSrv.Forget(alice, &pb.ForgetRequest{Selector: sel(bobs)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("alice erasing bob's memory: %v, want PermissionDenied", err)
	}
	if rows(bobs) != 1 {
		t.Fatal("a denied erase must leave bob's row")
	}
	if _, err := agentSrv.PreviewForget(alice, &pb.PreviewForgetRequest{Selector: sel(fenced)}); status.Code(err) != codes.NotFound {
		t.Fatalf("alice previewing a row fenced to carol: %v, want NotFound (absent)", err)
	}
	p, err := agentSrv.PreviewForget(alice, &pb.PreviewForgetRequest{Selector: sel(mine)})
	if err != nil || len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetId() != mine {
		t.Fatalf("preview of alice's own row = %+v, %v", p, err)
	}
	if rows(mine) != 1 {
		t.Fatal("a preview must erase nothing")
	}
	resp, err := agentSrv.Forget(alice, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatalf("Forget(token): %v", err)
	}
	if rows(mine) != 0 || tombstones(mine) != 1 {
		t.Fatalf("after Forget: chunk rows %d (want 0), tombstones %d (want 1)", rows(mine), tombstones(mine))
	}
	if r := resp.GetReceipt(); r.GetPrincipal() != "eng411c-alice" || r.GetPrincipalKind() != "agent" || r.GetReplayed() {
		t.Fatalf("receipt = %+v", r)
	}
	again, err := agentSrv.Forget(alice, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken()})
	if err != nil || !again.GetReceipt().GetReplayed() || again.GetReceipt().GetRequestId() != resp.GetReceipt().GetRequestId() {
		t.Fatalf("retry = %+v, %v; want the same request replayed", again, err)
	}
	list, err := agentSrv.ListMyErasures(alice, &pb.ListMyErasuresRequest{})
	if err != nil || len(list.GetReceipts()) == 0 || list.GetReceipts()[0].GetPrincipal() != "eng411c-alice" {
		t.Fatalf("ListMyErasures = %+v, %v", list, err)
	}

	// ── Operator plane: the compliance erase lifts the owner fence ──
	audit := operator.NewInMemoryAuditStore()
	opSvc := operatortest.New(t, nil, func(d *operator.Deps, o *operator.Options) {
		d.Audit = audit
		o.Forget = svc
	})
	opCtx := domain.WithPrincipal(domain.WithPrincipalRoles(ctx, []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng411c-ops"})
	op, err := opSvc.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{ChunkIds: []string{fenced}}})
	if err != nil || len(op.GetCandidates()) != 1 {
		t.Fatalf("operator preview of the fenced row = %+v, %v", op, err)
	}
	fr, err := opSvc.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: "eng411c-" + stamp, Reason: "KVKK erasure request", PreviewToken: op.GetPreviewToken()})
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
	if rows(fenced) != 0 || tombstones(fenced) != 1 || fr.GetReceipt().GetPrincipalKind() != "operator" || fr.GetReceipt().GetReason() != "compliance" {
		t.Fatalf("operator erase: rows %d tombstones %d receipt %+v", rows(fenced), tombstones(fenced), fr.GetReceipt())
	}
	dup, err := opSvc.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: "eng411c-" + stamp, Reason: "KVKK erasure request", PreviewToken: op.GetPreviewToken()})
	if err != nil || !dup.GetDeduped() || dup.GetReceipt().GetRequestId() != fr.GetReceipt().GetRequestId() {
		t.Fatalf("retried command = %+v, %v", dup, err)
	}
	all, err := opSvc.ListErasures(opCtx, &pb.ListErasuresOpRequest{})
	if err != nil || len(all.GetReceipts()) < 2 {
		t.Fatalf("operator ListErasures = %d receipts, %v; want both erasures", len(all.GetReceipts()), err)
	}
	if rows(bobs) != 1 {
		t.Fatal("bob's row must survive everything above")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.seen) != 2 {
		t.Fatalf("observer saw %d receipts, want 2 (the replay is not observed)", len(obs.seen))
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────
// ADR-0137 S8 gate (ENG-411 stage D): the TestPg_Forget_* suite. Every test runs on a
// real, auto-migrated Postgres (PG_TEST_DSN) through the production composition — the
// real adapter, PgErasureStore over the ADR-0138 lifecycle and a registry-backed
// knowledge store, NewMemoryStack with execution.erasure on, the forget service over
// MemoryStack.Eraser() — and ingests through the real IngestionManager.ProcessSync.
//
// No LLM is called. The derivatives an LLM or a background job produces in production
// are written with deterministic fakes into the stores the production writers use: KG
// triplets (the real ChunkTripletsBatcher driven by a fake domain.TripletExtractor, or
// SaveChunkTriplets), memory_fact items (PgKnowledgeStore.PutItem and the enforcing
// MemoryStack.FactWriter the fact extractor writes through) with item embeddings,
// PageRank, structure/Hebbian edges, evidence. The embedder is a deterministic
// bag-of-words hash.
// ─────────────────────────────────────────────────────────────────────────────────────

const gateNS = "eng411d"

// hashEmbedder is a deterministic bag-of-words embedder: each lower-cased token lands
// on one dimension, the vector is L2-normalised. Texts sharing a rare token are close.
type hashEmbedder struct{ dim int }

func (h hashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, h.dim)
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		f := fnv.New32a()
		_, _ = f.Write([]byte(tok))
		v[int(f.Sum32()%uint32(h.dim))]++
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		v[0] = 1
		return v, nil
	}
	s := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= s
	}
	return v, nil
}

// hashCosine is hashEmbedder's cosine between two texts.
func hashCosine(e hashEmbedder, a, b string) float64 {
	va, _ := e.Embed(context.Background(), a)
	vb, _ := e.Embed(context.Background(), b)
	var dot float64
	for i := range va {
		dot += float64(va[i]) * float64(vb[i])
	}
	return dot
}

// gate is one composed kernel memory stack over the scratch database.
type gate struct {
	t     *testing.T
	ctx   context.Context
	cfg   *config.Config
	vec   *postgres.PgVectorAdapter
	ks    *postgres.PgKnowledgeStore
	facts *postgres.PgFactItemReader
	mem   *MemoryStack
	svc   *forget.Service
	emb   hashEmbedder
	stamp string
	// eraser is the raw erasure store under the stack (the erasure worker's outbox).
	eraser *postgres.PgErasureStore
}

func newGate(t *testing.T) *gate { t.Helper(); return newGateWith(t) }

// newGateWith composes the gate with extra erasure-store options (ENG-1966: plugin
// tail steps).
func newGateWith(t *testing.T, eraserOpts ...postgres.ErasureStoreOption) *gate {
	t.Helper()
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	cfg.Execution.Facts.ExtractEnabled = false // no LLM: fact items are written by the test
	cfg.Execution.Facts.LaneEnabled = true
	cfg.Execution.Retrieval.RecallSimilarityFloor = 0
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	// Hebbian reinforcement ON (the shipped default), so every recall the gate probes
	// also runs the async co_activated write across the erase — the ENG-1951 race and
	// the no-resurrection fence of that write are both exercised under -race.
	cfg.Execution.Retrieval.HebbianEnabled = true
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := postgres.NewPgKnowledgeStore(vec.Pool(), reg)
	reader := postgres.NewPgFactItemReader(ks, gateNS, "gate-model", cfg.Embedder.Dimensions)
	backend := NewMemoryBackend(vec)
	lifecycle := postgres.NewPgDocumentLifecycle(ks)
	eraser := postgres.NewPgErasureStore(lifecycle, eraserOpts...)
	backend.Eraser = eraser
	emb := hashEmbedder{dim: cfg.Embedder.Dimensions}
	mem := NewMemoryStack(backend, nil, emb, cfg.Execution, nil, cfg.Chunker, nil,
		FactSubstrate{Store: ks, Reader: reader, Inspector: reader, EmbedModelID: "gate-model", Documents: lifecycle}, RetrievalDeps{})
	if mem.Eraser() == nil || mem.FactReader == nil || mem.FactWriter == nil || mem.FactInspector == nil {
		t.Fatal("the composed stack lacks the eraser or the fact chokepoints")
	}
	// The retrieval lanes app.go switches on (hybrid FTS, KG²RAG with entity seeding and
	// anchors, the neighbour window), so the end-to-end recall probe crosses each one.
	mem.QueryService.EnableHybrid(vec, 60)
	mem.QueryService.EnableKG2RAG(vec, 2, 20, 8, 8)
	mem.QueryService.EnableQueryEntitySeeding()
	mem.QueryService.EnableAnchorConstraint()
	mem.QueryService.EnableNeighborWindow()
	g := &gate{t: t, ctx: ctx, cfg: cfg, vec: vec, ks: ks, facts: reader, mem: mem,
		svc:    forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "gate"}),
		emb:    emb,
		eraser: eraser,
		stamp:  strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")}
	g.clean()
	t.Cleanup(g.clean)
	return g
}

// tok is a token unique to this test run (no other row in a shared scratch DB has it).
func (g *gate) tok(name string) string { return "zq" + name + g.stamp }

func (g *gate) clean() {
	c := context.Background()
	like := "%" + g.stamp + "%"
	for _, q := range []string{
		`DELETE FROM resolutions WHERE namespace_id = '` + gateNS + `'`,
		`DELETE FROM knowledge_assessments WHERE namespace_id = '` + gateNS + `'`,
		`DELETE FROM predicate_profiles WHERE namespace_id = '` + gateNS + `'`,
		`DELETE FROM knowledge_items WHERE namespace_id = '` + gateNS + `'`,
		`DELETE FROM knowledge_extractions WHERE namespace_id = '` + gateNS + `'`,
		`DELETE FROM memory_erasures WHERE principal LIKE 'eng411d-%'`,
		`DELETE FROM evidence_outbox WHERE evidence_id LIKE 'ev-%` + g.stamp + `%'`,
		`DELETE FROM evidence WHERE source_key LIKE '` + like + `'`,
		`DELETE FROM documents_legacy WHERE id LIKE '` + like + `'`,
		`DELETE FROM chunks WHERE document_id LIKE '` + like + `' OR id LIKE '` + like + `'`,
		`DELETE FROM documents WHERE id LIKE '` + like + `'`,
	} {
		_, _ = g.vec.Pool().Exec(c, q)
	}
}

func (g *gate) n(sql string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.vec.Pool().QueryRow(g.ctx, sql, args...).Scan(&n); err != nil {
		g.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (g *gate) exec(sql string, args ...any) {
	g.t.Helper()
	if _, err := g.vec.Pool().Exec(g.ctx, sql, args...); err != nil {
		g.t.Fatalf("%s: %v", sql, err)
	}
}

// agentGateCtx is what an agent-plane handler builds: the agent principal and the read
// predicate the OSS decision point grants it (bypass, owner fence on the agent).
func agentGateCtx(id string) context.Context {
	return domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal(id)),
		&domain.TagPredicate{Bypass: true, Owners: []string{id}})
}

// operatorGateCtx is what the operator-plane erase handlers build (owner decision
// 2026-11-16: the compliance erase lifts the owner fence).
func operatorGateCtx() context.Context {
	return domain.WithScope(domain.WithPrincipal(domain.WithPrincipalRoles(context.Background(), []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng411d-ops"}), domain.ScopeMaintenance)
}

func (g *gate) uri(name string) string { return "ingest_memory://eng411d/" + name + "-" + g.stamp }

// ingest writes one document as author through the real ingest path and returns its
// document id and content chunk ids (in id order).
func (g *gate) ingest(ctx context.Context, author, name, body string) (string, []string) {
	g.t.Helper()
	entity, err := g.mem.IngestionManager.ProcessSync(ctx, domain.ExternalDocument{
		SourceURI: g.uri(name), SourceType: "file_drop", Title: name, Body: body, Author: author, Timestamp: time.Now().UTC(),
	})
	if err != nil {
		g.t.Fatalf("ProcessSync %s: %v", name, err)
	}
	var doc string
	if err := g.vec.Pool().QueryRow(g.ctx, `SELECT document_id FROM chunks WHERE id = $1`, entity).Scan(&doc); err != nil {
		g.t.Fatalf("document of %s: %v", entity, err)
	}
	return doc, g.chunks(doc)
}

// stub is the document's source-doc entity id (mintSourceDoc), the id MCP get_document
// and the operator GetDocument resolve a document by.
func (g *gate) stub(doc string) string {
	g.t.Helper()
	var id string
	if err := g.vec.Pool().QueryRow(g.ctx, `SELECT id FROM chunks WHERE document_id = $1 AND id LIKE 'source_doc:%'`, doc).Scan(&id); err != nil {
		g.t.Fatalf("source-doc entity of %s: %v", doc, err)
	}
	return id
}

func (g *gate) chunks(doc string) []string {
	g.t.Helper()
	rows, err := g.vec.Pool().Query(g.ctx, `SELECT id FROM chunks WHERE document_id = $1 AND id NOT LIKE 'source_doc:%' ORDER BY id`, doc)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// paragraphs is a body long enough to chunk into several chunks, every paragraph
// carrying the secret token.
func paragraphs(secret string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "Paragraph %d. The %s ledger records the quarterly reconciliation of account %d. ", i, secret, i)
		b.WriteString(strings.Repeat("Ordinary operating notes about the reconciliation follow here. ", 18))
		b.WriteString("\n\n")
	}
	return b.String()
}

func gateFact(subject, attr, value, chunk, doc string, from time.Time) domain.KnowledgeItem {
	f := domain.Fact{Subject: subject, Attribute: attr, Value: value, SubjectKey: strings.ToLower(subject),
		AttributeKey: domain.NormalizeFactAttribute(attr), ValueNorm: domain.NormalizeFactText(value),
		ValidFrom: &from, AssertedAt: from, Actor: "eng411d-alice", SourceChunkID: chunk, SourceDocumentID: doc,
		TurnIndex: 0, Quote: value}
	return f.ToItem(gateNS)
}

// fact writes a memory_fact item sourced from chunk/doc, with an item embedding, into
// the store the fact extractor writes through.
func (g *gate) fact(subject, attr, value, chunk, doc string, from time.Time) domain.KnowledgeItem {
	g.t.Helper()
	it := gateFact(subject, attr, value, chunk, doc, from)
	if _, _, err := g.ks.PutItem(g.ctx, it); err != nil {
		g.t.Fatalf("PutItem: %v", err)
	}
	v, _ := g.emb.Embed(g.ctx, subject+" "+attr+" "+value)
	if err := g.ks.PutItemEmbeddings(g.ctx, []domain.ItemEmbedding{{ItemID: it.ID, ModelID: "gate-model", Vector: v}}); err != nil {
		g.t.Fatalf("PutItemEmbeddings: %v", err)
	}
	return it
}

// seedDerivatives gives doc's chunks the derivatives an LLM or a background job writes
// in production: KG triplets naming entity, PageRank, a Hebbian edge from other, a
// section with PART_OF edges, an evidence row + outbox, a documents_legacy copy, an
// extraction marker, and one memory_fact item per chunk.
func (g *gate) seedDerivatives(doc string, chunks []string, entity, secret, other string) {
	g.t.Helper()
	scores := map[string]float64{}
	for _, c := range chunks {
		if err := g.vec.SaveChunkTriplets(g.ctx, c, []domain.ChunkTriplet{{H: entity, R: "audits", T: secret, Weight: 1}}); err != nil {
			g.t.Fatalf("SaveChunkTriplets: %v", err)
		}
		scores[c] = 0.5
	}
	// PageRank rows as the recompute job writes them (SaveChunkPageRanks replaces the
	// whole table, so rows are added without truncating other documents').
	for c, sc := range scores {
		g.exec(`INSERT INTO chunk_pagerank (chunk_id, score, computed_at) VALUES ($1, $2, now()) ON CONFLICT DO NOTHING`, c, sc)
	}
	sec := doc + "#sec"
	g.exec(`INSERT INTO document_sections (id, document_id, title) VALUES ($1, $2, 'S') ON CONFLICT DO NOTHING`, sec, doc)
	for _, c := range chunks {
		g.exec(`INSERT INTO document_edges (source_id, target_id, edge_type) VALUES ($1, $2, 'PART_OF') ON CONFLICT DO NOTHING`, c, sec)
	}
	if other != "" {
		g.exec(`INSERT INTO document_edges (source_id, target_id, edge_type, weight) VALUES ($1, $2, 'co_activated', 0.9) ON CONFLICT DO NOTHING`, other, chunks[0])
	}
	ev := "ev-" + doc
	g.exec(`INSERT INTO evidence (id, source_id, source_key, source_revision, content_hash) VALUES ($1, 'eng411d-src', $2, '1', $3)`, ev, doc, "cid-"+doc)
	g.exec(`INSERT INTO evidence_outbox (evidence_id) VALUES ($1)`, ev)
	g.exec(`INSERT INTO documents_legacy (id, text, metadata) VALUES ($1, $2, jsonb_build_object('document_id', $3::text))`, doc+"-legacy", secret, doc)
	g.exec(`INSERT INTO knowledge_extractions (namespace_id, extractor, document_id, content_hash) VALUES ($1, 'memory_fact', $2, 'h')`, gateNS, doc)
	for i, c := range chunks {
		g.fact(entity, "audits_account", fmt.Sprintf("%s %d", secret, i), c, doc, time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC))
	}
}

// derivatives counts every row that holds or points at doc's content.
func (g *gate) derivatives(doc string, chunks []string, secret string) map[string]int {
	g.t.Helper()
	return map[string]int{
		"documents":             g.n(`SELECT count(*) FROM documents WHERE id = $1 OR text LIKE $2`, doc, "%"+secret+"%"),
		"chunks":                g.n(`SELECT count(*) FROM chunks WHERE document_id = $1 OR id = ANY($2) OR text LIKE $3`, doc, chunks, "%"+secret+"%"),
		"chunk_embeddings":      g.n(`SELECT count(*) FROM chunk_embeddings WHERE chunk_id = ANY($1)`, chunks),
		"chunk_triplets":        g.n(`SELECT count(*) FROM chunk_triplets WHERE chunk_id = ANY($1) OR t = $2`, chunks, secret),
		"chunk_pagerank":        g.n(`SELECT count(*) FROM chunk_pagerank WHERE chunk_id = ANY($1)`, chunks),
		"document_edges":        g.n(`SELECT count(*) FROM document_edges WHERE source_id = ANY($1) OR target_id = ANY($1)`, chunks),
		"document_sections":     g.n(`SELECT count(*) FROM document_sections WHERE document_id = $1`, doc),
		"facts":                 g.n(`SELECT count(*) FROM statement_values WHERE predicate = 'source_document' AND value_text = $1`, doc),
		"fact_values":           g.n(`SELECT count(*) FROM statement_values WHERE value_text LIKE $1`, "%"+secret+"%"),
		"knowledge_extractions": g.n(`SELECT count(*) FROM knowledge_extractions WHERE document_id = $1`, doc),
		"evidence":              g.n(`SELECT count(*) FROM evidence WHERE source_key = $1`, doc),
		"documents_legacy":      g.n(`SELECT count(*) FROM documents_legacy WHERE id LIKE $1 OR metadata->>'document_id' = $2`, doc+"%", doc),
		// ADR-0137 R4 row 17: agent profiles are performance data, never ingested
		// content (out of the cascade); none may name the erased document or its text.
		"profiles": g.n(`SELECT count(*) FROM chunks WHERE document_type = $1 AND (text LIKE $2 OR text LIKE $3)`,
			domain.DocTypeAgentProfile, "%"+secret+"%", "%"+doc+"%"),
	}
}

// probeLanes lists the lanes leaks probes; a positive control asserts each surfaces the
// document before an erase, so "nothing after" is not an artefact of a dead probe.
var probeLanes = []string{"dense", "fts", "document_body", "document_text", "get_by_id", "get_batch", "query_by_metadata",
	"kg_expansion", "entity_seeding", "pagerank", "neighbours", "fact_lane", "fact_inspect", "recall_pipeline"}

// leaks probes every principal-serving read lane for doc's content as reader and returns
// what each lane still surfaces (empty = nothing leaked).
func (g *gate) leaks(reader context.Context, doc, stub string, chunks []string, secret, entity string) map[string]int {
	g.t.Helper()
	pred, _ := domain.ScopeFromContext(reader)
	mine := func(d domain.Document) bool {
		return d.ID == doc || slices.Contains(chunks, d.ID) || strings.Contains(d.Text, secret) ||
			fmt.Sprint(d.Metadata["document_id"]) == doc
	}
	out := map[string]int{}
	add := func(lane string, n int) {
		if n > 0 {
			out[lane] += n
		}
	}
	query := secret + " " + entity + " ledger reconciliation"
	qv, _ := g.emb.Embed(g.ctx, query)

	// Dense lane through the tier-1 read chokepoint.
	dense, err := g.mem.Reader().Search(reader, qv, domain.SearchOptions{TopK: 100, Scope: pred})
	if err != nil {
		g.t.Fatalf("dense: %v", err)
	}
	for _, r := range dense {
		if mine(r.Document) {
			add("dense", 1)
		}
	}
	// FTS lane (wired to the adapter with the scope pushed into SQL, app.go).
	lex, err := g.vec.LexicalSearch(reader, secret, domain.SearchOptions{TopK: 100, Scope: pred})
	if err != nil {
		g.t.Fatalf("lexical: %v", err)
	}
	for _, r := range lex {
		if mine(r.Document) {
			add("fts", 1)
		}
	}
	// By-id reads. The document lane: operator GetDocument and MCP get_document resolve
	// a document through kernelDocumentReader, i.e. GetByID on the tier-1 reader
	// (app/document_reader.go), by its source-doc entity id; the ADR-0093 body itself is
	// documents.text, which the fact backfill re-reads (ENG-1947: it must not survive an
	// erase of any of the document's chunks).
	for _, id := range append([]string{stub}, chunks...) {
		d, err := g.mem.Reader().GetByID(reader, id)
		if err != nil {
			g.t.Fatalf("GetByID %s: %v", id, err)
		}
		if d != nil {
			if id == stub {
				add("document_body", 1)
			} else {
				add("get_by_id", 1)
			}
		}
	}
	add("document_text", g.n(`SELECT count(*) FROM documents WHERE id = $1 OR text LIKE $2`, doc, "%"+secret+"%"))
	batch, err := g.mem.Reader().GetBatch(reader, chunks)
	if err != nil {
		g.t.Fatalf("GetBatch: %v", err)
	}
	add("get_batch", len(batch))
	byMeta, err := g.mem.Reader().QueryByMetadata(reader, map[string]string{"document_id": doc}, 0)
	if err != nil {
		g.t.Fatalf("QueryByMetadata: %v", err)
	}
	add("query_by_metadata", len(byMeta))
	// KG expansion (the chunks' triplets), entity seeding (chunks mentioning the entity),
	// PageRank, neighbour/anchor structure (edges around the chunks).
	trip, err := g.vec.ForChunks(reader, chunks)
	if err != nil {
		g.t.Fatalf("ForChunks: %v", err)
	}
	for _, ts := range trip {
		add("kg_expansion", len(ts))
	}
	seeded, err := g.vec.ChunksMentioningEntity(reader, entity, 50, pred)
	if err != nil {
		g.t.Fatalf("ChunksMentioningEntity: %v", err)
	}
	add("entity_seeding", len(seeded))
	pr, err := g.vec.ChunkPageRanks(reader, chunks)
	if err != nil {
		g.t.Fatalf("ChunkPageRanks: %v", err)
	}
	add("pagerank", len(pr))
	edges, err := g.vec.GetAdjacentEdges(reader, chunks)
	if err != nil {
		g.t.Fatalf("GetAdjacentEdges: %v", err)
	}
	add("neighbours", len(edges))
	// Fact lane (the enforcing reader the QueryService holds) and fact inspection.
	fv, _ := g.emb.Embed(g.ctx, entity+" audits_account "+secret)
	hits, err := g.mem.FactReader.SearchFacts(reader, fv, domain.FactSearchOptions{TopK: 50, Scope: pred})
	if err != nil {
		g.t.Fatalf("SearchFacts: %v", err)
	}
	for _, h := range hits {
		if h.Fact.SourceDocumentID == doc || strings.Contains(h.Fact.Value, secret) {
			add("fact_lane", 1)
		}
	}
	listed, _, err := g.mem.FactInspector.ListFacts(reader, domain.FactListFilter{Subject: entity, Limit: 50}, pred)
	if err != nil {
		g.t.Fatalf("ListFacts: %v", err)
	}
	add("fact_inspect", len(listed))
	// The whole recall pipeline as an agent asks it (dense + FTS fused, KG²RAG with
	// entity seeding and anchors, spreading, neighbour window, fact lane, blend).
	res, err := g.mem.QueryService.Search(reader, query, domain.PrincipalFromContext(reader).ID)
	if err != nil {
		g.t.Fatalf("QueryService.Search: %v", err)
	}
	for _, r := range res {
		if mine(r.Document) {
			add("recall_pipeline", 1)
		}
	}
	return out
}

// TestPg_Forget_AllLanesReturnNothing is gate TestForget_AllLanesReturnNothing: a
// multi-chunk document with every derivative is visible on every lane; the agent that
// wrote it previews ONE chunk (ENG-1947: the preview names the whole document), erases
// it with the token, and then no lane — dense, FTS, by-id, document body, KG expansion,
// entity seeding, PageRank, neighbours, the fact lane, the recall pipeline — surfaces any
// of it, for the writer or for the broadest reader. An unrelated document is untouched.
func TestPg_Forget_AllLanesReturnNothing(t *testing.T) {
	g := newGate(t)
	alice := agentGateCtx("eng411d-alice")
	secret, entity := g.tok("secret"), g.tok("entity")
	doc, chunks := g.ingest(alice, "eng411d-alice", "secret", paragraphs(secret, 6))
	if len(chunks) < 2 {
		t.Fatalf("need a multi-chunk document, got %d chunk(s)", len(chunks))
	}
	ctrlSecret, ctrlEntity := g.tok("control"), g.tok("ctrlentity")
	ctrl, ctrlChunks := g.ingest(alice, "eng411d-alice", "control", paragraphs(ctrlSecret, 2))
	g.seedDerivatives(doc, chunks, entity, secret, ctrlChunks[0])
	g.seedDerivatives(ctrl, ctrlChunks, ctrlEntity, ctrlSecret, "")
	stub, ctrlStub := g.stub(doc), g.stub(ctrl)

	before := g.leaks(alice, doc, stub, chunks, secret, entity)
	for _, lane := range probeLanes {
		if before[lane] == 0 {
			t.Errorf("positive control: lane %s does not surface the document before the erase (%v)", lane, before)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	p, err := g.svc.Preview(alice, domain.ErasureSelector{ChunkIDs: []string{chunks[1]}})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Kind != domain.ErasureTargetDocument || p.Candidates[0].ID != doc ||
		p.Candidates[0].Via != chunks[1] || !slices.Equal(p.Selector.DocumentIDs, []string{doc}) {
		t.Fatalf("preview of one chunk = %+v; want the whole document %s (ENG-1947)", p, doc)
	}
	rec, err := g.svc.Forget(alice, forget.Request{PreviewToken: p.Token})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if rec.Counts["documents"] != 1 || rec.Counts["chunks"] < len(chunks) {
		t.Fatalf("receipt counts = %v", rec.Counts)
	}
	for _, reader := range []context.Context{alice, domain.WithScope(alice, domain.ScopeMaintenance)} {
		if got := g.leaks(reader, doc, stub, chunks, secret, entity); len(got) != 0 {
			t.Errorf("after Forget the erased document still surfaces: %v", got)
		}
	}
	if got := g.leaks(alice, ctrl, ctrlStub, ctrlChunks, ctrlSecret, ctrlEntity); got["document_body"] != 1 || got["document_text"] != 1 ||
		got["get_by_id"] != len(ctrlChunks) || got["fact_lane"] == 0 || got["pagerank"] != len(ctrlChunks) {
		t.Errorf("the unrelated document lost lanes: %v", got)
	}
}

// TestPg_Forget_ScopeDenied is gate TestForget_ScopeDenied: an agent cannot erase another
// agent's document (by document or by chunk) that it can read, sees a document
// owner-fenced to someone else as absent, gets ErrScopeMissing with no predicate and a
// plain NothingToErase for an id that matches nothing (denial ≠ nothing matched); an
// operator erases both foreign documents (the owner fence lifted, 2026-11-16).
func TestPg_Forget_ScopeDenied(t *testing.T) {
	g := newGate(t)
	alice, bob := agentGateCtx("eng411d-alice"), agentGateCtx("eng411d-bob")
	mine, _ := g.ingest(alice, "eng411d-alice", "mine", paragraphs(g.tok("mine"), 1))
	bobs, bobChunks := g.ingest(bob, "eng411d-bob", "bobs", paragraphs(g.tok("bobs"), 2))
	fenced, _ := g.ingest(alice, "eng411d-alice", "fenced", paragraphs(g.tok("fenced"), 1))
	g.exec(`UPDATE documents SET tags = '{owner:eng411d-carol}' WHERE id = $1`, fenced)
	g.exec(`UPDATE chunks SET metadata = jsonb_set(metadata, '{tags}', '["owner:eng411d-carol"]'::jsonb) WHERE document_id = $1`, fenced)
	docs := func(id string) int { return g.n(`SELECT count(*) FROM documents WHERE id = $1`, id) }

	if d, _ := g.mem.Reader().GetByID(alice, bobChunks[0]); d == nil {
		t.Fatal("precondition: alice can read bob's document")
	}
	for _, sel := range []domain.ErasureSelector{{DocumentIDs: []string{bobs}}, {ChunkIDs: []string{bobChunks[0]}}} {
		if _, err := g.svc.Forget(alice, forget.Request{Selector: sel}); !errors.Is(err, domain.ErrEraseDenied) {
			t.Fatalf("alice erasing bob's %+v: want ErrEraseDenied, got %v", sel, err)
		}
	}
	if _, err := g.svc.Preview(alice, domain.ErasureSelector{DocumentIDs: []string{fenced}}); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("alice previewing a document fenced to carol: want ErrNothingToErase (absent), got %v", err)
	}
	noScope := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("eng411d-alice"))
	if _, err := g.svc.Forget(noScope, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{mine}}}); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("unset scope: want ErrScopeMissing, got %v", err)
	}
	_, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{"eng411d-none-" + g.stamp}}})
	if !errors.Is(err, domain.ErrNothingToErase) || errors.Is(err, domain.ErrEraseDenied) {
		t.Fatalf("nothing matched: want ErrNothingToErase (not denied), got %v", err)
	}
	if docs(bobs) != 1 || docs(fenced) != 1 || docs(mine) != 1 {
		t.Fatal("a denied or unscoped erase removed rows")
	}
	if _, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{mine}}}); err != nil || docs(mine) != 0 {
		t.Fatalf("alice erasing her own document: %v (rows %d)", err, docs(mine))
	}
	op := operatorGateCtx()
	rec, err := g.svc.Forget(op, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{bobs, fenced}}, Reason: domain.ErasureReasonCompliance})
	if err != nil || docs(bobs) != 0 || docs(fenced) != 0 || rec.PrincipalKind != domain.ErasureByOperator {
		t.Fatalf("operator compliance erase: %+v %v (rows %d, %d)", rec, err, docs(bobs), docs(fenced))
	}
}

// TestPg_Forget_Idempotent is gate TestForget_Idempotent: the same request twice yields
// one receipt row, the same request id and counts, Replayed on the second; another
// principal asking for the already-erased ids gets NothingToErase naming the request.
func TestPg_Forget_Idempotent(t *testing.T) {
	g := newGate(t)
	alice := agentGateCtx("eng411d-alice")
	doc, chunks := g.ingest(alice, "eng411d-alice", "idem", paragraphs(g.tok("idem"), 2))
	g.seedDerivatives(doc, chunks, g.tok("idement"), g.tok("idem"), "")
	req := forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}}
	first, err := g.svc.Forget(alice, req)
	if err != nil || first.Replayed {
		t.Fatalf("first Forget: %+v %v", first, err)
	}
	second, err := g.svc.Forget(alice, req)
	if err != nil || !second.Replayed || second.RequestID != first.RequestID || second.Targets != first.Targets ||
		!second.ErasedAt.Equal(first.ErasedAt) || !maps.Equal(second.Counts, first.Counts) {
		t.Fatalf("second Forget = %+v, %v; want the first receipt replayed (%+v)", second, err, first)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasures WHERE request_id = $1`, first.RequestID); n != 1 {
		t.Fatalf("%d receipt rows for one request", n)
	}
	// A different principal's request for the same, already-erased ids is a different
	// request: nothing to erase (the chokepoint resolves nothing it could admit).
	if _, err := g.svc.Forget(operatorGateCtx(), req); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("operator asking again: want ErrNothingToErase, got %v", err)
	}
}

// TestPg_Forget_DerivativesGone is gate TestForget_DerivativesGone: after the erase no
// chunk, embedding, chunk_triplets, chunk_pagerank, document_edges, section, fact item,
// fact value, extraction marker, evidence, documents_legacy or profile row of the source
// remains, the record left behind is content-free, and an unrelated document keeps all
// of its own rows.
func TestPg_Forget_DerivativesGone(t *testing.T) {
	g := newGate(t)
	alice := agentGateCtx("eng411d-alice")
	secret := g.tok("deriv")
	doc, chunks := g.ingest(alice, "eng411d-alice", "deriv", paragraphs(secret, 3))
	ctrlSecret := g.tok("keep")
	ctrl, ctrlChunks := g.ingest(alice, "eng411d-alice", "keep", paragraphs(ctrlSecret, 2))
	g.seedDerivatives(doc, chunks, g.tok("derivent"), secret, ctrlChunks[0])
	g.seedDerivatives(ctrl, ctrlChunks, g.tok("keepent"), ctrlSecret, "")
	before := g.derivatives(doc, chunks, secret)
	for k, v := range before {
		// profiles: none by design (R4 row 17). chunk_embeddings: the ADR-0107 projection
		// is written only for a configured embedding model id, which this stack has not.
		if v == 0 && k != "profiles" && k != "chunk_embeddings" {
			t.Fatalf("seed: %s = 0 (%v)", k, before)
		}
	}
	ctrlBefore := g.derivatives(ctrl, ctrlChunks, ctrlSecret)
	// The one Hebbian edge from the unrelated document INTO the erased chunk goes with it
	// (an edge touching erased content); everything else of it stays.
	ctrlBefore["document_edges"]--

	rec, err := g.svc.Forget(operatorGateCtx(), forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}},
		Reason: domain.ErasureReasonCompliance})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	for k, v := range g.derivatives(doc, chunks, secret) {
		if v != 0 {
			t.Errorf("after erase: %s = %d", k, v)
		}
	}
	for _, store := range []string{"documents", "chunks", "chunk_triplets", "chunk_pagerank", "document_edges",
		"document_sections", "knowledge_items", "knowledge_extractions", "evidence", "evidence_outbox", "documents_legacy"} {
		if rec.Counts[store] == 0 {
			t.Errorf("receipt counts no %s (%v)", store, rec.Counts)
		}
	}
	if !maps.Equal(g.derivatives(ctrl, ctrlChunks, ctrlSecret), ctrlBefore) {
		t.Errorf("unrelated document changed: %v -> %v", ctrlBefore, g.derivatives(ctrl, ctrlChunks, ctrlSecret))
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(row_to_json(e)::text, ''), '') FROM memory_erasures e WHERE request_id = $1`,
		`SELECT coalesce(string_agg(row_to_json(t)::text, ''), '') FROM memory_erasure_targets t WHERE request_id = $1`,
		`SELECT coalesce(string_agg(row_to_json(o)::text, ''), '') FROM memory_erasure_outbox o WHERE request_id = $1`,
	} {
		var s string
		if err := g.vec.Pool().QueryRow(g.ctx, q, rec.RequestID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, secret) || strings.Contains(s, "reconciliation") {
			t.Errorf("the erasure record holds content: %s", s)
		}
	}
}

// gatedExtractor emits one deterministic triplet per chunk but holds each batch until
// released, so a triplet batch can be in flight across an erase.
type gatedExtractor struct {
	entity  string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *gatedExtractor) ExtractBatch(ctx context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]domain.ChunkTriplet, len(ids))
	for i := range ids {
		out[i] = []domain.ChunkTriplet{{H: e.entity, R: "audits", T: "resurrected", Weight: 1}}
	}
	return out, nil
}

// TestPg_Forget_NoResurrection is gate TestForget_NoResurrection: a document is erased
// while work that predates the erase is in flight for it — a KG triplet batch (the real
// ChunkTripletsBatcher, held by a gated fake extractor), a fact extraction (the fact
// extractor's enforcing writer), and a queued ingest / journal replay (ProcessSync under
// the original ingest origin). When the work lands nothing reappears; the batcher's
// goroutine is gone after Stop.
func TestPg_Forget_NoResurrection(t *testing.T) {
	g := newGate(t)
	leakOpts := goleak.IgnoreCurrent()
	alice := agentGateCtx("eng411d-alice")
	secret, entity := g.tok("resur"), g.tok("resurent")
	body := paragraphs(secret, 3)
	origin := time.Now().UTC()
	doc, chunks := g.ingest(domain.WithIngestOrigin(alice, origin), "eng411d-alice", "resur", body)
	stub := g.stub(doc)
	inFlight := domain.WithIngestOrigin(alice, origin)

	ext := &gatedExtractor{entity: entity, started: make(chan struct{}), release: make(chan struct{})}
	batcher := memory.NewChunkTripletsBatcher(nil, g.vec, memory.ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 64, MaxIdle: 10 * time.Millisecond, LLMTimeout: 30 * time.Second})
	batcher.UseExtractor(ext)
	batcher.Start(context.Background())
	for _, c := range chunks {
		batcher.Enqueue(inFlight, &domain.Document{ID: c, Text: secret})
	}
	select {
	case <-ext.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the triplet batch never started")
	}

	if _, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// A fact extraction job of the erased document lands after the erase.
	item := gateFact(entity, "audits_account", secret, chunks[0], doc, origin)
	if _, _, err := g.mem.FactWriter.PutClassifiedItem(inFlight, item); !errors.Is(err, domain.ErrSourceErased) {
		t.Errorf("in-flight fact write after the erase: want ErrSourceErased, got %v", err)
	}
	// A queued ingest / reactive journal replay of the same document carries its
	// original origin (ADR-0137 D5).
	_, err := g.mem.IngestionManager.ProcessSync(inFlight, domain.ExternalDocument{
		SourceURI: g.uri("resur"), SourceType: "file_drop", Title: "resur", Body: body, Author: "eng411d-alice", Timestamp: origin})
	if !errors.Is(err, domain.ErrSourceErased) {
		t.Errorf("replayed ingest after the erase: want ErrSourceErased, got %v", err)
	}
	// The held triplet batch lands now.
	close(ext.release)
	batcher.Stop()

	for k, v := range g.derivatives(doc, chunks, secret) {
		if v != 0 {
			t.Errorf("resurrected after the erase: %s = %d", k, v)
		}
	}
	if n := g.n(`SELECT count(*) FROM chunk_triplets WHERE h = $1`, entity); n != 0 {
		t.Errorf("the in-flight triplet batch wrote %d rows for erased chunks", n)
	}
	if got := g.leaks(alice, doc, stub, chunks, secret, entity); len(got) != 0 {
		t.Errorf("a lane surfaces resurrected content: %v", got)
	}
	goleak.VerifyNone(t, leakOpts)
}

// TestPg_Forget_HebbianWriteCannotResurrectErasedChunks (ENG-1951, ADR-0137 D5): the
// Hebbian reinforcement writes co_activated edges async, after the recall returned, so
// its write can land across an erase of a chunk it names. document_edges has no FK;
// the write goes through GraphStore.SaveChunkEdges, which writes only edges between
// live chunks under a FOR KEY SHARE lock, and the erase cascade deletes edges AFTER the
// chunk rows. Three interleavings: a write that lands before the erase is erased with
// it; a write that holds its lock while the erase runs is waited for and then erased
// (with edges deleted before chunks — the old order — this edge survived); a write
// that lands after the erase is skipped.
func TestPg_Forget_HebbianWriteCannotResurrectErasedChunks(t *testing.T) {
	g := newGate(t)
	alice := agentGateCtx("eng411d-alice")
	doc, chunks := g.ingest(alice, "eng411d-alice", "hebb", paragraphs(g.tok("hebb"), 3))
	_, keep := g.ingest(alice, "eng411d-alice", "hebbkeep", paragraphs(g.tok("hebbkeep"), 2))
	if len(chunks) < 3 || len(keep) < 2 {
		t.Fatalf("need >= 3 erased and >= 2 kept chunks, got %d and %d", len(chunks), len(keep))
	}
	pair := func(a, b string) []domain.DocumentEdge {
		return []domain.DocumentEdge{
			{SourceID: a, TargetID: b, EdgeType: domain.EdgeCoActivated, Weight: 0.3},
			{SourceID: b, TargetID: a, EdgeType: domain.EdgeCoActivated, Weight: 0.3},
		}
	}
	touching := func() int {
		return g.n(`SELECT count(*) FROM document_edges WHERE source_id = ANY($1) OR target_id = ANY($1)`, chunks)
	}

	// 1. Before the erase: written.
	if n, err := g.vec.SaveChunkEdges(g.ctx, pair(chunks[0], keep[0])); err != nil || n != 2 {
		t.Fatalf("SaveChunkEdges between live chunks = %d, %v; want 2", n, err)
	}

	// 2. A write in flight across the erase: hold the endpoint lock and the edge
	//    uncommitted (what SaveChunkEdges does between its lock and its commit) while
	//    the erase runs.
	tx, err := g.vec.Pool().Begin(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(g.ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(g.ctx, `SELECT id FROM chunks WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`,
		[]string{chunks[1], keep[1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(g.ctx, `INSERT INTO document_edges (source_id, target_id, edge_type, weight)
		VALUES ($1, $2, 'co_activated', 0.3)`, chunks[1], keep[1]); err != nil {
		t.Fatal(err)
	}
	erased := make(chan error, 1)
	go func() {
		_, err := g.svc.Forget(operatorGateCtx(), forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}},
			Reason: domain.ErasureReasonCompliance})
		erased <- err
	}()
	// The erase must reach the chunk DELETE and wait on the writer's lock.
	deadline := time.Now().Add(15 * time.Second)
	for g.n(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
		AND wait_event_type = 'Lock' AND query LIKE 'DELETE FROM chunks%'`) == 0 {
		select {
		case err := <-erased:
			t.Fatalf("the erase finished while a writer held its endpoint lock (err %v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the erase never waited on the writer's chunk lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(g.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-erased:
		if err != nil {
			t.Fatalf("Forget: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the erase never finished")
	}
	if n := touching(); n != 0 {
		t.Fatalf("%d edge(s) still name an erased chunk (the in-flight write survived the erase)", n)
	}

	// 3. After the erase: skipped, nothing written; an edge between live chunks still is.
	if n, err := g.vec.SaveChunkEdges(g.ctx, append(pair(chunks[2], keep[0]), pair(keep[0], keep[1])...)); err != nil || n != 2 {
		t.Fatalf("SaveChunkEdges after the erase = %d, %v; want only the 2 live-live edges", n, err)
	}
	if n := touching(); n != 0 {
		t.Fatalf("a Hebbian write after the erase re-created %d edge(s) to erased chunks", n)
	}

	// And the real recall path: recalls with Hebbian on after the erase never name an
	// erased chunk, however their async writes are scheduled.
	for i := 0; i < 3; i++ {
		if _, err := g.mem.QueryService.Search(alice, g.tok("hebbkeep"), "eng411d-alice"); err != nil {
			t.Fatalf("Search: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		if n := touching(); n != 0 {
			t.Fatalf("a recall's Hebbian write named an erased chunk (%d edges)", n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestPg_Forget_ReingestIsNewAct is gate TestForget_ReingestIsNewAct (ADR-0137 Q4): after
// an erase a genuinely new ingest of the same content is allowed, while a replay under
// the earlier origin is refused.
func TestPg_Forget_ReingestIsNewAct(t *testing.T) {
	g := newGate(t)
	alice := agentGateCtx("eng411d-alice")
	body := paragraphs(g.tok("again"), 1)
	origin := time.Now().UTC()
	doc, _ := g.ingest(domain.WithIngestOrigin(alice, origin), "eng411d-alice", "again", body)
	if _, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	_, err := g.mem.IngestionManager.ProcessSync(domain.WithIngestOrigin(alice, origin), domain.ExternalDocument{
		SourceURI: g.uri("again"), SourceType: "file_drop", Title: "again", Body: body, Author: "eng411d-alice", Timestamp: origin})
	if !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("replay with the earlier origin: want ErrSourceErased, got %v", err)
	}
	again, chunks := g.ingest(alice, "eng411d-alice", "again", body)
	if again != doc || len(chunks) == 0 || g.n(`SELECT count(*) FROM documents WHERE id = $1`, doc) != 1 {
		t.Fatalf("a new ingest after the erase: doc %s (was %s), %d chunks", again, doc, len(chunks))
	}
}

// TestPg_Forget_SupersessionIsNotErasure is gate TestForget_SupersessionIsNotErasure:
// a newer fact supersedes an older one without deleting anything (both rows stay, the
// older is closed and answerable as history, no erasure record exists); erasing the
// newer fact's document deletes its rows outright (not merely closes them) and the older
// value stays closed with an erased gap (ADR-0137 Q5), never current again.
func TestPg_Forget_SupersessionIsNotErasure(t *testing.T) {
	g := newGate(t)
	if _, err := g.ks.SeedDeclaredProfiles(g.ctx, gateNS, domain.KindMemoryFact, map[string]string{"home_city": domain.CardinalitySingle}, 3, "test"); err != nil {
		t.Fatal(err)
	}
	alice := agentGateCtx("eng411d-alice")
	subject := g.tok("person")
	docA, chunksA := g.ingest(alice, "eng411d-alice", "paris", "We moved. "+subject+" lives in Paris now.")
	docB, chunksB := g.ingest(alice, "eng411d-alice", "london", "Update: "+subject+" moved to London.")
	paris := g.fact(subject, "home_city", "Paris", chunksA[0], docA, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	london := g.fact(subject, "home_city", "London", chunksB[0], docB, time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC))
	items := func(id domain.KnowledgeItemID) int {
		return g.n(`SELECT count(*) FROM knowledge_items WHERE id = $1`, string(id))
	}
	chain := func() map[string]domain.Fact {
		t.Helper()
		c, err := g.facts.FactChain(g.ctx, domain.FactFromItem(paris), &domain.TagPredicate{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]domain.Fact{}
		for _, f := range c {
			out[f.Value] = f
		}
		return out
	}

	// Supersession: nothing deleted, history kept, no erasure record.
	c := chain()
	if items(paris.ID) != 1 || items(london.ID) != 1 || c["Paris"].Current || c["Paris"].SupersededBy != string(london.ID) || !c["London"].Current {
		t.Fatalf("supersession: paris rows %d london rows %d chain %+v", items(paris.ID), items(london.ID), c)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_targets WHERE document_id = ANY($1)`, []string{docA, docB}); n != 0 {
		t.Fatalf("supersession left %d erasure tombstones", n)
	}

	// Erasure: the newer fact's rows are gone, not closed; the older stays closed.
	if _, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{docB}}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if items(london.ID) != 0 || g.n(`SELECT count(*) FROM statement_values WHERE item_id = $1`, string(london.ID)) != 0 {
		t.Fatal("erasure merely invalidated the newer fact: its item rows remain")
	}
	c = chain()
	if _, ok := c["London"]; ok {
		t.Fatalf("the erased value is still answerable: %+v", c)
	}
	if p := c["Paris"]; items(paris.ID) != 1 || p.Current || p.ValidTo == nil || !p.ValidTo.Equal(time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the older value re-opened or vanished after the erase: %+v", p)
	}
}

// TestPg_Forget_QueryPreview is the ENG-1948 end-to-end test (contract 0106): the query
// form of PreviewForget through the real agent-plane handler, resolved by the caller's
// own recall (the composed QueryService — dense + hybrid FTS + the lanes newGate turns
// on) on a real Postgres:
//   - the search sees alice's document, bob's document (readable to alice) and not the
//     document owner-fenced to carol; the preview lists ONLY alice's document, once, as
//     the whole document, with a clipped snippet of alice's own text;
//   - the operator plane's preview is ids-only by type (no snippet field, no query);
//   - Forget with the token erases exactly the previewed document: bob's, carol's, the
//     control and a document alice wrote after the preview all survive;
//   - erasure disabled answers Unimplemented for the query form.
//
// ENG-1953 (contract 0107) adds the relevance floor and the candidate cap:
//   - the control document alice wrote shares the boilerplate but not the query term;
//     recall returns it (top-k, retrieval floor 0) with no cosine to the query, so the
//     preview's floor excludes it: it is neither listed nor erased;
//   - the floor is on the hit cosine (RawScore). The default (0.60) is on the scale of
//     the production embedder (bge-large); this test's bag-of-words hashEmbedder puts a
//     one-term query at cosine ~0.02 to a long matching chunk, so at the DEFAULT floor
//     nothing qualifies (NotFound, never a fallback to the unfloored list) and the
//     rest of the test sets a floor on the hash embedder's own scale (hashFloor);
//   - a preview cut by the candidate cap says truncated.
func TestPg_Forget_QueryPreview(t *testing.T) {
	g := newGate(t)
	const snippetChars = 60
	// hashFloor is a cosine floor on hashEmbedder's scale: above 0 (a row that shares
	// no token with the query has cosine 0) and below a one-term query's cosine to
	// the long matching chunks (~0.02).
	const hashFloor = 0.01
	svc := forget.New(g.mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "gate",
		Searcher: g.mem.QueryService, SnippetChars: snippetChars, MinCosine: hashFloor})
	agentSrv := &subnetwork.Server{Forgetter: svc}
	aliceRPC := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "eng411d-alice"))
	alice, bob := agentGateCtx("eng411d-alice"), agentGateCtx("eng411d-bob")
	// The query term must not share a hash bucket with any token of the control text,
	// or the control would have a (spurious) cosine to it under hashEmbedder; the
	// term is chosen deterministically among a few candidates so it shares none.
	ctrlText := paragraphs(g.tok("unrelated"), 1)
	secret := ""
	for i := 0; i < 32 && secret == ""; i++ {
		cand := g.tok(fmt.Sprintf("salary%d", i))
		if hashCosine(g.emb, cand, ctrlText) == 0 {
			secret = cand
		}
	}
	if secret == "" {
		t.Fatal("no query term without a hash collision against the control text")
	}

	mine, mineChunks := g.ingest(alice, "eng411d-alice", "salary", paragraphs(secret, 4))
	if len(mineChunks) < 2 {
		t.Fatalf("need a multi-chunk document, got %d chunk(s)", len(mineChunks))
	}
	bobs, _ := g.ingest(bob, "eng411d-bob", "bobsalary", "Bob private note. The "+secret+" bonus figures for the quarter.")
	fenced, _ := g.ingest(alice, "eng411d-alice", "carolsalary", "Carol fenced note. The "+secret+" figures are private to Carol.")
	g.exec(`UPDATE documents SET tags = '{owner:eng411d-carol}' WHERE id = $1`, fenced)
	g.exec(`UPDATE chunks SET metadata = jsonb_set(metadata, '{tags}', '["owner:eng411d-carol"]'::jsonb) WHERE document_id = $1`, fenced)
	ctrl, _ := g.ingest(alice, "eng411d-alice", "unrelated", ctrlText)
	docs := func(id string) int { return g.n(`SELECT count(*) FROM documents WHERE id = $1`, id) }
	docOf := func(chunk string) string {
		var d string
		_ = g.vec.Pool().QueryRow(g.ctx, `SELECT COALESCE(document_id, '') FROM chunks WHERE id = $1`, chunk).Scan(&d)
		return d
	}

	// Precondition: alice's own search reaches her document, bob's and (the recall
	// floor is 0) the boilerplate-sharing control, never carol's. The control's rows
	// have no cosine to the query.
	hits, err := g.mem.QueryService.SearchMatches(alice, secret, "eng411d-alice")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		d := docOf(h.Document.ID)
		seen[d] = true
		if d == ctrl && h.RawScore > 0 {
			t.Fatalf("control hit %s has cosine %g to the query; want none", h.Document.ID, h.RawScore)
		}
	}
	if !seen[mine] || !seen[bobs] || seen[fenced] {
		t.Fatalf("precondition: alice's search saw mine=%t bobs=%t fenced=%t", seen[mine], seen[bobs], seen[fenced])
	}
	// The control is checked against this test's own documents, not the whole database
	// (ENG-2181): its rows have no cosine to the query, so in a shared PG_TEST_DSN they
	// tie with every unrelated row and need not make the top k of the secret search.
	// What the preview below relies on is that alice CAN recall it — a search for its
	// own token reaches it — while it has no cosine to the secret (above, and the
	// hashCosine choice of the secret).
	ctrlHits, err := g.mem.QueryService.SearchMatches(alice, g.tok("unrelated"), "eng411d-alice")
	if err != nil {
		t.Fatalf("Search (control): %v", err)
	}
	ctrlSeen := false
	for _, h := range ctrlHits {
		if docOf(h.Document.ID) == ctrl {
			ctrlSeen = true
		}
	}
	if !ctrlSeen {
		t.Fatal("precondition: alice cannot recall the control document by its own token")
	}

	// At the default floor (0.60, the production embedder's scale) no hash-embedder hit
	// qualifies: NotFound, never the unfloored list.
	defSrv := &subnetwork.Server{Forgetter: forget.New(g.mem.Eraser(), forget.Config{PolicyFingerprint: "gate", Searcher: g.mem.QueryService})}
	if _, err := defSrv.PreviewForget(aliceRPC, &pb.PreviewForgetRequest{Query: secret}); status.Code(err) != codes.NotFound {
		t.Fatalf("default floor on hash-embedder cosines: %v, want NotFound", err)
	}

	p, err := agentSrv.PreviewForget(aliceRPC, &pb.PreviewForgetRequest{Query: secret})
	if err != nil {
		t.Fatalf("PreviewForget(query): %v", err)
	}
	// What must hold: alice's matching document is the ONLY candidate, as one
	// whole-document candidate. The control (recalled, but with no cosine to the
	// query) is below the floor; bob's and carol's never appear; not truncated.
	cands := p.GetCandidates()
	if len(cands) != 1 || p.GetTruncated() {
		t.Fatalf("query preview = %+v truncated=%t; want exactly alice's matching document", cands, p.GetTruncated())
	}
	if len(cands) == 0 || cands[0].GetKind() != domain.ErasureTargetDocument || cands[0].GetId() != mine ||
		cands[0].GetDocumentId() != mine || !slices.Contains(mineChunks, cands[0].GetVia()) {
		t.Fatalf("query preview = %+v; want alice's whole document %s first", cands, mine)
	}
	listed := map[string]bool{}
	for _, c := range cands {
		id := c.GetId()
		if listed[id] {
			t.Fatalf("candidate %s listed twice (hits must be deduplicated per document)", id)
		}
		listed[id] = true
		if id == bobs || id == fenced || c.GetKind() != domain.ErasureTargetDocument ||
			g.n(`SELECT count(*) FROM documents WHERE id = $1 AND written_by = 'eng411d-alice'`, id) != 1 {
			t.Fatalf("candidate %+v is not a whole document alice wrote", c)
		}
		sn := c.GetSnippet()
		if sn == "" || len([]rune(sn)) > snippetChars || strings.Contains(sn, "Bob") || strings.Contains(sn, "Carol") {
			t.Fatalf("snippet = %q; want a non-empty excerpt of alice's own text, at most %d chars", sn, snippetChars)
		}
		var viaText string
		if err := g.vec.Pool().QueryRow(g.ctx, `SELECT text FROM chunks WHERE id = $1 AND document_id = $2`, c.GetVia(), id).Scan(&viaText); err != nil {
			t.Fatalf("hit chunk %s of %s: %v", c.GetVia(), id, err)
		}
		if !strings.HasPrefix(strings.Join(strings.Fields(viaText), " "), strings.TrimSuffix(sn, "…")) {
			t.Fatalf("snippet %q is not an excerpt of the hit chunk %s", sn, c.GetVia())
		}
	}
	if docs(mine) != 1 {
		t.Fatal("a preview must erase nothing")
	}

	// The operator plane: ids/counts only, by type — no snippet, no query form.
	if (&pb.ErasureCandidateOp{}).ProtoReflect().Descriptor().Fields().ByName("snippet") != nil ||
		(&pb.PreviewForgetMemoryOpRequest{}).ProtoReflect().Descriptor().Fields().ByName("query") != nil {
		t.Fatal("operator previews must stay ids/counts only")
	}
	opSvc := operatortest.New(t, nil, func(_ *operator.Deps, o *operator.Options) { o.Forget = svc })
	opCtx := domain.WithPrincipal(domain.WithPrincipalRoles(context.Background(), []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng411d-ops"})
	op, err := opSvc.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{DocumentIds: []string{mine}}})
	if err != nil || len(op.GetCandidates()) != 1 || op.GetCountsByKind()[domain.ErasureTargetDocument] != 1 {
		t.Fatalf("operator preview = %+v, %v", op, err)
	}

	// Written after the preview, matching the query: not part of the token.
	late, _ := g.ingest(alice, "eng411d-alice", "late", "A later note about "+secret+".")

	// The cap: two of alice's documents now match; with a cap of 1 the preview lists
	// the top-ranked one and says truncated (its token is never used).
	capSrv := &subnetwork.Server{Forgetter: forget.New(g.mem.Eraser(), forget.Config{PolicyFingerprint: "gate",
		Searcher: g.mem.QueryService, MinCosine: hashFloor, MaxCandidates: 1})}
	cp, err := capSrv.PreviewForget(aliceRPC, &pb.PreviewForgetRequest{Query: secret})
	if err != nil || len(cp.GetCandidates()) != 1 || !cp.GetTruncated() ||
		(cp.GetCandidates()[0].GetId() != mine && cp.GetCandidates()[0].GetId() != late) {
		t.Fatalf("capped preview = %+v, %v; want one of alice's two matching documents, truncated", cp, err)
	}
	resp, err := agentSrv.Forget(aliceRPC, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatalf("Forget(token): %v", err)
	}
	if r := resp.GetReceipt(); r.GetPrincipal() != "eng411d-alice" || int(r.GetCounts()["documents"]) != len(cands) {
		t.Fatalf("receipt = %+v; want %d documents", r, len(cands))
	}
	for id := range listed {
		if docs(id) != 0 || g.n(`SELECT count(*) FROM chunks WHERE document_id = $1`, id) != 0 {
			t.Fatalf("previewed document %s must be gone, every chunk of it", id)
		}
	}
	for name, id := range map[string]string{"bob's": bobs, "carol's": fenced, "the control": ctrl, "the late": late} {
		if !listed[id] && docs(id) != 1 {
			t.Errorf("%s document was erased; only the previewed set may be", name)
		}
	}
	if docs(late) != 1 || docs(bobs) != 1 || docs(fenced) != 1 || docs(ctrl) != 1 {
		t.Fatal("bob's, carol's, the control and the late document must survive")
	}

	off := &subnetwork.Server{Forgetter: forget.New(nil, forget.Config{Searcher: g.mem.QueryService})}
	if _, err := off.PreviewForget(aliceRPC, &pb.PreviewForgetRequest{Query: secret}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("erasure disabled: %v, want Unimplemented", err)
	}
}
