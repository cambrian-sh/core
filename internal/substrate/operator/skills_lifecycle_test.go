package operator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
)

// fakeLifecycle stands in for *lifecycle.Service; the lifecycle rules themselves
// are tested in internal/skill/lifecycle. This holds the operator plane to its
// contract: mapping, audit, dedupe, the role gate and the ListSkills fields.
type fakeLifecycle struct {
	skills    []lifecycle.Skill
	proposals []lifecycle.Proposal
	ledger    []lifecycle.LedgerEntry
	decides   []lifecycle.DecideInput
	byCommand map[string]lifecycle.MutationResult
	err       error
}

func (f *fakeLifecycle) Skills(context.Context) ([]lifecycle.Skill, error) { return f.skills, nil }
func (f *fakeLifecycle) Proposals(_ context.Context, fl domain.SkillProposalFilter) ([]lifecycle.Proposal, int, error) {
	var out []lifecycle.Proposal
	for _, p := range f.proposals {
		if (fl.State == "" || p.State == fl.State) && (fl.SkillName == "" || p.Name == fl.SkillName) {
			out = append(out, p)
		}
	}
	return out, len(out), nil
}

// mutate mimics the service's ledger-level command_id idempotency.
func (f *fakeLifecycle) mutate(cmd string) (lifecycle.MutationResult, error) {
	if f.err != nil {
		return lifecycle.MutationResult{}, f.err
	}
	if f.byCommand == nil {
		f.byCommand = map[string]lifecycle.MutationResult{}
	}
	if r, ok := f.byCommand[cmd]; ok {
		r.Deduped = true
		return r, nil
	}
	r := lifecycle.MutationResult{Version: "2", LedgerEntryID: fmt.Sprintf("sle-%d", len(f.byCommand)+1)}
	f.byCommand[cmd] = r
	return r, nil
}
func (f *fakeLifecycle) Decide(_ context.Context, in lifecycle.DecideInput) (lifecycle.MutationResult, error) {
	f.decides = append(f.decides, in)
	return f.mutate(in.CommandID)
}
func (f *fakeLifecycle) SetLock(_ context.Context, cmd, _ string, _ bool, _, _ string) (lifecycle.MutationResult, error) {
	return f.mutate(cmd)
}
func (f *fakeLifecycle) Ledger(_ context.Context, _ string, limit, offset int) ([]lifecycle.LedgerEntry, int, error) {
	es := f.ledger
	if offset > len(es) {
		offset = len(es)
	}
	es = es[offset:]
	if limit > 0 && limit < len(es) {
		es = es[:limit]
	}
	return es, len(f.ledger), nil
}
func (f *fakeLifecycle) Rollback(_ context.Context, cmd, _, _, _, _ string) (lifecycle.MutationResult, error) {
	return f.mutate(cmd)
}

func lifecycleService(f *fakeLifecycle) (*Service, *InMemoryAuditStore) {
	svc := NewService(NewSpool(SpoolConfig{}))
	audit := NewInMemoryAuditStore()
	svc.SetCommandSources(audit, domain.NewInMemoryGrantsStore())
	svc.SetSkillLifecycle(f)
	return svc, audit
}

func operatorCtx() context.Context {
	return ContextWithPrincipal(context.Background(), "alice", RoleOperator)
}

func TestSkillLifecycleRPCs_UnimplementedWithoutStore(t *testing.T) {
	svc := NewService(NewSpool(SpoolConfig{}))
	svc.SetCommandSources(NewInMemoryAuditStore(), domain.NewInMemoryGrantsStore())
	ctx := operatorCtx()
	checks := []error{
		func() error { _, err := svc.ListSkillProposals(ctx, &pb.ListSkillProposalsOpRequest{}); return err }(),
		func() error {
			_, err := svc.DecideSkillProposal(ctx, &pb.DecideSkillProposalOpRequest{CommandId: "c", Reason: "r", ProposalId: "p"})
			return err
		}(),
		func() error {
			_, err := svc.SetSkillLock(ctx, &pb.SetSkillLockOpRequest{CommandId: "c", Reason: "r", SkillName: "s"})
			return err
		}(),
		func() error { _, err := svc.ListSkillLedger(ctx, &pb.ListSkillLedgerOpRequest{}); return err }(),
		func() error {
			_, err := svc.RollbackSkill(ctx, &pb.RollbackSkillOpRequest{CommandId: "c", Reason: "r", SkillName: "s", LedgerEntryId: "e"})
			return err
		}(),
	}
	for i, err := range checks {
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("rpc %d: %v, want Unimplemented", i, err)
		}
	}
	if svc.HasSkillLifecycle() {
		t.Fatal("capability gate reports wired")
	}
}

