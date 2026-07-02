package operator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// CommandEffects applies the kernel-side effect of an operator mutation. It is
// the seam between the audited command surface and the kernel surfaces (scope
// resolver, skill/MCP registries, lifecycle). NoopEffects audits without effect;
// real adapters wire the live surfaces. ADR-0047 0047-07.
type CommandEffects interface {
	TagMemory(ctx context.Context, docID, tag string, add bool) error
	SetScope(ctx context.Context, agentID string, required, anyOf, forbidden []string) error
	RegisterSkill(ctx context.Context, name, description, instructions string, toolGrants, scopeTags []string) error
	RegisterMCP(ctx context.Context, name, command, url string) error
	TriggerConsolidation(ctx context.Context, scope string) error
	// SetRuntimeConfig hot-applies numeric tunables (e.g. blend weights) to the
	// live kernel without a restart. Ephemeral (in-memory). ADR-0054 tuning seam.
	SetRuntimeConfig(ctx context.Context, params map[string]float64) error
}

// NoopEffects audits a command without applying a kernel effect (honest default
// where the kernel surface is not yet wired).
type NoopEffects struct{}

func (NoopEffects) TagMemory(context.Context, string, string, bool) error             { return nil }
func (NoopEffects) SetScope(context.Context, string, []string, []string, []string) error { return nil }
func (NoopEffects) RegisterSkill(context.Context, string, string, string, []string, []string) error {
	return nil
}
func (NoopEffects) RegisterMCP(context.Context, string, string, string) error { return nil }
func (NoopEffects) TriggerConsolidation(context.Context, string) error        { return nil }
func (NoopEffects) SetRuntimeConfig(context.Context, map[string]float64) error { return nil }

// EventBusEffects implements the effects that can be driven purely off the
// EventBus today — TriggerConsolidation publishes a MemoryPressureEvent. The
// remaining effects fall through to NoopEffects until their surfaces are wired.
type EventBusEffects struct {
	NoopEffects
	Bus domain.EventBus
}

// TriggerConsolidation publishes a manual MemoryPressureEvent (ADR-0030/0047).
func (e EventBusEffects) TriggerConsolidation(_ context.Context, scope string) error {
	if e.Bus == nil {
		return status.Error(codes.Unimplemented, "event bus not configured")
	}
	return e.Bus.Publish(domain.MemoryPressureEvent{Trigger: "operator:" + scope})
}

// CommandEffectsFuncs adapts plain functions to CommandEffects so the
// composition root can bind kernel surfaces (scope resolver, skill/MCP
// registries, lifecycle) without the operator package importing them. A nil
// function yields Unimplemented (honest where a surface is not yet wired).
type CommandEffectsFuncs struct {
	TagMemoryFn            func(ctx context.Context, docID, tag string, add bool) error
	SetScopeFn            func(ctx context.Context, agentID string, required, anyOf, forbidden []string) error
	RegisterSkillFn       func(ctx context.Context, name, description, instructions string, toolGrants, scopeTags []string) error
	RegisterMCPFn         func(ctx context.Context, name, command, url string) error
	TriggerConsolidationFn func(ctx context.Context, scope string) error
	SetRuntimeConfigFn     func(ctx context.Context, params map[string]float64) error
}

func (f CommandEffectsFuncs) TagMemory(ctx context.Context, docID, tag string, add bool) error {
	if f.TagMemoryFn == nil {
		return status.Error(codes.Unimplemented, "tag memory not wired")
	}
	return f.TagMemoryFn(ctx, docID, tag, add)
}

func (f CommandEffectsFuncs) SetScope(ctx context.Context, agentID string, required, anyOf, forbidden []string) error {
	if f.SetScopeFn == nil {
		return status.Error(codes.Unimplemented, "set scope not wired")
	}
	return f.SetScopeFn(ctx, agentID, required, anyOf, forbidden)
}

func (f CommandEffectsFuncs) RegisterSkill(ctx context.Context, name, description, instructions string, toolGrants, scopeTags []string) error {
	if f.RegisterSkillFn == nil {
		return status.Error(codes.Unimplemented, "register skill not wired")
	}
	return f.RegisterSkillFn(ctx, name, description, instructions, toolGrants, scopeTags)
}

