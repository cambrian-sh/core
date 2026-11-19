package app

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2108 end to end (owner decision 2026-11-18: a short document that answers the
// question verbatim must not lose to boosted generic chunks).
//
// Real path: a migrated Postgres (PG_TEST_DSN), the memory stack NewMemoryStack builds
// at the shipped defaults (hybrid, Stage-A blend, KG²RAG, anchors, structure graph,
// section scoping), the operator ingest binding (operatorMemoryIngestor →
// IngestionManager.ProcessSync → leaves-as-chunks + structure persistence), and the
// operator recall pass (QueryService.SearchSystem, what operator QueryMemory calls).
// Only the docling sidecar and the embedder are stand-ins: the sidecar builds the
// markdown hierarchy the way agents/system/docling_agent/structure.py does (ordinal
// per-document paths "n0.n2", breadcrumbs of ancestor headings), the embedder is a
// deterministic bag of words.
//
// The corpus is the readiness phase-B shape: sectioned SOPs at importance 0.6, four
// revisions of one of them (same headings, so the same ordinal paths), and two
// one-paragraph operator notes. Before the fix, the canary question promoted chunks
// to 1.0 + cosine because "readiness" matched SOP-007's "Recall Readiness" heading —
// and, through the ordinal-path join, the chunks at the same position in every other
// SOP — so the memo that answers it word for word fell out of the top 3.
func TestPg_VerbatimMemoOutranksSectionMatchedChunks(t *testing.T) {
	dsn, dim := pgscratch.Migrated(t)
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	if !cfg.Execution.Retrieval.StructureGraphEnabled || !cfg.Execution.Retrieval.BlendEnabled || !cfg.Execution.Retrieval.HybridSearchEnabled {
		t.Fatal("the shipped defaults must have structure scoping, the blend and hybrid on: that is the regime the defect lives in")
	}
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	mem := kernel.NewMemoryStack(kernel.NewMemoryBackend(vec), nil, e2eHashEmbedder{dim: cfg.Embedder.Dimensions},
		cfg.Execution, nil, cfg.Chunker, nil, kernel.FactSubstrate{}, kernel.RetrievalDeps{})
	mem.ChunkTripletsBatcher.UseExtractor(noTriplets{})
	mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	mem.IngestionManager.SetStructureGraph(markdownSidecar{}, vec.StructureGraphStore())
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mem.Start(runCtx) }()
	t.Cleanup(func() { cancel(); <-done; mem.Shutdown(context.Background()) })

	// A per-run stamp keeps the corpus distinct from anything else in a shared test
	// database (document identity is content-keyed) without changing what is ranked.
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	ingest := operatorMemoryIngestor(mem.IngestionManager)
	opCtx := domain.WithPrincipal(ctx, domain.UserPrincipal("eng2108-op-"+stamp))
	put := func(filename, text string, importance float64, tags ...string) string {
		t.Helper()
		id, err := ingest.Ingest(opCtx, operator.IngestRequest{
			Text: text, Filename: filename, Importance: importance,
			SessionID: "eng2108-" + stamp, Tags: append([]string{"scope-docs"}, tags...),
			EventTime: domain.IngestEventTime{EventTime: time.Now().UTC(), Source: domain.TimeSourceIngestDefault},
		})
		if err != nil {
			t.Fatalf("ingest %s: %v", filename, err)
		}
		return id
	}
	// The revisions carry the identity tags the document importer writes
	// (cambrian-premium cmd/cambrian-apply docTags): the document id, rev<N>, and
	// in-force for the current revision, superseded for every older one (ENG-2158).
	for rev := 1; rev <= 4; rev++ {
		lifecycle := "superseded"
		if rev == 4 {
			lifecycle = "in-force"
		}
		put(fmt.Sprintf("sop-001-rev%d-%s.md", rev, stamp), sop001(rev), 0.6, "sop-001", fmt.Sprintf("rev%d", rev), lifecycle)
	}
	put("sop-002-"+stamp+".md", sop002, 0.6)
	put("sop-007-"+stamp+".md", sop007, 0.6)
	put("larkspur-memo-"+stamp+".txt", "The readiness canary codeword for project Larkspur is ZEBRA-PLUM-13342b.", 0)
	put("antalya-note-"+stamp+".txt", "The Antalya warehouse manager is Selin Kaya. She signs off every dispatch leaving the Antalya site.", 0.6)

	// The structure graph really was built, with per-document ordinal paths that
	// collide across documents — the precondition of the cross-document leak.
	var sections, colliding int
	if err := vec.Pool().QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE section_ltree::text = 'n0.n4')
		FROM document_sections WHERE title <> ''`).Scan(&sections, &colliding); err != nil {
		t.Fatalf("count sections: %v", err)
	}
	if sections < 20 || colliding < 5 {
		t.Fatalf("structure graph: %d sections, %d at n0.n4; want the SOP hierarchy persisted with colliding ordinal paths", sections, colliding)
	}

	rankOf := func(query, want string) (int, []domain.SearchResult) {
		t.Helper()
		res, err := mem.QueryService.SearchSystem(ctx, query)
		if err != nil {
			t.Fatalf("SearchSystem(%q): %v", query, err)
		}
		for i, r := range res {
			if strings.Contains(r.Document.Text, want) {
				return i + 1, res
			}
		}
		return 0, res
	}
	dump := func(res []domain.SearchResult) string {
		var b strings.Builder
		for i, r := range res {
			if i == 8 {
				break
			}
			fmt.Fprintf(&b, "  %d. score=%.3f cos=%.3f %-4s %q\n", i+1, r.Score, r.RawScore, revTag(r), firstLine(r.Document.Text))
		}
		return b.String()
	}

	for _, tc := range []struct{ query, want string }{
		{"What is the readiness canary codeword for project Larkspur?", "ZEBRA-PLUM-13342b"},
		{"Who manages the Antalya warehouse?", "Selin Kaya"},
	} {
		rank, res := rankOf(tc.query, tc.want)
		t.Logf("%q -> answer at rank %d\n%s", tc.query, rank, dump(res))
		if rank == 0 || rank > 3 {
			t.Errorf("%q: the note that answers it verbatim ranks %d (0 = absent); want top 3", tc.query, rank)
		}
	}

	// ENG-2158: superseded revisions rank below their successor. With the demotion
	// switched off the defect reproduces — the same SOP-001 chunk once per revision
	// fills the window — and with the shipped default the in-force copy leads, the
	// older copies leave the top 8, and an old revision asked for by name still
	// comes back.
	antalya := "Who manages the Antalya warehouse?"
	top8 := func() []domain.SearchResult {
		t.Helper()
		res, err := mem.QueryService.SearchSystem(ctx, antalya)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) > 8 {
			res = res[:8]
		}
		return res
	}
	mem.QueryService.EnableSupersededDemotion(nil, 0)
	before := top8()
	t.Logf("demotion OFF %q\n%s", antalya, dump(before))
	if n := supersededCopies(before); n < 3 {
		t.Fatalf("demotion off: %d superseded copies of an in-force chunk in the top 8; the corpus no longer reproduces ENG-2158", n)
	}
	mem.QueryService.EnableSupersededDemotion(cfg.Execution.Retrieval.SupersededTags, cfg.Execution.Retrieval.SupersededRankFactor)
	after := top8()
	t.Logf("demotion ON (shipped default) %q\n%s", antalya, dump(after))
	if n, was := supersededCopies(after), supersededCopies(before); n > 1 || n >= was {
		t.Errorf("%q: %d superseded copies of an in-force chunk in the top 8 (%d with the demotion off); want at most one: the old revisions must not fill the window", antalya, n, was)
	}
	if bad := supersededAboveSuccessor(after); bad != "" {
		t.Errorf("%q: %s", antalya, bad)
	}
	if r := firstSOP001(after); r == nil || revTag(*r) != "rev4" {
		t.Errorf("%q: the first SOP-001 chunk is %v; want the newest revision (rev4)", antalya, revOf(r))
	}
	if rank, _ := rankOf(antalya, "Selin Kaya"); rank == 0 || rank > 3 {
		t.Errorf("%q: the note that answers it ranks %d; want top 3", antalya, rank)
	}
	rev1q := "What changed in SOP-001 revision 1?"
	rev1Rank, rev1Res := rankOf(rev1q, "Revision 1 of the receiving steps")
	t.Logf("%q -> rev1 at rank %d\n%s", rev1q, rev1Rank, dump(rev1Res))
	if rev1Rank == 0 || rev1Rank > 8 {
		t.Errorf("%q: revision 1's own chunk ranks %d (0 = absent); an old revision asked for by name must stay findable (top 8)", rev1q, rev1Rank)
	}

	// Section scoping still works when the query names a section: the named heading's
	// chunks lead, and only from the document that has that heading.
	q := "What does the Recall Readiness section say?"
	res, err := mem.QueryService.SearchSystem(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%q\n%s", q, dump(res))
	// The ranked window only: rows past it are document-completion rows (ENG-430),
	// appended in document order, never ranked.
	if bad := supersededAboveSuccessor(res[:min(8, len(res))]); bad != "" {
		t.Errorf("%q: %s", q, bad)
	}
	if len(res) == 0 || !strings.Contains(res[0].Document.Text, "mock recall") || res[0].Score < 1 {
		t.Errorf("%q: top hit %q (score %.3f); want SOP-007's Recall Readiness chunk promoted (score >= 1)", q, firstLine(textOf(res, 0)), scoreOf(res, 0))
	}
	for i, r := range res {
		if r.Score >= 1 && !strings.Contains(r.Document.Text, "mock recall") {
			t.Errorf("%q: rank %d %q was section-promoted (score %.3f) but is not under the Recall Readiness heading", q, i+1, firstLine(r.Document.Text), r.Score)
		}
	}

	// The adapter itself: a heading match returns chunks of that heading's document only.
	ids, err := vec.ChunksInMatchingSections(ctx, []string{"readiness"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("ChunksInMatchingSections(readiness) found nothing; SOP-007 has that heading")
	}
	docs, err := vec.GetBatch(domain.WithScope(ctx, &domain.TagPredicate{Bypass: true}), ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if !strings.Contains(d.Text, "mock recall") {
			t.Errorf("ChunksInMatchingSections(readiness) returned %q from outside the Recall Readiness section", firstLine(d.Text))
		}
	}
}

// revTag is the rev<N> identity tag of a row, "" for a row without one.
func revTag(r domain.SearchResult) string {
	for _, tg := range authz.DocTags(&r.Document) {
		if strings.HasPrefix(tg, "rev") {
			return tg
		}
	}
	return ""
}

func revOf(r *domain.SearchResult) string {
	if r == nil {
		return "none"
	}
	return revTag(*r)
}

func hasTag(r domain.SearchResult, tag string) bool {
	for _, tg := range authz.DocTags(&r.Document) {
		if tg == tag {
			return true
		}
	}
	return false
}

func firstSOP001(res []domain.SearchResult) *domain.SearchResult {
	for i := range res {
		if hasTag(res[i], "sop-001") {
			return &res[i]
		}
	}
	return nil
}

// supersededCopies counts the rows of a superseded revision whose text is also the
// text of another row in the window: a duplicate that takes a slot from an answer.
func supersededCopies(res []domain.SearchResult) int {
	seen := map[string]int{}
	for _, r := range res {
		seen[r.Document.Text]++
	}
	n := 0
	for _, r := range res {
		if hasTag(r, "superseded") && seen[r.Document.Text] > 1 {
			n++
		}
	}
	return n
}

// supersededAboveSuccessor names the first superseded row ranked above a row of the
// same text that is not superseded (its successor's copy), "" when there is none.
func supersededAboveSuccessor(res []domain.SearchResult) string {
	for i, r := range res {
		if !hasTag(r, "superseded") {
			continue
		}
		for j := i + 1; j < len(res); j++ {
			if !hasTag(res[j], "superseded") && res[j].Document.Text == r.Document.Text {
				return fmt.Sprintf("superseded %s copy at rank %d is above its successor's copy at rank %d (%q)",
					revTag(r), i+1, j+1, firstLine(r.Document.Text))
			}
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}

func textOf(res []domain.SearchResult, i int) string {
	if i < len(res) {
		return res[i].Document.Text
	}
	return ""
}

func scoreOf(res []domain.SearchResult, i int) float64 {
	if i < len(res) {
		return res[i].Score
	}
	return 0
}

func sop001(rev int) string {
	return fmt.Sprintf(`# SOP-001 Warehouse Goods Receipt and Put-Away

