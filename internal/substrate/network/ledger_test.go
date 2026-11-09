package network

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

type memLedgerReader struct{ rows map[string]domain.LedgerEntry }

func (m memLedgerReader) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	e, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &e, nil
}
func (memLedgerReader) Find(context.Context, domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	return nil, nil
}

func ledgerServer() *Server {
	raw := memLedgerReader{rows: map[string]domain.LedgerEntry{
		"led_alice": {ID: "led_alice", Kind: domain.LedgerStepOutput, RunID: "r", StepIndex: 1,
			Tags: []string{domain.OwnerTag("alice")}, Body: []byte("total 1330.50")},
	}}
	return &Server{Ledger: &LedgerAccess{Writer: nopLedgerWriter{}, Reader: authz.NewEnforcingLedger(raw, nil)}}
}

type nopLedgerWriter struct{}

func (nopLedgerWriter) Record(context.Context, domain.LedgerRecord) (domain.LedgerEntry, error) {
	return domain.LedgerEntry{}, errors.New("unused")
}

// ADR-0145 D1: a ledger reference handed to a worker (a shortened step result, an
// offloaded tool result) resolves through GetContextNode — via the ENFORCED reader
// under the caller's own predicate, so another person's entry reads as unknown.
func TestGetContextNode_ResolvesLedgerIDsThroughTheEnforcedReader(t *testing.T) {
	s := ledgerServer()
	alice := domain.WithConversationOwner(context.Background(), "alice")
	resp, err := s.GetContextNode(alice, &pb.ContextNodeRequest{Cid: "led_alice"})
	if err != nil || string(resp.Data) != "total 1330.50" || resp.Type != "ledger_step_output" {
		t.Fatalf("owner read: %+v %v", resp, err)
	}
	bob := domain.WithConversationOwner(context.Background(), "bob")
	if resp, _ = s.GetContextNode(bob, &pb.ContextNodeRequest{Cid: "led_alice"}); len(resp.Data) != 0 {
		t.Fatalf("bob read alice's ledger entry: %q", resp.Data)
	}
	// Ledger off: a led_ id is unknown, never looked up in the content store.
	off := &Server{ContentStore: &fakeCS{nodes: map[domain.CID]*domain.ContextNode{"led_alice": {Data: []byte("x")}}}}
	if resp, _ = off.GetContextNode(alice, &pb.ContextNodeRequest{Cid: "led_alice"}); len(resp.Data) != 0 {
		t.Fatalf("ledger off served %q", resp.Data)
	}
}

// ADR-0145 D8: the structured Execute result rides the metadata and consumes the
// executor's internal step index.
func TestStampExecuteResult(t *testing.T) {
	s := ledgerServer()
	refs, _ := json.Marshal([]domain.ExecuteStepRef{
		{Step: 0, Query: "fetch", Status: domain.ExecuteStepCompleted, Ref: "led_a", Preview: "[...]", Bytes: 5},
		{Step: 1, Query: "total", Status: domain.ExecuteStepNotRun},
	})
	meta := map[string]string{executer.PlanStepsKey: string(refs)}
	actions := []domain.PlanAction{{Step: 0, Tool: "mcp:crm/update", Status: domain.PlanActionDone, IDs: []string{"id=C-4"}}}
	pe := &executer.PartialPlanError{FailedStep: 1, LastError: errors.New("boom")}
	s.stampExecuteResult(meta, "report text", "run-1", "ses-1", actions, pe, errors.New("plan_idle_timeout_ms"))
	if _, left := meta[executer.PlanStepsKey]; left {
		t.Fatal("internal step index leaked into the response metadata")
	}
	var got domain.ExecuteResult
	if err := json.Unmarshal([]byte(meta[domain.HandoffKeyExecuteResult]), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run-1" || got.SessionID != "ses-1" || got.Report != "report text" || !got.Ledger ||
		!got.Partial || !got.TimedOut || got.FailedStep != 1 || got.Error != "boom" ||
		len(got.Steps) != 2 || got.Steps[0].Ref != "led_a" || len(got.Actions) != 1 || got.Actions[0].IDs[0] != "id=C-4" {
		t.Fatalf("result = %+v", got)
	}
}

func TestAppendKernelTools(t *testing.T) {
	kernel := []domain.SystemTool{{Name: "ledger_get"}, {Name: "ledger_find"}}
	menu := appendKernelTools([]domain.SystemTool{{Name: "crm_get"}}, kernel, nil)
	if len(menu) != 3 {
		t.Fatalf("menu = %+v", menu)
	}
	named := appendKernelTools(nil, kernel, []string{"ledger_find"})
	if len(named) != 1 || named[0].Name != "ledger_find" {
		t.Fatalf("describe_tool menu = %+v", named)
	}
}
