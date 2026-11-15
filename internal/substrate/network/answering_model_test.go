package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/llmusage"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// ENG-453 (owner decision 2026-11-14): every result records the model that
// answered — the Execute result as _llm_models, the memory-call trailer as
// generator:<id>.calls#.

func TestFormatLLMUsage_CountsAnsweringGenerators(t *testing.T) {
	_, tally := domain.WithLLMUsageTally(context.Background())
	tally.Add("facts", domain.LLMUsage{Calls: 2})
	tally.AddGenerator("haiku-local", 2)
	f := usageFields(t, llmusage.Format(tally))
	if f["generator:haiku-local.calls#"] != "2" {
		t.Fatalf("trailer = %v, want generator:haiku-local.calls#=2", f)
	}
}

func TestStampSpend_RecordsAnsweringModels(t *testing.T) {
	meter := llm.NewSpendMeter(0, nil, nil)
	s := &Server{SpendMeter: meter}
	base := meter.Session("ses-1")
	meter.RecordSession("ses-1", "planner", "haiku-local", llm.TokenUsage{PromptTokens: 10})
	meter.RecordSession("ses-1", "agent_step", "haiku-local", llm.TokenUsage{PromptTokens: 10})
	meter.RecordSession("ses-1", "facts", "gemini", llm.TokenUsage{PromptTokens: 10})
	meta := map[string]string{}
	s.stampSpend(meta, "ses-1", base)
	if meta[MetaLLMModels] != "gemini=1,haiku-local=2" {
		t.Fatalf("_llm_models = %q", meta[MetaLLMModels])
	}
}

// The single-agent lane's result carries the same spend figures as a plan's,
// _llm_models included (it carried none before).
func TestExecute_SingleAgentLane_StampsTheAnsweringModel(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	sm := session.New(newMemSessionStore())
	fake := &bookingAgent{meter: meter, sm: sm}
	s := &Server{
		Dispatcher: fake,
		SpendMeter: meter,
		SessionMgr: sm,
		ExecCfg: config.ExecutionConfig{Lane: config.LaneConfig{
			Default:     config.LaneSingleAgent,
			SingleAgent: config.SingleAgentLaneConfig{AgentID: "lane_agent"},
		}},
	}
	out, err := s.Execute(context.Background(), &pb.Handoff{Payload: &pb.Object{Data: []byte("do it")}})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetMetadata()[MetaLLMModels] != "haiku-local=1" {
		t.Fatalf("metadata = %v, want _llm_models=haiku-local=1", out.GetMetadata())
	}
}

// bookingAgent answers and books one model call to the run's session, as an
// agent's gateway call would.
type bookingAgent struct {
	meter *llm.SpendMeter
	sm    *session.SessionManager
}

func (b *bookingAgent) Execute(context.Context, *domain.DispatchTask, *domain.Handoff) (*domain.DispatchResult, error) {
	return nil, nil
}

func (b *bookingAgent) CallAgent(ctx context.Context, _ string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	sid, _ := domain.SessionIDFromContext(ctx)
	b.meter.RecordSession(sid, "agent_step", "haiku-local", llm.TokenUsage{PromptTokens: 5})
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}, nil
}
