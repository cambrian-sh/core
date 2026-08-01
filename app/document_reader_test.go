package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// stubStore records the CONTEXT it was called with, because the scope the
// enforcing decorator resolves against travels there.
type stubStore struct {
	domain.VectorStore
	gotCtx context.Context
	doc    *domain.Document
	err    error
	calls  int
}

func (s *stubStore) GetByID(ctx context.Context, _ string) (*domain.Document, error) {
	s.calls++
	s.gotCtx = ctx
	return s.doc, s.err
}

func TestKernelDocumentReader_DelegatesToTheEnforcingStore(t *testing.T) {
	st := &stubStore{doc: &domain.Document{ID: "d1", Text: "body"}}
	r := &kernelDocumentReader{store: st}

	got, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("expected one delegated read, got %d", st.calls)
	}
	if got.Text != "body" {
		t.Fatalf("body not returned: %q", got.Text)
	}
}

// The whole point of taking a principal: the scope must reach the decorator, or
// the read is enforced against whatever the context happened to carry.
func TestKernelDocumentReader_StampsTheScopeFromThePrincipal(t *testing.T) {
	st := &stubStore{doc: &domain.Document{ID: "d1"}}
	r := &kernelDocumentReader{store: st}

	if _, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1"); err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if st.gotCtx == nil {
		t.Fatal("the store was called with no context")
	}
	if _, ok := domain.ScopeFromContext(st.gotCtx); !ok {
		t.Fatal("no scope was stamped on the context; the enforcing store would " +
			"resolve against whatever the caller happened to carry")
	}
}

// nil from the enforcing store means absent OR unreadable — the decorator makes
// them indistinguishable on purpose, and this must not turn one into a different
// answer than the other.
func TestKernelDocumentReader_NilBecomesNotFound(t *testing.T) {
	r := &kernelDocumentReader{store: &stubStore{doc: nil}}

	_, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "gone")
	if !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("a filtered/absent read produced %v", err)
	}
}

// A real store failure must stay an error. Collapsing it into not-found would
// report "this document does not exist" when the truth is "the database is down".
func TestKernelDocumentReader_StoreFailureIsNotMissing(t *testing.T) {
	boom := errors.New("connection refused")
	r := &kernelDocumentReader{store: &stubStore{err: boom}}

	_, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1")
	if errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatal("a store failure was reported as a missing document")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the underlying failure was lost: %v", err)
	}
}

func TestKernelDocumentReader_NoStoreIsAnHonestError(t *testing.T) {
	r := &kernelDocumentReader{}
	_, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected a stated absence, got %v", err)
	}
}

func TestKernelDocumentReader_EmptyIDIsRejectedBeforeReading(t *testing.T) {
	st := &stubStore{}
	r := &kernelDocumentReader{store: st}
	if _, err := r.GetDocument(context.Background(), domain.SystemPrincipal, ""); err == nil {
		t.Fatal("an empty id was accepted")
	}
	if st.calls != 0 {
		t.Fatal("the store was reached with an empty id")
	}
}