// Mutations are Operator-only; the reads are open to a Viewer.
func TestSkillLifecycleRPCs_RoleGate(t *testing.T) {
	for _, m := range []string{"DecideSkillProposal", "SetSkillLock", "RollbackSkill"} {
		if !isOperatorOnly(operatorMethodPrefix+m, nil) {
			t.Errorf("%s must be operator-only", m)
		}
	}
	for _, m := range []string{"ListSkillProposals", "ListSkillLedger"} {
		if isOperatorOnly(operatorMethodPrefix+m, nil) {
			t.Errorf("%s is a read", m)
		}
	}
}

func TestDecideSkillProposal_AuditedAndDedupedOnCommandID(t *testing.T) {
	f := &fakeLifecycle{}
	svc, audit := lifecycleService(f)
	req := &pb.DecideSkillProposalOpRequest{CommandId: "cmd-1", Reason: "looks right", ProposalId: "sp-1", Approve: true, LockAfterApprove: true}
	first, err := svc.DecideSkillProposal(operatorCtx(), req)
	if err != nil || first.GetDeduped() || first.GetSkillVersion() != "2" || first.GetLedgerEntryId() == "" {
		t.Fatalf("first = %v err=%v", first, err)
	}
	if in := f.decides[0]; in.Actor != "alice" || !in.LockAfter || in.Reason != "looks right" {
		t.Fatalf("decide input = %+v", in)
	}
	again, err := svc.DecideSkillProposal(operatorCtx(), req)
	if err != nil || !again.GetDeduped() || again.GetLedgerEntryId() != first.GetLedgerEntryId() {
		t.Fatalf("retry = %v err=%v", again, err)
	}
	rows, _ := audit.Query(context.Background(), AuditFilter{CommandID: "cmd-1"})
	if len(rows) != 1 || rows[0].ActionType != "approve_skill_proposal" || rows[0].TargetID != "sp-1" {
		t.Fatalf("audit = %+v", rows)
	}
	if _, err := svc.DecideSkillProposal(operatorCtx(), &pb.DecideSkillProposalOpRequest{CommandId: "c2", ProposalId: "sp-1"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing reason: %v", err)
	}
}

func TestSkillLifecycle_ErrorMappingWritesNoAudit(t *testing.T) {
	f := &fakeLifecycle{err: fmt.Errorf("%w: stale base", lifecycle.ErrFailedPrecondition)}
	svc, audit := lifecycleService(f)
	_, err := svc.DecideSkillProposal(operatorCtx(), &pb.DecideSkillProposalOpRequest{CommandId: "c", Reason: "r", ProposalId: "p", Approve: true})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v", err)
	}
	f.err = fmt.Errorf("%w: skill x", lifecycle.ErrNotFound)
	if _, err := svc.SetSkillLock(operatorCtx(), &pb.SetSkillLockOpRequest{CommandId: "c", Reason: "r", SkillName: "x", Locked: true}); status.Code(err) != codes.NotFound {
		t.Fatalf("lock err = %v", err)
	}
	if rows, _ := audit.Query(context.Background(), AuditFilter{CommandID: "c"}); len(rows) != 0 {
		t.Fatalf("a refused command must not consume its command_id: %+v", rows)
	}
	f.err = nil
	if ack, err := svc.SetSkillLock(operatorCtx(), &pb.SetSkillLockOpRequest{CommandId: "c", Reason: "r", SkillName: "x", Locked: true}); err != nil || ack.GetDeduped() {
		t.Fatalf("retry after the fix = %v err=%v", ack, err)
	}
	rb, err := svc.RollbackSkill(operatorCtx(), &pb.RollbackSkillOpRequest{CommandId: "rb", Reason: "r", SkillName: "x", LedgerEntryId: "sle-1"})
	if err != nil || rb.GetLedgerEntryId() == "" {
		t.Fatalf("rollback = %v err=%v", rb, err)
	}
}

