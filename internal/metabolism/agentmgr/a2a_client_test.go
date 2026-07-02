package agentmgr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ─── Cycle 6: AgentManager dispatches RuntimeA2A to A2AClient ────────────────

func TestAgentManager_CallAgent_RuntimeA2A_UsesA2AClient(t *testing.T) {
	type a2aPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type a2aResultMessage struct {
		Parts []a2aPart `json:"parts"`
	}
	type a2aResult struct {
		Message a2aResultMessage `json:"message"`
	}
	type a2aTaskResponse struct {
		Result a2aResult `json:"result"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := a2aTaskResponse{
			Result: a2aResult{
				Message: a2aResultMessage{
					Parts: []a2aPart{{Type: "text", Text: "a2a-result"}},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	reg := newTestRegistry()
	reg.agents["a2a-agent"] = domain.AgentDefinition{
		ID:          "a2a-agent",
		Runtime:     domain.RuntimeA2A,
		A2AEndpoint: srv.URL,
	}

	manager := NewAgentManager(reg, "python", "127.0.0.1:50051", nil)

	handoff := &domain.Handoff{
		ID:      "test-handoff",
		Payload: &domain.Payload{Data: []byte("input")},
	}

	result, err := manager.CallAgent(context.Background(), "a2a-agent", handoff)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if string(result.Payload.Data) != "a2a-result" {
		t.Errorf("expected a2a-result, got %q", string(result.Payload.Data))
	}
}
