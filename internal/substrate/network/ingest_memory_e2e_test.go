package network

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/memory"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"

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

type e2eResolver struct {
	known     map[string]bool
	writeTags map[string][]string
}

func (r e2eResolver) EffectiveForAgent(_ context.Context, id string) (*domain.EffectiveScope, bool) {
	if !r.known[id] {
		return nil, false
	}
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, domain.ScopeConfig{})
	return &eff, true
}
func (r e2eResolver) DefaultWriteTags(_ context.Context, id string) []string {
	return r.writeTags[id]
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

// newIngestServer assembles a Server exactly as cmd/orchestrator/main.go does:
// MemoryWriter = RememberService over a ScopedStoreWriter (kernel-derived
// classification + provenance). The capture store and log buffer are returned so the
// test can assert the persisted document and observability warnings.
func newIngestServer(known map[string]bool, writeTags map[string][]string, vocab []string) (*Server, *e2eCaptureStore, *bytes.Buffer) {
	cap := &e2eCaptureStore{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	writeStore := scope.NewScopedStoreWriter(cap, scope.NewVocabulary(vocab), logger)
	resolver := e2eResolver{known: known, writeTags: writeTags}
	s := &Server{MemoryWriter: memory.NewRememberService(writeStore, e2eEmbedder{}, resolver)}
	return s, cap, &logBuf
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
	if !strings.Contains(logBuf.String(), "scope_write_unclassified") {
		t.Errorf("expected a scope_write_unclassified warning, log was: %s", logBuf.String())
	}
}
