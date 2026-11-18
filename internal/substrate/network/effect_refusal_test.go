package network

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	metabolism "github.com/cambrian-sh/core/internal/metabolism"
	agentmgr "github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

// Effect-checked dispatch (T-20261106-kernel-does-not-check-that-a-declared): the
// server threads a step's required_effects into the DispatchTask, and a plan that
// ends on an effect refusal comes back as a QUESTION for the user, never as the
// work of an arbitrary agent.

// refusingDispatcher refuses every step that declares effects, and records them.
type refusingDispatcher struct {
	mu      sync.Mutex
	effects [][]string
}

func (d *refusingDispatcher) Execute(_ context.Context, task *domain.DispatchTask, _ *domain.Handoff) (*domain.DispatchResult, error) {
	d.mu.Lock()
	d.effects = append(d.effects, task.RequiredEffects)
	d.mu.Unlock()
	if len(task.RequiredEffects) > 0 {
		return nil, &domain.NoEffectMatchError{TaskID: task.ID, Missing: task.RequiredEffects, Check: &domain.EffectCheckResult{
			Required: task.RequiredEffects, Outcome: domain.EffectCheckRefused,
			AgentEffects: map[string][]string{"assistant": {"read", "write"}},
		}}
	}
	return &domain.DispatchResult{Handoff: &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}}, nil
}

func (d *refusingDispatcher) CallAgent(context.Context, string, *domain.Handoff, string) (*domain.Handoff, error) {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
}

type effectPlanner struct{ plan *domain.ExecutionPlan }

func (p *effectPlanner) GetExecutionPlan(context.Context, string) (*domain.ExecutionPlan, error) {
	return p.plan.Clone(), nil
}
func (p *effectPlanner) Generate(context.Context, string) (string, error) { return "", nil }

func TestServer_EffectRefusalEndsAsAClarification(t *testing.T) {
	disp := &refusingDispatcher{}
	s := &Server{
		Planner: &effectPlanner{plan: &domain.ExecutionPlan{Subject: "receipts", Steps: []domain.Step{
			{Query: "Move each receipt into Documents/Receipts", RequiredEffects: []string{"move"}},
		}}},
		ExecCfg:    tokenTestExecCfg(), // MaxReplanAttempts 0: the one replan is not available, ask at once
		Dispatcher: disp,
		Manager:    agentmgr.NewAgentManager(metabolism.NewInMemoryRegistry(), "unix:///tmp/test.sock", nil),
	}
	resp, err := s.Execute(context.Background(), handoffWithBody("file my receipts"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := disp.effects; len(got) == 0 || !reflect.DeepEqual(got[0], []string{"move"}) {
		t.Fatalf("the step's required_effects did not reach dispatch: %v", got)
	}
	if resp.Payload.Type != "clarification" {
		t.Fatalf("payload.type = %q, want clarification (data: %s)", resp.Payload.Type, resp.Payload.Data)
	}
	var body struct {
		Question string   `json:"question"`
		Missing  []string `json:"missing_effects"`
		Options  []struct {
			Label       string `json:"label"`
			Recommended bool   `json:"recommended"`
		} `json:"options"`
	}
	if err := json.Unmarshal(resp.Payload.Data, &body); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if !strings.Contains(body.Question, "can move") || !reflect.DeepEqual(body.Missing, []string{"move"}) || len(body.Options) == 0 || !body.Options[0].Recommended {
		t.Errorf("clarification = %+v", body)
	}
	if resp.Metadata["_effect_refused"] != "move" {
		t.Errorf("metadata does not name the refusal: %v", resp.Metadata)
	}
}

// A legacy step (no required_effects) reaches dispatch with nil — "unverified",
// not "needs none" — and runs as before.
func TestServer_LegacyStepDispatchesWithNilEffects(t *testing.T) {
	disp := &refusingDispatcher{}
	s := &Server{
		Planner:    &effectPlanner{plan: &domain.ExecutionPlan{Subject: "x", Steps: []domain.Step{{Query: "do it"}}}},
		ExecCfg:    tokenTestExecCfg(),
		Dispatcher: disp,
		Manager:    agentmgr.NewAgentManager(metabolism.NewInMemoryRegistry(), "unix:///tmp/test.sock", nil),
	}
	resp, err := s.Execute(context.Background(), handoffWithBody("do it"))
	if err != nil || resp.Payload.Type == "clarification" {
		t.Fatalf("legacy step: %v / %+v", err, resp)
	}
	if len(disp.effects) != 1 || disp.effects[0] != nil {
		t.Fatalf("want nil effects for a legacy step, got %#v", disp.effects)
	}
}
