package authz_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// fakeRawEraser stands in for postgres.PgErasureStore: it resolves from a fixed record
// table and records what Erase was finally asked to erase.
type fakeRawEraser struct {
	records  []domain.ErasureCandidate
	erased   []domain.ErasureRequest
	receipts map[string]domain.ErasureReceipt
	eraseErr error
}

func (f *fakeRawEraser) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, r := range f.records {
		switch {
		case r.Kind == domain.ErasureTargetDocument && r.Via == r.ID && slices.Contains(sel.DocumentIDs, r.ID),
			r.Kind == domain.ErasureTargetDocument && r.Via != r.ID && slices.Contains(sel.ThreadIDs, r.Via),
			r.Kind == domain.ErasureTargetChunk && slices.Contains(sel.ChunkIDs, r.ID),
			r.Kind == domain.ErasureTargetEntity && slices.Contains(sel.EntityRefs, r.ID):
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRawEraser) Erase(_ context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	if f.eraseErr != nil {
		return domain.ErasureReceipt{}, f.eraseErr
	}
	f.erased = append(f.erased, req)
	return domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, PrincipalKind: req.PrincipalKind}, nil
}

func (f *fakeRawEraser) Receipt(_ context.Context, id string) (domain.ErasureReceipt, bool, error) {
	r, ok := f.receipts[id]
	return r, ok, nil
}

func eraseDoc(id, writer string, tags ...string) domain.ErasureCandidate {
	return domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: id, Via: id, DocumentID: id, WrittenBy: writer, Tags: tags}
}

func agentCtx(id string, pred *domain.TagPredicate) context.Context {
	return domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal(id)), pred)
}

func eraseReq(sel domain.ErasureSelector) domain.ErasureRequest {
	// Principal and kind here are LIES the chokepoint must overwrite from ctx.
	return domain.ErasureRequest{Principal: "someone-else", PrincipalKind: domain.ErasureByOperator,
		Reason: domain.ErasureReasonUserRequest, Selector: sel}
}

func newEraser(raw *fakeRawEraser, a domain.Authorizer, agentErase bool) *authz.EnforcingEraser {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	return authz.NewEnforcingEraser(raw, a, authz.EraserPolicy{AgentErase: agentErase, MaxTargets: 10}, nil)
}

// ADR-0137 D3 / gate TestForget_ScopeDenied (unit half): agent A can erase what it
// wrote, cannot erase B's record it can read, and the request's own claims about who
// asks are ignored.
func TestEnforcingEraser_AgentErasesOnlyWhatItWrote(t *testing.T) {
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{eraseDoc("a-doc", "agent-a"), eraseDoc("b-doc", "agent-b"), eraseDoc("legacy", "")}}
	e := newEraser(raw, nil, true)
	ctx := agentCtx("agent-a", domain.ScopeSystem)

	rec, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"a-doc"}}))
	if err != nil {
		t.Fatalf("own document: %v", err)
	}
	if got := raw.erased[0]; got.Principal != "agent-a" || got.PrincipalKind != domain.ErasureByAgent {
		t.Fatalf("principal must come from ctx, got %q/%q", got.Principal, got.PrincipalKind)
	}
	if rec.Principal != "agent-a" {
		t.Fatalf("receipt principal = %q", rec.Principal)
	}

	for _, id := range []string{"b-doc", "legacy"} {
		_, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{id}}))
		if !errors.Is(err, authz.ErrEraseDenied) {
			t.Fatalf("%s: want ErrEraseDenied, got %v", id, err)
		}
	}
	// All-or-nothing: one foreign record in the request erases nothing.
	n := len(raw.erased)
	if _, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"a-doc", "b-doc"}})); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("mixed request: want ErrEraseDenied, got %v", err)
	}
	if len(raw.erased) != n {
		t.Fatal("a denied request must erase nothing")
	}
}

