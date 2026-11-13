package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// ADR-0136 Amendment A1 — the substrate-generic tables of migration 0022 on
// PgKnowledgeStore: assessments (keyed judge verdicts), predicate profiles (learned
// cardinality), the item embedding projection and the extraction marker; plus the
// widened derivation an input-taking authority needs (deriveKeys).

var (
	_ domain.AssessmentStore       = (*PgKnowledgeStore)(nil)
	_ domain.PredicateProfileStore = (*PgKnowledgeStore)(nil)
)

// resolutionKey is (entity, actor) inside one (namespace, kind).
type resolutionKey struct{ entity, actor string }

func (k resolutionKey) less(o resolutionKey) bool {
	if k.entity != o.entity {
		return k.entity < o.entity
	}
	return k.actor < o.actor
}

// derivedKey is one key's derivation: its items (with values), the inputs it was
// derived from, and the authority's outcome.
type derivedKey struct {
	items   []domain.KnowledgeItem
	inputs  domain.ResolutionInputs
	outcome domain.ResolutionOutcome
}

// deriveKeys derives several keys of one (namespace, kind) in a constant number of
// queries (items, assessments, related items, profiles), then runs the authority per
// key in Go. scope, when non-nil, restricts every item read to rows the predicate
// admits IN SQL (the read path); a nil scope is the write path's full read.
func (s *PgKnowledgeStore) deriveKeys(ctx context.Context, q pgxQuerier, ia domain.InputResolutionAuthority,
	ns, kind string, keys []resolutionKey, scope *domain.TagPredicate) (map[resolutionKey]derivedKey, error) {
	return s.deriveKeysHiding(ctx, q, ia, ns, kind, keys, scope, nil)
}

