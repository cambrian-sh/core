package memory

// Frozen-KG validation for ADR-0053 D2 (revised): does the *rule* KG
// (spacy_patterns, frozen in table chunk_triplets_rule) drive the production
// kgExpand to the same QA-evidence reach as the LLM KG (frozen in
// chunk_triplets_llm_canon)? This exercises the real kgExpand expansion code
// (with its hardcoded limit-5-per-entity, top-N-entity bounds), isolating the
// KG's quality from the embedder/LLM by seeding expansion directly from each
// question's evidence chunks.
//
// Gated: set CAMBRIAN_KGVALIDATE=1 (it touches the live DB and the frozen
// tables produced by the freeze step). Inputs via env:
//   CAMBRIAN_PG_DSN              postgres DSN (default: local cambrian_db)
//   CAMBRIAN_KGVALIDATE_EVID     path to eval_evidence.json (required)
//
// graph_recall = fraction of QA whose evidence chunks are all reachable from
// the first evidence chunk via one-component kgExpand traversal — the same
// definition the offline notebook uses, here computed on the production path.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"

	"github.com/jackc/pgx/v5/pgxpool"
)

type evidenceQA struct {
	Sample           string   `json:"sample"`
	QID              string   `json:"qid"`
	Category         string   `json:"category"`
	NEvidence        int      `json:"n_evidence"`
	EvidenceChunkIDs []string `json:"evidence_chunk_ids"`
}

// memTripletStore is an in-memory ChunkTripletsStore loaded once from a frozen
// table. The production kgExpand algorithm runs against it unchanged; only the
// I/O is in-memory so 1977 QA × full BFS doesn't issue millions of queries.
type memTripletStore struct {
	byChunk  map[string][]ChunkTriplet
	byEntity map[string][]string // entity (lowercase) -> sorted chunk ids
}

func (s *memTripletStore) SaveChunkTriplets(context.Context, string, []ChunkTriplet) error {
	return nil
}
func (s *memTripletStore) ForChunk(_ context.Context, id string) ([]ChunkTriplet, error) {
	return s.byChunk[id], nil
}
func (s *memTripletStore) ForChunks(_ context.Context, ids []string) (map[string][]ChunkTriplet, error) {
	out := make(map[string][]ChunkTriplet, len(ids))
	for _, id := range ids {
		if ts, ok := s.byChunk[id]; ok {
			out[id] = ts
		}
	}
	return out, nil
}
func (s *memTripletStore) ChunksMentioningEntity(_ context.Context, entity string, limit int) ([]string, error) {
	ids := s.byEntity[entity] // already sorted; matches production "lowercase entity" match
	if limit > 0 && limit < len(ids) {
		ids = ids[:limit]
	}
	return ids, nil
}

// idVectorSearch is the minimal kgExpandVectorSearch: connectivity only needs
// the chunk ID, so every expanded chunk is materialized as an ID-only doc.
type idVectorSearch struct{}

func (idVectorSearch) GetByID(_ context.Context, id string) (*domain.Document, error) {
	return &domain.Document{ID: id}, nil
}