func TestListSkillProposalsAndLedger(t *testing.T) {
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	f := &fakeLifecycle{
		proposals: []lifecycle.Proposal{
			{SkillProposal: domain.SkillProposal{ID: "sp-1", Name: "tidy", State: "pending", ScanVerdict: "suspicious",
				ScanFindings: []string{"PI-05"}, SourceRunID: "run-1", ProposerAgentID: "agent-a", BaseVersion: "1", CreatedAt: at}, CurrentVersion: "2"},
			{SkillProposal: domain.SkillProposal{ID: "sp-2", Name: "tidy", State: "rejected"}},
		},
		ledger: []lifecycle.LedgerEntry{
			{SkillLedgerEntry: domain.SkillLedgerEntry{ID: "sle-2", SkillName: "tidy", Mutation: "rolled_back", RollbackOf: "sle-1", At: at}, Restorable: true},
			{SkillLedgerEntry: domain.SkillLedgerEntry{ID: "sle-1", SkillName: "tidy", Mutation: "approved"}, Restorable: true},
		},
	}
	svc, _ := lifecycleService(f)
	resp, err := svc.ListSkillProposals(operatorCtx(), &pb.ListSkillProposalsOpRequest{State: "pending"})
	if err != nil || resp.GetTotal() != 1 {
		t.Fatalf("proposals = %v err=%v", resp, err)
	}
	p := resp.GetProposals()[0]
	if p.GetSourceRunId() != "run-1" || p.GetCurrentVersion() != "2" || p.GetScanVerdict() != "suspicious" || p.GetCreatedAt() == nil {
		t.Fatalf("proposal op = %v", p)
	}
	l, err := svc.ListSkillLedger(operatorCtx(), &pb.ListSkillLedgerOpRequest{SkillName: "tidy", PageSize: 1})
	if err != nil || l.GetTotal() != 2 || len(l.GetEntries()) != 1 || l.GetEntries()[0].GetRollbackOf() != "sle-1" || !l.GetEntries()[0].GetRestorable() {
		t.Fatalf("ledger = %v err=%v", l, err)
	}
}

type staticSkills []domain.Skill

func (s staticSkills) All() []domain.Skill { return s }

func TestListSkills_LifecycleFieldsAndFilters(t *testing.T) {
	used := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeLifecycle{skills: []lifecycle.Skill{
		{SkillRecord: domain.SkillRecord{Name: "summarise", Source: "builtin", Version: "sha-abc", State: "active", Locked: true}},
		{SkillRecord: domain.SkillRecord{Name: "tidy", Source: "agent-proposed", Version: "3", State: "stale", UseCount: 7, SuccessCount: 5, LastUsedAt: used, LedgerEntryID: "sle-9"}, ExemptReason: "pinned"},
		{SkillRecord: domain.SkillRecord{Name: "old", Source: "agent-proposed", Version: "1", State: "archived"},
			Content: domain.Skill{Description: "Old one.", Instructions: "Old body."}},
	}}
	svc, _ := lifecycleService(f)
	svc.SetReadSources(nil, staticSkills{
		{Name: "summarise", Description: "Summarise.", SourcePath: "skills/summarise/SKILL.md"},
		{Name: "tidy", Description: "Tidy."},
		{Name: "wire", Description: "Registered over the wire."},
	}, nil)
	names := func(req *pb.ListSkillsOpRequest) []string {
		resp, err := svc.ListSkills(operatorCtx(), req)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range resp.GetSkills() {
			out = append(out, s.GetName())
		}
		return out
	}
	if got := fmt.Sprint(names(&pb.ListSkillsOpRequest{})); got != "[summarise tidy wire]" {
		t.Fatalf("default listing = %s (archived excluded)", got)
	}
	if got := fmt.Sprint(names(&pb.ListSkillsOpRequest{IncludeArchived: true})); got != "[old summarise tidy wire]" {
		t.Fatalf("include_archived = %s", got)
	}
	if got := fmt.Sprint(names(&pb.ListSkillsOpRequest{LifecycleState: "stale"})); got != "[tidy]" {
		t.Fatalf("state filter = %s", got)
	}
	if got := fmt.Sprint(names(&pb.ListSkillsOpRequest{Source: "agent-proposed", IncludeArchived: true})); got != "[old tidy]" {
		t.Fatalf("source filter = %s", got)
	}
	resp, _ := svc.ListSkills(operatorCtx(), &pb.ListSkillsOpRequest{IncludeArchived: true})
	byName := map[string]*pb.SkillOp{}
	for _, s := range resp.GetSkills() {
		byName[s.GetName()] = s
	}
	tidy := byName["tidy"]
	if tidy.GetVersion() != "3" || tidy.GetUseCount() != 7 || tidy.GetSuccessCount() != 5 || tidy.GetLastUsedAt() == nil ||
		tidy.GetExemptReason() != "pinned" || tidy.GetLedgerEntryId() != "sle-9" || tidy.GetLifecycleState() != "stale" {
		t.Fatalf("tidy = %v", tidy)
	}
	if !byName["summarise"].GetLocked() || byName["summarise"].GetSourcePath() != "skills/summarise/SKILL.md" {
		t.Fatalf("summarise = %v", byName["summarise"])
	}
	if byName["old"].GetBody() != "Old body." {
		t.Fatalf("archived skill listed from stored content: %v", byName["old"])
	}
	if byName["wire"].GetLifecycleState() != "active" || byName["wire"].GetSource() != "" {
		t.Fatalf("unmanaged skill = %v", byName["wire"])
	}
}
