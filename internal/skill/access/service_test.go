package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func profiles(m map[string][]string) ProfileSkills {
	return func(id string) ([]string, bool) { s, ok := m[id]; return s, ok }
}

func newSvc(t *testing.T, st domain.SkillGrantStore, hub domain.ApprovalController, cfg Config) *Service {
	t.Helper()
	svc, err := New(st, profiles(map[string][]string{"analyst": {"file-report", "summarise"}}),
		func(n string) bool { return n != "ghost" }, hub, cfg, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func eventActions(st *memStore) []string {
	var out []string
	for _, e := range st.events {
		out = append(out, e.Action)
	}
	return out
}

// Effective set = profile skills ∪ active grants; unbound agents get no set.
func TestEffective_ProfileUnionGrants(t *testing.T) {
	st := newMemStore()
	svc := newSvc(t, st, nil, Config{})
	ctx := context.Background()
	if _, err := svc.Grant(ctx, GrantInput{AgentID: "analyst", Skill: "proposed-x", Source: domain.SkillGrantSourceOperator, By: "op", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	set, bound := svc.Effective(ctx, "analyst")
	if !bound || len(set) != 3 || set["file-report"] != domain.SkillAccessProfile || set["proposed-x"] != domain.SkillAccessGranted {
		t.Fatalf("set=%v bound=%v", set, bound)
	}
	if set, bound := svc.Effective(ctx, "legacy-agent"); bound || set != nil {
		t.Fatalf("a non-profile agent must be unbound: %v %v", set, bound)
	}
	// A grant-store failure fails closed to the profile skills.
	st.failRead = errors.New("db down")
	if set, _ := svc.Effective(ctx, "analyst"); len(set) != 2 || set["proposed-x"] != "" {
		t.Fatalf("fail closed: %v", set)
	}
}

// Grant and revoke are idempotent, audited, and keyed by agent id.
func TestGrantRevoke_AuditedAndIdempotent(t *testing.T) {
	st := newMemStore()
	svc := newSvc(t, st, nil, Config{})
	ctx := context.Background()
	in := GrantInput{CommandID: "c1", AgentID: "analyst", Skill: "proposed-x", Source: domain.SkillGrantSourceOperator, By: "op", Reason: "r"}
	if r, err := svc.Grant(ctx, in); err != nil || !r.Changed {
		t.Fatalf("grant: %+v %v", r, err)
	}
	if r, err := svc.Grant(ctx, in); err != nil || !r.Deduped {
		t.Fatalf("same command must dedupe: %+v %v", r, err)
	}
	in.CommandID = "c2"
	if r, err := svc.Grant(ctx, in); err != nil || r.Changed {
		t.Fatalf("an active grant is a no-op: %+v %v", r, err)
	}
	if _, err := svc.Revoke(ctx, RevokeInput{CommandID: "c3", AgentID: "analyst", Skill: "proposed-x", By: "op", Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	if set, _ := svc.Effective(ctx, "analyst"); set["proposed-x"] != "" {
		t.Fatalf("revoked grant still effective: %v", set)
	}
	if _, err := svc.Revoke(ctx, RevokeInput{CommandID: "c4", AgentID: "analyst", Skill: "proposed-x", By: "op", Reason: "again"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking nothing = %v", err)
	}
	if r, err := svc.Revoke(ctx, RevokeInput{CommandID: "c3", AgentID: "analyst", Skill: "proposed-x", By: "op", Reason: "done"}); err != nil || !r.Deduped {
		t.Fatalf("revoke retry must dedupe: %+v %v", r, err)
	}
	all, _ := svc.Grants(ctx, domain.SkillGrantFilter{IncludeRevoked: true})
	if len(all) != 1 || all[0].Active() || all[0].ID != domain.SkillGrantID("analyst", "proposed-x") {
		t.Fatalf("grants = %+v", all)
	}
	if got := eventActions(st); len(got) != 2 || got[0] != "granted" || got[1] != "revoked" {
		t.Fatalf("audit = %v", got)
	}
	if _, err := svc.Grant(ctx, GrantInput{AgentID: "analyst", Skill: "ghost", Source: domain.SkillGrantSourceOperator, By: "op"}); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("granting an unknown skill = %v", err)
	}
	if _, err := svc.Grant(ctx, GrantInput{AgentID: "analyst", Skill: "x", Source: "magic"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown source = %v", err)
	}
}

// answer subscribes to the hub and answers the first request with approve.
func answer(hub *domain.AddressedHub, approve bool) (<-chan domain.ApprovalRequest, func()) {
	ch, cancel := opRequests(hub)
	seen := make(chan domain.ApprovalRequest, 1)
	go func() {
		r, ok := <-ch
		if !ok {
			return
		}
		seen <- r
		opSubmit(hub, r.ID, approve, "op-ann")
	}()
	return seen, cancel
}

// Request → an operator approves live → a durable grant, granted in the same call.
func TestRequest_ApprovedLiveGrantsDurably(t *testing.T) {
	st := newMemStore()
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: time.Minute})
	svc := newSvc(t, st, hub, Config{RequestTimeout: 5 * time.Second})
	seen, cancel := answer(hub, true)
	defer cancel()
	out := svc.Request(context.Background(), RequestInput{AgentID: "analyst", Skill: "proposed-x", RunID: "run-1", Rationale: "need it"})
	if out.Access != domain.SkillAccessGranted {
		t.Fatalf("outcome = %+v", out)
	}
	r := <-seen
	if r.Kind != domain.ApprovalKindSkillAccess || r.ToolName != "skill:proposed-x" || r.AgentID != "analyst" || r.Timeout != 5*time.Second {
		t.Fatalf("approval request = %+v", r)
	}
	if set, _ := svc.Effective(context.Background(), "analyst"); set["proposed-x"] != domain.SkillAccessGranted {
		t.Fatalf("approved skill not effective: %v", set)
	}
	g, _ := svc.Grants(context.Background(), domain.SkillGrantFilter{AgentID: "analyst"})
	if len(g) != 1 || g[0].Source != domain.SkillGrantSourceRequest || g[0].GrantedBy != "op-ann" {
		t.Fatalf("grant = %+v", g)
	}
	reqs, _ := svc.Requests(context.Background(), domain.SkillGrantFilter{})
	if len(reqs) != 1 || reqs[0].State != domain.SkillAccessGranted || reqs[0].RunID != "run-1" {
		t.Fatalf("request = %+v", reqs)
	}
}

// No decision in time → pending, the request stays open; a later operator grant
// resolves it and the NEXT run has the skill.
func TestRequest_TimeoutIsPendingThenLaterGrantApplies(t *testing.T) {
	st := newMemStore()
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: time.Minute})
	_, cancel := opRequests(hub) // an operator is watching but never answers
	defer cancel()
	svc := newSvc(t, st, hub, Config{RequestTimeout: 30 * time.Millisecond})
	out := svc.Request(context.Background(), RequestInput{AgentID: "analyst", Skill: "proposed-x", RunID: "run-1"})
	if out.Access != domain.SkillAccessPending {
		t.Fatalf("outcome = %+v", out)
	}
	pend, _ := svc.Requests(context.Background(), domain.SkillGrantFilter{State: domain.SkillAccessPending})
	if len(pend) != 1 {
		t.Fatalf("pending requests = %+v", pend)
	}
	if set, _ := svc.Effective(context.Background(), "analyst"); set["proposed-x"] != "" {
		t.Fatal("a pending request must not grant")
	}
	if _, err := svc.Grant(context.Background(), GrantInput{CommandID: "late", AgentID: "analyst", Skill: "proposed-x", Source: domain.SkillGrantSourceOperator, By: "op", Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	if set, _ := svc.Effective(context.Background(), "analyst"); set["proposed-x"] != domain.SkillAccessGranted {
		t.Fatalf("later grant not effective: %v", set)
	}
	if pend, _ := svc.Requests(context.Background(), domain.SkillGrantFilter{State: domain.SkillAccessPending}); len(pend) != 0 {
		t.Fatalf("the grant must resolve the pending request: %+v", pend)
	}
}

// No operator subscribed → pending at once (the hub fails closed without a ruling).
func TestRequest_NoApproverIsPending(t *testing.T) {
	svc := newSvc(t, newMemStore(), domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: time.Minute}), Config{})
	if out := svc.Request(context.Background(), RequestInput{AgentID: "analyst", Skill: "s"}); out.Access != domain.SkillAccessPending {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestRequest_DeniedIsRecorded(t *testing.T) {
	st := newMemStore()
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: time.Minute})
	svc := newSvc(t, st, hub, Config{RequestTimeout: 5 * time.Second})
	_, cancel := answer(hub, false)
	defer cancel()
	out := svc.Request(context.Background(), RequestInput{AgentID: "analyst", Skill: "proposed-x"})
	if out.Access != domain.SkillAccessDenied {
		t.Fatalf("outcome = %+v", out)
	}
	reqs, _ := svc.Requests(context.Background(), domain.SkillGrantFilter{})
	if len(reqs) != 1 || reqs[0].State != domain.SkillAccessDenied || reqs[0].DecidedBy != "op-ann" {
		t.Fatalf("request = %+v", reqs)
	}
	if set, _ := svc.Effective(context.Background(), "analyst"); set["proposed-x"] != "" {
		t.Fatal("a denial must not grant")
	}
	if got := eventActions(st); len(got) != 2 || got[0] != "requested" || got[1] != "denied" {
		t.Fatalf("audit = %v", got)
	}
}

// access_auto_approve grants without asking; the hub is never consulted.
func TestRequest_AutoApprove(t *testing.T) {
	st := newMemStore()
	svc := newSvc(t, st, panicHub{}, Config{AutoApprove: true})
	if out := svc.Request(context.Background(), RequestInput{AgentID: "analyst", Skill: "proposed-x"}); out.Access != domain.SkillAccessGranted {
		t.Fatalf("outcome = %+v", out)
	}
	g, _ := svc.Grants(context.Background(), domain.SkillGrantFilter{})
	if len(g) != 1 || g[0].GrantedBy != ApproverAuto {
		t.Fatalf("grant = %+v", g)
	}
}

type panicHub struct{}

func (panicHub) Request(context.Context, domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	panic("auto-approve must not ask the hub")
}

// Without a grant store, requests are denied with a reason and bound agents get
// exactly their profile skills.
func TestNoStore_FailsClosed(t *testing.T) {
	svc := newSvc(t, nil, nil, Config{})
	if out := svc.Request(context.Background(), RequestInput{AgentID: "analyst", Skill: "x"}); out.Access != domain.SkillAccessDenied || out.Detail == "" {
		t.Fatalf("outcome = %+v", out)
	}
	if set, bound := svc.Effective(context.Background(), "analyst"); !bound || len(set) != 2 {
		t.Fatalf("set=%v bound=%v", set, bound)
	}
	if svc.HasStore() {
		t.Fatal("no store")
	}
}

// D13 hook: GrantProposer grants with source "proposal".
func TestGrantProposer(t *testing.T) {
	st := newMemStore()
	svc := newSvc(t, st, nil, Config{})
	if err := svc.GrantProposer(context.Background(), "analyst", "proposed-x", "op", "sp-1"); err != nil {
		t.Fatal(err)
	}
	g, _ := svc.Grants(context.Background(), domain.SkillGrantFilter{})
	if len(g) != 1 || g[0].Source != domain.SkillGrantSourceProposal || g[0].GrantedBy != "op" {
		t.Fatalf("grant = %+v", g)
	}
}
