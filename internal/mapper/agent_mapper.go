// Package mapper converts between storage DTOs and domain entities.
// It is the ONLY package in the codebase that knows both representations,
// making it the explicit boundary between the persistence layer and the domain.
package mapper

import (
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
	}
}

// TaskEventToDomain converts a storage task event DTO to a domain task event.
func (m *AgentMapper) TaskEventToDomain(rec storage.TaskEventRecord) domain.TaskEvent {
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
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
		PromptTokens:         rec.PromptTokens,
		CompletionTokens:     rec.CompletionTokens,
		TotalTokens:          rec.TotalTokens,
		EstimatedCost:        rec.EstimatedCost,
		BudgetOverrun:        rec.BudgetOverrun,
		FallbackModelUsed:    rec.FallbackModelUsed,
		ActualModelID:        rec.ActualModelID,
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
		PromptTokens:         event.PromptTokens,
		CompletionTokens:     event.CompletionTokens,
		TotalTokens:          event.TotalTokens,
		EstimatedCost:        event.EstimatedCost,
		BudgetOverrun:        event.BudgetOverrun,
		FallbackModelUsed:    event.FallbackModelUsed,
		ActualModelID:        event.ActualModelID,
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
	default:
		return domain.RuntimeBinary
	}
}

// SessionToRecord converts a domain Session to a storage SessionRecord.
func (m *AgentMapper) SessionToRecord(s domain.Session) storage.SessionRecord {
	return storage.SessionRecord{
		ID:          s.ID,
		ParentID:    s.ParentID,
		Goal:        s.Goal,
		Status:      string(s.Status),
		Summary:     s.Summary,
		CreatedAt:   s.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt:   s.UpdatedAt.Format(time.RFC3339Nano),
		CompletedAt: formatTime(s.CompletedAt),
		CallerScope: scopeToRecord(s.CallerScope),
	}
}

// scopeToRecord converts a domain.ScopeConfig to its storage mirror, returning
// nil for a zero (unrestricted) scope. ADR-0034.
func scopeToRecord(c domain.ScopeConfig) *storage.ScopeConfigRecord {
	if c.IsZero() {
		return nil
	}
	return &storage.ScopeConfigRecord{
		RequiredTags:  c.RequiredTags,
		AnyOfTags:     c.AnyOfTags,
		ForbiddenTags: c.ForbiddenTags,
	}
}

// scopeFromRecord converts a storage mirror back to a domain.ScopeConfig.
func scopeFromRecord(r *storage.ScopeConfigRecord) domain.ScopeConfig {
	if r == nil {
		return domain.ScopeConfig{}
	}
	return domain.ScopeConfig{
		RequiredTags:  r.RequiredTags,
		AnyOfTags:     r.AnyOfTags,
		ForbiddenTags: r.ForbiddenTags,
	}
}

// SessionToDomain converts a storage SessionRecord to a domain Session.
func (m *AgentMapper) SessionToDomain(rec storage.SessionRecord) domain.Session {
	return domain.Session{
		ID:          rec.ID,
		ParentID:    rec.ParentID,
		Goal:        rec.Goal,
		Status:      domain.SessionStatus(rec.Status),
		Summary:     rec.Summary,
		CreatedAt:   parseTime(rec.CreatedAt),
		UpdatedAt:   parseTime(rec.UpdatedAt),
		CompletedAt: parseTime(rec.CompletedAt),
		CallerScope: scopeFromRecord(rec.CallerScope),
	}
}

// SessionEventToRecord converts a domain SessionEvent to a storage EventRecord.
func (m *AgentMapper) SessionEventToRecord(e domain.SessionEvent) storage.EventRecord {
	artifactIDs := e.ArtifactIDs
	if artifactIDs == nil {
		artifactIDs = []string{}
	}
	return storage.EventRecord{
		SessionID:   e.SessionID,
		Type:        string(e.Type),
		Timestamp:   e.Timestamp.Format(time.RFC3339Nano),
		Payload:     e.Payload,
		ArtifactIDs: artifactIDs,
	}
}

// SessionEventToDomain converts a storage EventRecord to a domain SessionEvent.
func (m *AgentMapper) SessionEventToDomain(rec storage.EventRecord) domain.SessionEvent {
	return domain.SessionEvent{
		SessionID:   rec.SessionID,
		Type:        domain.SessionEventType(rec.Type),
		Timestamp:   parseTime(rec.Timestamp),
		Payload:     rec.Payload,
		ArtifactIDs: rec.ArtifactIDs,
	}
}

// ArtifactToRecord converts a domain Artifact to a storage ArtifactRecord.
func (m *AgentMapper) ArtifactToRecord(a domain.Artifact) storage.ArtifactRecord {
	meta := a.Metadata
	if meta == nil {
		meta = make(map[string]string)
	}
	return storage.ArtifactRecord{
		Hash:            a.Hash,
		ContentType:     a.ContentType,
		SizeBytes:       a.SizeBytes,
		SessionID:       a.SessionID,
		StepIndex:       a.StepIndex,
		Metadata:        meta,
		ParentHash:      a.ParentHash,
		SemanticSummary: a.SemanticSummary,
		CreatedAt:       a.CreatedAt.Format(time.RFC3339Nano),
		Tags:            a.Tags,
	}
}

