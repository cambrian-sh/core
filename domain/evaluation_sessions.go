package domain

import "sync"

// InMemoryEvaluationSessions is the default EvaluationSessionSet: a thread-safe
// set of session token IDs that are currently part of a sandboxed evaluation
// (the graded interview, ADR-0037). The interview runner Marks a token when it
// mints the session and Unmarks it on completion; the ToolExecutor consults
// IsEvaluation at the dangerous-tool approval gate to auto-approve within the
// sandbox. It is deliberately tiny — membership, nothing else.
type InMemoryEvaluationSessions struct {
	mu     sync.RWMutex
	tokens map[string]struct{}
}

// NewInMemoryEvaluationSessions constructs an empty evaluation-session set.
func NewInMemoryEvaluationSessions() *InMemoryEvaluationSessions {
	return &InMemoryEvaluationSessions{tokens: make(map[string]struct{})}
}

// Mark records a session token as an active evaluation session. A no-op on empty.
func (s *InMemoryEvaluationSessions) Mark(sessionTokenID string) {
	if sessionTokenID == "" {
		return
	}
	s.mu.Lock()
	s.tokens[sessionTokenID] = struct{}{}
	s.mu.Unlock()
}

// Unmark removes a session token (called when the evaluation session completes).
func (s *InMemoryEvaluationSessions) Unmark(sessionTokenID string) {
	if sessionTokenID == "" {
		return
	}
	s.mu.Lock()
	delete(s.tokens, sessionTokenID)
	s.mu.Unlock()
}

// IsEvaluation reports whether the token is a currently-active evaluation session.
// An empty token is never an evaluation (fail-closed to operator approval).
func (s *InMemoryEvaluationSessions) IsEvaluation(sessionTokenID string) bool {
	if sessionTokenID == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.tokens[sessionTokenID]
	s.mu.RUnlock()
	return ok
}

var _ EvaluationSessionSet = (*InMemoryEvaluationSessions)(nil)
