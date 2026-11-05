// Package fakemodels is a test double for the LLM gateway's model source
// (network.StepModels, which *llm.Provider implements in production).
//
// It lives in its own leaf package — importing only domain — so the gateway's own
// in-package tests, the supervision tests and the system harness can share it
// without an import cycle. It records every request it serves, which is how the
// gateway tests prove an RPC field reached "the adapter".
//
// It does NOT re-implement the Provider: no breaker, no fallback walk. The first
// registered candidate serves the call. Fallback policy is tested on the Provider.
package fakemodels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// Models maps generator ids to fake models. A model may be a domain.RequestGenerator
// (served as is), or a legacy fake: a domain.LLMStreamer for streams, a
// domain.ToolCallingGenerator or domain.Generator for unary turns.
type Models struct {
	mu       sync.Mutex
	byID     map[string]any
	requests []domain.GenerateRequest
	served   []string
}

// New returns an empty model source.
func New() *Models { return &Models{byID: make(map[string]any)} }

// Set registers model under id. A "llm:" agent-id prefix is accepted and stripped,
// as the gateway strips it from allocations.
func (m *Models) Set(id string, model any) *Models {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[strings.TrimPrefix(id, "llm:")] = model
	return m
}

// Requests returns every request served, in order.
func (m *Models) Requests() []domain.GenerateRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.GenerateRequest(nil), m.requests...)
}

// Served returns the generator id of every call served, in order.
func (m *Models) Served() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.served...)
}

// ErrNoModel is returned when no candidate is registered.
var ErrNoModel = errors.New("fakemodels: no candidate registered")

func (m *Models) pick(candidates []string, req domain.GenerateRequest) (any, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range candidates {
		if model, ok := m.byID[id]; ok {
			m.requests = append(m.requests, req)
			m.served = append(m.served, id)
			return model, id, nil
		}
	}
	return nil, "", fmt.Errorf("%w: %v", ErrNoModel, candidates)
}

// GenerateOn implements network.StepModels.
func (m *Models) GenerateOn(ctx context.Context, candidates []string, req domain.GenerateRequest) (domain.GenerateResponse, string, error) {
	model, id, err := m.pick(candidates, req)
	if err != nil {
		return domain.GenerateResponse{}, "", err
	}
	if rg, ok := model.(domain.RequestGenerator); ok {
		resp, err := rg.GenerateRequest(ctx, req)
		return resp, id, err
	}
	if len(req.Tools) > 0 || hasToolTurns(req) {
		tg, ok := model.(domain.ToolCallingGenerator)
		if !ok || !domain.SupportsToolCalling(model) {
			return domain.GenerateResponse{}, id, domain.UnsupportedField(domain.FieldTools, "fake has no native tool-calling")
		}
		turn, err := tg.GenerateWithTools(ctx, req.Messages, req.Tools)
		return domain.GenerateResponse{Text: turn.Text, ToolCalls: turn.ToolCalls, StopReason: turn.StopReason}, id, err
	}
	if g, ok := model.(domain.Generator); ok {
		text, err := g.Generate(ctx, Prompt(req))
		return domain.GenerateResponse{Text: text, StopReason: domain.StopEndTurn}, id, err
	}
	return domain.GenerateResponse{}, id, fmt.Errorf("fakemodels: %T cannot serve a unary call", model)
}

// StreamOn implements network.StepModels.
func (m *Models) StreamOn(ctx context.Context, candidates []string, req domain.GenerateRequest) (<-chan domain.StreamChunk, string, error) {
	model, id, err := m.pick(candidates, req)
	if err != nil {
		return nil, "", err
	}
	if rg, ok := model.(domain.RequestGenerator); ok {
		ch, err := rg.StreamRequest(ctx, req)
		return ch, id, err
	}
	if s, ok := model.(domain.LLMStreamer); ok {
		ch, err := s.GenerateStream(ctx, Prompt(req))
		return ch, id, err
	}
	return nil, id, fmt.Errorf("fakemodels: %T cannot stream", model)
}

// Prompt flattens a request into the single string a legacy fake expects: the
// system text, then each message's content, newline-separated. A request built
// from domain.UserPrompt(p) flattens to exactly p.
func Prompt(req domain.GenerateRequest) string {
	parts := make([]string, 0, len(req.System)+len(req.Messages))
	for _, s := range req.System {
		parts = append(parts, s.Text)
	}
	for _, msg := range req.Messages {
		parts = append(parts, msg.Content)
	}
	return strings.Join(parts, "\n")
}

func hasToolTurns(req domain.GenerateRequest) bool {
	for _, msg := range req.Messages {
		if msg.Role == domain.RoleTool || len(msg.ToolCalls) > 0 {
			return true
		}
	}
	return false
}
