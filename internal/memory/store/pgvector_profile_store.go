package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ProfileStore combines domain.ProfileStore and domain.JudicialStore.
// It is the interface expected by MetabolismStack, SupervisionStack, and MemoryStack.
type ProfileStore interface {
	domain.ProfileStore
	domain.JudicialStore
	// HasInterviewVector reports whether a non-empty interview embedding is stored for
	// (agentID, sourceHash) — lets the startup backfill re-interview agents whose vector
	// is missing (e.g. written while the embedder was down).
	HasInterviewVector(ctx context.Context, agentID, sourceHash string) (bool, error)
}

// PgVectorProfileStore implements domain.ProfileStore and domain.JudicialStore
// by persisting AgentProfiles and Judicial Records as documents in a VectorStore.
type PgVectorProfileStore struct {
	store domain.VectorStore
}

// NewProfileStore returns a PgVectorProfileStore backed by the given VectorStore.
func NewProfileStore(store domain.VectorStore) *PgVectorProfileStore {
	return &PgVectorProfileStore{store: store}
}

// SaveProfile serialises the AgentProfile to JSON, builds a Document with
// DocumentType = domain.DocTypeAgentProfile, and persists it via VectorStore.Save.
func (p *PgVectorProfileStore) SaveProfile(ctx context.Context, agentID, sourceHash string, embedding []float32, profile domain.AgentProfile) error {
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("pgVectorProfileStore: marshal profile for agent %s: %w", agentID, err)
	}

	doc := &domain.Document{
		ID:           fmt.Sprintf("profile:%s:%s", agentID, sourceHash),
		DocumentType: domain.DocTypeAgentProfile,
		Text:         string(profileJSON),
		Embedding: domain.Embedding{
			Vector: embedding,
		},
		Metadata: map[string]interface{}{
			"agent_id":    agentID,
			"source_hash": sourceHash,
		},
	}

	if err := p.store.Save(ctx, doc); err != nil {
		return fmt.Errorf("pgVectorProfileStore: save profile for agent %s: %w", agentID, err)
	}
	return nil
}

// judicialRecordID derives a judicial record's id from its CONTENT: the critique
// text plus the (agent, source_hash) it is about.
//
// It used to be fmt.Sprintf("judicial:%d", time.Now().UnixNano()) — a clock read.
// That made the write non-idempotent: a retry after a timeout, a redelivered
// verification, or two workers handling the same task wrote a SECOND row saying
// the same thing, and GetJudicialRecords would then return the same critique
// several times and let it dominate the top-K a profile is built from. Nothing
// upstream deduplicates, so the clock was the only thing making them distinct, and
// it was distinguishing retries rather than facts.
//
// SaveProfile two functions up already keys on (agentID, sourceHash) for exactly
// this reason. This makes the identity strategy consistent across the file.
//
// The trade-off, stated rather than discovered: an identical critique about the
// same agent and the same source hash collapses onto one row, so the store no
// longer records how MANY times a verifier said the same thing. That count was
// never read — GetJudicialRecords returns texts — and "said twice" was
// indistinguishable from "retried once" anyway, so the multiplicity it preserved
// was not trustworthy. If occurrence counts are ever needed, they belong in a
// counter on the row, not in duplicate rows.
func judicialRecordID(text string, metadata map[string]interface{}) string {
	agentID, _ := metadata["agent_id"].(string)
	sourceHash, _ := metadata["source_hash"].(string)
	// The separator is a byte that cannot occur in the fields, so
	// ("ab","c") and ("a","bc") cannot collide.
	h := sha256.Sum256([]byte(agentID + "\x00" + sourceHash + "\x00" + text))
	return "judicial:" + hex.EncodeToString(h[:16])
}

// Save implements domain.JudicialStore. It persists a verifier critique
// as a document with DocumentType = domain.DocTypeJudicialRecord.
//
// The id is content-derived, so re-saving the same critique overwrites its row
// instead of adding one. See judicialRecordID.
func (p *PgVectorProfileStore) Save(ctx context.Context, text string, embedding []float32, metadata map[string]interface{}) error {
	doc := &domain.Document{
		ID:           judicialRecordID(text, metadata),
		DocumentType: domain.DocTypeJudicialRecord,
		Text:         text,
		Embedding: domain.Embedding{
			Vector: embedding,
		},
		Metadata: metadata,
	}
	return p.store.Save(ctx, doc)
}

