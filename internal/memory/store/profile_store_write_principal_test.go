package store

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// failClosedAuthorizer reproduces the WRITE semantics of the premium authorizer
// (cambrian-premium/authz/authorizer.go ClassifyWrite) that matter here, because
// core may not import premium (AGENTS.md §5.1):
//
//   - zero principal          → denied, reason no_principal (fail-closed)
//   - domain.SystemPrincipal  → allowed, kernel-derived hint kept, stamped
//     provenance:source=system:kernel unless the hint already carries a stamp
//     (ENG-263, owner decision 2026-11-15)
//   - any other principal     → allowed, provenance:source=<WriterID> stamped
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
		if domain.HasProvenanceSource(hint) {
			return hint, dec
		}
		return domain.StampProvenanceSource(hint, p.WriterID()), dec
	case p.IsZero():
		dec.Reason = domain.ReasonNoPrincipal
		return nil, dec
	default:
		dec.Allowed, dec.Reason = true, domain.ReasonAllowed
		return domain.StampProvenanceSource(hint, p.WriterID()), dec
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
	ps := NewProfileStore(readWrite{stub, authz.NewEnforcingStoreWriter(stub, az, slog.Default())})

	// context.Background(): exactly what ProfileSeeder.Start hands Seed.
	err := ps.SaveProfile(context.Background(), "chat_agent", "h1", []float32{1}, domain.AgentProfile{AgentID: "chat_agent"})
	if errors.Is(err, authz.ErrWriteDenied) {
		t.Fatalf("seeded profile save denied with no principal (the ticket's bug): %v", err)
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
	if got := authz.DocTags(stub.savedDoc); !isKernelStamped(got) {
		t.Errorf("kernel-authored row must carry exactly provenance:source=system:kernel: %v", got)
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
	ps := NewProfileStore(readWrite{stub, authz.NewEnforcingStoreWriter(stub, az, slog.Default())})

	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("chat_agent"))
	if err := ps.Save(ctx, "critique", []float32{1}, map[string]interface{}{"agent_id": "chat_agent", "source_hash": "h1"}); err != nil {
		t.Fatalf("judicial Save: %v", err)
	}
	if got := az.principals(); len(got) != 1 || got[0] != domain.SystemPrincipal {
		t.Fatalf("classified as %v, want [%s]", got, domain.SystemPrincipal)
	}
	if got := authz.DocTags(stub.savedDoc); !isKernelStamped(got) {
		t.Errorf("judicial record must carry the kernel's stamp, not the caller agent's: %v", got)
	}
}

// isKernelStamped: the row's only provenance stamp is the kernel's reserved,
// kind-qualified writer id (ENG-263) — never an agent's.
func isKernelStamped(tags []string) bool {
	n := 0
	for _, t := range tags {
		if strings.HasPrefix(t, "provenance:source=") {
			if t != "provenance:source=system:kernel" {
				return false
			}
			n++
		}
	}
	return n == 1
}

// TestPg_SaveProfile_FailClosedAuthz_PersistsKernelAuthoredRow runs the same save
// against the real schema, through the store exactly as NewMemoryStack builds it
// (raw adapter → EnforcingVectorStore → EnforcingStoreWriter), with a fail-closed
// decision point, and reads the persisted rows back to check their provenance.
//
// Requires PG_TEST_DSN (handoff §0 recipe: port 5433). Skipped otherwise.
func TestPg_SaveProfile_FailClosedAuthz_PersistsKernelAuthoredRow(t *testing.T) {
	// The database is migrated to HEAD first and the embedder follows the dimension it
	// carries (pgscratch.Migrated, ENG-2100): a different one sends NewPgVectorAdapter
	// down its dimension change path, which recreated the corpus tables and left
	// cambrian_test unmigratable (ENG-1832), or refuses to boot.
	dsn, dim := pgscratch.Migrated(t)
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)

	vec, err := postgres.NewPgVectorAdapter(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	defer vec.Close()
	az := &failClosedAuthorizer{}
	var logs syncBuffer
	// The kernel's composition (ENG-1476): reads through the enforcing reader, writes
	// through the enforcing writer over the adapter's write side.
	ps := NewProfileStore(readWrite{authz.NewEnforcingVectorStore(vec, slog.Default()),
		authz.NewEnforcingStoreWriter(vec, az, slog.New(slog.NewTextHandler(&logs, nil)))})

	agent := "test-kernelwrite-" + time.Now().Format("150405.000000")
	md := map[string]interface{}{"agent_id": agent, "source_hash": "h1"}
	judicialID := judicialRecordID("test critique", md)
	bypass := domain.WithScope(context.Background(), domain.ScopeSystem)
	t.Cleanup(func() {
		_ = vec.Delete(bypass, "profile:"+agent+":h1")
		_ = vec.Delete(bypass, judicialID)
	})

	emb := make([]float32, cfg.Embedder.Dimensions)
	emb[0] = 1
	// No principal on the context: the ProfileSeeder's condition.
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
		if tags := authz.DocTags(doc); !isKernelStamped(tags) {
			t.Errorf("%s: kernel-authored row must carry provenance:source=system:kernel: %v", id, tags)
		}
	}
	// ENG-263: a kernel save no longer warns "write carries no classification".
	if strings.Contains(logs.String(), "authz_write_unclassified") {
		t.Errorf("kernel-authored save warned as unclassified: %s", logs.String())
	}
	for _, p := range az.principals() {
		if p != domain.SystemPrincipal {
			t.Errorf("write classified as %s, want %s", p, domain.SystemPrincipal)
		}
	}
	// The profile is readable on the kernel read path (Gatekeeper / re-seed).
	if got, err := ps.GetProfile(context.Background(), agent, "h1"); err != nil || got == nil || got.AgentID != agent {
		t.Fatalf("GetProfile after save = %+v, %v", got, err)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// readWrite composes a reader and a writer into the domain.VectorReadWriter the
// profile store takes, the way kernel.NewMemoryStack composes the two tiers.
type readWrite struct {
	domain.VectorReader
	domain.VectorWriter
}