// A record outside the caller's predicate is ABSENT (nothing to erase), not denied: a
// denial would confirm that someone else's record exists. Denied ≠ nothing-matched.
func TestEnforcingEraser_HiddenIsAbsentNotDenied(t *testing.T) {
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{
		eraseDoc("mine", "agent-a", "team-x"),
		eraseDoc("secret", "agent-a", "hr"),
		eraseDoc("fenced", "agent-a", "team-x", domain.OwnerTag("bob")),
	}}
	e := newEraser(raw, nil, true)
	ctx := agentCtx("agent-a", &domain.TagPredicate{RequiredTags: []string{"team-x"}})

	for _, id := range []string{"secret", "fenced", "does-not-exist"} {
		_, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{id}}))
		if !errors.Is(err, domain.ErrNothingToErase) || errors.Is(err, authz.ErrEraseDenied) {
			t.Fatalf("%s: want ErrNothingToErase, got %v", id, err)
		}
	}
	// A visible id plus a hidden one: only the visible one reaches the raw eraser.
	if _, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"mine", "secret"}})); err != nil {
		t.Fatal(err)
	}
	if got := raw.erased[len(raw.erased)-1].Selector.DocumentIDs; !slices.Equal(got, []string{"mine"}) {
		t.Fatalf("raw eraser asked for %v, want only the visible id", got)
	}
}

// Unset scope fails closed; so does no principal.
func TestEnforcingEraser_UnsetScopeFailsClosed(t *testing.T) {
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{eraseDoc("a-doc", "agent-a")}}
	e := newEraser(raw, nil, true)
	sel := domain.ErasureSelector{DocumentIDs: []string{"a-doc"}}

	noScope := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("agent-a"))
	if _, err := e.Erase(noScope, eraseReq(sel)); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("no predicate: want ErrScopeMissing, got %v", err)
	}
	noPrincipal := domain.WithScope(context.Background(), domain.ScopeSystem)
	if _, err := e.Erase(noPrincipal, eraseReq(sel)); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("no principal: want ErrScopeMissing, got %v", err)
	}
	if _, err := e.Erase(agentCtx("agent-a", nil), eraseReq(sel)); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("nil predicate (no read authorized): want ErrEraseDenied, got %v", err)
	}
	if _, _, err := e.Receipt(noScope, "x"); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("Receipt without scope: want ErrScopeMissing, got %v", err)
	}
	if len(raw.erased) != 0 {
		t.Fatal("nothing may be erased without scope")
	}
}

// An operator (person with the operator role, from the interceptor's role set) erases
// any record it can see; the kernel erases under its bypass without resolution.
func TestEnforcingEraser_OperatorAndKernel(t *testing.T) {
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{eraseDoc("b-doc", "agent-b"), eraseDoc("legacy", ""),
		{Kind: domain.ErasureTargetEntity, ID: "person:bob", Via: "person:bob"}}}
	e := newEraser(raw, nil, false) // agent erase off does not touch operators
	op := domain.WithPrincipalRoles(domain.WithScope(domain.WithPrincipal(context.Background(), domain.UserPrincipal("ops-1")), domain.ScopeSystem), []string{"operator"})

	if _, err := e.Erase(op, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"b-doc", "legacy"}, EntityRefs: []string{"person:bob"}})); err != nil {
		t.Fatalf("operator: %v", err)
	}
	if got := raw.erased[0]; got.PrincipalKind != domain.ErasureByOperator || got.Principal != "ops-1" {
		t.Fatalf("operator request = %+v", got)
	}
	// The same person without the role is an agent principal (and agent erase is off).
	viewer := domain.WithScope(domain.WithPrincipal(context.Background(), domain.UserPrincipal("ops-1")), domain.ScopeSystem)
	if _, err := e.Erase(viewer, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"b-doc"}})); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("person without operator role: want ErrEraseDenied, got %v", err)
	}
	kernel := domain.WithScope(domain.WithPrincipal(context.Background(), domain.SystemPrincipal), domain.ScopeMaintenance)
	if _, err := e.Erase(kernel, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"b-doc"}})); err != nil {
		t.Fatalf("kernel: %v", err)
	}
	if got := raw.erased[len(raw.erased)-1]; got.PrincipalKind != domain.ErasureByKernel {
		t.Fatalf("kernel request kind = %q", got.PrincipalKind)
	}
}