// GetProfile retrieves the AgentProfile stored for (agentID, sourceHash).
// Returns nil, nil when no profile exists.
func (p *PgVectorProfileStore) GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	id := fmt.Sprintf("profile:%s:%s", agentID, sourceHash)
	doc, err := p.store.GetByID(kernelRead(ctx), id)
	if err != nil {
		return nil, fmt.Errorf("pgVectorProfileStore: get profile for agent %s: %w", agentID, err)
	}
	if doc == nil {
		return nil, nil
	}

	var profile domain.AgentProfile
	if err := json.Unmarshal([]byte(doc.Text), &profile); err != nil {
		return nil, fmt.Errorf("pgVectorProfileStore: unmarshal profile for agent %s: %w", agentID, err)
	}
	return &profile, nil
}

// HasInterviewVector reports whether a non-empty interview embedding is stored for
// (agentID, sourceHash). A profile row can exist with a NULL/empty vector — e.g. it was
// written while the embedder was failing — which makes the agent invisible to the
// Gatekeeper's Layer-2 semantic gate (it is never returned by the interview searcher, so
// it is eliminated from every task). The backfill uses this to re-interview such agents
// once the embedder is healthy, instead of trusting the mere existence of a profile.
func (p *PgVectorProfileStore) HasInterviewVector(ctx context.Context, agentID, sourceHash string) (bool, error) {
	id := fmt.Sprintf("profile:%s:%s", agentID, sourceHash)
	doc, err := p.store.GetByID(kernelRead(ctx), id)
	if err != nil {
		return false, fmt.Errorf("pgVectorProfileStore: get profile vector for agent %s: %w", agentID, err)
	}
	if doc == nil {
		return false, nil
	}
	return len(doc.Embedding.Vector) > 0, nil
}

// kernelRead seeds the explicit, greppable kernel-read bypass on a context.
//
// An agent profile and a judicial record are KERNEL-AUTHORED artifacts about an
// agent, not documents belonging to a principal — the Gatekeeper reads them to
// decide whether an agent may bid at all, long before any principal is in scope.
// So the correct predicate here is ScopeSystem, stated once and visible, rather
// than whatever the caller happened to be carrying.
//
// This is required now that the store is wired to the enforcing chokepoint rather
// than the raw adapter: a by-id read with no predicate is refused fail-closed
// (authz.ErrScopeMissing), which is the intended behaviour for a dropped predicate
// and the wrong behaviour for a read that legitimately has no principal. It mirrors
// what GetJudicialRecords already did through SearchOptions.Scope.
//
// An existing bypass on the context is left alone; this only fills in the absence.
func kernelRead(ctx context.Context) context.Context {
	if _, ok := domain.ScopeFromContext(ctx); ok {
		return ctx
	}
	return domain.WithScope(ctx, domain.ScopeSystem)
}

// sqlQuoteLiteral escapes a value for interpolation into a SearchOptions.Filter.
//
// Filter is applied by the adapter as `goqu.L(opts.Filter)` — a RAW SQL literal
// with no parameter binding — so a value carrying a single quote terminates the
// string and the remainder is parsed as SQL. agentID is not a trusted constant:
// it comes from an agent's own manifest at registration, so an agent registering
// itself as `x' OR '1'='1` would have read every judicial record in the store.
//
// Doubling embedded quotes is the standard PostgreSQL escape and is sufficient
// here because the value is always interpolated INSIDE a single-quoted literal.
func sqlQuoteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// GetJudicialRecords returns the top-K critique texts stored as judicial_record
// documents for (agentID, sourceHash). Returns nil when no records exist.
func (p *PgVectorProfileStore) GetJudicialRecords(ctx context.Context, agentID, sourceHash string, topK int) ([]string, error) {
	results, err := p.store.Search(kernelRead(ctx), nil, domain.SearchOptions{
		DocumentType: domain.DocTypeJudicialRecord,
		TopK:         topK,
		Filter: fmt.Sprintf("metadata->>'agent_id' = '%s' AND metadata->>'source_hash' = '%s'",
			sqlQuoteLiteral(agentID), sqlQuoteLiteral(sourceHash)),
		Scope: domain.ScopeSystem, // ADR-0034: judicial-record retrieval is a kernel read
	})
	if err != nil {
		return nil, fmt.Errorf("pgVectorProfileStore: get judicial records for agent %s: %w", agentID, err)
	}

	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Document.Text)
	}
	return out, nil
}

// EmbeddingDistance returns a normalised cosine distance in [0, 1] between two
// vectors. A distance of 0 means identical; 1 means maximally distant.
func (p *PgVectorProfileStore) EmbeddingDistance(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 1.0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 1.0
	}

	cosineSim := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	if cosineSim > 1.0 {
		cosineSim = 1.0
	}
	if cosineSim < -1.0 {
		cosineSim = -1.0
	}
	return 1.0 - cosineSim
}
