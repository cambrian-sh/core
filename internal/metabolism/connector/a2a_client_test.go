package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ─── Cycle 1: FetchCard happy path ───────────────────────────────────────────

func TestA2AClient_FetchCard_HappyPath(t *testing.T) {
	card := domain.AgentCard{
		Name:        "test-agent",
		Description: "A test agent",
		Version:     "1.2.3",
		Skills: []domain.A2ASkill{
			{Description: "does something"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/agent.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer srv.Close()

	client := New()
	got, err := client.FetchCard(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchCard returned error: %v", err)
	}
	if got.Name != "test-agent" {
		t.Errorf("expected Name=%q, got %q", "test-agent", got.Name)
	}
	if got.Description != "A test agent" {
		t.Errorf("expected Description=%q, got %q", "A test agent", got.Description)
	}
	if got.Version != "1.2.3" {
		t.Errorf("expected Version=%q, got %q", "1.2.3", got.Version)
	}
}

// ─── Cycle 2: FetchCard error on 404 ─────────────────────────────────────────

func TestA2AClient_FetchCard_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := New()
	_, err := client.FetchCard(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected non-nil error on 404, got nil")
	}
}

// ─── Cycle 3 & 4: Execute — Handoff→Task field mapping + response→Handoff ────

func TestA2AClient_Execute_RequestAndResponse(t *testing.T) {
	type a2aPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type a2aMessage struct {
		Role  string    `json:"role"`
		Parts []a2aPart `json:"parts"`
	}
	type a2aTaskRequest struct {
		ID       string            `json:"id"`
		Message  a2aMessage        `json:"message"`
		Metadata map[string]string `json:"metadata"`
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

	var capturedBody a2aTaskRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp := a2aTaskResponse{
			Result: a2aResult{
				Message: a2aResultMessage{
					Parts: []a2aPart{{Type: "text", Text: "response text"}},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := New()
	agent := domain.AgentDefinition{
		ID:          "agent-a2a",
		A2AEndpoint: srv.URL,
	}
	handoff := &domain.Handoff{
		ID:      "handoff-123",
		Payload: &domain.Payload{Data: []byte("hello")},
		Context: map[string]string{"key": "val"},
	}

	result, err := client.Execute(context.Background(), agent, handoff)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if capturedBody.ID != "handoff-123" {
		t.Errorf("expected id=%q, got %q", "handoff-123", capturedBody.ID)
	}
	if capturedBody.Message.Role != "user" {
		t.Errorf("expected role=%q, got %q", "user", capturedBody.Message.Role)
	}
	if len(capturedBody.Message.Parts) == 0 || capturedBody.Message.Parts[0].Text != "hello" {
		t.Errorf("expected parts[0].text=%q, got %v", "hello", capturedBody.Message.Parts)
	}
	if capturedBody.Metadata["key"] != "val" {
		t.Errorf("expected metadata.key=%q, got %q", "val", capturedBody.Metadata["key"])
	}

	if string(result.Payload.Data) != "response text" {
		t.Errorf("expected Payload.Data=%q, got %q", "response text", string(result.Payload.Data))
	}
	if result.FromAgent != "agent-a2a" {
		t.Errorf("expected FromAgent=%q, got %q", "agent-a2a", result.FromAgent)
	}
	if result.ID != "handoff-123" {
		t.Errorf("expected Id=%q, got %q", "handoff-123", result.ID)
	}
}

// ─── Cycle 5: Execute — error on HTTP 500 ────────────────────────────────────

func TestA2AClient_Execute_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := New()
	agent := domain.AgentDefinition{
		ID:          "agent-err",
		A2AEndpoint: srv.URL,
	}
	handoff := &domain.Handoff{
		ID:      "h-err",
		Payload: &domain.Payload{Data: []byte("data")},
	}

	result, err := client.Execute(context.Background(), agent, handoff)
	if err == nil {
		t.Fatal("expected non-nil error on HTTP 500, got nil")
	}
	if result != nil {
		t.Errorf("expected nil Handoff on error, got %v", result)
	}
}
