package authz_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ENG-263: the kernel's own records (agent profiles, judicial records) are
// unclassified by design; they no longer warn "write carries no classification" on
// every save. Anyone else's unclassified write still does.
func TestEnforcingWriter_KernelWritesDoNotWarnUnclassified(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	w := authz.NewEnforcingStoreWriter(&recordingStore{}, nil, logger)

	kernel := domain.WithPrincipal(context.Background(), domain.SystemPrincipal)
	if err := w.Save(kernel, &domain.Document{ID: "profile:a:1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "authz_write_unclassified") {
		t.Fatalf("kernel write warned: %s", buf.String())
	}
	agent := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("a"))
	if err := w.Save(agent, &domain.Document{ID: "d"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "authz_write_unclassified") {
		t.Fatal("an agent's unclassified write must still warn")
	}
}

type itemRecorder struct {
	domain.KnowledgeStore
	put []domain.KnowledgeItem
}

func (r *itemRecorder) PutItem(_ context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	r.put = append(r.put, it)
	return it.ID, true, nil
}

// ENG-449: the plugin seam's item writes are classified by the decision point for
// the principal on ctx; the producer's classification is only the hint.
func TestEnforcingKnowledgeStore_ClassifiesItemWrites(t *testing.T) {
	t.Parallel()
	inner := &itemRecorder{}
	az := &stubAuthorizer{classify: func(hint []string) ([]string, domain.AccessDecision) {
		return append(append([]string(nil), hint...), "classified"), domain.AccessDecision{Allowed: true}
	}}
	s := authz.NewEnforcingKnowledgeStore(inner, az, nil)
	src := domain.SourcePrincipal("slack", "ws")
	ctx := domain.WithPrincipal(context.Background(), src)
	if _, _, err := s.PutItem(ctx, domain.KnowledgeItem{ID: "k", Kind: "policy_statement", Classification: []string{"hr"}}); err != nil {
		t.Fatal(err)
	}
	if len(az.seen) != 1 || az.seen[0] != src {
		t.Fatalf("decision point asked for %v, want %v", az.seen, src)
	}
	if got := inner.put[0].Classification; len(got) != 2 || got[0] != "hr" || got[1] != "classified" {
		t.Fatalf("stored classification %v, want the decision point's answer", got)
	}

	deny := authz.NewEnforcingKnowledgeStore(inner, &stubAuthorizer{classify: func([]string) ([]string, domain.AccessDecision) {
		return nil, domain.AccessDecision{Reason: domain.ReasonNoPrincipal}
	}}, nil)
	if _, _, err := deny.PutItem(context.Background(), domain.KnowledgeItem{ID: "x"}); !errors.Is(err, authz.ErrWriteDenied) || len(inner.put) != 1 {
		t.Fatalf("a denied item must not reach the store: %v (%d stored)", err, len(inner.put))
	}
}
