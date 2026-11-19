package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1761: ledger_find's text filter used to see only the first 16 KiB of a
// text body (search_text's head). A word past the head is found now (the row is
// re-checked against its vault body), case-insensitively; a long body that does
// not hold the word is not returned; the head and metadata matches still work;
// and the limit applies to the matches, not to the long candidates.
func TestPg_LedgerFindTextSearchesPastTheIndexedHead(t *testing.T) {
	st, _, ctx := newLedgerStore(t)
	later := time.Now().Add(time.Hour)
	long := func(id, tail string) domain.LedgerEntry {
		body := strings.Repeat("filler line of an exported report\n", 1200) + tail // ~40 KiB
		e := pgEntry(id, domain.LedgerToolResult, "pgled-run-find", "pgled-turn-find", 1, "alice", body, later)
		e.Preview, e.ContentType = "report head", "text/plain"
		return e
	}
	base := time.Now().Add(-time.Hour)
	for i, e := range []domain.LedgerEntry{ // Find lists in creation order
		long("led_pgled_find_tail", "invoice INV-7731 overdue"),
		long("led_pgled_find_none", "nothing to see"),
		pgEntry("led_pgled_find_short", domain.LedgerToolResult, "pgled-run-find", "pgled-turn-find", 2, "alice", `{"invoice":"inv-7731"}`, later),
	} {
		e.CreatedAt = base.Add(time.Duration(i) * time.Second)
		if err := st.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q domain.LedgerQuery) []string {
		t.Helper()
		got, err := st.Find(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range got {
			out = append(out, e.ID)
		}
		return out
	}
	q := domain.LedgerQuery{RunID: "pgled-run-find", Text: "Inv-7731"}
	if got := ids(q); strings.Join(got, ",") != "led_pgled_find_tail,led_pgled_find_short" {
		t.Fatalf("find past the head = %v, want the long tail match and the short one", got)
	}
	q.Limit = 1
	if got := ids(q); len(got) != 1 || got[0] != "led_pgled_find_tail" {
		t.Fatalf("limit 1 = %v", got)
	}
	// A long row still matches on its metadata (the preview) without its body.
	if got := ids(domain.LedgerQuery{RunID: "pgled-run-find", Text: "REPORT HEAD"}); len(got) != 2 {
		t.Fatalf("preview match = %v, want both long rows", got)
	}
	if got := ids(domain.LedgerQuery{RunID: "pgled-run-find", Text: "absent-word"}); len(got) != 0 {
		t.Fatalf("absent word = %v", got)
	}
}
