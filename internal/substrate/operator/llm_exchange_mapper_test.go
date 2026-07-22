package operator

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0079: AgentLLMExchangeEvent maps to the AgentLLMExchangeOp feed payload — the
// provider-tap capture of one agent reasoning turn (full prompt + completion).
func TestToOperatorEvent_AgentLLMExchange(t *testing.T) {
	se := domain.SequencedEvent{
		Seq: 11,
		At:  time.Unix(0, 0).UTC(),
		Event: domain.AgentLLMExchangeEvent{
			SessionID:     "sess-1",
			AgentID:       "analyst_agent",
			StepIndex:     2,
			Purpose:       "agent_llm",
			ModelID:       "trait-model-x",
			Prompt:        "PROMPT BODY",
			Completion:    `{"action":"memory_query","query":"q4 revenue"}`,
			PromptChars:   4096,
			ResponseChars: 44,
		},
	}
	ev := toOperatorEvent(se)
	ex := ev.GetLlmExchange()
	if ex == nil {
		t.Fatalf("expected AgentLLMExchangeOp payload, got %T", ev.GetPayload())
	}
	if ev.GetSessionId() != "sess-1" {
		t.Fatalf("session id not propagated to envelope: %q", ev.GetSessionId())
	}
	if ex.GetAgentId() != "analyst_agent" || ex.GetStepIndex() != 2 ||
		ex.GetPurpose() != "agent_llm" || ex.GetModelId() != "trait-model-x" ||
		ex.GetRequest() != "PROMPT BODY" ||
		ex.GetResponse() != `{"action":"memory_query","query":"q4 revenue"}` ||
		ex.GetRequestChars() != 4096 || ex.GetResponseChars() != 44 {
		t.Fatalf("unexpected mapping: %+v", ex)
	}
}
