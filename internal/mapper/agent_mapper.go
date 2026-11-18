// Package mapper converts between storage DTOs and domain entities.
// It is the ONLY package in the codebase that knows both representations,
// making it the explicit boundary between the persistence layer and the domain.
package mapper

import (
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/storage"
)

// AgentMapper converts between storage.AgentRecord and domain.AgentDefinition.
type AgentMapper struct{}

// NewAgentMapper creates a new AgentMapper.
func NewAgentMapper() *AgentMapper { return &AgentMapper{} }

// ToDomain converts a storage DTO to a domain entity.
func (m *AgentMapper) ToDomain(rec storage.AgentRecord) domain.AgentDefinition {
	return domain.AgentDefinition{
		ID:              rec.ID,
		Name:            rec.Name,
		Description:     rec.Description,
		Runtime:         toDomainRuntime(rec.Runtime),
		ExecPath:        rec.ExecPath,
		Dir:             rec.Dir,
		A2AEndpoint:     rec.A2AEndpoint,
		SourceHash:      rec.SourceHash,
		ManifestVersion: rec.ManifestVersion,
		Provisional:     rec.Provisional,
		Trait:           domain.AgentTrait(rec.Trait),
		Capabilities:    rec.Capabilities,
		System:          rec.System,
		LaunchArgs:      rec.LaunchArgs,
		Profile:         rec.Profile,
		TrustTier:       rec.TrustTier,
		ModelPreference: rec.ModelPreference,
	}
}

// ToRecord converts a domain entity to a storage DTO.
func (m *AgentMapper) ToRecord(def domain.AgentDefinition) storage.AgentRecord {
	return storage.AgentRecord{
		ID:              def.ID,
		Name:            def.Name,
		Description:     def.Description,
		Runtime:         string(def.Runtime),
		ExecPath:        def.ExecPath,
		Dir:             def.Dir,
		A2AEndpoint:     def.A2AEndpoint,
		SourceHash:      def.SourceHash,
		ManifestVersion: def.ManifestVersion,
		Provisional:     def.Provisional,
		Trait:           string(def.Trait),
		Capabilities:    def.Capabilities,
		System:          def.System,
		LaunchArgs:      def.LaunchArgs,
		Profile:         def.Profile,
		TrustTier:       def.TrustTier,
		ModelPreference: def.ModelPreference,
	}
}

// ManifestToDomain converts a storage manifest DTO to a domain manifest.
func (m *AgentMapper) ManifestToDomain(rec storage.ManifestRecord) domain.AgentManifest {
	return domain.AgentManifest{
		Version:          rec.Version,
		Trait:            domain.AgentTrait(rec.Trait),
		Tools:            rec.Tools,
		Capabilities:     rec.Capabilities,  // ROUTE-03
		MemoryLimitMB:    rec.MemoryLimitMB, // SEC-01
		PythonDeps:       rec.PythonDeps,    // PLAT-01
		SupportedFormats: rec.SupportedFormats,
		InputSchema:      rec.InputSchema,
		OutputSchema:     rec.OutputSchema,
		ReleaseNotes:     rec.ReleaseNotes,
		Dependencies:     rec.Dependencies,
		ToolAllowlist:    rec.ToolAllowlist,
		Skills:           rec.Skills,
		Examples:         rec.Examples,
		StreamsOutput:    rec.StreamsOutput,
		SandboxPaths:     sandboxPathsToDomain(rec.SandboxPaths),     // ADR-0157 D1.2
		PythonPackages:   pythonPackagesToDomain(rec.PythonPackages), // ADR-0157 D7.6
	}
}

// ManifestToRecord converts a domain manifest to its storage DTO — the reverse of
// ManifestToDomain, used by the SetAgentWithManifest persist path (ADR-0075) so an
// AgentSource can carry the manifest EXTRAS (PythonDeps/MemoryLimitMB/schemas).
func (m *AgentMapper) ManifestToRecord(man domain.AgentManifest) storage.ManifestRecord {
	return storage.ManifestRecord{
		Version:          man.Version,
		Trait:            string(man.Trait),
		Tools:            man.Tools,
		Capabilities:     man.Capabilities,  // ROUTE-03
		MemoryLimitMB:    man.MemoryLimitMB, // SEC-01
		PythonDeps:       man.PythonDeps,    // PLAT-01
		SupportedFormats: man.SupportedFormats,
		InputSchema:      man.InputSchema,
		OutputSchema:     man.OutputSchema,
		ReleaseNotes:     man.ReleaseNotes,
		Dependencies:     man.Dependencies,
		ToolAllowlist:    man.ToolAllowlist,
		Skills:           man.Skills,
		Examples:         man.Examples,
		StreamsOutput:    man.StreamsOutput,
		SandboxPaths:     sandboxPathsToRecord(man.SandboxPaths),     // ADR-0157 D1.2
		PythonPackages:   pythonPackagesToRecord(man.PythonPackages), // ADR-0157 D7.6
	}
}

