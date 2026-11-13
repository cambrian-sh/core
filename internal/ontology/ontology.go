// Package ontology holds the live ontology declarations (ADR-0149 D2, D5): the
// kind and relation declarations each namespace is validated against, as
// versioned rows in ontology_declarations and as one immutable snapshot per
// namespace behind an atomic swap.
//
// Its surface is small on purpose:
//
//   - Load seeds and reads the declarations at boot;
//   - Current / KindsFor / RelationsFor return a namespace's snapshot (the
//     stores take an *Ontology as their domain.KindSource / RelationSource);
//   - Apply classifies a proposed change and applies it unless it is breaking;
//   - Violations lists existing rows the current declarations would refuse.
//
// The registries themselves are unchanged and still the only validators
// (domain.KindRegistry, domain.RelationRegistry): this package only decides
// which immutable pair is current. Apply is the one mutator, and its only
// effect on memory is a single atomic pointer swap — a write that already took
// a snapshot keeps validating against it.
package ontology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// DefaultNamespace is where a change or a lookup that names no namespace goes
// (the substrate stores' default).
const DefaultNamespace = "default"

// BootAuthor is the author of a base row.
const BootAuthor = "boot"

// lockKey serialises declaration writers across processes (pg_advisory_xact_lock).
const lockKey int64 = 0x0149_0d2

// Ontology is the live declarations holder. Build it with Load.
type Ontology struct {
	pool        *pgxpool.Pool
	authorities []domain.ResolutionAuthority
	seed        domain.OntologyDoc

	mu  sync.Mutex // serialises Apply in this process
	cur atomic.Pointer[state]
}

// state is one immutable generation: the base snapshot and every namespace that
// has its own declarations. Replaced whole, never edited.
type state struct {
	base *domain.OntologySnapshot
	ns   map[string]*domain.OntologySnapshot
}

var (
	_ domain.KindSource     = (*Ontology)(nil)
	_ domain.RelationSource = (*Ontology)(nil)
)

// Load builds the live ontology at boot.
//
// seed is what code and deployment config declare today (plugin kinds,
// CAMBRIAN_SUBSTRATE_KINDS, built-ins, the config's relations). It is validated
// FIRST, by the same registry constructors and with the same refusals as before
// this package existed, so a malformed seed still refuses the boot with the
// same message. When it differs from the last base row it is appended as a new
// base row (the first boot writes version 1); then every namespace's changes are
// replayed over it. A replayed change that no longer validates over a changed
// seed refuses the boot, naming the namespace — never a silent fallback.
func Load(ctx context.Context, pool *pgxpool.Pool, seed domain.OntologyDoc, authorities []domain.ResolutionAuthority) (*Ontology, error) {
	if _, err := domain.NewOntologySnapshot(domain.OntologyBaseNamespace, 0, seed, authorities); err != nil {
		return nil, err
	}
	o := &Ontology{pool: pool, authorities: authorities, seed: seed}
	rows, baseVersion, err := o.seedAndRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("ontology declarations: %w", err)
	}
	st, err := build(seed, baseVersion, rows, authorities)
	if err != nil {
		return nil, err
	}
	o.cur.Store(st)
	return o, nil
}

// row is one persisted version.
type row struct {
	version   int64
	namespace string
	doc       domain.OntologyDoc
}

// seedAndRead appends the seed as a base row when it changed and returns the
// namespace rows (in version order) and the current base version.
func (o *Ontology) seedAndRead(ctx context.Context) ([]row, int64, error) {
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return nil, 0, err
	}
	all, err := readRows(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	var nsRows []row
	var lastBase *row
	var maxVersion int64
	for i := range all {
		maxVersion = max(maxVersion, all[i].version)
		if all[i].namespace == domain.OntologyBaseNamespace {
			lastBase = &all[i]
		} else {
			nsRows = append(nsRows, all[i])
		}
	}
	want, err := o.seed.Canonical()
	if err != nil {
		return nil, 0, err
	}
	baseVersion := int64(0)
	if lastBase != nil {
		have, err := lastBase.doc.Canonical()
		if err != nil {
			return nil, 0, err
		}
		if string(have) == string(want) {
			baseVersion = lastBase.version
		}
	}
	if baseVersion == 0 {
		baseVersion = maxVersion + 1
		if err := insertRow(ctx, tx, baseVersion, domain.OntologyBaseNamespace, o.seed, BootAuthor); err != nil {
			return nil, 0, err
		}
	}
	return nsRows, baseVersion, tx.Commit(ctx)
}

