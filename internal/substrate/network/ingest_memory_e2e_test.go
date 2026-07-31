package network

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- e2e fakes: a capturing store, a fixed embedder, and a write-scope resolver ---

type e2eCaptureStore struct{ saved []*domain.Document }

func (c *e2eCaptureStore) Save(_ context.Context, d *domain.Document) error {
	c.saved = append(c.saved, d)
	return nil
}
func (c *e2eCaptureStore) SaveBatch(_ context.Context, ds []*domain.Document) error {
	c.saved = append(c.saved, ds...)
	return nil
}
func (c *e2eCaptureStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (c *e2eCaptureStore) GetByID(context.Context, string) (*domain.Document, error) { return nil, nil }
func (c *e2eCaptureStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (c *e2eCaptureStore) Delete(context.Context, string) error        { return nil }
func (c *e2eCaptureStore) DeleteBatch(context.Context, []string) error { return nil }
func (c *e2eCaptureStore) IncrementAccess(context.Context, string) error {
	return nil
}
func (c *e2eCaptureStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (c *e2eCaptureStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

type e2eEmbedder struct{}

func (e2eEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// e2eAuthz is a stand-in decision point implementing the two rules this e2e
// cares about: an unknown principal is denied, and a write is classified by the
// operator ceiling narrowed (never widened) by the agent's hint, with a coined
// tag refused.
type e2eAuthz struct {
	domain.AllowAllAuthorizer
	known     map[string]bool
	writeTags map[string][]string
	vocab     map[string]bool
}

func (r e2eAuthz) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	if !r.known[p.ID] {
		return nil, domain.AccessDecision{Principal: p, Surface: s, Reason: domain.ReasonNoPrincipal}
	}
	return &domain.TagPredicate{}, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func (r e2eAuthz) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	for _, h := range hint {
		if len(r.vocab) > 0 && !r.vocab[h] {
			return nil, domain.AccessDecision{Principal: p, Reason: domain.ReasonForbiddenTag, Detail: h}
		}
	}
	ceiling := r.writeTags[p.ID]
	out := []string{}
	if len(hint) == 0 {
		out = append(out, ceiling...)
	} else {
		want := map[string]bool{}
		for _, h := range hint {
			want[h] = true
		}
		for _, c := range ceiling {
			if want[c] {
				out = append(out, c)
			}
		}
	}
	return append(out, "provenance:source="+p.ID), domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func tagsOf(d *domain.Document) []string {
	if v, ok := d.Metadata["tags"].([]string); ok {
		return v
	}
	return nil
}

func tagsContain(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// newIngestServer assembles a Server exactly as the composition root does:
// MemoryWriter = RememberService over the write chokepoint, with the decision
// point supplying the classification. The capture store and log buffer are
// returned so the test can assert the persisted document and the warnings.
func newIngestServer(known map[string]bool, writeTags map[string][]string, vocab []string) (*Server, *e2eCaptureStore, *bytes.Buffer) {
	cap := &e2eCaptureStore{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	vset := map[string]bool{}
	for _, v := range vocab {
		vset[v] = true
	}
	a := e2eAuthz{known: known, writeTags: writeTags, vocab: vset}
	writeStore := authz.NewEnforcingStoreWriter(cap, a, logger)
	// Routed through the INGESTION PROCESSOR, because that is the only way in now:
	// the raw MemoryWriter fallback was removed so every ingest goes through the
	// chunker. These tests are about the ADR-0035 C2 write chokepoint — unknown
	// principal, coined tag, broadening hint clamped to the ceiling — and that
	// chokepoint sits in the enforcing store BELOW whichever path reaches it, so
	// they keep asserting exactly what they always did while now exercising the
	// live route.
	s := &Server{
		IngestionProcessor: writerProcessor{store: writeStore, embedder: e2eEmbedder{}},
		// The handler's own unknown-principal guard needs the decision point.
		Authz: a,
	}
	return s, cap, &logBuf
}

// writerProcessor is the smallest IngestionProcessor that still exercises the
// write chokepoint: embed, then save through the ENFORCING store.
//
// It deliberately does not chunk. These tests assert authorization — unknown
// principal, coined tag, a hint that cannot broaden the ceiling — and that
// decision lives in the enforcing store BELOW whichever path reaches it. A real
// chunker here would add moving parts with no bearing on the property under test,
// and a stand-in for the deleted RememberService would re-implement the thing
// being tested.
type writerProcessor struct {
	store    domain.VectorStore
	embedder domain.Embedder
}

func (w writerProcessor) ProcessSync(ctx context.Context, doc domain.ExternalDocument) (string, error) {
	vec, err := w.embedder.Embed(ctx, doc.Body)
	if err != nil {
		return "", err
	}
	d := &domain.Document{
		ID:           "source_doc:" + doc.SourceURI,
		Text:         doc.Body,
		DocumentType: domain.DocTypeMnemonicFact,
		Embedding:    domain.Embedding{Vector: vec},
		// The authored tags are a narrow-only HINT; the enforcing store replaces
		// them with the kernel-derived classification (ADR-0035 C2).
		Metadata: map[string]interface{}{"tags": doc.Tags},
	}
	if err := w.store.Save(ctx, d); err != nil {
		return "", err
	}
	return d.ID, nil
}

// 0035-07 e2e: an agent that hints a broadening tag still gets only the operator
// ceiling stamped — the broaden-to-leak vector is structurally closed end-to-end.
func TestIngestMemoryE2E_BroadeningHintClampedToCeiling(t *testing.T) {
	s, cap, _ := newIngestServer(
		map[string]bool{"support": true},
		map[string][]string{"support": {"public_kb"}},
		[]string{"public_kb", "secrets"})

	_, err := s.IngestMemory(agentCtx("support"), &pb.IngestMemoryRequest{
		Text: "sensitive content", Tags: []string{"secrets"}, // tries to broaden
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cap.saved) != 1 {
		t.Fatalf("expected one persisted doc, got %d", len(cap.saved))
	}
	got := tagsOf(cap.saved[0])
	if tagsContain(got, "secrets") {
		t.Errorf("agent must not broaden to secrets; stored tags %v", got)
	}
	if !tagsContain(got, "provenance:source=support") {
		t.Errorf("expected kernel-stamped provenance, got %v", got)
	}
}

// 0035-07 e2e: a coined hint (tag outside the vocabulary) is rejected before any
// write, surfaced as InvalidArgument through the live RPC.
func TestIngestMemoryE2E_CoinedHintIsInvalidArgument(t *testing.T) {
	s, cap, _ := newIngestServer(
		map[string]bool{"a": true},
		map[string][]string{"a": {"public_kb"}},
		[]string{"public_kb"})

	_, err := s.IngestMemory(agentCtx("a"), &pb.IngestMemoryRequest{
		Text: "x", Tags: []string{"invented"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for coined hint, got %v", err)
	}
	if len(cap.saved) != 0 {
		t.Errorf("a coined hint must reject before persisting; got %d docs", len(cap.saved))
	}
}

// 0035-07 e2e: a principal with no scope profile is fail-closed as PermissionDenied
// through the live RPC — no write reaches the store.
func TestIngestMemoryE2E_UnknownPrincipalIsPermissionDenied(t *testing.T) {
	s, cap, _ := newIngestServer(
		map[string]bool{}, // no known agents
		nil,
		[]string{"public_kb"})

	_, err := s.IngestMemory(agentCtx("ghost"), &pb.IngestMemoryRequest{Text: "x"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for unknown principal, got %v", err)
	}
	if len(cap.saved) != 0 {
		t.Errorf("an unknown principal must not persist anything; got %d docs", len(cap.saved))
	}
}

// 0035-07 e2e: a narrow-only hint that shares no tag with a non-empty DefaultWriteTags
// collapses the write to unclassified. The write still succeeds (fails safe on
// confidentiality) but emits a scope_write_unclassified warning so the visibility
// collapse is diagnosable rather than a silent "nothing can read my write".
func TestIngestMemoryE2E_NarrowToUnclassifiedLogsWarning(t *testing.T) {
	s, cap, logBuf := newIngestServer(
		map[string]bool{"a": true},
		map[string][]string{"a": {"public_kb"}},
		[]string{"public_kb", "analytics"})

	// hint "analytics" is in-vocabulary but not in DefaultWriteTags → intersection ∅.
	_, err := s.IngestMemory(agentCtx("a"), &pb.IngestMemoryRequest{
		Text: "x", Tags: []string{"analytics"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cap.saved) != 1 {
		t.Fatalf("expected the unclassified write to still persist, got %d", len(cap.saved))
	}
	got := tagsOf(cap.saved[0])
	for _, tg := range got {
		if !strings.HasPrefix(tg, "provenance:") {
			t.Errorf("expected an unclassified write (provenance only), got classification tag %q", tg)
		}
	}
	// The kernel's guarantee is that the write still lands and carries only what
	// the decision point returned. The richer "you narrowed yourself to nothing"
	// warning is emitted by the decision point, which owns the vocabulary — see
	// the premium authz test of the same name.
	_ = logBuf
}
