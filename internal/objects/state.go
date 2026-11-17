package objects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The maintainer (ADR-0149 D3 "Not a second source of truth", D8). This file
// holds the ONLY statements that write object table rows; an architecture test
// (arch_test.go) fails the build if any other file does.

// NotRetired is the predicate every counted read of a typed row carries
// (ADR-0147 D6): the row's source_ref has no live retirement. alias is the
// table alias, or the table name when the query has none. A row with an empty
// source_ref can never be retired, so it always passes. The one rendering:
// the postgres stores use it too.
func NotRetired(alias string) string {
	return ` AND NOT EXISTS (SELECT 1 FROM typed_row_retirements rt
		WHERE rt.namespace_id = ` + alias + `.namespace_id AND rt.source_ref = ` + alias + `.source_ref
		  AND rt.reinstated_at IS NULL)`
}

// ApplyStateTx recomputes the object rows of entityIDs in namespace, inside
// tx — the transaction of the history write that may have changed them. Every
// write path that changes "current" calls it: observation writes, retirements
// and reinstatements, knowledge item writes and re-derivations, erasures and
// fact resets. An entity that is not an object of a table kind costs one
// advisory lock and nothing else.
//
// namespace must already be normalised (the stores' default namespace).
//
// Each entity is recomputed from history under a per-entity transaction lock,
// so two writers of one object serialise and the second reads the first's
// committed row (READ COMMITTED). The lock is taken BEFORE the declarations are
// consulted: a rebuild that starts after a live declarations change therefore
// either waits for an in-flight writer or is followed by one that already sees
// the new declarations.
func ApplyStateTx(ctx context.Context, tx pgx.Tx, kinds domain.KindSource, namespace string, entityIDs ...string) error {
	for _, e := range dedupe(entityIDs) {
		if err := applyEntity(ctx, tx, kinds, namespace, e, nil); err != nil {
			return err
		}
	}
	return nil
}

func dedupe(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out) // a stable lock order across writers
	return out
}

func entityLockKey(ns, entity string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "objects|%s|%s", ns, entity)
	return int64(h.Sum64()) //nolint:gosec // deliberate wraparound: the lock key only needs stability
}

// applyEntity recomputes one entity's row. rebuilding names the table a rebuild
// is walking: a row there whose entity is no longer an object of that table's
// kind is removed.
func applyEntity(ctx context.Context, tx pgx.Tx, kinds domain.KindSource, ns, entity string, rebuilding *Table) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, entityLockKey(ns, entity)); err != nil {
		return fmt.Errorf("objects: lock %s: %w", entity, err)
	}
	reg := kinds.KindsFor(ns)
	spec, ok := reg.ObjectKind(entity)
	isObject := ok && spec.Table
	if rebuilding != nil && (!isObject || spec.Kind != rebuilding.Kind) {
		return deleteRow(ctx, tx, rebuilding.Kind, ns, entity)
	}
	if !isObject {
		return nil
	}
	cands, err := readCandidates(ctx, tx, reg, ns, entity)
	if err != nil {
		return fmt.Errorf("objects: read history of %s: %w", entity, err)
	}
	if len(cands) == 0 {
		return deleteRow(ctx, tx, spec.Kind, ns, entity)
	}
	st := Current(spec, cands)
	classes, err := rowClasses(ctx, tx, spec, ns, entity, st)
	if err != nil {
		return fmt.Errorf("objects: fence classes of %s: %w", entity, err)
	}
	return upsertRow(ctx, tx, spec, reg.Version(), ns, entity, st, classes)
}

// classTriple is one canonical (tags, parties, row_tags) triple of the class
// dictionary (object_fence_classes, migrations 0043 and 0055): the three
// companions a property value's fence reads (ADR-0158 D2).
type classTriple struct {
	Tags    []string `json:"tags"`
	Parties []string `json:"parties"`
	RowTags []string `json:"row_tags"`
}

