package pypackages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// ToolName is the runtime package request tool (ADR-0157 D7.9).
const ToolName = "request_python_package"

// maxRationale bounds the agent's rationale on the card.
const maxRationale = 1000

// ToolDeps are what the tool reads to decide who may call it. Every one is
// required: a missing one refuses the tool (fail closed).
type ToolDeps struct {
	// LeaseAgent resolves a lease to the agent it was dispatched to ("" or
	// false when unknown).
	LeaseAgent func(lease domain.LeaseID) (string, bool)
	// IsChild reports a run_subtasks worker's lease (ADR-0150: fan-out workers
	// are read-only and never offered it).
	IsChild func(lease domain.LeaseID) bool
	// Agent returns the agent's definition and manifest.
	Agent func(ctx context.Context, agentID string) (*domain.AgentDefinition, *domain.AgentManifest, error)
}

// Tool is request_python_package: a kernel tool, so the executor dispatches
// it before grants, effects and approvals (domain/tool_executor.go) — it gates
// itself. Its approval is the python_package card (D7.8) every request raises;
// the agent never installs anything itself.
//
// Who may call it (D7.9.1-2): a Python agent the kernel spawns, whose profile
// tool allowlist names request_python_package exactly (a glob such as "*" does
// not grant an admin-effect tool by accident), and never a subtask worker.
type Tool struct {
	svc  *Service
	wait time.Duration
	deps ToolDeps
}

var (
	_ domain.KernelTool      = (*Tool)(nil)
	_ domain.KernelToolOffer = (*Tool)(nil)
)

// NewTool builds the tool over the package service. wait is the tool window
// (execution.agents.python.request_wait_seconds).
func NewTool(svc *Service, wait time.Duration, deps ToolDeps) (*Tool, error) {
	if svc == nil {
		return nil, errors.New("request_python_package: no python package service")
	}
	if deps.LeaseAgent == nil || deps.IsChild == nil || deps.Agent == nil {
		return nil, errors.New("request_python_package: lease, subtask and agent lookups are all required")
	}
	if wait <= 0 {
		wait = 60 * time.Second
	}
	return &Tool{svc: svc, wait: wait, deps: deps}, nil
}

// Spec is the menu entry. Effect admin (D7.9.2): it changes the kernel-managed
// environment the agent runs on.
func (t *Tool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolName,
		Description: fmt.Sprintf("Ask for a Python package you need that is not installed. An operator approves it; "+
			"once approved it is installed into your own environment (wheels only, hash-checked) and importable from "+
			"your next run. Give the exact version and why you need it. Waits up to %d s for the decision, then returns "+
			"status installed, pending (the request stays open; asking again returns the same request), denied, "+
			"unresolvable (with the reason) or already_installed. Never install packages any other way.", int(t.wait/time.Second)),
		Schema: []byte(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"the package's name on the package index"},` +
			`"version":{"type":"string","description":"the exact version, e.g. 1.2.3"},` +
			`"extras":{"type":"array","items":{"type":"string"},"description":"optional extras"},` +
			`"rationale":{"type":"string","description":"what you need it for (shown to the operator)"}},` +
			`"required":["name","version","rationale"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectAdmin},
	}
}

// OfferedTo lists the tool only for an eligible caller's lease.
func (t *Tool) OfferedTo(ctx context.Context, lease domain.LeaseID) bool {
	agentID, ok := t.deps.LeaseAgent(lease)
	if !ok || agentID == "" {
		return false
	}
	return t.eligible(ctx, agentID, lease) == nil
}

// eligible is the one gate OfferedTo and Call share.
func (t *Tool) eligible(ctx context.Context, agentID string, lease domain.LeaseID) error {
	if t.deps.IsChild(lease) {
		return errors.New("request_python_package is not available to a subtask worker (read-only fan-out, ADR-0150)")
	}
	def, man, err := t.deps.Agent(ctx, agentID)
	if err != nil || def == nil {
		return fmt.Errorf("request_python_package: agent %q is not registered", agentID)
	}
	if def.Runtime != domain.RuntimePython {
		return fmt.Errorf("request_python_package is for Python agents the kernel runs (agent %q runs %q)", agentID, def.Runtime)
	}
	if man == nil || !namesExactly(man.ToolAllowlist, ToolName) {
		return fmt.Errorf("request_python_package is not in agent %q's tool allowlist; its profile must name it", agentID)
	}
	return nil
}

func namesExactly(allow []string, name string) bool {
	for _, p := range allow {
		if p == name {
			return true
		}
	}
	return false
}

type requestArgs struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Extras    []string `json:"extras,omitempty"`
	Rationale string   `json:"rationale"`
}

// Call raises (or reuses) the request and waits the tool window for the answer.
func (t *Tool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	if call.AgentID == "" {
		return nil, errors.New("request_python_package: no authenticated caller")
	}
	if err := t.eligible(ctx, call.AgentID, call.SessionTokenID); err != nil {
		return nil, err
	}
	var a requestArgs
	dec := json.NewDecoder(strings.NewReader(string(call.ArgsJSON)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("request_python_package: arguments must be {name, version, extras?, rationale}: %v", err)
	}
	a.Rationale = strings.TrimSpace(a.Rationale)
	if a.Rationale == "" {
		return nil, errors.New("request_python_package: rationale is required (say what you need the package for)")
	}
	if utf8.RuneCountInString(a.Rationale) > maxRationale {
		a.Rationale = string([]rune(a.Rationale)[:maxRationale]) + "…"
	}
	res, err := t.svc.Request(ctx, call.AgentID,
		domain.PythonPackage{Name: strings.TrimSpace(a.Name), Version: strings.TrimSpace(a.Version), Extras: a.Extras},
		a.Rationale, RuntimeRequester+call.AgentID, t.wait)
	if err != nil {
		return nil, fmt.Errorf("request_python_package: %w", err)
	}
	return json.Marshal(res)
}
