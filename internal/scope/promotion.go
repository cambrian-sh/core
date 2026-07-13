package scope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// defaultKAnonymityFloor is the minimum distinct source sessions a cluster must
// span before promotion. ADR-0034 (D11).
const defaultKAnonymityFloor = 5

// ThemeCluster is a group of Tier-0 documents sharing a theme, produced by a
// ThemeClusterer. Promotion eligibility is decided by DistinctSessions, NOT by
// document count — this is what makes the output structurally non-re-identifiable.
type ThemeCluster struct {
	Docs []domain.Document
}

// DistinctSessions counts the unique source sessions contributing to the cluster
// (the k-anonymity denominator). Sessions are read from Document.Metadata
// "source_session" (falling back to "session_id").
func (c ThemeCluster) DistinctSessions() int {
	seen := map[string]struct{}{}
	for _, d := range c.Docs {
		if s := sessionOf(d); s != "" {
			seen[s] = struct{}{}
		}
	}
	return len(seen)
}

// SourceHashes returns the sorted contributing source hashes (irreversible — for
// audit and the dedup key, never re-identification).
func (c ThemeCluster) SourceHashes() []string {
	out := make([]string, 0, len(c.Docs))
	for _, d := range c.Docs {
		if h := metaString(d, "source_hash"); h != "" {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

func sessionOf(d domain.Document) string {
	if s := metaString(d, "source_session"); s != "" {
		return s
	}
	return metaString(d, "session_id")
}

func metaString(d domain.Document, key string) string {
	if d.Metadata == nil {
		return ""
	}
	if v, ok := d.Metadata[key].(string); ok {
		return v
	}
	return ""
}

// --- injected collaborators (kept small so the orchestration is testable) ----

// Tier0Reader returns scope-filtered Tier-0 documents in a lookback window. The
// implementation MUST run under ScopeConsolidator (tag filtering enforced — no
// secrets/PII/internal_only ever enters the read set).
type Tier0Reader interface {
	ReadTier0(ctx context.Context, since time.Time) ([]domain.Document, error)
}

// ThemeClusterer groups Tier-0 docs into theme clusters.
type ThemeClusterer interface {
	Cluster(docs []domain.Document) []ThemeCluster
}

// Generalizer extracts a single anonymized insight from a cluster's docs. The LLM
// is confined to generalization here; it is NEVER the security boundary.
type Generalizer interface {
	ExtractInsight(ctx context.Context, docs []domain.Document) (string, error)
}

// PromotionWriter upserts a derived insight at a broader scope, keyed by dedupKey
// for idempotency. A re-run on the same cluster upserts; a grown cluster (new key)
// supersedes the prior insight.
type PromotionWriter interface {
	UpsertInsight(ctx context.Context, key string, doc *domain.Document) error
}

// PromotionLedger is the OUT-OF-BAND idempotency record (cluster key + source_hash
// → key). Raw Tier-0 docs are NEVER mutated, preserving the append-only audit
// guarantee. Idempotency is keyed by the CLUSTER (dedupKey of its sorted source
// hashes), not by individual docs — this lets a grown cluster (new key) re-form and
// supersede its narrower predecessor while an unchanged cluster is skipped.
type PromotionLedger interface {
	// AlreadyPromoted reports whether this exact cluster (dedup key) was emitted.
	AlreadyPromoted(key string) bool
	// Record marks the cluster key + its source hashes as promoted.
	Record(sourceHashes []string, key string) error
	// PriorKeyFor returns the dedup key of an earlier narrower insight that any of
	// the cluster's source hashes was promoted under, so it can be superseded.
	PriorKeyFor(cluster ThemeCluster) string
}

// Promoter runs the deterministic, audited Raw→Derived promotion (ADR-0034 D11).
// Safety rests entirely on deterministic gates — scope membership (enforced by the
// reader), a counted k-anonymity floor, and a mandatory regex PII scrub — never on
// model behavior.
type Promoter struct {
	reader    Tier0Reader
	clusterer ThemeClusterer
	gen       Generalizer
	masker    domain.PIIMasker
	writer    PromotionWriter
	ledger    PromotionLedger
	kFloor    int
	logger    *slog.Logger
}

// NewPromoter builds a Promoter. kFloor<=1 uses the default (5); a nil logger uses
// slog.Default(). The masker is mandatory — a nil masker is replaced with a
// RegexPIIMasker so the deterministic backstop can never be silently skipped.
func NewPromoter(reader Tier0Reader, clusterer ThemeClusterer, gen Generalizer, masker domain.PIIMasker, writer PromotionWriter, ledger PromotionLedger, kFloor int, logger *slog.Logger) *Promoter {
	if kFloor <= 1 {
		kFloor = defaultKAnonymityFloor
	}
	if masker == nil {
		masker = domain.NewRegexPIIMasker()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Promoter{reader: reader, clusterer: clusterer, gen: gen, masker: masker, writer: writer, ledger: ledger, kFloor: kFloor, logger: logger}
}

// PromoteBatch performs one cross-session promotion pass over Tier-0 docs since the
// lookback floor. It is idempotent (ledger-deduped) and one-way (raw docs untouched).
func (p *Promoter) PromoteBatch(ctx context.Context, since time.Time) error {
	docs, err := p.reader.ReadTier0(ctx, since)
	if err != nil {
		return err
	}

	for _, cluster := range p.clusterer.Cluster(docs) {
		// Aggregation floor (k-anonymity): only clusters spanning >= K distinct
		// SESSIONS are eligible; per-record promotion is forbidden.
		if cluster.DistinctSessions() < p.kFloor {
			continue
		}

		hashes := cluster.SourceHashes()
		key := dedupKey(hashes)
		// Idempotency: this exact cluster was already promoted → skip. A GROWN
		// cluster has a different key, so it falls through and supersedes below.
		if p.ledger.AlreadyPromoted(key) {
			continue
		}

		insight, err := p.gen.ExtractInsight(ctx, cluster.Docs) // LLM = generalizer only
		if err != nil {
			p.logger.WarnContext(ctx, "promotion: insight extraction failed; skipping cluster",
				slog.String("event", "promotion_extract_error"), slog.Any("err", err))
			continue
		}
		// Deterministic backstop: mandatory regex PII scrub of the LLM output.
		insight = p.masker.Mask(insight)

		doc := &domain.Document{
			Text: insight,
			Metadata: map[string]interface{}{
				"tier":          "derived",
				"source_hashes": hashes, // irreversible — audit, not re-id
				"session_count": cluster.DistinctSessions(),
				"supersedes":    p.ledger.PriorKeyFor(cluster),
				// ADR-0035 C2: classification tags are kernel-derived from the
				// Consolidator's DefaultWriteTags by the ScopedStoreWriter — not set here.
			},
		}
		if err := p.writer.UpsertInsight(ctx, key, doc); err != nil {
			p.logger.WarnContext(ctx, "promotion: upsert failed",
				slog.String("event", "promotion_upsert_error"), slog.Any("err", err))
			continue
		}
		if err := p.ledger.Record(hashes, key); err != nil {
			p.logger.WarnContext(ctx, "promotion: ledger record failed",
				slog.String("event", "promotion_ledger_error"), slog.Any("err", err))
			continue
		}
		p.logger.InfoContext(ctx, "promotion: derived insight written",
			slog.String("event", "scope_promotion"),
			slog.String("key", key),
			slog.Int("session_count", cluster.DistinctSessions()))
	}
	return nil
}

// dedupKey is the hash of the sorted contributing source hashes. A re-run on the
// same cluster yields the same key (upsert); a grown cluster yields a new key,
// superseding the prior insight. ADR-0034 (D11).
func dedupKey(sortedHashes []string) string {
	sum := sha256.Sum256([]byte(strings.Join(sortedHashes, "|")))
	return hex.EncodeToString(sum[:])
}