// ArtifactToDomain converts a storage ArtifactRecord to a domain Artifact.
func (m *AgentMapper) ArtifactToDomain(rec storage.ArtifactRecord) domain.Artifact {
	return domain.Artifact{
		Hash:            rec.Hash,
		ContentType:     rec.ContentType,
		SizeBytes:       rec.SizeBytes,
		SessionID:       rec.SessionID,
		StepIndex:       rec.StepIndex,
		Metadata:        rec.Metadata,
		ParentHash:      rec.ParentHash,
		SemanticSummary: rec.SemanticSummary,
		CreatedAt:       parseTime(rec.CreatedAt),
		Tags:            rec.Tags,
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

// RetrievalSessionToRecord converts a domain RetrievalSession to a storage RetrievalSessionRecord.
func (m *AgentMapper) RetrievalSessionToRecord(session domain.RetrievalSession) storage.RetrievalSessionRecord {
	docs := make([]storage.RetrievedDocRecord, len(session.RetrievedDocs))
	for i, d := range session.RetrievedDocs {
		docs[i] = storage.RetrievedDocRecord{
			DocID:              d.DocID,
			Score:              d.Score,
			ActivationStrength: d.ActivationStrength,
			DocType:            d.DocType,
			Rank:               d.Rank,
		}
	}
	return storage.RetrievalSessionRecord{
		SessionID:       session.SessionID,
		Query:           session.Query,
		QueryEmbedding:  session.QueryEmbedding,
		Caller:          session.Caller,
		SceneHits:       session.SceneHits,
		FactHits:        session.FactHits,
		RetrievedDocs:   docs,
		Truncated:       session.Truncated,
		PlanID:          session.PlanID,
		PlanOutcome:     string(session.PlanOutcome),
		ExplorationSlot: session.ExplorationSlot,
		Timestamp:       session.Timestamp.Format(time.RFC3339Nano),
	}
}

// RetrievalSessionToDomain converts a storage RetrievalSessionRecord to a domain RetrievalSession.
func (m *AgentMapper) RetrievalSessionToDomain(rec storage.RetrievalSessionRecord) domain.RetrievalSession {
	docs := make([]domain.RetrievedDoc, len(rec.RetrievedDocs))
	for i, d := range rec.RetrievedDocs {
		docs[i] = domain.RetrievedDoc{
			DocID:              d.DocID,
			Score:              d.Score,
			ActivationStrength: d.ActivationStrength,
			DocType:            d.DocType,
			Rank:               d.Rank,
		}
	}
	return domain.RetrievalSession{
		SessionID:       rec.SessionID,
		Query:           rec.Query,
		QueryEmbedding:  rec.QueryEmbedding,
		Caller:          rec.Caller,
		SceneHits:       rec.SceneHits,
		FactHits:        rec.FactHits,
		RetrievedDocs:   docs,
		Truncated:       rec.Truncated,
		PlanID:          rec.PlanID,
		PlanOutcome:     domain.PlanOutcome(rec.PlanOutcome),
		ExplorationSlot: rec.ExplorationSlot,
		Timestamp:       parseTime(rec.Timestamp),
	}
}

// TraversalLogEntryToRecord converts a domain TraversalLogEntry to a storage TraversalLogEntryRecord.
func (m *AgentMapper) TraversalLogEntryToRecord(entry domain.TraversalLogEntry) storage.TraversalLogEntryRecord {
	return storage.TraversalLogEntryRecord{
		EntryID:           entry.EntryID,
		SourceID:          entry.SourceID,
		TargetID:          entry.TargetID,
		EdgeType:          entry.EdgeType,
		EdgeWeight:        entry.EdgeWeight,
		TransferredEnergy: entry.TransferredEnergy,
		Depth:             entry.Depth,
		PlanID:            entry.PlanID,
		PlanOutcome:       string(entry.PlanOutcome),
		Timestamp:         entry.Timestamp.Format(time.RFC3339Nano),
	}
}

// TraversalLogEntryToDomain converts a storage TraversalLogEntryRecord to a domain TraversalLogEntry.
func (m *AgentMapper) TraversalLogEntryToDomain(rec storage.TraversalLogEntryRecord) domain.TraversalLogEntry {
	return domain.TraversalLogEntry{
		EntryID:           rec.EntryID,
		SourceID:          rec.SourceID,
		TargetID:          rec.TargetID,
		EdgeType:          rec.EdgeType,
		EdgeWeight:        rec.EdgeWeight,
		TransferredEnergy: rec.TransferredEnergy,
		Depth:             rec.Depth,
		PlanID:            rec.PlanID,
		PlanOutcome:       domain.PlanOutcome(rec.PlanOutcome),
		Timestamp:         parseTime(rec.Timestamp),
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
		ID:                 cfg.ID,
		Name:               cfg.Name,
		Description:        cfg.Description,
		SourceType:         cfg.Source.Type,
		SourceStreamID:     cfg.Source.StreamID,
		Condition:          cfg.Condition,
		ConditionType:      cfg.ConditionType,
		ActionType:         cfg.Action.Type,
		ActionTargetType:   cfg.Action.TargetType,
		ActionTarget:       cfg.Action.Target,
		ActionPayload:      cfg.Action.Payload,
		Active:             cfg.Active,
		ResponseMode:       cfg.ResponseMode,
		DaemonParams:         cfg.DaemonParams,
		MaxConcurrentPlans:   cfg.MaxConcurrentPlans,
		DebounceSeconds:      cfg.DebounceSeconds,
		ConditionPayloadKeys: cfg.ConditionPayloadKeys,
		Approved:             cfg.Approved,
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
		},
		Condition:     rec.Condition,
		ConditionType: rec.ConditionType,
		Action: domain.WatchAction{
			Type:       rec.ActionType,
			TargetType: rec.ActionTargetType,
			Target:     rec.ActionTarget,
			Payload:    rec.ActionPayload,
		},
		Active:             rec.Active,
		ResponseMode:       rec.ResponseMode,
		DaemonParams:         rec.DaemonParams,
		MaxConcurrentPlans:   rec.MaxConcurrentPlans,
		DebounceSeconds:      rec.DebounceSeconds,
		ConditionPayloadKeys: rec.ConditionPayloadKeys,
		Approved:             rec.Approved,
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