func readRows(ctx context.Context, q pgx.Tx) ([]row, error) {
	rs, err := q.Query(ctx, `SELECT version, namespace, declarations FROM ontology_declarations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		var raw []byte
		if err := rs.Scan(&r.version, &r.namespace, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &r.doc); err != nil {
			return nil, fmt.Errorf("version %d: %w", r.version, err)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func insertRow(ctx context.Context, tx pgx.Tx, version int64, namespace string, doc domain.OntologyDoc, author string) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ontology_declarations (version, namespace, declarations, author)
		VALUES ($1, $2, $3, $4)`, version, namespace, raw, author)
	return err
}

// build replays rows over the seed: a namespace's snapshot is the seed overlaid
// by its rows in version order, at the higher of the base version and its last row.
func build(seed domain.OntologyDoc, baseVersion int64, rows []row, authorities []domain.ResolutionAuthority) (*state, error) {
	base, err := domain.NewOntologySnapshot(domain.OntologyBaseNamespace, baseVersion, seed, authorities)
	if err != nil {
		return nil, err
	}
	docs := map[string]domain.OntologyDoc{}
	versions := map[string]int64{}
	for _, r := range rows {
		d, ok := docs[r.namespace]
		if !ok {
			d = seed
		}
		docs[r.namespace] = d.Overlay(r.doc)
		versions[r.namespace] = max(baseVersion, r.version)
	}
	st := &state{base: base, ns: map[string]*domain.OntologySnapshot{}}
	for ns, d := range docs {
		snap, err := domain.NewOntologySnapshot(ns, versions[ns], d, authorities)
		if err != nil {
			return nil, fmt.Errorf("ontology declarations of namespace %q no longer validate over the boot declarations "+
				"(fix the conflicting kind or relation in code or config): %w", ns, err)
		}
		st.ns[ns] = snap
	}
	return st, nil
}

func orDefault(ns string) string {
	if ns == "" {
		return DefaultNamespace
	}
	return ns
}

// Current returns namespace's snapshot: its own declarations, or the base when
// it has none. Never nil after Load.
func (o *Ontology) Current(namespace string) *domain.OntologySnapshot {
	st := o.cur.Load()
	if s, ok := st.ns[orDefault(namespace)]; ok {
		return s
	}
	return st.base
}

// KindsFor implements domain.KindSource.
func (o *Ontology) KindsFor(namespace string) *domain.KindRegistry {
	return o.Current(namespace).Kinds
}

// RelationsFor implements domain.RelationSource.
func (o *Ontology) RelationsFor(namespace string) *domain.RelationRegistry {
	return o.Current(namespace).Relations
}

// Apply proposes change (specs to add, or to replace by name) to namespace's
// declarations. It always returns the classification. A breaking change, a
// change whose result does not validate, or one raced by another writer is
// refused with domain.ErrOntologyChangeRefused and applies nothing. Otherwise,
// unless dryRun, the change is appended as a new version and the namespace's
// snapshot is swapped: the next write validates against it, with no restart.
// A tightening applies to new writes only; existing rows are reported by
// Violations, never rejected.
func (o *Ontology) Apply(ctx context.Context, namespace string, change domain.OntologyDoc, author string, dryRun bool) (domain.OntologyApplyResult, error) {
	return o.apply(ctx, namespace, change, author, dryRun, nil)
}