func sandboxPathsToDomain(in []storage.SandboxPathRecord) []domain.SandboxPath {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.SandboxPath, len(in))
	for i, p := range in {
		out[i] = domain.SandboxPath{Path: p.Path, Write: p.Write}
	}
	return out
}

func sandboxPathsToRecord(in []domain.SandboxPath) []storage.SandboxPathRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]storage.SandboxPathRecord, len(in))
	for i, p := range in {
		out[i] = storage.SandboxPathRecord{Path: p.Path, Write: p.Write}
	}
	return out
}

func pythonPackagesToDomain(in []storage.PythonPackageRecord) []domain.PythonPackage {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.PythonPackage, len(in))
	for i, p := range in {
		out[i] = domain.PythonPackage{Name: p.Name, Version: p.Version, Extras: append([]string(nil), p.Extras...)}
	}
	return out
}

func pythonPackagesToRecord(in []domain.PythonPackage) []storage.PythonPackageRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]storage.PythonPackageRecord, len(in))
	for i, p := range in {
		out[i] = storage.PythonPackageRecord{Name: p.Name, Version: p.Version, Extras: append([]string(nil), p.Extras...)}
	}
	return out
}

// TaskEventToDomain converts a storage task event DTO to a domain task event.
func (m *AgentMapper) TaskEventToDomain(rec storage.TaskEventRecord) domain.TaskEvent {
	ts := parseTime(rec.Timestamp)
	return domain.TaskEvent{
		TaskID:               rec.TaskID,
		AgentID:              rec.AgentID,
		SourceHash:           rec.SourceHash,
		BidConfidence:        rec.BidConfidence,
		VerifierScore:        rec.VerifierScore,
		NetworkLatencyMs:     rec.NetworkLatencyMs,
		ComputationLatencyMs: rec.ComputationLatencyMs,
		ContextGrowthBytes:   rec.ContextGrowthBytes,
		Timestamp:            ts,
		Verified:             rec.Verified,
		Capability:           rec.Capability,
		PromptTokens:         rec.PromptTokens,
		CompletionTokens:     rec.CompletionTokens,
		TotalTokens:          rec.TotalTokens,
		EstimatedCost:        rec.EstimatedCost,
		BudgetOverrun:        rec.BudgetOverrun,
		FallbackModelUsed:    rec.FallbackModelUsed,
		ActualModelID:        rec.ActualModelID,
		UsageEstimated:       rec.UsageEstimated,
	}
}

// TaskEventToRecord converts a domain task event to a storage DTO.
func (m *AgentMapper) TaskEventToRecord(event domain.TaskEvent) storage.TaskEventRecord {
	return storage.TaskEventRecord{
		TaskID:               event.TaskID,
		AgentID:              event.AgentID,
		SourceHash:           event.SourceHash,
		BidConfidence:        event.BidConfidence,
		VerifierScore:        event.VerifierScore,
		NetworkLatencyMs:     event.NetworkLatencyMs,
		ComputationLatencyMs: event.ComputationLatencyMs,
		ContextGrowthBytes:   event.ContextGrowthBytes,
		Timestamp:            event.Timestamp.Format(time.RFC3339Nano),
		Verified:             event.Verified,
		Capability:           event.Capability,
		PromptTokens:         event.PromptTokens,
		CompletionTokens:     event.CompletionTokens,
		TotalTokens:          event.TotalTokens,
		EstimatedCost:        event.EstimatedCost,
		BudgetOverrun:        event.BudgetOverrun,
		FallbackModelUsed:    event.FallbackModelUsed,
		ActualModelID:        event.ActualModelID,
		UsageEstimated:       event.UsageEstimated,
	}
}

// toDomainRuntime maps a plain string runtime to the domain enum.
func toDomainRuntime(r string) domain.AgentRuntime {
	switch r {
	case "python":
		return domain.RuntimePython
	case "wasm":
		return domain.RuntimeWasm
	case "a2a":
		return domain.RuntimeA2A
	case "bun":
		return domain.RuntimeBun
	case "node":
		return domain.RuntimeNode
	default:
		return domain.RuntimeBinary
	}
}

// SessionToRecord converts a domain Session to a storage SessionRecord.
func (m *AgentMapper) SessionToRecord(s domain.Session) storage.SessionRecord {
	return storage.SessionRecord{
		ID:          string(s.ID),
		ParentID:    string(s.ParentID),
		Goal:        s.Goal,
		Status:      string(s.Status),
		Summary:     s.Summary,
		CreatedAt:   s.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt:   s.UpdatedAt.Format(time.RFC3339Nano),
		CompletedAt: formatTime(s.CompletedAt),
		CallerScope: scopeToRecord(s.CallerScope),
	}
}