func (f CommandEffectsFuncs) RegisterMCP(ctx context.Context, name, command, url string) error {
	if f.RegisterMCPFn == nil {
		return status.Error(codes.Unimplemented, "register mcp not wired")
	}
	return f.RegisterMCPFn(ctx, name, command, url)
}

func (f CommandEffectsFuncs) TriggerConsolidation(ctx context.Context, scope string) error {
	if f.TriggerConsolidationFn == nil {
		return status.Error(codes.Unimplemented, "trigger consolidation not wired")
	}
	return f.TriggerConsolidationFn(ctx, scope)
}

func (f CommandEffectsFuncs) SetRuntimeConfig(ctx context.Context, params map[string]float64) error {
	if f.SetRuntimeConfigFn == nil {
		return status.Error(codes.Unimplemented, "set runtime config not wired")
	}
	return f.SetRuntimeConfigFn(ctx, params)
}

// SetCommandEffects wires the effect adapter for the remaining mutations.
func (s *Service) SetCommandEffects(e CommandEffects) { s.effects = e }

// runMutation is the shared command body: validate, audit (idempotent), and
// apply the effect once. before/after capture the request intent.
func (s *Service) runMutation(ctx context.Context, commandID, reason, action, targetType, targetID, after string, apply func() error) (*pb.CommandAck, error) {
	if commandID == "" || reason == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: commandID, At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: action,
		TargetType: targetType, TargetID: targetID, After: after, Reason: reason, Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	if !deduped {
		if err := apply(); err != nil {
			return nil, err
		}
	}
	return &pb.CommandAck{CommandId: commandID, Deduped: deduped}, nil
}

// TagMemory applies/removes a scope or evaluation tag on a memory document.
func (s *Service) TagMemory(ctx context.Context, req *pb.TagMemoryRequest) (*pb.CommandAck, error) {
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "tag_memory", "document", req.GetDocId(),
		req.GetTag(), func() error { return s.effects.TagMemory(ctx, req.GetDocId(), req.GetTag(), req.GetAdd()) })
}

// SetScope adjusts an agent's EffectiveScope / write tags.
func (s *Service) SetScope(ctx context.Context, req *pb.SetScopeRequest) (*pb.CommandAck, error) {
	after := strings.Join(req.GetRequiredTags(), ",") + "|" + strings.Join(req.GetAnyOfTags(), ",") + "|" + strings.Join(req.GetForbiddenTags(), ",")
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "set_scope", "agent", req.GetAgentId(),
		after, func() error {
			return s.effects.SetScope(ctx, req.GetAgentId(), req.GetRequiredTags(), req.GetAnyOfTags(), req.GetForbiddenTags())
		})
}

// RegisterSkill registers a new system skill.
func (s *Service) RegisterSkill(ctx context.Context, req *pb.RegisterSkillRequest) (*pb.CommandAck, error) {
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "register_skill", "skill", req.GetName(),
		req.GetName(), func() error {
			return s.effects.RegisterSkill(ctx, req.GetName(), req.GetDescription(), req.GetInstructions(), req.GetToolGrants(), req.GetScopeTags())
		})
}

// RegisterMCP registers a new MCP connection.
func (s *Service) RegisterMCP(ctx context.Context, req *pb.RegisterMCPRequest) (*pb.CommandAck, error) {
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "register_mcp", "mcp_server", req.GetName(),
		req.GetName(), func() error { return s.effects.RegisterMCP(ctx, req.GetName(), req.GetCommand(), req.GetUrl()) })
}

// TriggerConsolidation drives a manual memory-pressure consolidation.
func (s *Service) TriggerConsolidation(ctx context.Context, req *pb.TriggerConsolidationRequest) (*pb.CommandAck, error) {
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "trigger_consolidation", "lifecycle", req.GetScope(),
		req.GetScope(), func() error { return s.effects.TriggerConsolidation(ctx, req.GetScope()) })
}

// SetRuntimeConfig hot-applies numeric runtime tunables (e.g. blend weights) to
// the live kernel (ADR-0054 tuning seam). Idempotent + audited via runMutation;
// the audit "after" is the sorted param set so each tuning step is attributable.
func (s *Service) SetRuntimeConfig(ctx context.Context, req *pb.SetRuntimeConfigRequest) (*pb.CommandAck, error) {
	params := req.GetParams()
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%g", k, params[k])
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "set_runtime_config", "config", "runtime_tunables",
		b.String(), func() error { return s.effects.SetRuntimeConfig(ctx, params) })
}
