package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
)

// A minimal "decision" case modelled PURELY AS DATA: the kind arrives as the same JSON a
// deployment puts in CAMBRIAN_SUBSTRATE_KINDS (premium parses it into []domain.KindSpec
// unchanged), and everything else is the generic bridge, store, hub and query plane.
// No decision-specific Go exists anywhere on this path.
const decisionKindJSON = `[{
  "Kind": "casetest_e2e_decision",
  "Policy": "designated_actor",
  "resolution_scope": "entity",
  "designated": {"approved_via_commit": true},
  "commit_class": "decision_commit",
  "Predicates": {
    "question":   {"Type": "text"},
    "owner":      {"Type": "entity"},
    "status":     {"Type": "text", "enum": ["open", "decided", "superseded", "dropped"]},
    "options":    {"Type": "text"},
    "assumption": {"Type": "text"},
    "chosen":     {"Type": "text"}
  },
  "lifecycle": {
    "state_predicate": "status",
    "initial": ["open"],
    "transitions": {"open": ["decided", "dropped"], "decided": ["superseded"]},
    "required": {"open": ["question", "owner"], "decided": ["chosen", "owner"]}
  },
  "event_types": [{"type": "casetest_e2e_decided", "roles": ["decision", "decider"], "required_roles": ["decision", "decider"]}]
}]`

func TestPg_DecisionCaseAsData_EndToEnd(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		for _, q := range []string{
			`DELETE FROM resolutions WHERE kind = 'casetest_e2e_decision'`,
			`DELETE FROM knowledge_items WHERE kind = 'casetest_e2e_decision'`,
		} {
			_, _ = pool.Exec(context.Background(), q)
		}
	}
	clean()
	t.Cleanup(clean)

	var specs []domain.KindSpec
	if err := json.Unmarshal([]byte(decisionKindJSON), &specs); err != nil {
		t.Fatal(err)
	}
	reg, err := domain.NewKindRegistry(specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewPgKnowledgeStore(pool, reg)
	rel, _ := domain.NewRelationRegistry(nil)
	plane := postgres.NewPgQueryPlane(pool, postgres.NewPgEventStore(pool, reg), store, nil,
		postgres.NewPgLinkStore(pool, rel), postgres.NewPgEntityStore(pool), rel)

	// The deployment's approvals config says who commits a decision (data, not Go):
	// the decision's class routes to the self rule — the person the agent works for.
	pcfg := domain.DefaultApprovalPolicyConfig()
	pcfg.Rules = append(pcfg.Rules, domain.ApprovalClassRule{Class: "decision_commit", Approvers: []string{"self"}})
	policy, err := domain.NewConfigApprovalPolicy(pcfg)
	if err != nil {
		t.Fatal(err)
	}
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Policy: policy})
	svc, err := knowledgecommit.New(store, reg, nil, hub, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, alice := domain.AgentPrincipal("assistant"), domain.UserPrincipal("alice")
	committed := func() map[string]any {
		t.Helper()
		res, err := plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: "casetest_e2e_decision",
			Policy: domain.ResolutionPolicyDesignatedActor, EntityID: "decision/pricing"}, &domain.TagPredicate{Bypass: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) == 0 {
			return nil
		}
		return res.Rows[0]["values"].(map[string]any)
	}
	approve := func(id string) {
		t.Helper()
		if _, err := hub.Answer(ctx, id, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: alice}}); err != nil {
			t.Fatal(err)
		}
	}
	tv := func(p, v string) domain.StatementValue {
		return domain.StatementValue{Predicate: p, Type: "text", Text: v}
	}
	owner := domain.StatementValue{Predicate: "owner", Type: "entity", EntityRef: "person/alice"}

	// 1. Opening needs a question and an owner (lifecycle data).
	if _, err := svc.Propose(ctx, agent, alice, knowledgecommit.Proposal{Kind: "casetest_e2e_decision", EntityID: "decision/pricing",
		Values: []domain.StatementValue{tv("status", "open")}}, true, ""); err == nil || !strings.Contains(err.Error(), `requires predicate "question"`) {
		t.Fatalf("open without a question: %v", err)
	}
	open, err := svc.Propose(ctx, agent, alice, knowledgecommit.Proposal{Kind: "casetest_e2e_decision", EntityID: "decision/pricing",
		Values: []domain.StatementValue{tv("status", "open"), tv("question", "Raise 2027 list prices?"), owner,
			tv("options", "raise 5%; hold"), tv("assumption", "competitors hold prices")}}, true, "from the 3 Nov meeting")
	if err != nil {
		t.Fatal(err)
	}
	if got := committed(); got != nil {
		t.Fatalf("a proposal awaiting approval must not be committed: %v", got)
	}
	approve(open.ApprovalID)
	if got := committed(); got["status"] != "open" || got["question"] != "Raise 2027 list prices?" {
		t.Fatalf("after approving the opening: %v", got)
	}

	// 2. The agent proposes the decision; until alice approves, the committed state
	//    stays "open" while the agent's proposal is visible as its own word.
	dec, err := svc.Propose(ctx, agent, alice, knowledgecommit.Proposal{Kind: "casetest_e2e_decision", EntityID: "decision/pricing",
		Values: []domain.StatementValue{tv("status", "decided"), tv("chosen", "raise 5%")}}, true, "agreed")
	if err != nil {
		t.Fatal(err)
	}
	if got := committed(); got["status"] != "open" {
		t.Fatalf("the proposal leaked into the committed state: %v", got)
	}
	approve(dec.ApprovalID)
	got := committed()
	if got["status"] != "decided" || got["chosen"] != "raise 5%" {
		t.Fatalf("after approving the decision: %v", got)
	}
	// The committed row is asserted by the approver, not the proposing agent.
	asOf, err := plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: "casetest_e2e_decision",
		EntityID: "decision/pricing", AsOf: time.Now().Add(time.Minute)}, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawCommitted, sawAgent bool
	for _, r := range asOf.Rows {
		switch r["policy"] {
		case domain.ResolutionPolicyDesignatedActor:
			sawCommitted = r["asserted_by"] == "user:alice"
		case domain.ResolutionPolicyLatestAssertion:
			sawAgent = sawAgent || r["actor"] == "agent:assistant"
		}
	}
	if !sawCommitted || !sawAgent {
		t.Fatalf("want the committed row by user:alice and the agent's own row: %+v", asOf.Rows)
	}

	// 3. "decided" is only left by superseding: re-opening is refused at proposal time.
	if _, err := svc.Propose(ctx, agent, alice, knowledgecommit.Proposal{Kind: "casetest_e2e_decision", EntityID: "decision/pricing",
		Values: []domain.StatementValue{tv("status", "open"), tv("question", "again?"), owner}}, true, ""); err == nil ||
		!strings.Contains(err.Error(), `transition status "decided" -> "open" is not declared`) {
		t.Fatalf("re-open: %v", err)
	}
	// 4. The agent cannot write the committed value by any other door: asserting as
	//    alice through the proposal path is impossible (no actor field), and the
	//    commit marker is the kernel's.
	if _, err := svc.Propose(ctx, domain.UserPrincipal("alice"), alice, knowledgecommit.Proposal{Kind: "casetest_e2e_decision",
		EntityID: "decision/pricing", Values: []domain.StatementValue{tv("chosen", "hold")}}, false, ""); err != nil {
		t.Fatalf("a person's own proposal is a candidate like any other: %v", err)
	}
	if got := committed(); got["chosen"] != "raise 5%" {
		t.Fatalf("a candidate changed the committed value: %v", got)
	}
}