// scopeToRecord converts a domain.TagSet to its storage mirror, returning
// nil for a zero (unrestricted) scope. ADR-0034.
func scopeToRecord(c domain.TagSet) *storage.TagSetRecord {
	if c.IsZero() {
		return nil
	}
	return &storage.TagSetRecord{
		RequiredTags:  c.RequiredTags,
		AnyOfTags:     c.AnyOfTags,
		ForbiddenTags: c.ForbiddenTags,
	}
}

// scopeFromRecord converts a storage mirror back to a domain.TagSet.
func scopeFromRecord(r *storage.TagSetRecord) domain.TagSet {
	if r == nil {
		return domain.TagSet{}
	}
	return domain.TagSet{
		RequiredTags:  r.RequiredTags,
		AnyOfTags:     r.AnyOfTags,
		ForbiddenTags: r.ForbiddenTags,
	}
}

// SessionToDomain converts a storage SessionRecord to a domain Session.
func (m *AgentMapper) SessionToDomain(rec storage.SessionRecord) domain.Session {
	return domain.Session{
		ID:          domain.SessionID(rec.ID),
		ParentID:    domain.SessionID(rec.ParentID),
		Goal:        rec.Goal,
		Status:      domain.SessionStatus(rec.Status),
		Summary:     rec.Summary,
		CreatedAt:   parseTime(rec.CreatedAt),
		UpdatedAt:   parseTime(rec.UpdatedAt),
		CompletedAt: parseTime(rec.CompletedAt),
		CallerScope: scopeFromRecord(rec.CallerScope),
	}
}

// PlanEventToRecord converts a domain PlanEvent to a storage PlanEventRecord.
func (m *AgentMapper) PlanEventToRecord(event domain.PlanEvent) storage.PlanEventRecord {
	return storage.PlanEventRecord{
		PlanID:                event.PlanID,
		Subject:               event.Subject,
		StepCount:             event.StepCount,
		Outcome:               string(event.Outcome),
		TotalPromptTokens:     event.TotalPromptTokens,
		TotalCompletionTokens: event.TotalCompletionTokens,
		TotalTokens:           event.TotalTokens,
		TotalEstimatedCost:    event.TotalEstimatedCost,
		ReplanCount:           event.ReplanCount,
		FailedStepIndex:       event.FailedStepIndex,
		FallbackCount:         event.FallbackCount,
		BudgetOverrunCount:    event.BudgetOverrunCount,
		StartTime:             event.StartTime.Format(time.RFC3339Nano),
		EndTime:               event.EndTime.Format(time.RFC3339Nano),
		DurationMs:            event.DurationMs,
		RetrievalSessionID:    event.RetrievalSessionID,
		PlannerPromptVersion:  event.PlannerPromptVersion,
	}
}

// PlanEventToDomain converts a storage PlanEventRecord to a domain PlanEvent.
func (m *AgentMapper) PlanEventToDomain(rec storage.PlanEventRecord) domain.PlanEvent {
	return domain.PlanEvent{
		PlanID:                rec.PlanID,
		Subject:               rec.Subject,
		StepCount:             rec.StepCount,
		Outcome:               domain.PlanOutcome(rec.Outcome),
		TotalPromptTokens:     rec.TotalPromptTokens,
		TotalCompletionTokens: rec.TotalCompletionTokens,
		TotalTokens:           rec.TotalTokens,
		TotalEstimatedCost:    rec.TotalEstimatedCost,
		ReplanCount:           rec.ReplanCount,
		FailedStepIndex:       rec.FailedStepIndex,
		FallbackCount:         rec.FallbackCount,
		BudgetOverrunCount:    rec.BudgetOverrunCount,
		StartTime:             parseTime(rec.StartTime),
		EndTime:               parseTime(rec.EndTime),
		DurationMs:            rec.DurationMs,
		RetrievalSessionID:    rec.RetrievalSessionID,
		PlannerPromptVersion:  rec.PlannerPromptVersion,
	}
}

// ContradictionResolutionToRecord converts a domain ContradictionResolution to a storage ContradictionResolutionRecord.
func (m *AgentMapper) ContradictionResolutionToRecord(res domain.ContradictionResolution) storage.ContradictionResolutionRecord {
	return storage.ContradictionResolutionRecord{
		ResolutionID:           res.ResolutionID,
		DocAID:                 res.DocAID,
		DocBID:                 res.DocBID,
		WinnerID:               res.WinnerID,
		DocAAS:                 res.DocAAS,
		DocBAS:                 res.DocBAS,
		DocAAccessCount:        res.DocAAccessCount,
		DocBAccessCount:        res.DocBAccessCount,
		DocAAgeDays:            res.DocAAgeDays,
		DocBAgeDays:            res.DocBAgeDays,
		SemanticSimilarity:     res.SemanticSimilarity,
		ConsolidatorAgentTrust: res.ConsolidatorAgentTrust,
		VerifiedA:              res.VerifiedA,
		VerifiedB:              res.VerifiedB,
		Timestamp:              res.Timestamp.Format(time.RFC3339Nano),
	}
}

