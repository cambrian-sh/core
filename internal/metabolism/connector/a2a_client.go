package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cambrian-sh/core/domain"
)

// a2aPart is a single content part inside an A2A message.
type a2aPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// a2aMessage is the message envelope for an A2A task.
type a2aMessage struct {
	Role  string    `json:"role"`
	Parts []a2aPart `json:"parts"`
}

// a2aTaskRequest is the outgoing JSON body for POST {endpoint}/tasks.
type a2aTaskRequest struct {
	ID       string            `json:"id"`
	Message  a2aMessage        `json:"message"`
	Metadata map[string]string `json:"metadata"`
}

// a2aTaskResponse is the incoming JSON body from POST {endpoint}/tasks.
type a2aTaskResponse struct {
	Result a2aResult `json:"result"`
}

type a2aResult struct {
	Message a2aResultMessage `json:"message"`
}

type a2aResultMessage struct {
	Parts []a2aPart `json:"parts"`
}

// ─── A2AClient ────────────────────────────────────────────────────────────────

// A2AClient owns all Google A2A protocol concerns: fetching Agent Cards and
// executing tasks via the A2A task protocol.
type A2AClient struct {
	http *http.Client
}

// NewA2AClient creates a new A2AClient backed by the standard http.Client.
func New() *A2AClient {
	return &A2AClient{http: &http.Client{}}
}

// FetchCard GETs {endpoint}/.well-known/agent.json and parses the Agent Card.
func (c *A2AClient) FetchCard(ctx context.Context, endpoint string) (*domain.AgentCard, error) {
	url := endpoint + "/.well-known/agent.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("a2a: build FetchCard request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: FetchCard HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: FetchCard returned status %d", resp.StatusCode)
	}

	var card domain.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, fmt.Errorf("a2a: FetchCard decode: %w", err)
	}
	return &card, nil
}

// Execute translates a Handoff into an A2A Task, POSTs to {agent.A2AEndpoint}/tasks,
// and translates the response back to a Handoff.
// A2A agents have no local workspace, so no CoW snapshot is taken.
func (c *A2AClient) Execute(ctx context.Context, agent domain.AgentDefinition, handoff *domain.Handoff) (*domain.Handoff, error) {
	metadata := make(map[string]string)
	for k, v := range handoff.Context {
		metadata[k] = v
	}

	payloadText := ""
	if handoff.Payload != nil {
		payloadText = string(handoff.Payload.Data)
	}

	body := a2aTaskRequest{
		ID: handoff.ID,
		Message: a2aMessage{
			Role: "user",
			Parts: []a2aPart{
				{Type: "text", Text: payloadText},
			},
		},
		Metadata: metadata,
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("a2a: marshal task request: %w", err)
	}

	url := agent.A2AEndpoint + "/tasks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("a2a: build Execute request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: Execute HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: Execute returned status %d", resp.StatusCode)
	}

	var taskResp a2aTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&taskResp); err != nil {
		return nil, fmt.Errorf("a2a: Execute decode response: %w", err)
	}

	var resultText string
	if len(taskResp.Result.Message.Parts) > 0 {
		resultText = taskResp.Result.Message.Parts[0].Text
	}

	return &domain.Handoff{
		ID:        handoff.ID,
		FromAgent: agent.ID,
		Payload:   &domain.Payload{Data: []byte(resultText)},
	}, nil
}
