package postgres

import (
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Small helpers shared by the substrate stores (ADR-0149 P0: one copy each).

// defaultNamespace is the namespace a substrate row lands in when its writer
// names none.
const defaultNamespace = "default"

// orDefaultNamespace returns ns, or defaultNamespace when ns is empty.
func orDefaultNamespace(ns string) string {
	if ns == "" {
		return defaultNamespace
	}
	return ns
}

// typedValueColumns splits a typed value into the four nullable value columns
// (value_date, value_number, value_text, value_entity_id) that observations and
// statement_values share: exactly one is non-nil, the one v.Type names; a date is
// stored in UTC. ok is false for an unknown type, which each caller reports in its
// own words.
func typedValueColumns(v domain.StatementValue) (date *time.Time, num *float64, text, ent *string, ok bool) {
	switch v.Type {
	case "date":
		d := v.Date.UTC()
		date = &d
	case "number":
		n := v.Number
		num = &n
	case "text":
		t := v.Text
		text = &t
	case "entity":
		e := v.EntityRef
		ent = &e
	default:
		return nil, nil, nil, nil, false
	}
	return date, num, text, ent, true
}

// declVersionOrNil renders a declarations version for a decl_version column
// (ADR-0149 D2): 0 — a registry built directly, not from an ontology snapshot —
// is stored NULL.
func declVersionOrNil(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}