## Purpose
This procedure governs how inbound goods are received, inspected and put away at every site.

## Scope
Applies to all warehouses and all inbound deliveries of finished goods and components.

## Roles
- Site Manager: owns the receiving dock schedule and approves deviations at the site.
- Receiving Clerk: checks delivery notes against purchase orders.
- Quality Specialist: releases quarantined lots.

## Procedure
Unload, count, compare with the purchase order, label the pallet and move it to the assigned bin.

## Related SOPs
- SOP-003 — Deviation Reporting and Investigation
- SOP-004 — Corrective and Preventive Action
- SOP-007 — Lot Traceability and Recall Readiness

## Revision History
| Rev | Date | Change |
|---|---|---|
| %d | 2026-0%d-01 | Revision %d of the receiving steps |
`, rev, rev, rev)
}

const sop002 = `# SOP-002 Cycle Counting

## Purpose
Keep recorded stock equal to physical stock through scheduled cycle counts.

## Scope
All storage locations in every warehouse.

## Roles
- Site Manager: approves count adjustments above the tolerance.
- Inventory Controller: plans the count calendar.

## Procedure
Count the bin, compare with the system quantity, recount on a variance and post the adjustment.

## Related SOPs
- SOP-001 — Goods Receipt and Put-Away
- SOP-007 — Lot Traceability and Recall Readiness