// applyCheck is an extra precondition on the snapshot a change is classified
// against, evaluated under the same lock as the classification and the write
// (an approved draft's "still classifies as drafted", drafts.go). A non-nil
// error refuses the change before anything else is decided.
type applyCheck func(cur *domain.OntologySnapshot, changes []domain.OntologyChange) error

// apply is Apply with an optional check: one body, so a draft's apply runs the
// same classification, breaking refusal, validation and write as an operator's.
func (o *Ontology) apply(ctx context.Context, namespace string, change domain.OntologyDoc, author string, dryRun bool, check applyCheck) (domain.OntologyApplyResult, error) {
	ns := orDefault(namespace)
	if ns == domain.OntologyBaseNamespace {
		return domain.OntologyApplyResult{}, fmt.Errorf("%w: namespace %q holds the boot declarations; "+
			"they change in code or config", domain.ErrOntologyChangeRefused, ns)
	}
	if strings.TrimSpace(author) == "" {
		return domain.OntologyApplyResult{}, fmt.Errorf("%w: an author is required", domain.ErrOntologyChangeRefused)
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	cur := o.Current(ns)
	next := cur.Doc.Overlay(change)
	changes := domain.ClassifyOntologyChange(cur.Doc, next)
	res := domain.OntologyApplyResult{Namespace: ns, Version: cur.Version,
		Class: domain.MaxChangeClass(changes), Changes: changes}
	if check != nil {
		if err := check(cur, changes); err != nil {
			return res, fmt.Errorf("%w: %w", domain.ErrOntologyChangeRefused, err)
		}
	}
	if len(changes) == 0 {
		return res, nil
	}
	if res.Class == domain.ChangeBreaking {
		return res, fmt.Errorf("%w: breaking changes need an explicit migration, which this API does not take: %s",
			domain.ErrOntologyChangeRefused, describe(changes, domain.ChangeBreaking))
	}
	if _, err := domain.NewOntologySnapshot(ns, 0, next, o.authorities); err != nil {
		return res, fmt.Errorf("%w: %w", domain.ErrOntologyChangeRefused, err)
	}
	if dryRun {
		return res, nil
	}

	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return res, err
	}
	var latest, maxVersion int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version) FILTER (WHERE namespace = ANY($1)), 0), COALESCE(MAX(version), 0)
		FROM ontology_declarations`, []string{domain.OntologyBaseNamespace, ns}).Scan(&latest, &maxVersion); err != nil {
		return res, err
	}
	if latest != cur.Version {
		return res, fmt.Errorf("%w: namespace %q is at version %d in the database but %d in this kernel "+
			"(changed by another process); restart to reload", domain.ErrOntologyChangeRefused, ns, latest, cur.Version)
	}
	version := maxVersion + 1
	if err := insertRow(ctx, tx, version, ns, change, author); err != nil {
		return res, err
	}
	snap, err := domain.NewOntologySnapshot(ns, version, next, o.authorities)
	if err != nil {
		return res, err // validated above; unreachable
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	o.swap(ns, snap)
	res.Version, res.Applied = version, true
	return res, nil
}

// swap installs snap as namespace's snapshot: the one mutation of in-memory
// state, a copy-on-write of the map behind one atomic store.
func (o *Ontology) swap(ns string, snap *domain.OntologySnapshot) {
	old := o.cur.Load()
	next := &state{base: old.base, ns: make(map[string]*domain.OntologySnapshot, len(old.ns)+1)}
	for k, v := range old.ns {
		next.ns[k] = v
	}
	next.ns[ns] = snap
	o.cur.Store(next)
}

func describe(changes []domain.OntologyChange, class domain.OntologyChangeClass) string {
	var parts []string
	for _, c := range changes {
		if c.Class == class {
			parts = append(parts, c.Subject+": "+c.Detail)
		}
	}
	return strings.Join(parts, "; ")
}

// errNilScope refuses a violations read with no access predicate (fail closed).
var errNilScope = errors.New("ontology violations: no access predicate")