// Agent erasure is off by default; entity and experience selectors are operator-only;
// the request size is bounded.
func TestEnforcingEraser_PolicyLimits(t *testing.T) {
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{eraseDoc("a-doc", "agent-a")}}
	ctx := agentCtx("agent-a", domain.ScopeSystem)
	if _, err := newEraser(raw, nil, false).Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"a-doc"}})); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("agent erase disabled: want ErrEraseDenied, got %v", err)
	}
	e := newEraser(raw, nil, true)
	if _, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{EntityRefs: []string{"person:x"}})); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("agent entity selector: want ErrEraseDenied, got %v", err)
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = "d" + string(rune('a'+i))
	}
	if _, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: many})); !errors.Is(err, domain.ErrInvalidErasureRequest) {
		t.Fatalf("over max targets: want ErrInvalidErasureRequest, got %v", err)
	}
}

// denyErase is a decision point that refuses every erase: it can narrow what the
// kernel's own rules admitted.
type denyErase struct{ domain.AllowAllAuthorizer }

func (denyErase) AuthorizeErase(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef, t domain.Taggable) domain.AccessDecision {
	return domain.AccessDecision{Principal: p, Surface: s, Resource: t.AuthzRef(), Reason: domain.ReasonForbiddenTag, Detail: "closed tag"}
}

func TestEnforcingEraser_DecisionPointNarrows(t *testing.T) {
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{eraseDoc("a-doc", "agent-a")}}
	e := newEraser(raw, denyErase{}, true)
	if _, err := e.Erase(agentCtx("agent-a", domain.ScopeSystem), eraseReq(domain.ErasureSelector{DocumentIDs: []string{"a-doc"}})); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("want ErrEraseDenied from the decision point, got %v", err)
	}
	if len(raw.erased) != 0 {
		t.Fatal("a decision-point denial must erase nothing")
	}
}

// A thread is named whole only when every document on it is admitted; otherwise only
// the visible documents are erased.
func TestEnforcingEraser_ThreadNarrowsToVisibleDocuments(t *testing.T) {
	inThread := func(id, writer string, tags ...string) domain.ErasureCandidate {
		c := eraseDoc(id, writer, tags...)
		c.Via = "th-1"
		return c
	}
	raw := &fakeRawEraser{records: []domain.ErasureCandidate{inThread("t1", "agent-a", "team-x"), inThread("t2", "agent-a", "hr")}}
	e := newEraser(raw, nil, true)
	ctx := agentCtx("agent-a", &domain.TagPredicate{RequiredTags: []string{"team-x"}})
	if _, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{ThreadIDs: []string{"th-1"}})); err != nil {
		t.Fatal(err)
	}
	got := raw.erased[0].Selector
	if len(got.ThreadIDs) != 0 || !slices.Equal(got.DocumentIDs, []string{"t1"}) {
		t.Fatalf("selector = %+v, want only document t1", got)
	}
	ctx = agentCtx("agent-a", domain.ScopeSystem)
	if _, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{ThreadIDs: []string{"th-1"}})); err != nil {
		t.Fatal(err)
	}
	if got := raw.erased[1].Selector; !slices.Equal(got.ThreadIDs, []string{"th-1"}) || len(got.DocumentIDs) != 0 {
		t.Fatalf("selector = %+v, want the whole thread", got)
	}
}