// canonical sorts and de-duplicates every side; absent parties and row tags are
// '{}'. The fence reads a triple only through set operations (and the row-tags
// side through cardinality and concatenation), so this changes no decision and
// makes one triple one dictionary row.
func canonical(tags, parties, rowTags []string) classTriple {
	set := func(xs []string) []string {
		out := make([]string, 0, len(xs))
		seen := map[string]bool{}
		for _, x := range xs {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		sort.Strings(out)
		return out
	}
	return classTriple{Tags: set(tags), Parties: set(parties), RowTags: set(rowTags)}
}

// rowClasses is the row's vis_classes (ADR-0149 D6, the existence rule of
// ADR-0128 D12 interned): the dictionary ids of the triples that can make the
// object exist for a caller —
//   - every declared property's current value's (tags, parties, row_tags),
//     exactly the companions upsertRow writes; a value with no classification
//     (Tags nil, a NULL tags column) is never a class, as it is visible to no
//     filtered caller;
//   - the (classification, parties, first_seen_classification) of the mint:
//     the minting evidence's classification and parties and the minting row's
//     own classification (ADR-0158 D4; empty for an unclassified mint), read
//     with the existence rule's own join (entities.first_seen_evidence). That row
//     is written by the entity mint, which calls the maintainer when it creates
//     one (PgEntityStore.EnsureEntity), and neither it nor the evidence row is
//     ever updated, so the class cannot go stale.
//
// The object exists for a reader exactly when one of these triples passes the
// fence: the per-property fence is sqlfence.ObjectProperty(...).WithRowTags, the
// dictionary's is sqlfence.FenceClassRowTags (the same shape), and the evidence
// fence differs from it only by the present term, which a NOT NULL
// classification always passes.
func rowClasses(ctx context.Context, tx pgx.Tx, spec domain.KindSpec, ns, entity string, st State) ([]int32, error) {
	var pairs []classTriple
	seen := map[string]bool{}
	add := func(tags, parties, rowTags []string) {
		c := canonical(tags, parties, rowTags)
		raw, _ := json.Marshal(c) // three string slices always marshal
		if key := string(raw); !seen[key] {
			seen[key] = true
			pairs = append(pairs, c)
		}
	}
	for p := range spec.Predicates {
		if v, ok := st.Props[p]; ok && v.Tags != nil {
			add(v.Tags, v.Parties, v.RowTags)
		}
	}
	var tags, parties, mintTags []string
	err := tx.QueryRow(ctx, `SELECT e.classification, e.parties, en.first_seen_classification
		FROM entities en JOIN evidence e ON e.id = en.first_seen_evidence
		WHERE en.id = $1 AND en.namespace_id = $2`, entity, ns).Scan(&tags, &parties, &mintTags)
	switch {
	case err == nil:
		add(tags, parties, mintTags)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	return internClasses(ctx, tx, pairs)
}

// internClasses returns the dictionary ids of triples (canonical, distinct),
// sorted, inserting the triples the dictionary lacks. The dictionary is
// append-only and unique on the triple, so a triple has one id forever and a
// retry reuses it (a pair interned before migration 0055 is the triple with
// empty row tags, under its old id). A triple another transaction inserted after this statement's
// snapshot is skipped by ON CONFLICT and not yet visible to the same statement;
// the next attempt (a new READ COMMITTED snapshot, taken after ON CONFLICT
// waited for that transaction) sees it.
//
// Waiting on another writer's uncommitted pair is the one new lock wait on the
// write path. Two transactions that each hold a NEW pair the other needs can
// deadlock; Postgres aborts one, and its history write fails and is retried
// like any transient failure. That needs two concurrent writers both minting
// new classification combinations, which is rare once a deployment's classes
// exist; pairs inside one statement are inserted in sorted order.
func internClasses(ctx context.Context, tx pgx.Tx, pairs []classTriple) ([]int32, error) {
	if len(pairs) == 0 {
		return []int32{}, nil
	}
	doc, err := json.Marshal(pairs)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		rows, err := tx.Query(ctx, `
			WITH want AS (SELECT x.tags, x.parties, x.row_tags FROM jsonb_to_recordset($1::jsonb) AS x(tags text[], parties text[], row_tags text[])),
			ins AS (
				INSERT INTO object_fence_classes (tags, parties, row_tags)
				SELECT w.tags, w.parties, w.row_tags FROM want w
				WHERE NOT EXISTS (SELECT 1 FROM object_fence_classes c WHERE c.tags = w.tags AND c.parties = w.parties AND c.row_tags = w.row_tags)
				ORDER BY w.tags, w.parties, w.row_tags
				ON CONFLICT (tags, parties, row_tags) DO NOTHING
				RETURNING id)
			SELECT id FROM ins
			UNION SELECT c.id FROM object_fence_classes c JOIN want w ON c.tags = w.tags AND c.parties = w.parties AND c.row_tags = w.row_tags`, string(doc))
		if err != nil {
			return nil, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		if err != nil {
			return nil, err
		}
		if len(ids) == len(pairs) {
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			return ids, nil
		}
	}
	return nil, fmt.Errorf("objects: %d fence classes did not resolve to ids", len(pairs))
}

// readCandidates reads what history offers for one object: per (predicate,
// source, value type) the latest non-retired observation — all Current needs
// of the observations, since it ranks by source and keeps only declared types
// — and every value of every knowledge item that is a current resolution
// winner about the entity. An entity-scoped kind contributes only its
// cross-actor row (its per-actor rows are each speaker's word, kept for
// contradictions, ADR-0106). An observation's own row classification
// (o.classification, ADR-0158 D1) rides along as the candidate's RowTags.
func readCandidates(ctx context.Context, tx pgx.Tx, reg *domain.KindRegistry, ns, entity string) ([]Candidate, error) {
	var out []Candidate
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (o.predicate, COALESCE(e.source_id, ''), o.value_type)
			o.id, o.predicate, o.value_type, o.value_date, o.value_number, o.value_text, o.value_entity_id,
			o.occurred_at, COALESCE(e.source_id, ''), e.classification, e.parties, COALESCE(o.evidence_id, ''), o.classification
		FROM observations o
		LEFT JOIN evidence e ON e.id = o.evidence_id
		WHERE o.namespace_id = $1 AND o.entity_id = $2`+NotRetired("o")+`
		ORDER BY o.predicate, COALESCE(e.source_id, ''), o.value_type, o.occurred_at DESC, o.id DESC`, ns, entity)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Candidate
		var id int64
		var tags, parties, rowTags []string
		if err := scanValue(rows, &c, func(v ...any) []any {
			return append([]any{&id}, append(v, &c.At, &c.Source, &tags, &parties, &c.EvidenceID, &rowTags)...)
		}); err != nil {
			rows.Close()
			return nil, err
		}
		c.ID = fmt.Sprintf("o%020d", id)
		c.Tags, c.Parties = tags, parties
		if c.Tags != nil && c.Parties == nil {
			c.Parties = []string{}
		}
		if len(rowTags) > 0 { // '{}' is no row classification: nil, so the companion stays NULL
			c.RowTags = rowTags
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `
		SELECT r.kind, r.actor, ki.id, ki.asserted_by, ki.asserted_at, ki.classification, COALESCE(ki.evidence_id, ''),
			sv.predicate, sv.value_type, sv.value_date, sv.value_number, sv.value_text, sv.value_entity_id
		FROM resolutions r
		JOIN knowledge_items ki ON ki.id = r.item_id AND NOT ki.negation
		JOIN statement_values sv ON sv.item_id = ki.id
		WHERE r.namespace_id = $1 AND r.entity_id = $2 AND r.system_to IS NULL
		ORDER BY ki.id, sv.predicate`, ns, entity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var c Candidate
		var kind, actor, itemID string
		var tags []string
		if err := scanValue(rows, &c, func(v ...any) []any {
			return append([]any{&kind, &actor, &itemID, &c.Source, &c.At, &tags, &c.EvidenceID}, v...)
		}); err != nil {
			return nil, err
		}
		if reg.EntityScoped(kind) && actor != domain.ResolutionActorAll {
			continue
		}
		if key := itemID + "\x00" + c.Predicate; seen[key] {
			continue // one item can win two resolution rows
		} else {
			seen[key] = true
		}
		c.ID = "i" + itemID
		c.Tags, c.Parties = tags, []string{}
		if c.Tags == nil {
			c.Tags = []string{}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanValue scans a row whose typed-value columns (predicate, value_type,
// value_date, value_number, value_text, value_entity_id) sit where dest places
// them, into c.Predicate and c.Value.
func scanValue(rows pgx.Rows, c *Candidate, dest func(value ...any) []any) error {
	var date *time.Time
	var num *float64
	var text, ent *string
	if err := rows.Scan(dest(&c.Predicate, &c.Value.Type, &date, &num, &text, &ent)...); err != nil {
		return err
	}
	c.Value.Predicate = c.Predicate
	switch {
	case date != nil:
		c.Value.Date = date.UTC()
	case num != nil:
		c.Value.Number = *num
	case text != nil:
		c.Value.Text = *text
	case ent != nil:
		c.Value.EntityRef = *ent
	}
	c.At = c.At.UTC()
	return nil
}

func deleteRow(ctx context.Context, tx pgx.Tx, kind, ns, entity string) error {
	_, err := tx.Exec(ctx, `DELETE FROM `+ident(TableName(kind))+` WHERE namespace_id = $1 AND entity_id = $2`, ns, entity)
	if err != nil {
		return fmt.Errorf("objects: delete %s: %w", entity, err)
	}
	return nil
}

// provenance is a property's meta in the prov / extra jsonb: who said it, when,
// from which row and evidence, and whether another source disagrees.
type provenance struct {
	Type     string   `json:"t,omitempty"`
	Value    any      `json:"v,omitempty"`
	Source   string   `json:"src"`
	At       string   `json:"at"`
	ID       string   `json:"id"`
	Evidence string   `json:"ev,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Parties  []string `json:"parties,omitempty"`
	// RowTags is an extra entry's row classification (ADR-0158 D2; read by
	// sqlfence.ObjectExtraRowTags). omitempty: an entry whose row carries none is
	// byte-identical to one written before row format 3.
	RowTags  []string `json:"row_tags,omitempty"`
	Conflict bool     `json:"conflict"`
}

func provOf(p Property) provenance {
	return provenance{Source: p.Source, At: p.At.UTC().Format(time.RFC3339Nano), ID: p.ID,
		Evidence: p.EvidenceID, Conflict: p.Conflict}
}

// columnValue is a typed value as its column's parameter.
func columnValue(v domain.StatementValue) any {
	switch v.Type {
	case "date":
		return v.Date.UTC()
	case "number":
		return v.Number
	case "entity":
		return v.EntityRef
	default:
		return v.Text
	}
}

// jsonValue is a typed value inside the extra jsonb.
func jsonValue(v domain.StatementValue) any {
	if v.Type == "date" {
		return v.Date.UTC().Format(time.RFC3339Nano)
	}
	return columnValue(v)
}

// upsertRow writes one object's row. It names only the columns spec declares
// (a column another namespace's declaration added stays NULL for this row)
// and writes nothing when the row is unchanged.
func upsertRow(ctx context.Context, tx pgx.Tx, spec domain.KindSpec, declVersion int64, ns, entity string, st State, classes []int32) error {
	prov := map[string]provenance{}
	for p, v := range st.Props {
		prov[p] = provOf(v)
	}
	extra := map[string]provenance{}
	for p, v := range st.Extra {
		e := provOf(v)
		e.Type, e.Value, e.Tags, e.Parties, e.RowTags = v.Value.Type, jsonValue(v.Value), v.Tags, v.Parties, v.RowTags
		extra[p] = e
	}
	provJSON, err := json.Marshal(prov)
	if err != nil {
		return err
	}
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	var dv *int64
	if declVersion > 0 {
		dv = &declVersion
	}
	cols := []string{colNamespace, colEntity, colExtra, colProv, colDeclVersion, colVisClasses}
	args := []any{ns, entity, extraJSON, provJSON, dv, classes}
	preds := make([]string, 0, len(spec.Predicates))
	for p := range spec.Predicates {
		preds = append(preds, p)
	}
	sort.Strings(preds)
	for _, p := range preds {
		cols = append(cols, ValueColumn(p, spec.Predicates[p].Type), TagsColumn(p), PartiesColumn(p), RowTagsColumn(p))
		if v, ok := st.Props[p]; ok {
			// An empty row classification is written as NULL (the fence reads NULL
			// as empty): a row whose values carry none is then byte-identical to
			// what row format 2 wrote, and the format-3 rebuild skips it.
			var rowTags []string
			if len(v.RowTags) > 0 {
				rowTags = v.RowTags
			}
			args = append(args, columnValue(v.Value), v.Tags, v.Parties, rowTags)
		} else {
			args = append(args, nil, nil, nil, nil)
		}
	}
	quoted := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	var set, mine, theirs []string
	for i, c := range cols {
		quoted[i] = ident(c)
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		if i >= 2 {
			set = append(set, quoted[i]+" = EXCLUDED."+quoted[i])
			mine = append(mine, "t."+quoted[i])
			theirs = append(theirs, "EXCLUDED."+quoted[i])
		}
	}
	sql := `INSERT INTO ` + ident(TableName(spec.Kind)) + ` AS t (` + strings.Join(quoted, ", ") + `)
		VALUES (` + strings.Join(placeholders, ", ") + `)
		ON CONFLICT (` + colNamespace + `, ` + colEntity + `) DO UPDATE SET ` + strings.Join(set, ", ") + `, ` + colUpdatedAt + ` = now()
		WHERE ROW(` + strings.Join(mine, ", ") + `) IS DISTINCT FROM ROW(` + strings.Join(theirs, ", ") + `)`
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("objects: write %s: %w", entity, err)
	}
	return nil
}

// rebuildBatch is how many objects one rebuild transaction recomputes (each
// holds one advisory lock per object until it commits).
const rebuildBatch = 500

// Rebuild recomputes every row of t from history through the same function the
// incremental path uses (applyEntity), so a rebuild equals the incremental
// result; it is idempotent. It walks every entity history knows under t's
// prefixes plus every row already in t (a row whose entity is no longer an
// object of t's kind is removed), in batches, and finally records t.Hash as
// the declarations t was built from. Returns the number of entities walked.
//
// Writes that land while it runs maintain themselves (the live declarations
// were swapped in before it started) and serialise with it per object.
func Rebuild(ctx context.Context, pool *pgxpool.Pool, kinds domain.KindSource, t Table) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT namespace_id, entity_id FROM observations WHERE split_part(entity_id, '/', 1) = ANY($1)
		UNION SELECT namespace_id, entity_id FROM resolutions WHERE system_to IS NULL AND split_part(entity_id, '/', 1) = ANY($1)
		UNION SELECT namespace_id, entity_id FROM `+ident(t.Name)+`
		ORDER BY 1, 2`, t.Prefixes)
	if err != nil {
		return 0, fmt.Errorf("objects: rebuild %s: list: %w", t.Name, err)
	}
	type key struct{ ns, entity string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.ns, &k.entity); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for start := 0; start < len(keys); start += rebuildBatch {
		end := min(start+rebuildBatch, len(keys))
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			for _, k := range keys[start:end] {
				if err := applyEntity(ctx, tx, kinds, k.ns, k.entity, &t); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return start, fmt.Errorf("objects: rebuild %s: %w", t.Name, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO object_table_builds (table_name, spec_hash, built_at) VALUES ($1, $2, now())
		ON CONFLICT (table_name) DO UPDATE SET spec_hash = EXCLUDED.spec_hash, built_at = EXCLUDED.built_at`,
		t.Name, t.Hash); err != nil {
		return len(keys), fmt.Errorf("objects: rebuild %s: record: %w", t.Name, err)
	}
	return len(keys), nil
}
