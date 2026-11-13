package postgres

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func TestOrDefaultNamespace(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "default", "default": "default", "tenant-a": "tenant-a", " ": " "} {
		if got := orDefaultNamespace(in); got != want {
			t.Errorf("orDefaultNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTypedValueColumns(t *testing.T) {
	t.Parallel()
	ist := time.FixedZone("IST", 3*3600)
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, ist)

	date, num, text, ent, ok := typedValueColumns(domain.StatementValue{Type: "date", Date: at})
	if !ok || date == nil || num != nil || text != nil || ent != nil {
		t.Fatalf("date: wrong columns set: %v %v %v %v %v", date, num, text, ent, ok)
	}
	if date.Location() != time.UTC || !date.Equal(at) {
		t.Errorf("date must be the same instant in UTC, got %v", date)
	}

	_, num, _, _, ok = typedValueColumns(domain.StatementValue{Type: "number", Number: 0})
	if !ok || num == nil || *num != 0 {
		t.Errorf("number 0 must be stored, not dropped: %v %v", num, ok)
	}

	_, _, text, _, ok = typedValueColumns(domain.StatementValue{Type: "text", Text: ""})
	if !ok || text == nil || *text != "" {
		t.Errorf("empty text must be stored as '': %v %v", text, ok)
	}

	d, n, tx, ent, ok := typedValueColumns(domain.StatementValue{Type: "entity", EntityRef: "ent:x"})
	if !ok || ent == nil || *ent != "ent:x" || d != nil || n != nil || tx != nil {
		t.Errorf("entity: %v %v %v %v %v", d, n, tx, ent, ok)
	}

	d, n, tx, ent, ok = typedValueColumns(domain.StatementValue{Type: "money", Text: "x"})
	if ok || d != nil || n != nil || tx != nil || ent != nil {
		t.Errorf("unknown type must report !ok with no columns: %v %v %v %v %v", d, n, tx, ent, ok)
	}
}