// A principal sees only its own receipts; the earlier-request pointer of
// NothingToErase is shown only when that receipt is the caller's.
func TestEnforcingEraser_ReceiptsAreTheCallers(t *testing.T) {
	raw := &fakeRawEraser{
		records:  []domain.ErasureCandidate{eraseDoc("gone", "agent-a")},
		receipts: map[string]domain.ErasureReceipt{"erase:a": {RequestID: "erase:a", Principal: "agent-a"}, "erase:b": {RequestID: "erase:b", Principal: "agent-b"}},
	}
	e := newEraser(raw, nil, true)
	ctx := agentCtx("agent-a", domain.ScopeSystem)
	if _, found, _ := e.Receipt(ctx, "erase:a"); !found {
		t.Fatal("own receipt must be visible")
	}
	if _, found, _ := e.Receipt(ctx, "erase:b"); found {
		t.Fatal("another principal's receipt must read as not found")
	}
	raw.eraseErr = &domain.NothingToEraseError{EarlierRequestID: "erase:b"}
	_, err := e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"gone"}}))
	var nothing *domain.NothingToEraseError
	if !errors.As(err, &nothing) || nothing.EarlierRequestID != "" {
		t.Fatalf("foreign earlier request must not be named: %v", err)
	}
	raw.eraseErr = &domain.NothingToEraseError{EarlierRequestID: "erase:a"}
	_, err = e.Erase(ctx, eraseReq(domain.ErasureSelector{DocumentIDs: []string{"gone"}}))
	if !errors.As(err, &nothing) || nothing.EarlierRequestID != "erase:a" {
		t.Fatalf("own earlier request must be named: %v", err)
	}
}

func TestNewEnforcingEraser_RefusesNilDecisionPoint(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an eraser without a decision point must not be constructible")
		}
	}()
	authz.NewEnforcingEraser(&fakeRawEraser{}, nil, authz.EraserPolicy{}, nil)
}

// Row deletes on the write tier are kernel maintenance only (ADR-0137 D3); a classified
// write is stamped with its writer of record, whatever the writer claimed.
func TestEnforcingWriter_DeleteIsKernelMaintenanceOnly(t *testing.T) {
	w := authz.NewEnforcingStoreWriter(&recordingStore{}, nil, nil)
	if err := w.Delete(context.Background(), "c1"); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("no principal: want ErrScopeMissing, got %v", err)
	}
	agent := domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("a")), domain.ScopeSystem)
	if err := w.DeleteBatch(agent, []string{"c1"}); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("agent: want ErrEraseDenied, got %v", err)
	}
	kernelNoScope := domain.WithPrincipal(context.Background(), domain.SystemPrincipal)
	if err := w.Delete(kernelNoScope, "c1"); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("kernel without bypass: want ErrScopeMissing, got %v", err)
	}
	kernel := domain.WithScope(kernelNoScope, domain.ScopeMaintenance)
	if err := w.Delete(kernel, "c1"); err != nil {
		t.Fatalf("kernel GC: %v", err)
	}
}

func TestEnforcingWriter_StampsWriterOfRecord(t *testing.T) {
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, nil, nil)
	forged := func() *domain.Document {
		return &domain.Document{ID: "c", Metadata: map[string]any{domain.MetaWrittenBy: "agent-b"}}
	}
	if err := w.Save(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("agent-a")), forged()); err != nil {
		t.Fatal(err)
	}
	if got := store.saved[0].Metadata[domain.MetaWrittenBy]; got != "agent-a" {
		t.Fatalf("written_by = %v, want the ctx principal", got)
	}
	if err := w.Save(context.Background(), forged()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.saved[1].Metadata[domain.MetaWrittenBy]; ok {
		t.Fatal("a write with no principal must carry no writer of record")
	}
	if err := w.Save(domain.WithPrincipal(context.Background(), domain.SystemPrincipal), forged()); err != nil {
		t.Fatal(err)
	}
	if got := store.saved[2].Metadata[domain.MetaWrittenBy]; got != "system:kernel" {
		t.Fatalf("kernel written_by = %v", got)
	}
}
