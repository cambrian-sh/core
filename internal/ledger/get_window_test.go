package ledger

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a: resolve_cid on a led_
// id is ledger_get, so ledger_get is the ONE bounded read: every page says how much
// there is and the next call, and find searches inside one entry.
func TestLedgerGet_PageNoteAndFind(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, nil, RecorderConfig{PrivateByDefault: true})
	body := strings.Repeat("0123456789", 9) + "Leipzig is the pilot city." // 116 bytes, 3 pages of 40
	e, _ := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: prov,
		Writer: domain.AgentPrincipal("worker"), Body: []byte(body)})
	get, _ := toolsOver(st)
	alice := domain.WithScope(context.Background(), &domain.TagPredicate{Bypass: true, Owners: []string{"alice"}})

	m := call(t, get, alice, `{"id":"`+e.ID+`"}`)
	if m["page"] != float64(1) || m["pages"] != float64(3) || m["total_chars"] != float64(len(body)) || len(m["text"].(string)) != 40 {
		t.Fatalf("page 1: %v", m)
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "116 chars total, showing 1–40 (page 1 of 3)") ||
		!strings.Contains(note, "next: ledger_get id='"+e.ID+"' page=2") || !strings.Contains(note, "find=") {
		t.Fatalf("note = %q", note)
	}
	if m = call(t, get, alice, `{"id":"`+e.ID+`","page":3}`); strings.Contains(m["note"].(string), "next:") {
		t.Fatalf("the last page must not offer a next page: %v", m["note"])
	}

	m = call(t, get, alice, `{"id":"`+e.ID+`","find":"leipzig"}`)
	matches, _ := m["matches"].([]any)
	if m["match_count"] != float64(1) || len(matches) != 1 {
		t.Fatalf("find: %v", m)
	}
	hit := matches[0].(map[string]any)
	if hit["page"] != float64(3) || !strings.Contains(hit["snippet"].(string), "Leipzig") {
		t.Fatalf("hit = %v", hit)
	}
	if _, hasText := m["text"]; hasText {
		t.Fatal("a find answer is a pointer into the entry, not a page of it")
	}
}