// deriveKeysHiding is deriveKeys with the hidden items treated as absent — neither a
// key's own item nor a related one. The two-phase backfill derives "as the serial run
// saw it" this way (domain.FactStagedReader.SimilarFactsHiding); nil hides nothing.
func (s *PgKnowledgeStore) deriveKeysHiding(ctx context.Context, q pgxQuerier, ia domain.InputResolutionAuthority,
	ns, kind string, keys []resolutionKey, scope *domain.TagPredicate, hidden map[domain.KnowledgeItemID]bool) (map[resolutionKey]derivedKey, error) {
	out := map[resolutionKey]derivedKey{}
	if len(keys) == 0 {
		return out, nil
	}
	entities := make([]string, 0, len(keys))
	want := map[resolutionKey]bool{}
	for _, k := range keys {
		if !want[k] {
			want[k] = true
			entities = append(entities, k.entity)
		}
	}
	scopeSQL, scopeArgs := sqlfence.TextArray("ki.classification").Where(scope, 3)
	items, err := loadItems(ctx, q, `WHERE ki.namespace_id = $1 AND ki.kind = $2 AND ki.entity_id = ANY($3)`+scopeSQL,
		append([]any{ns, kind, entities}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("derive: items: %w", err)
	}
	byKey := map[resolutionKey][]domain.KnowledgeItem{}
	var allIDs []string
	keyOf := map[domain.KnowledgeItemID]resolutionKey{}
	for _, it := range items {
		k := resolutionKey{it.EntityID, it.AssertedBy}
		if !want[k] || hidden[it.ID] {
			continue // same entity, another actor (or hidden)
		}
		byKey[k] = append(byKey[k], it)
		allIDs = append(allIDs, string(it.ID))
		keyOf[it.ID] = k
	}
	asms, err := assessmentsCiting(ctx, q, ns, kind, allIDs)
	if err != nil {
		return nil, fmt.Errorf("derive: assessments: %w", err)
	}
	// The other side of cross-key verdicts. An item of ANOTHER requested key is already
	// loaded and serves as a related item too (two keys derived together must see each
	// other's verdicts exactly as when derived alone); the rest is loaded here.
	relByID := map[domain.KnowledgeItemID]domain.KnowledgeItem{}
	for _, it := range items {
		if _, ok := keyOf[it.ID]; ok {
			relByID[it.ID] = it
		}
	}
	relIDs := map[string]bool{}
	for _, a := range asms {
		for _, v := range a.Verdicts {
			for _, side := range []domain.KnowledgeItemID{v.A, v.B} {
				if _, have := relByID[side]; !have && !hidden[side] {
					relIDs[string(side)] = true
				}
			}
		}
	}
	if len(relIDs) > 0 {
		ids := make([]string, 0, len(relIDs))
		for id := range relIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rs, rargs := sqlfence.TextArray("ki.classification").Where(scope, 2)
		rel, err := loadItems(ctx, q, `WHERE ki.kind = $1 AND ki.id = ANY($2)`+rs, append([]any{kind, ids}, rargs...)...)
		if err != nil {
			return nil, fmt.Errorf("derive: related items: %w", err)
		}
		for _, r := range rel {
			relByID[r.ID] = r
		}
	}
	// Profiles of every predicate involved.
	preds := map[string]bool{}
	predOf := map[resolutionKey]string{}
	for k, its := range byKey {
		p := ia.ProfilePredicate(its)
		predOf[k] = p
		if p != "" {
			preds[p] = true
		}
	}
	cards, err := currentCardinalities(ctx, q, ns, kind, preds)
	if err != nil {
		return nil, fmt.Errorf("derive: profiles: %w", err)
	}
	for k, its := range byKey {
		in := domain.ResolutionInputs{Cardinality: domain.CardinalityUnknown}
		if c, ok := cards[predOf[k]]; ok {
			in.Cardinality = c
		}
		ids := map[domain.KnowledgeItemID]bool{}
		for _, it := range its {
			ids[it.ID] = true
		}
		relSeen := map[domain.KnowledgeItemID]bool{}
		for _, a := range asms {
			cites := false
			for _, id := range a.InputItemIDs {
				if ids[id] {
					cites = true
					break
				}
			}
			if !cites {
				continue
			}
			in.Assessments = append(in.Assessments, a)
			for _, v := range a.Verdicts {
				for _, side := range []domain.KnowledgeItemID{v.A, v.B} {
					if r, ok := relByID[side]; ok && !ids[side] && !relSeen[side] {
						relSeen[side] = true
						in.Related = append(in.Related, r)
					}
				}
			}
		}
		out[k] = derivedKey{items: its, inputs: in, outcome: ia.ResolveWith(its, in)}
	}
	return out, nil
}

const assessmentColumns = `id, namespace_id, kind, authority, authority_version, prompt_hash,
	input_item_ids, verdicts, raw_output, classification, created_at`

func scanAssessment(row pgx.Row) (domain.Assessment, error) {
	var a domain.Assessment
	var inputs []string
	var verdicts []byte
	if err := row.Scan(&a.ID, &a.NamespaceID, &a.Kind, &a.Authority, &a.AuthorityVersion, &a.PromptHash,
		&inputs, &verdicts, &a.RawOutput, &a.Classification, &a.CreatedAt); err != nil {
		return a, err
	}
	for _, id := range inputs {
		a.InputItemIDs = append(a.InputItemIDs, domain.KnowledgeItemID(id))
	}
	if len(verdicts) > 0 {
		if err := json.Unmarshal(verdicts, &a.Verdicts); err != nil {
			return a, fmt.Errorf("assessment %s: verdicts: %w", a.ID, err)
		}
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

func assessmentsCiting(ctx context.Context, q pgxQuerier, ns, kind string, ids []string) ([]domain.Assessment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT `+assessmentColumns+` FROM knowledge_assessments
		WHERE namespace_id = $1 AND kind = $2 AND input_item_ids && $3::text[]
		ORDER BY id`, ns, kind, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Assessment
	for rows.Next() {
		a, err := scanAssessment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func currentCardinalities(ctx context.Context, q pgxQuerier, ns, kind string, preds map[string]bool) (map[string]string, error) {
	out := map[string]string{}
	if len(preds) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(preds))
	for p := range preds {
		list = append(list, p)
	}
	rows, err := q.Query(ctx, `SELECT predicate, cardinality FROM predicate_profiles
		WHERE namespace_id = $1 AND kind = $2 AND predicate = ANY($3) AND system_to IS NULL`, ns, kind, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p, c string
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		out[p] = c
	}
	return out, rows.Err()
}

// GetAssessment implements domain.AssessmentStore.
func (s *PgKnowledgeStore) GetAssessment(ctx context.Context, id string) (*domain.Assessment, error) {
	a, err := scanAssessment(s.pool.QueryRow(ctx, `SELECT `+assessmentColumns+` FROM knowledge_assessments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// PutAssessment implements domain.AssessmentStore: insert-if-absent by the keyed id,
// then — in the same transaction, under each key's advisory lock, taken in a stable
// order so two assessment writers cannot deadlock — re-derive every key its inputs
// belong to (A1.2 rule 6). A replay (id present) writes nothing and derives nothing.
func (s *PgKnowledgeStore) PutAssessment(ctx context.Context, a domain.Assessment) (bool, error) {
	if a.ID == "" || a.Kind == "" || a.Authority == "" || len(a.InputItemIDs) == 0 {
		return false, fmt.Errorf("assessment put: id, kind, authority and inputs are required")
	}
	ns := a.NamespaceID
	ns = orDefaultNamespace(ns)
	inputs := make([]string, 0, len(a.InputItemIDs))
	for _, id := range a.InputItemIDs {
		inputs = append(inputs, string(id))
	}
	sort.Strings(inputs)
	verdicts := a.Verdicts
	if verdicts == nil {
		verdicts = []domain.AssessmentVerdict{}
	}
	vj, err := json.Marshal(verdicts)
	if err != nil {
		return false, err
	}
	tags := a.Classification
	if tags == nil {
		tags = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// Lock the cited keys BEFORE inserting, so no PutItem on one of them can derive
	// between this insert and this re-derive.
	rows, err := tx.Query(ctx, `SELECT DISTINCT entity_id, asserted_by FROM knowledge_items
		WHERE namespace_id = $1 AND kind = $2 AND id = ANY($3)`, ns, a.Kind, inputs)
	if err != nil {
		return false, err
	}
	var keys []resolutionKey
	for rows.Next() {
		var k resolutionKey
		if err := rows.Scan(&k.entity, &k.actor); err != nil {
			rows.Close()
			return false, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLockKey(ns, a.Kind, k.entity, k.actor)); err != nil {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO knowledge_assessments (id, namespace_id, kind, authority,
			authority_version, prompt_hash, input_item_ids, verdicts, raw_output, classification)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10)
		ON CONFLICT (id) DO NOTHING`,
		a.ID, ns, a.Kind, a.Authority, a.AuthorityVersion, a.PromptHash, inputs, string(vj), a.RawOutput, tags)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx) // replay: the verdict is already on record
	}
	for _, k := range keys {
		if err := s.rederive(ctx, tx, ns, a.Kind, k.entity, k.actor); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

const profileColumns = `namespace_id, kind, predicate, cardinality, source, single_votes, multi_votes,
	evidence_assessment_ids, alias_of, set_by, reason, system_from`

func scanProfile(row pgx.Row) (domain.PredicateProfile, error) {
	var p domain.PredicateProfile
	err := row.Scan(&p.NamespaceID, &p.Kind, &p.Predicate, &p.Cardinality, &p.Source, &p.SingleVotes, &p.MultiVotes,
		&p.EvidenceAssessmentIDs, &p.AliasOf, &p.SetBy, &p.Reason, &p.SystemFrom)
	return p, err
}

func currentProfile(ctx context.Context, q pgxQuerier, ns, kind, predicate string) (domain.PredicateProfile, bool, error) {
	p, err := scanProfile(q.QueryRow(ctx, `SELECT `+profileColumns+` FROM predicate_profiles
		WHERE namespace_id = $1 AND kind = $2 AND predicate = $3 AND system_to IS NULL`, ns, kind, predicate))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PredicateProfile{NamespaceID: ns, Kind: kind, Predicate: predicate,
			Cardinality: domain.CardinalityUnknown, Source: domain.ProfileSourceLearned}, false, nil
	}
	return p, err == nil, err
}

// writeProfileVersion closes the current version (if any) and inserts next.
func writeProfileVersion(ctx context.Context, tx pgx.Tx, next domain.PredicateProfile, hadCurrent bool) error {
	if hadCurrent {
		if _, err := tx.Exec(ctx, `UPDATE predicate_profiles SET system_to = now()
			WHERE namespace_id = $1 AND kind = $2 AND predicate = $3 AND system_to IS NULL`,
			next.NamespaceID, next.Kind, next.Predicate); err != nil {
			return err
		}
	}
	ids := next.EvidenceAssessmentIDs
	if ids == nil {
		ids = []string{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO predicate_profiles (namespace_id, kind, predicate, cardinality, source,
			single_votes, multi_votes, evidence_assessment_ids, alias_of, set_by, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		next.NamespaceID, next.Kind, next.Predicate, next.Cardinality, next.Source,
		next.SingleVotes, next.MultiVotes, ids, next.AliasOf, next.SetBy, next.Reason)
	return err
}

func profileLockKey(ns, kind, predicate string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "profile|%s|%s|%s", ns, kind, predicate)
	return int64(h.Sum64()) //nolint:gosec // deliberate wraparound: the lock key only needs stability
}

// PredicateProfile implements domain.PredicateProfileStore.
func (s *PgKnowledgeStore) PredicateProfile(ctx context.Context, namespace, kind, predicate string) (domain.PredicateProfile, error) {
	namespace = orDefaultNamespace(namespace)
	p, _, err := currentProfile(ctx, s.pool, namespace, kind, predicate)
	return p, err
}

// VoteProfile implements domain.PredicateProfileStore.
func (s *PgKnowledgeStore) VoteProfile(ctx context.Context, namespace, kind, predicate string, v domain.ProfileVote, minVotes int) (bool, error) {
	namespace = orDefaultNamespace(namespace)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, profileLockKey(namespace, kind, predicate)); err != nil {
		return false, err
	}
	cur, had, err := currentProfile(ctx, tx, namespace, kind, predicate)
	if err != nil {
		return false, err
	}
	next, changed := domain.ApplyProfileVote(cur, v, minVotes)
	if !changed {
		return false, tx.Commit(ctx)
	}
	if err := writeProfileVersion(ctx, tx, next, had); err != nil {
		return false, err
	}
	return next.Cardinality != cur.Cardinality, tx.Commit(ctx)
}

// SeedDeclaredProfiles implements domain.PredicateProfileStore. Config is v1's override
// channel (A1-Q5): a declared predicate takes its configured cardinality; a declared row
// whose predicate left the config reverts to learned, keeping its votes. An override
// row (the future operator RPC) is never touched.
func (s *PgKnowledgeStore) SeedDeclaredProfiles(ctx context.Context, namespace, kind string, declared map[string]string, minVotes int, setBy string) ([]string, error) {
	namespace = orDefaultNamespace(namespace)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	rows, err := tx.Query(ctx, `SELECT `+profileColumns+` FROM predicate_profiles
		WHERE namespace_id = $1 AND kind = $2 AND system_to IS NULL AND source = 'declared'`, namespace, kind)
	if err != nil {
		return nil, err
	}
	var stale []domain.PredicateProfile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if _, still := declared[p.Predicate]; !still {
			stale = append(stale, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var changed []string
	for _, p := range stale {
		next := p
		next.Source = domain.ProfileSourceLearned
		next.Cardinality = domain.LearnedCardinality(p.SingleVotes, p.MultiVotes, minVotes)
		next.SetBy, next.Reason = setBy, "removed from config"
		if err := writeProfileVersion(ctx, tx, next, true); err != nil {
			return nil, err
		}
		if next.Cardinality != p.Cardinality {
			changed = append(changed, p.Predicate)
		}
	}
	preds := make([]string, 0, len(declared))
	for p := range declared {
		preds = append(preds, p)
	}
	sort.Strings(preds)
	for _, pred := range preds {
		card := declared[pred]
		cur, had, err := currentProfile(ctx, tx, namespace, kind, pred)
		if err != nil {
			return nil, err
		}
		if had && (cur.Source == domain.ProfileSourceOverride || (cur.Source == domain.ProfileSourceDeclared && cur.Cardinality == card)) {
			continue
		}
		next := cur
		next.NamespaceID, next.Kind, next.Predicate = namespace, kind, pred
		next.Source, next.Cardinality, next.SetBy, next.Reason = domain.ProfileSourceDeclared, card, setBy, "declared in config"
		if err := writeProfileVersion(ctx, tx, next, had); err != nil {
			return nil, err
		}
		if cur.Cardinality != card {
			changed = append(changed, pred)
		}
	}
	return changed, tx.Commit(ctx)
}

// RederivePredicate implements domain.PredicateProfileStore: the bounded re-derive a
// changed profile triggers (A1.4 "Effect"). One transaction per key, so a large
// predicate never holds one long lock set.
func (s *PgKnowledgeStore) RederivePredicate(ctx context.Context, namespace, kind, predicate string, limit int) (int, error) {
	namespace = orDefaultNamespace(namespace)
	q := `SELECT DISTINCT entity_id, asserted_by FROM knowledge_items
		WHERE namespace_id = $1 AND kind = $2 AND split_part(entity_id, '/', 3) = $3
		ORDER BY entity_id, asserted_by`
	args := []any{namespace, kind, predicate}
	if limit > 0 {
		q += ` LIMIT $4`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var keys []resolutionKey
	for rows.Next() {
		var k resolutionKey
		if err := rows.Scan(&k.entity, &k.actor); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, k := range keys {
		if err := s.rederiveOne(ctx, namespace, kind, k); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *PgKnowledgeStore) rederiveOne(ctx context.Context, ns, kind string, k resolutionKey) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLockKey(ns, kind, k.entity, k.actor)); err != nil {
		return err
	}
	if err := s.rederive(ctx, tx, ns, kind, k.entity, k.actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutItemEmbeddings writes the item projection (knowledge_item_embeddings; the ADR-0107
// pattern for items). Upsert by (item, model): re-embedding under the same model
// replaces the vector.
func (s *PgKnowledgeStore) PutItemEmbeddings(ctx context.Context, embs []domain.ItemEmbedding) error {
	if len(embs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range embs {
		if e.ItemID == "" || e.ModelID == "" || len(e.Vector) == 0 {
			continue
		}
		batch.Queue(`INSERT INTO knowledge_item_embeddings (item_id, model_id, model_version, dims, embedding)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (item_id, model_id)
			DO UPDATE SET embedding = EXCLUDED.embedding, dims = EXCLUDED.dims, model_version = EXCLUDED.model_version, created_at = now()`,
			string(e.ItemID), e.ModelID, e.ModelVersion, len(e.Vector), pgvector.NewVector(e.Vector))
	}
	if batch.Len() == 0 {
		return nil
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// ExtractionMarker returns the marker of this content, or nil (ADR-0136 A2.4: version
// and coverage ride along; a legacy row reads version "" and 0 of 0 passages).
func (s *PgKnowledgeStore) ExtractionMarker(ctx context.Context, namespace, extractor, documentID, contentHash string) (*domain.ExtractionMarker, error) {
	namespace = orDefaultNamespace(namespace)
	m := domain.ExtractionMarker{NamespaceID: namespace, Extractor: extractor, DocumentID: documentID, ContentHash: contentHash}
	var covered, total *int
	err := s.pool.QueryRow(ctx, `SELECT items, extractor_version, passages_covered, passages_total FROM knowledge_extractions
		WHERE namespace_id = $1 AND extractor = $2 AND document_id = $3 AND content_hash = $4`,
		namespace, extractor, documentID, contentHash).Scan(&m.Items, &m.ExtractorVersion, &covered, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if covered != nil {
		m.PassagesCovered = *covered
	}
	if total != nil {
		m.PassagesTotal = *total
	}
	return &m, nil
}

// MarkExtracted upserts the extraction marker: a resumed or re-versioned run over the
// same content replaces the row's version, coverage and item count.
func (s *PgKnowledgeStore) MarkExtracted(ctx context.Context, m domain.ExtractionMarker) error {
	ns := m.NamespaceID
	ns = orDefaultNamespace(ns)
	_, err := s.pool.Exec(ctx, `INSERT INTO knowledge_extractions
			(namespace_id, extractor, document_id, content_hash, items, extractor_version, passages_covered, passages_total)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (namespace_id, extractor, document_id, content_hash) DO UPDATE SET
			items = EXCLUDED.items, extractor_version = EXCLUDED.extractor_version,
			passages_covered = EXCLUDED.passages_covered, passages_total = EXCLUDED.passages_total,
			extracted_at = now()`,
		ns, m.Extractor, m.DocumentID, m.ContentHash, m.Items, m.ExtractorVersion, m.PassagesCovered, m.PassagesTotal)
	return err
}

var _ domain.ExtractionWindowJournal = (*PgKnowledgeStore)(nil)

func idStrings(ids []domain.KnowledgeItemID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// PutExtractionWindow upserts one journal row of the two-phase backfill
// (knowledge_extraction_windows, migration 0025).
func (s *PgKnowledgeStore) PutExtractionWindow(ctx context.Context, w domain.ExtractionWindow) error {
	ns := w.NamespaceID
	ns = orDefaultNamespace(ns)
	_, err := s.pool.Exec(ctx, `INSERT INTO knowledge_extraction_windows
			(namespace_id, extractor, document_id, content_hash, extractor_version, passage_start, passage_end, item_ids, new_item_ids)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (namespace_id, extractor, document_id, content_hash, extractor_version, passage_start) DO UPDATE SET
			passage_end = EXCLUDED.passage_end, item_ids = EXCLUDED.item_ids, new_item_ids = EXCLUDED.new_item_ids,
			created_at = now()`,
		ns, w.Extractor, w.DocumentID, w.ContentHash, w.ExtractorVersion, w.PassageStart, w.PassageEnd,
		idStrings(w.ItemIDs), idStrings(w.NewItemIDs))
	return err
}

// ExtractionWindows returns one document content's journal rows under one version, by
// passage start.
func (s *PgKnowledgeStore) ExtractionWindows(ctx context.Context, namespace, extractor, documentID, contentHash, version string) ([]domain.ExtractionWindow, error) {
	namespace = orDefaultNamespace(namespace)
	rows, err := s.pool.Query(ctx, `SELECT passage_start, passage_end, item_ids, new_item_ids FROM knowledge_extraction_windows
		WHERE namespace_id = $1 AND extractor = $2 AND document_id = $3 AND content_hash = $4 AND extractor_version = $5
		ORDER BY passage_start`, namespace, extractor, documentID, contentHash, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExtractionWindow
	for rows.Next() {
		w := domain.ExtractionWindow{NamespaceID: namespace, Extractor: extractor, DocumentID: documentID,
			ContentHash: contentHash, ExtractorVersion: version}
		var items, fresh []string
		if err := rows.Scan(&w.PassageStart, &w.PassageEnd, &items, &fresh); err != nil {
			return nil, err
		}
		for _, id := range items {
			w.ItemIDs = append(w.ItemIDs, domain.KnowledgeItemID(id))
		}
		for _, id := range fresh {
			w.NewItemIDs = append(w.NewItemIDs, domain.KnowledgeItemID(id))
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteExtractionWindows removes one document content's journal rows (every version).
func (s *PgKnowledgeStore) DeleteExtractionWindows(ctx context.Context, namespace, extractor, documentID, contentHash string) error {
	namespace = orDefaultNamespace(namespace)
	_, err := s.pool.Exec(ctx, `DELETE FROM knowledge_extraction_windows
		WHERE namespace_id = $1 AND extractor = $2 AND document_id = $3 AND content_hash = $4`,
		namespace, extractor, documentID, contentHash)
	return err
}
