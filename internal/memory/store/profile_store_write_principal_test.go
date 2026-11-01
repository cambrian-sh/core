package store

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// failClosedAuthorizer reproduces the WRITE semantics of the premium authorizer
// (cambrian-premium/authz/authorizer.go ClassifyWrite) that matter here, because
// core may not import premium (AGENTS.md §5.1):
//
//   - zero principal          → denied, reason no_principal (fail-closed)
//   - domain.SystemPrincipal  → allowed, kernel-derived hint kept, no stamp
//   - any other principal     → allowed, provenance:source=<id> stamped
//
// The premium side of this contract is pinned by premium's own
// TestClassifyWrite_InterviewProfileWrite_* tests. It records every principal
// it was asked about so a test can assert the write's attribution.
type failClosedAuthorizer struct {
	domain.AllowAllAuthorizer
	mu   sync.Mutex
	seen []domain.PrincipalRef
}

func (a *failClosedAuthorizer) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	a.mu.Lock()
	a.seen = append(a.seen, p)
	a.mu.Unlock()
	dec := domain.AccessDecision{Principal: p}
	switch {
	case p.Kind == domain.PrincipalSystem:
		dec.Allowed, dec.Reason = true, domain.ReasonBypass
		return hint, dec
	case p.IsZero():
		dec.Reason = domain.ReasonNoPrincipal
		return nil, dec
	default:
		dec.Allowed, dec.Reason = true, domain.ReasonAllowed
		return append(append([]string{}, hint...), "provenance:source="+p.ID), dec
	}
}

func (a *failClosedAuthorizer) principals() []domain.PrincipalRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]domain.PrincipalRef(nil), a.seen...)
}

// T-20261101-interviewworker-saves-agent-profiles-with-no: the InterviewWorker,
// VerificationWorker and ProfileAggregator all save on a background context with
// no principal. Behind a fail-closed decision point every such save was denied.
// The store must attribute its own writes to the kernel.
func TestSaveProfile_BackgroundCtx_IsAttributedToKernelUnderFailClosedAuthz(t *testing.T) {
	t.Parallel()
	stub := &stubVectorStore{}
	az := &failClosedAuthorizer{}
	ps := NewProfileStore(authz.NewEnforcingStoreWriter(stub, az, slog.Default()))

	// context.Background(): exactly what InterviewWorker.Start hands processAgent.
	err := ps.SaveProfile(context.Background(), "chat_agent", "h1", []float32{1}, domain.AgentProfile{AgentID: "chat_agent"})
	if errors.Is(err, authz.ErrWriteDenied) {
		t.Fatalf("interview profile save denied with no principal (the ticket's bug): %v", err)
	}
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	md := map[string]interface{}{"agent_id": "chat_agent", "source_hash": "h1"}
	if err := ps.Save(context.Background(), "critique", []float32{1}, md); err != nil {
		t.Fatalf("judicial Save: %v", err)
	}
	for _, p := range az.principals() {
		if p != domain.SystemPrincipal {
			t.Errorf("write classified as %s, want %s", p, domain.SystemPrincipal)
		}
	}
	if got := authz.DocTags(stub.savedDoc); hasSourceProvenance(got) {
		t.Errorf("kernel-authored row carries an agent source stamp: %v", got)
	}
}

// A caller that happens to carry an agent principal (a verification running on a
// step's context, say) must not get the kernel's artifact classified under that
// agent's write ceiling or stamped with its provenance: under an own-writes scope
// (ADR-0118) that would make the agent's own critiques readable to it.
func TestSaveProfile_CallerAgentPrincipal_IsReplacedByKernel(t *testing.T) {
	t.Parallel()
	stub := &stubVectorStore{}
	az := &failClosedAuthorizer{}
	ps := NewProfileStore(authz.NewEnforcingStoreWriter(stub, az, slog.Default()))

	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("chat_agent"))
	if err := ps.Save(ctx, "critique", []float32{1}, map[string]interface{}{"agent_id": "chat_agent", "source_hash": "h1"}); err != nil {
		t.Fatalf("judicial Save: %v", err)
	}
	if got := az.principals(); len(got) != 1 || got[0] != domain.SystemPrincipal {
		t.Fatalf("classified as %v, want [%s]", got, domain.SystemPrincipal)
	}
	if got := authz.DocTags(stub.savedDoc); hasSourceProvenance(got) {
		t.Errorf("judicial record stamped with the caller agent's provenance: %v", got)
	}
}

func hasSourceProvenance(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, "provenance:source=") {
			return true
		}
	}
	return false
}

// TestPg_SaveProfile_FailClosedAuthz_PersistsKernelAuthoredRow runs the same save
// against the real schema, through the store exactly as NewMemoryStack builds it
// (raw adapter → EnforcingVectorStore → EnforcingStoreWriter), with a fail-closed
// decision point, and reads the persisted rows back to check their provenance.
//
// Requires PG_TEST_DSN (handoff §0 recipe: port 5433). Skipped otherwise.
func TestPg_SaveProfile_FailClosedAuthz_PersistsKernelAuthoredRow(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping Postgres profile-write test")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("PG_TEST_DSN: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Database.Host = u.Hostname()
	cfg.Database.Port = u.Port()
	cfg.Database.User = u.User.Username()
	cfg.Database.Password, _ = u.User.Password()
	cfg.Database.DBName = u.Path[1:]
	cfg.Embedder.Dimensions = 1536

	vec, err := postgres.NewPgVectorAdapter(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	defer vec.Close()
	az := &failClosedAuthorizer{}
	ps := NewProfileStore(authz.NewEnforcingStoreWriter(authz.NewEnforcingVectorStore(vec, slog.Default()), az, slog.Default()))

	agent := "test-kernelwrite-" + time.Now().Format("150405.000000")
	md := map[string]interface{}{"agent_id": agent, "source_hash": "h1"}
	judicialID := judicialRecordID("test critique", md)
	bypass := domain.WithScope(context.Background(), domain.ScopeSystem)
	t.Cleanup(func() {
		_ = vec.Delete(bypass, "profile:"+agent+":h1")
		_ = vec.Delete(bypass, judicialID)
	})

	emb := make([]float32, 1536)
	emb[0] = 1
	// No principal on the context: the InterviewWorker's condition.
	if err := ps.SaveProfile(context.Background(), agent, "h1", emb, domain.AgentProfile{AgentID: agent, SourceHash: "h1"}); err != nil {
		t.Fatalf("SaveProfile under fail-closed authz: %v", err)
	}
	if err := ps.Save(context.Background(), "test critique", emb, md); err != nil {
		t.Fatalf("judicial Save under fail-closed authz: %v", err)
	}

	for _, id := range []string{"profile:" + agent + ":h1", judicialID} {
		doc, err := vec.GetByID(bypass, id)
		if err != nil || doc == nil {
			t.Fatalf("GetByID(%s) = %v, %v; want the persisted row", id, doc, err)
		}
		if tags := authz.DocTags(doc); hasSourceProvenance(tags) {
			t.Errorf("%s: kernel-authored row carries an agent source stamp: %v", id, tags)
		}
	}
	for _, p := range az.principals() {
		if p != domain.SystemPrincipal {
			t.Errorf("write classified as %s, want %s", p, domain.SystemPrincipal)
		}
	}
	// The profile is readable on the kernel read path (Gatekeeper / re-interview).
	if got, err := ps.GetProfile(context.Background(), agent, "h1"); err != nil || got == nil || got.AgentID != agent {
		t.Fatalf("GetProfile after save = %+v, %v", got, err)
	}
}
