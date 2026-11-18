package migrate

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// idx_links_answers (ENG-2050) is partial on exactly the clause the review
// inbox's answers probe repeats (postgres openCandidate), built from the same
// marker; a different prefix here would leave the probe unindexed.
func TestLinksAnswersIndexMatchesTheMarker(t *testing.T) {
	for _, ix := range concurrentIndexes {
		if ix.name != "idx_links_answers" {
			continue
		}
		want := "WHERE source_ref LIKE '" + domain.LinkAnswersSourceRefPrefix + "%' AND retracted_at IS NULL"
		if ix.table != "links" || !strings.HasSuffix(ix.using, want) {
			t.Fatalf("idx_links_answers = %+v, want a partial index on links ending %q", ix, want)
		}
		return
	}
	t.Fatal("idx_links_answers is not in concurrentIndexes")
}
