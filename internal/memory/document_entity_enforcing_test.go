package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingDocStore captures what actually reached the store.
type recordingDocStore struct {
	got   domain.SourceDocument
	calls int
}

func (r *recordingDocStore) SaveDocument(_ context.Context, d domain.SourceDocument) ([]string, error) {
	r.got = d
	r.calls++
	return d.Tags, nil
}

// narrowingAuthorizer stands in for a premium decision point: it replaces whatever the
// writer asked for with the classification it decides, and can refuse outright.
type narrowingAuthorizer struct {
	domain.AllowAllAuthorizer
	classification []string
	deny           bool
}

func (a narrowingAuthorizer) ClassifyWrite(_ context.Context, _ domain.PrincipalRef, _ []string) ([]string, domain.AccessDecision) {
	if a.deny {
		return nil, domain.AccessDecision{Allowed: false, Reason: "no_principal", Detail: "identity could not be established"}
	}
	return a.classification, domain.AccessDecision{Allowed: true}
}

// The tags an ingesting agent supplies are a REQUEST. If they were the answer, any agent
// could classify a document however it liked simply by ingesting it — and every chunk
// beneath that document would inherit the classification.
func TestEnforcingDocumentStore_DecisionPointOverridesRequestedTags(t *testing.T) {
	inner := &recordingDocStore{}
	w := NewEnforcingDocumentStore(inner, narrowingAuthorizer{classification: []string{"support"}}, nil)

	stored, err := w.SaveDocument(context.Background(), domain.SourceDocument{
		ID: "doc-a", Tags: []string{"public", "airline"},
	})
	if err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	// The returned value is what chunks will be stamped with, so it has to be the
	// decision point's answer too — not the caller's request echoed back.
	if len(stored) != 1 || stored[0] != "support" {
		t.Errorf("returned tags = %v, want [support]; chunks would inherit the wrong classification", stored)
	}
	if len(inner.got.Tags) != 1 || inner.got.Tags[0] != "support" {
		t.Errorf("stored tags = %v, want the decision point's [support] — the agent's request must not win",
			inner.got.Tags)
	}
}

// A refused classification must not reach the store at all. Writing the row and letting
// the tags be wrong later is the failure this whole chokepoint exists to prevent.
func TestEnforcingDocumentStore_DeniedWriteNeverReachesTheStore(t *testing.T) {
	inner := &recordingDocStore{}
	w := NewEnforcingDocumentStore(inner, narrowingAuthorizer{deny: true}, nil)

	_, err := w.SaveDocument(context.Background(), domain.SourceDocument{ID: "doc-a", Tags: []string{"x"}})
	if !errors.Is(err, ErrDocumentWriteDenied) {
		t.Fatalf("want ErrDocumentWriteDenied, got %v", err)
	}
	if inner.calls != 0 {
		t.Errorf("the store was written %d time(s) despite a denial", inner.calls)
	}
}

// OSS has no policy plugin, and an unscoped deployment must keep working exactly as
// before: nil authorizer ⇒ allow-all ⇒ the authored tags survive.
func TestEnforcingDocumentStore_NilAuthorizerFailsOpen(t *testing.T) {
	inner := &recordingDocStore{}
	w := NewEnforcingDocumentStore(inner, nil, nil)

	if _, err := w.SaveDocument(context.Background(), domain.SourceDocument{
		ID: "doc-a", Tags: []string{"public"},
	}); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	if len(inner.got.Tags) != 1 || inner.got.Tags[0] != "public" {
		t.Errorf("stored tags = %v, want [public] — OSS must be unaffected", inner.got.Tags)
	}
}