// ContradictionResolutionToDomain converts a storage ContradictionResolutionRecord to a domain ContradictionResolution.
func (m *AgentMapper) ContradictionResolutionToDomain(rec storage.ContradictionResolutionRecord) domain.ContradictionResolution {
	return domain.ContradictionResolution{
		ResolutionID:           rec.ResolutionID,
		DocAID:                 rec.DocAID,
		DocBID:                 rec.DocBID,
		WinnerID:               rec.WinnerID,
		DocAAS:                 rec.DocAAS,
		DocBAS:                 rec.DocBAS,
		DocAAccessCount:        rec.DocAAccessCount,
		DocBAccessCount:        rec.DocBAccessCount,
		DocAAgeDays:            rec.DocAAgeDays,
		DocBAgeDays:            rec.DocBAgeDays,
		SemanticSimilarity:     rec.SemanticSimilarity,
		ConsolidatorAgentTrust: rec.ConsolidatorAgentTrust,
		VerifiedA:              rec.VerifiedA,
		VerifiedB:              rec.VerifiedB,
		Timestamp:              parseTime(rec.Timestamp),
	}
}

// WatchConfigToRecord converts a domain WatchConfig to a storage WatchConfigRecord. ADR-0032.
func (m *AgentMapper) WatchConfigToRecord(cfg domain.WatchConfig) storage.WatchConfigRecord {
	return storage.WatchConfigRecord{
		ID:                   cfg.ID,
		Name:                 cfg.Name,
		Description:          cfg.Description,
		SourceType:           cfg.Source.Type,
		SourceStreamID:       cfg.Source.StreamID,
		SourceCron:           cfg.Source.Cron,
		SourceTimezone:       cfg.Source.Timezone,
		MissedFirePolicy:     cfg.MissedFirePolicy,
		Condition:            cfg.Condition,
		ConditionType:        cfg.ConditionType,
		ActionType:           cfg.Action.Type,
		ActionTargetType:     cfg.Action.TargetType,
		ActionTarget:         cfg.Action.Target,
		ActionPayload:        cfg.Action.Payload,
		Active:               cfg.Active,
		ResponseMode:         cfg.ResponseMode,
		DaemonParams:         cfg.DaemonParams,
		MaxConcurrentPlans:   cfg.MaxConcurrentPlans,
		DebounceSeconds:      cfg.DebounceSeconds,
		ConditionPayloadKeys: cfg.ConditionPayloadKeys,
		Approved:             cfg.Approved,
		DryRun:               cfg.DryRun,
		Owner:                cfg.Owner,
	}
}

// WatchConfigFromRecord converts a storage WatchConfigRecord to a domain WatchConfig. ADR-0032.
func (m *AgentMapper) WatchConfigFromRecord(rec storage.WatchConfigRecord) domain.WatchConfig {
	return domain.WatchConfig{
		ID:          rec.ID,
		Name:        rec.Name,
		Description: rec.Description,
		Source: domain.WatchSource{
			Type:     rec.SourceType,
			StreamID: rec.SourceStreamID,
			Cron:     rec.SourceCron,
			Timezone: rec.SourceTimezone,
		},
		MissedFirePolicy: rec.MissedFirePolicy,
		Condition:        rec.Condition,
		ConditionType:    rec.ConditionType,
		Action: domain.WatchAction{
			Type:       rec.ActionType,
			TargetType: rec.ActionTargetType,
			Target:     rec.ActionTarget,
			Payload:    rec.ActionPayload,
		},
		Active:               rec.Active,
		ResponseMode:         rec.ResponseMode,
		DaemonParams:         rec.DaemonParams,
		MaxConcurrentPlans:   rec.MaxConcurrentPlans,
		DebounceSeconds:      rec.DebounceSeconds,
		ConditionPayloadKeys: rec.ConditionPayloadKeys,
		Approved:             rec.Approved,
		DryRun:               rec.DryRun,
		Owner:                rec.Owner,
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// parseTime reads a stored RFC3339Nano stamp. "" is the documented "unset" and
// yields the zero time quietly; anything else that does not parse also yields the
// zero time (a read must not fail over one bad stamp) but is logged, because a
// stored record whose time silently became 0001-01-01 sorts and ages wrongly
// (AGENTS.md §3.2; T-20261113-persisted-writes-and-builder-resolver-errors).
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		slog.Warn("mapper: stored timestamp does not parse; using the zero time", "value", s, "err", err)
		return time.Time{}
	}
	return t
}
