package objects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// The generator (ADR-0149 D3): one function from the live declarations to the
// DDL of every objects_<kind> table. Every statement is idempotent and additive
// (CREATE TABLE / ADD COLUMN / CREATE INDEX ... IF NOT EXISTS): a breaking
// declarations change is refused before it gets here (domain.ClassifyOntologyChange),
// so nothing is ever dropped or retyped.
//
// One table per kind name across every namespace (namespace_id is part of the
// key). A property's value column carries its type in its name
// (<property>__<type>), so two namespaces declaring one property with two types
// get two columns instead of a conflict; its visibility companions
// (<property>__tags, <property>__parties) are shared.

// Fixed columns of every object table.
const (
	colNamespace   = "namespace_id"
	colEntity      = "entity_id"
	colExtra       = "extra"
	colProv        = "prov"
	colDeclVersion = "decl_version"
	colUpdatedAt   = "updated_at"
)

// TableName is the object table of kind.
func TableName(kind string) string { return "objects_" + kind }

// ValueColumn is the typed column of a property declared with valueType.
func ValueColumn(property, valueType string) string { return property + "__" + valueType }

// TagsColumn and PartiesColumn are a property's visibility companions: the
// classification and parties of the row its current value came from, so the
// fence applies per property in filter, sort and aggregate (P4).
func TagsColumn(property string) string    { return property + "__tags" }
func PartiesColumn(property string) string { return property + "__parties" }

// sqlTypes maps a declared value type to its column type.
var sqlTypes = map[string]string{
	"date":   "timestamptz",
	"number": "double precision",
	"text":   "text",
	"entity": "text",
}

// Column is one generated column.
type Column struct {
	Name    string
	SQLType string
}

// Table is one generated object table: everything its DDL and its rebuild need.
type Table struct {
	Kind string
	Name string
	// Prefixes are the entity id stems of this kind's objects across the
	// namespaces' declarations (a rebuild reads history by them).
	Prefixes []string
	// Columns are the property columns, sorted by name.
	Columns []Column
	// Indexed are the value columns with a btree index (ValueSpec.Indexed).
	Indexed []string
	// Hash fingerprints the declarations the rows are derived from: the
	// distinct specs of this kind across the snapshots. A table whose recorded
	// hash differs is rebuilt.
	Hash string
}

// PlanTables lists the object tables the snapshots declare, sorted by name.
// Pure: same snapshots, same tables.
func PlanTables(snaps []*domain.OntologySnapshot) []Table {
	type acc struct {
		prefixes map[string]bool
		columns  map[string]string
		indexed  map[string]bool
		specs    map[string]bool
	}
	byKind := map[string]*acc{}
	for _, snap := range snaps {
		if snap == nil {
			continue
		}
		for _, d := range snap.Doc.Kinds {
			spec, ok := snap.Kinds.Spec(d.Kind)
			if !ok || !spec.Table {
				continue
			}
			a := byKind[spec.Kind]
			if a == nil {
				a = &acc{prefixes: map[string]bool{}, columns: map[string]string{}, indexed: map[string]bool{}, specs: map[string]bool{}}
				byKind[spec.Kind] = a
			}
			a.prefixes[spec.ObjectPrefix()] = true
			for p, v := range spec.Predicates {
				vc := ValueColumn(p, v.Type)
				a.columns[vc] = sqlTypes[v.Type]
				a.columns[TagsColumn(p)] = "text[]"
				a.columns[PartiesColumn(p)] = "text[]"
				if v.Indexed {
					a.indexed[vc] = true
				}
			}
			raw, _ := json.Marshal(spec) // a KindSpec always marshals
			a.specs[string(raw)] = true
		}
	}
	out := make([]Table, 0, len(byKind))
	for kind, a := range byKind {
		t := Table{Kind: kind, Name: TableName(kind), Prefixes: sortedKeys(a.prefixes), Indexed: sortedKeys(a.indexed)}
		names := make([]string, 0, len(a.columns))
		for n := range a.columns {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			t.Columns = append(t.Columns, Column{Name: n, SQLType: a.columns[n]})
		}
		h := sha256.New()
		for _, s := range sortedKeys(a.specs) {
			h.Write([]byte(s))
			h.Write([]byte{0})
		}
		t.Hash = hex.EncodeToString(h.Sum(nil))
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ident quotes an identifier. Every name reaching it was built from a
// declaration the kind registry checked (plain lowercase snake case), and is
// quoted anyway.
func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// indexName is a short, stable name for the index on one column (Postgres
// truncates identifiers at 63 bytes; a hash never collides by truncation).
func indexName(table, column string) string {
	sum := sha256.Sum256([]byte(table + "." + column))
	return "ox_" + hex.EncodeToString(sum[:])[:24]
}

// DDL is the idempotent statements creating t and every column and index it
// declares. Pure.
func (t Table) DDL() []string {
	tbl := ident(t.Name)
	out := []string{fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	%s text NOT NULL,
	%s text NOT NULL,
	%s jsonb NOT NULL DEFAULT '{}',
	%s jsonb NOT NULL DEFAULT '{}',
	%s bigint,
	%s timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (%s, %s))`, tbl,
		colNamespace, colEntity, colExtra, colProv, colDeclVersion, colUpdatedAt, colNamespace, colEntity)}
	for _, c := range t.Columns {
		out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`, tbl, ident(c.Name), c.SQLType))
	}
	for _, c := range t.Indexed {
		out = append(out, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s)`,
			ident(indexName(t.Name, c)), tbl, colNamespace, ident(c)))
	}
	return out
}

// Generate brings the database's object tables up to the snapshots' declarations
// inside tx (the caller's declarations transaction, under its lock, so the DDL
// commits or rolls back with the change that asked for it) and returns the
// tables whose rows must be rebuilt: new ones, and ones whose declarations
// changed since their last rebuild.
func Generate(ctx context.Context, tx pgx.Tx, snaps []*domain.OntologySnapshot) ([]Table, error) {
	tables := PlanTables(snaps)
	var stale []Table
	for _, t := range tables {
		for _, stmt := range t.DDL() {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("objects: generate %s: %w", t.Name, err)
			}
		}
		var built string
		err := tx.QueryRow(ctx, `SELECT spec_hash FROM object_table_builds WHERE table_name = $1`, t.Name).Scan(&built)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("objects: generate %s: %w", t.Name, err)
		}
		if built != t.Hash {
			stale = append(stale, t)
		}
	}
	return stale, nil
}