## Revision History
| Rev | Date | Change |
|---|---|---|
| 1 | 2026-02-01 | First issue |
`

const sop007 = `# SOP-007 Lot Traceability

## Purpose
Trace every lot from supplier to customer within four hours.

## Scope
All lots of regulated products.

## Roles
- Quality Specialist: runs the trace and signs the trace report.

## Procedure
Look up the lot, list every shipment that carried it and notify the affected customers.

## Recall Readiness
Run a mock recall every quarter and record how long the full trace took.
`

// markdownSidecar stands in for the docling_agent's markdown path
// (agents/system/docling_agent/structure.py StructureBuilder): ATX headings open
// sections, blank-line-separated blocks are leaves, every node carries an ordinal
// per-document path ("n0.n2.n1") and a breadcrumb of its ancestor headings. Text with
// no heading is a flat document (leaves under the root, no sections).
type markdownSidecar struct{}

var mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)

func (markdownSidecar) Parse(_ context.Context, req memory.StructureParseRequest) (*memory.StructuredDocument, error) {
	doc := &memory.StructuredDocument{DocID: req.DocID, Title: req.Title, SourceType: req.SourceType, Backend: "markdown", OK: true}
	type open struct {
		id, path, crumb string
		level           int
	}
	root := open{id: "doc"}
	doc.Nodes = append(doc.Nodes, memory.StructNode{ID: "doc", Kind: "document", Title: req.Title})
	stack := []open{root}
	children := map[string]int{}
	pathOf := func(parent open) (string, int) {
		n := children[parent.id]
		children[parent.id] = n + 1
		seg := fmt.Sprintf("n%d", n)
		if parent.path == "" {
			return seg, n
		}
		return parent.path + "." + seg, n
	}
	var para []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(para, "\n"))
		para = para[:0]
		if text == "" {
			return
		}
		parent := stack[len(stack)-1]
		p, order := pathOf(parent)
		doc.Nodes = append(doc.Nodes, memory.StructNode{ID: "chk:" + p, Kind: "paragraph", Level: parent.level, Text: text,
			ParentID: parent.id, Order: order, Path: p, SectionPath: parent.crumb})
	}
	for _, line := range strings.Split(req.Text, "\n") {
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			flush()
			level := len(m[1])
			for len(stack) > 1 && stack[len(stack)-1].level >= level {
				stack = stack[:len(stack)-1]
			}
			parent := stack[len(stack)-1]
			p, order := pathOf(parent)
			crumb := m[2]
			if parent.crumb != "" {
				crumb = parent.crumb + " › " + m[2]
			}
			doc.Nodes = append(doc.Nodes, memory.StructNode{ID: "sec:" + p, Kind: "section", Level: level, Title: m[2],
				ParentID: parent.id, Order: order, Path: p, SectionPath: crumb})
			stack = append(stack, open{id: "sec:" + p, path: p, crumb: crumb, level: level})
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		para = append(para, line)
	}
	flush()
	return doc, nil
}