func loadFrozenStore(ctx context.Context, pool *pgxpool.Pool, table string) (*memTripletStore, error) {
	rows, err := pool.Query(ctx, fmt.Sprintf(
		`SELECT chunk_id, h, r, t, COALESCE(weight,1.0) FROM %s`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st := &memTripletStore{byChunk: map[string][]ChunkTriplet{}, byEntity: map[string][]string{}}
	entSeen := map[string]map[string]struct{}{} // entity -> set of chunk ids (dedupe)
	add := func(ent, chunk string) {
		if ent == "" {
			return
		}
		if entSeen[ent] == nil {
			entSeen[ent] = map[string]struct{}{}
		}
		if _, ok := entSeen[ent][chunk]; !ok {
			entSeen[ent][chunk] = struct{}{}
			st.byEntity[ent] = append(st.byEntity[ent], chunk)
		}
	}
	for rows.Next() {
		var cid, h, r, tt string
		var w float64
		if err := rows.Scan(&cid, &h, &r, &tt, &w); err != nil {
			return nil, err
		}
		st.byChunk[cid] = append(st.byChunk[cid], ChunkTriplet{H: h, R: r, T: tt, Weight: w})
		add(h, cid)
		add(tt, cid)
	}
	for e := range st.byEntity {
		sort.Strings(st.byEntity[e]) // deterministic limit-5 selection
	}
	return st, rows.Err()
}

func graphRecall(ctx context.Context, store ChunkTripletsStore, qas []evidenceQA) (map[string][2]int, [2]int) {
	// returns per-category {hits,total} and overall {hits,total}
	byCat := map[string][2]int{}
	var overall [2]int
	vs := idVectorSearch{}
	for _, qa := range qas {
		ev := qa.EvidenceChunkIDs
		if len(ev) == 0 {
			continue
		}
		hit := true
		if len(ev) > 1 {
			seeds := []domain.SearchResult{{Document: domain.Document{ID: ev[0]}, Score: 1.0}}
			// High caps so the only bound is the production limit-5-per-entity;
			// many hops to measure full one-component reachability.
			out := kgExpand(ctx, seeds, store, vs, nil, kgExpandOpts{
				Hops: 8, MaxExpanded: 1_000_000, MaxEntities: 1_000_000,
			})
			reached := make(map[string]bool, len(out))
			for _, s := range out {
				reached[s.Document.ID] = true
			}
			for _, id := range ev[1:] {
				if !reached[id] {
					hit = false
					break
				}
			}
		}
		c := byCat[qa.Category]
		c[1]++
		overall[1]++
		if hit {
			c[0]++
			overall[0]++
		}
		byCat[qa.Category] = c
	}
	return byCat, overall
}

func TestKGFrozenGraphRecall(t *testing.T) {
	if os.Getenv("CAMBRIAN_KGVALIDATE") == "" {
		t.Skip("set CAMBRIAN_KGVALIDATE=1 to run the frozen-KG graph_recall validation")
	}
	dsn := os.Getenv("CAMBRIAN_PG_DSN")
	if dsn == "" {
		dsn = "postgres://cambrian:" + os.Getenv("CAMBRIAN_DB_PASSWORD") + "@localhost:5432/cambrian_db"
	}
	evPath := os.Getenv("CAMBRIAN_KGVALIDATE_EVID")
	if evPath == "" {
		t.Fatal("CAMBRIAN_KGVALIDATE_EVID must point at eval_evidence.json")
	}
	raw, err := os.ReadFile(evPath)
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	var qas []evidenceQA
	if err := json.Unmarshal(raw, &qas); err != nil {
		t.Fatalf("parse evidence: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	tables := []struct{ label, table string }{
		{"LLM (chunk_triplets, canonical)", "chunk_triplets_llm_canon"},
		{"rule (spacy_patterns, frozen)", "chunk_triplets_rule"},
	}
	cats := []string{"single-hop", "multi-hop", "temporal", "open-domain", "adversarial"}

	t.Logf("QA with resolvable evidence: %d", len(qas))
	for _, tb := range tables {
		store, err := loadFrozenStore(ctx, pool, tb.table)
		if err != nil {
			t.Fatalf("load %s: %v", tb.table, err)
		}
		byCat, overall := graphRecall(ctx, store, qas)
		gr := 0.0
		if overall[1] > 0 {
			gr = float64(overall[0]) / float64(overall[1])
		}
		t.Logf("=== %s ===", tb.label)
		t.Logf("  graph_recall overall: %.4f  (%d/%d)", gr, overall[0], overall[1])
		for _, c := range cats {
			v, ok := byCat[c]
			if !ok || v[1] == 0 {
				continue
			}
			t.Logf("    %-12s %.4f  (%d/%d)", c, float64(v[0])/float64(v[1]), v[0], v[1])
		}
	}
}
