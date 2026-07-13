package scope_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"
)

// --- fakes -------------------------------------------------------------------

type fakeReader struct{ docs []domain.Document }

func (f *fakeReader) ReadTier0(context.Context, time.Time) ([]domain.Document, error) {
	return f.docs, nil
}

// oneCluster lumps all docs into a single theme cluster.
type oneCluster struct{}

func (oneCluster) Cluster(docs []domain.Document) []scope.ThemeCluster {
	if len(docs) == 0 {
		return nil
	}
	return []scope.ThemeCluster{{Docs: docs}}
}

// echoGen returns a fixed insight, optionally embedding PII to test the masker.
type echoGen struct{ out string }

func (g echoGen) ExtractInsight(context.Context, []domain.Document) (string, error) {
	return g.out, nil
}

type capturingWriter struct{ docs map[string]*domain.Document }

func newWriter() *capturingWriter { return &capturingWriter{docs: map[string]*domain.Document{}} }
func (w *capturingWriter) UpsertInsight(_ context.Context, key string, doc *domain.Document) error {
	w.docs[key] = doc
	return nil
}

func tier0Doc(session, hash string) domain.Document {
	return domain.Document{Metadata: map[string]interface{}{
		"source_session": session,
		"source_hash":    hash,
		"tags":           []string{"chat_raw"},
	}}
}

func TestPromote_SubKClusterNotPromoted(t *testing.T) {
	docs := []domain.Document{tier0Doc("s1", "h1"), tier0Doc("s2", "h2")} // 2 < K=5
	w := newWriter()
	p := scope.NewPromoter(&fakeReader{docs}, oneCluster{}, echoGen{"insight"}, nil, w, scope.NewInMemoryLedger(), 5, nil)

	if err := p.PromoteBatch(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(w.docs) != 0 {
		t.Errorf("sub-K cluster must NOT be promoted, got %d", len(w.docs))
	}
}

func fiveSessions() []domain.Document {
	return []domain.Document{
		tier0Doc("s1", "h1"), tier0Doc("s2", "h2"), tier0Doc("s3", "h3"),
		tier0Doc("s4", "h4"), tier0Doc("s5", "h5"),
	}
}

func TestPromote_KReachedPromotes(t *testing.T) {
	w := newWriter()
	p := scope.NewPromoter(&fakeReader{fiveSessions()}, oneCluster{}, echoGen{"checkout slowness is the #1 complaint"}, nil, w, scope.NewInMemoryLedger(), 5, nil)

	if err := p.PromoteBatch(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(w.docs) != 1 {
		t.Fatalf(">=K cluster must be promoted once, got %d", len(w.docs))
	}
	for _, d := range w.docs {
		// ADR-0035 C2: classification tags are derived by the ScopedStoreWriter
		// (from the Consolidator's DefaultWriteTags), not set by the Promoter. The
		// Promoter sets the tier marker; the writer stamps company_wide/analytics/derived.
		if d.Metadata["tier"] != "derived" {
			t.Errorf("promoted doc must be tier=derived, got %+v", d.Metadata)
		}
	}
}

func TestPromote_MaskerScrubsOutput(t *testing.T) {
	w := newWriter()
	// LLM echoes an email the masker must scrub.
	p := scope.NewPromoter(&fakeReader{fiveSessions()}, oneCluster{}, echoGen{"contact jane@example.com about returns"}, nil, w, scope.NewInMemoryLedger(), 5, nil)
	_ = p.PromoteBatch(context.Background(), time.Time{})

	for _, d := range w.docs {
		if strings.Contains(d.Text, "jane@example.com") {
			t.Errorf("RegexPIIMasker must scrub email from promoted text, got %q", d.Text)
		}
	}
}

func TestPromote_IsIdempotent(t *testing.T) {
	ledger := scope.NewInMemoryLedger()
	w := newWriter()
	p := scope.NewPromoter(&fakeReader{fiveSessions()}, oneCluster{}, echoGen{"insight"}, nil, w, ledger, 5, nil)

	_ = p.PromoteBatch(context.Background(), time.Time{})
	first := len(w.docs)
	// Second run: the ledger excludes already-promoted hashes → nothing new.
	_ = p.PromoteBatch(context.Background(), time.Time{})
	if len(w.docs) != first {
		t.Errorf("re-run must be idempotent (ledger dedup), got %d then %d", first, len(w.docs))
	}
}

func TestPromote_GrownClusterSupersedes(t *testing.T) {
	ledger := scope.NewInMemoryLedger()
	w := newWriter()

	// First pass: 5 sessions → key K1.
	p1 := scope.NewPromoter(&fakeReader{fiveSessions()}, oneCluster{}, echoGen{"insight"}, nil, w, ledger, 5, nil)
	_ = p1.PromoteBatch(context.Background(), time.Time{})
	var firstKey string
	for k := range w.docs {
		firstKey = k
	}

	// Second pass: a grown cluster (7 sessions incl. 2 new) → new key, supersedes K1.
	grown := append(fiveSessions(), tier0Doc("s6", "h6"), tier0Doc("s7", "h7"))
	p2 := scope.NewPromoter(&fakeReader{grown}, oneCluster{}, echoGen{"insight v2"}, nil, w, ledger, 5, nil)
	_ = p2.PromoteBatch(context.Background(), time.Time{})

	if len(w.docs) != 2 {
		t.Fatalf("grown cluster must write a new (superseding) insight, got %d", len(w.docs))
	}
	var supersededFound bool
	for k, d := range w.docs {
		if k == firstKey {
			continue
		}
		if d.Metadata["supersedes"] == firstKey {
			supersededFound = true
		}
	}
	if !supersededFound {
		t.Errorf("grown insight must mark supersedes=%s", firstKey)
	}
}

// The Consolidator profile structurally blocks secrets/PII/internal_only.
func TestScopeConsolidator_ForbidsSensitive(t *testing.T) {
	for _, tag := range []string{"secrets", "internal_only", "PII"} {
		if !domain.ScopeConsolidator.Forbids(tag) {
			t.Errorf("ScopeConsolidator must forbid %q", tag)
		}
	}
}
