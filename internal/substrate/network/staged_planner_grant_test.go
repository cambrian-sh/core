package network

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/ledger"
)

// T-20261112-staged-planner-s-ledger-reads-return-found: the staged planner reads
// the ledger entries of the runs its own Execute owns (the run-scope grant), and
// nothing else — not another run's entries by the same worker principal, not
// another person's private entries — through the enforcing ledger only.

// grantFixture records, through the real classified writer with
// private_by_default on (every entry stamped owner:<conversation owner>), what a
// composed worker writes in the planner's own run and in another run, plus a
// private entry of another person. The planner is a chat agent that delegated
// with its lease in the handoff, so its ctx carries an agent id and no
// conversation owner — the live run's situation.
func grantFixture(t *testing.T) (*Server, context.Context, map[string]string) {
	t.Helper()
	s := minimalServer(t)
	store := &memLedgerStore{rows: map[string]domain.LedgerEntry{}}
	w := ledger.NewRecorder(store, nil, ledger.RecorderConfig{PrivateByDefault: true})
	reader := authz.NewEnforcingLedger(store, nil)
	s.Ledger = &LedgerAccess{Writer: w, Reader: reader}
	s.ToolExecutor = &domain.ToolExecutor{KernelTools: ledger.Tools(reader, 0, nil)}

	ids := map[string]string{}
	rec := func(name, run, owner, writer, body string) {
		e, err := w.Record(context.Background(), domain.LedgerRecord{
			Kind:        domain.LedgerToolResult,
			Provenance:  domain.LedgerProvenance{RunID: run, StepIndex: 0, Owner: owner, TurnID: "turn-1"},
			ToolName:    "crm_get",
			Writer:      domain.AgentPrincipal(writer),
			ContentType: "application/json",
			Body:        []byte(body),
		})
		if err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
		ids[name] = e.ID
	}
	rec("own_stage1", "aaaa1111", "alice", "worker", `{"order":"stage one"}`)
	rec("own_stage2", "aaaa1111-s2", "alice", "worker", `{"order":"stage two"}`)
	rec("own_replan", "aaaa1111-s2-g1", "alice", "worker", `{"order":"replanned"}`)
	rec("other_run", "bbbb2222", "alice", "worker", `{"order":"another run"}`)
	rec("bob_in_run", "aaaa1111", "bob", "worker", `{"order":"bob private"}`)
	rec("bob_other", "cccc3333", "bob", "worker", `{"order":"bob elsewhere"}`)
	for name, id := range ids {
		if e, _ := store.Get(context.Background(), id); e == nil || !strings.Contains(strings.Join(e.Tags, ","), domain.OwnerTagPrefix) {
			t.Fatalf("%s was not stamped owner-private: %+v", name, e)
		}
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "chat_agent"))
	return s, ctx, ids
}

func plannerRun() *stagedRun {
	return &stagedRun{prov: domain.LedgerProvenance{RunID: "aaaa1111", Owner: "alice", StepIndex: -1},
		planRun: "ffff0000", stageRuns: []domain.RunID{"aaaa1111", "aaaa1111-s2"}}
}

func readable(t *testing.T, s *Server, ctx context.Context, eff *domain.TagPredicate, id string) bool {
	t.Helper()
	e, err := s.Ledger.Reader.Get(domain.WithScope(ctx, eff), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return e != nil
}

// The root cause, pinned: the caller's own predicate (an agent acting for
// nobody — no conversation owner on Execute's ctx) holds no owner, so the owner
// fence hides every private_by_default entry of the planner's own run.
func TestPlannerReadScope_CallerPredicateAloneHidesOwnRun(t *testing.T) {
	s, ctx, ids := grantFixture(t)
	eff, _ := s.authorizer().ReadFilter(ctx, artifactPrincipal(ctx), artifactSurface)
	if eff == nil || len(eff.Owners) != 0 {
		t.Fatalf("caller predicate = %+v, want one with no owners", eff)
	}
	if readable(t, s, ctx, eff, ids["own_stage1"]) {
		t.Fatal("the caller predicate alone read an owner-private entry; the fixture no longer reproduces the bug")
	}
}

func TestPlannerReadScope_GrantReadsOwnRunOnly(t *testing.T) {
	s, ctx, ids := grantFixture(t)
	eff := s.plannerReadScope(ctx, plannerRun())
	if eff == nil || eff.LedgerRuns == nil || eff.AllOwners || len(eff.Owners) != 0 {
		t.Fatalf("planner scope = %+v, want the caller predicate plus a run grant", eff)
	}
	for _, name := range []string{"own_stage1", "own_stage2", "own_replan"} {
		if !readable(t, s, ctx, eff, ids[name]) {
			t.Errorf("planner cannot read its own run's entry %s", name)
		}
	}
	if readable(t, s, ctx, eff, ids["other_run"]) {
		t.Error("planner read another run's entry by the same worker principal")
	}
	for _, name := range []string{"bob_in_run", "bob_other"} {
		if readable(t, s, ctx, eff, ids[name]) {
			t.Errorf("planner read another person's private entry %s", name)
		}
	}
}

// The same rule on the paths the planner actually uses: its ledger_get /
// ledger_find lookups and the per-stage ledger index.
func TestPlannerLookupAndIndex_UnderGrant(t *testing.T) {
	s, ctx, ids := grantFixture(t)
	r := plannerRun()
	look := s.plannerLedgerLookup(r)
	get := func(id string) bool {
		args, _ := json.Marshal(map[string]string{"id": id})
		out := look(ctx, domain.LedgerLookup{Tool: "ledger_get", Arguments: args})
		return strings.Contains(out, `"found":true`)
	}
	if !get(ids["own_stage2"]) {
		t.Error("ledger_get of the planner's own stage-2 entry is not found")
	}
	if get(ids["other_run"]) || get(ids["bob_in_run"]) || get(ids["bob_other"]) {
		t.Error("ledger_get crossed the grant")
	}
	idx := s.ledgerIndex(ctx, &stagedRun{prov: r.prov, planRun: r.planRun, stageRuns: r.stageRuns}, 20)
	if !strings.Contains(idx, ids["own_stage1"]) || !strings.Contains(idx, ids["own_stage2"]) {
		t.Errorf("ledger index misses the planner's own run:\n%s", idx)
	}
	for _, name := range []string{"other_run", "bob_in_run", "bob_other"} {
		if strings.Contains(idx, ids[name]) {
			t.Errorf("ledger index lists %s:\n%s", name, idx)
		}
	}
}

// Fail closed: no run id, no grant.
func TestPlannerReadScope_NoRunIDNoGrant(t *testing.T) {
	s, ctx, ids := grantFixture(t)
	eff := s.plannerReadScope(ctx, &stagedRun{prov: domain.LedgerProvenance{Owner: "alice"}})
	if eff == nil || eff.LedgerRuns != nil {
		t.Fatalf("planner scope without a run = %+v, want the caller predicate with no grant", eff)
	}
	if readable(t, s, ctx, eff, ids["own_stage1"]) {
		t.Error("an entry was read without a run grant")
	}
}
