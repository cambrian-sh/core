package storage

// AgentRecord is the raw JSON shape stored in the bbolt agents bucket.
// It intentionally has no domain semantics — just flat fields.
type AgentRecord struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Runtime         string   `json:"runtime"` // plain string, NOT domain.AgentRuntime
	ExecPath        string   `json:"exec_path"`
	Dir             string   `json:"dir"`
	A2AEndpoint     string   `json:"a2a_endpoint,omitempty"`
	SourceHash      string   `json:"source_hash,omitempty"`
	ManifestVersion string   `json:"manifest_version,omitempty"`
	Provisional     bool     `json:"provisional,omitempty"`
	Trait           string   `json:"trait,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	// System is true for privileged kernel organs; set by the seeder from
	// domain.IsSystemAgent(id) — the field is the registry-side mirror of that
	// lookup so callers holding an AgentRecord don't need a second map hit.
	System bool `json:"system,omitempty"`
}

// ManifestRecord is the raw JSON shape stored in the bbolt manifests bucket.
type ManifestRecord struct {
	Version          string         `json:"version,omitempty"`
	Trait            string         `json:"trait,omitempty"`
	Tools            []string       `json:"tools,omitempty"`
	Capabilities     []string       `json:"capabilities,omitempty"`   // ROUTE-03: declared capability tags (manifest source of truth)
	MemoryLimitMB    int            `json:"memory_limit_mb,omitempty"` // SEC-01: per-agent memory cap (0 = global default)
	PythonDeps       []string       `json:"python_deps,omitempty"`     // PLAT-01: import names verified before spawn
	SupportedFormats []string       `json:"supported_formats,omitempty"`
	InputSchema      map[string]any `json:"input_schema,omitempty"`
	OutputSchema     map[string]any `json:"output_schema,omitempty"`
	ReleaseNotes     string         `json:"release_notes,omitempty"`
	Dependencies     []string       `json:"dependencies,omitempty"`
}

// TaskEventRecord is the raw JSON shape stored in the bbolt task_events bucket.
type TaskEventRecord struct {
	TaskID               string  `json:"task_id,omitempty"`
	AgentID              string  `json:"agent_id,omitempty"`
	SourceHash           string  `json:"source_hash,omitempty"`
	BidConfidence        float64 `json:"bid_confidence,omitempty"`
	VerifierScore        float64 `json:"verifier_score,omitempty"`
	NetworkLatencyMs     int     `json:"network_latency_ms,omitempty"`
	ComputationLatencyMs int     `json:"computation_latency_ms,omitempty"`
	ContextGrowthBytes   int     `json:"context_growth_bytes,omitempty"`
	Timestamp            string  `json:"timestamp,omitempty"`
	Verified             bool    `json:"verified,omitempty"`
	Capability           string  `json:"capability,omitempty"` // ROUTE-06 / ADR-0069
	PromptTokens         int     `json:"prompt_tokens,omitempty"`
	CompletionTokens     int     `json:"completion_tokens,omitempty"`
	TotalTokens          int     `json:"total_tokens,omitempty"`
	EstimatedCost        float64 `json:"estimated_cost,omitempty"`
	BudgetOverrun        bool    `json:"budget_overrun,omitempty"`
	FallbackModelUsed    bool    `json:"fallback_model_used,omitempty"`
	ActualModelID        string  `json:"actual_model_id,omitempty"`
}

// SessionRecord is the raw JSON shape stored in the bbolt sessions bucket.
type SessionRecord struct {
	ID           string             `json:"id"`
	ParentID     string             `json:"parent_id,omitempty"`
	Goal         string             `json:"goal"`
	Status       string             `json:"status"`
	Summary      string             `json:"summary,omitempty"`
	CreatedAt    string             `json:"created_at"`
	UpdatedAt    string             `json:"updated_at"`
	CompletedAt  string             `json:"completed_at,omitempty"`
	CriticalData []string           `json:"critical_data,omitempty"`
	CallerScope  *ScopeConfigRecord `json:"caller_scope,omitempty"` // ADR-0034 (D13): non-forgeable caller_scope
}

// ScopeConfigRecord is the storage mirror of domain.ScopeConfig (storage holds no
// domain imports). ADR-0034.
type ScopeConfigRecord struct {
	RequiredTags  []string `json:"required_tags,omitempty"`
	AnyOfTags     []string `json:"any_of_tags,omitempty"`
	ForbiddenTags []string `json:"forbidden_tags,omitempty"`
}

// EventRecord is the raw JSON shape stored in the bbolt events bucket.
type EventRecord struct {
	SessionID   string   `json:"session_id"`
	Type        string   `json:"type"`
	Timestamp   string   `json:"timestamp"`
	Payload     string   `json:"payload"`
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
}

// ArtifactRecord is the raw JSON shape stored in the bbolt artifacts bucket.
type ArtifactRecord struct {
	Hash            string            `json:"hash"`
	ContentType     string            `json:"content_type"`
	SizeBytes       int64             `json:"size_bytes"`
	SessionID       string            `json:"session_id"`
	StepIndex       int               `json:"step_index"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	ParentHash      string            `json:"parent_hash,omitempty"`
	SemanticSummary string            `json:"semantic_summary,omitempty"`
	CreatedAt       string            `json:"created_at"`
	Tags            []string          `json:"tags,omitempty"` // ADR-0034: classification + provenance tags
}

// PlanEventRecord is the raw JSON shape stored in the bbolt plan_events bucket.
type PlanEventRecord struct {
	PlanID                string  `json:"plan_id"`
	Subject               string  `json:"subject,omitempty"`
	StepCount             int     `json:"step_count"`
	Outcome               string  `json:"outcome"`
	TotalPromptTokens     int     `json:"total_prompt_tokens"`
	TotalCompletionTokens int     `json:"total_completion_tokens"`
	TotalTokens           int     `json:"total_tokens"`
	TotalEstimatedCost    float64 `json:"total_estimated_cost"`
	ReplanCount           int     `json:"replan_count"`
	FailedStepIndex       int     `json:"failed_step_index,omitempty"`
	FallbackCount         int     `json:"fallback_count"`
	BudgetOverrunCount    int     `json:"budget_overrun_count"`
	StartTime             string  `json:"start_time"`
	EndTime               string  `json:"end_time"`
	DurationMs            int64   `json:"duration_ms"`
	RetrievalSessionID    string  `json:"retrieval_session_id,omitempty"`
	PlannerPromptVersion  string  `json:"planner_prompt_version,omitempty"`
}

// RetrievedDocRecord is a single retrieved document in a RetrievalSessionRecord.
type RetrievedDocRecord struct {
	DocID              string  `json:"doc_id"`
	Score              float64 `json:"score"`
	ActivationStrength float64 `json:"activation_strength"`
	DocType            string  `json:"doc_type"`
	Rank               int     `json:"rank"`
}

// RetrievalSessionRecord is the raw JSON shape stored in the bbolt retrieval_sessions bucket.
type RetrievalSessionRecord struct {
	SessionID       string               `json:"session_id"`
	Query           string               `json:"query"`
	QueryEmbedding  []float32            `json:"query_embedding,omitempty"`
	Caller          string               `json:"caller"`
	SceneHits       int                  `json:"scene_hits"`
	FactHits        int                  `json:"fact_hits"`
	RetrievedDocs   []RetrievedDocRecord `json:"retrieved_docs"`
	Truncated       bool                 `json:"truncated"`
	PlanID          string               `json:"plan_id,omitempty"`
	PlanOutcome     string               `json:"plan_outcome,omitempty"`
	ExplorationSlot bool                 `json:"exploration_slot"`
	Timestamp       string               `json:"timestamp"`
}

// TraversalLogEntryRecord is the raw JSON shape stored in the bbolt traversal_log bucket.
type TraversalLogEntryRecord struct {
	EntryID           string  `json:"entry_id"`
	SourceID          string  `json:"source_id"`
	TargetID          string  `json:"target_id"`
	EdgeType          string  `json:"edge_type"`
	EdgeWeight        float64 `json:"edge_weight"`
	TransferredEnergy float64 `json:"transferred_energy"`
	Depth             int     `json:"depth"`
	PlanID            string  `json:"plan_id,omitempty"`
	PlanOutcome       string  `json:"plan_outcome,omitempty"`
	Timestamp         string  `json:"timestamp"`
}

// WatchConfigRecord is the raw JSON shape stored in the bbolt watch_configs bucket.
// It mirrors domain.WatchConfig — all fields are flat strings/primitives or JSON-safe maps.
// ADR-0032.
type WatchConfigRecord struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name,omitempty"`
	Description        string         `json:"description,omitempty"`
	SourceType         string         `json:"source_type,omitempty"`
	SourceStreamID     string         `json:"source_stream_id,omitempty"`
	Condition          string         `json:"condition,omitempty"`
	ConditionType      string         `json:"condition_type,omitempty"`
	ActionType         string         `json:"action_type,omitempty"`
	ActionTargetType   string         `json:"action_target_type,omitempty"`
	ActionTarget       string         `json:"action_target,omitempty"`
	ActionPayload      string         `json:"action_payload,omitempty"`
	Active             bool           `json:"active"`
	ResponseMode       string         `json:"response_mode,omitempty"`
	DaemonParams         map[string]any `json:"daemon_params,omitempty"`
	MaxConcurrentPlans   int            `json:"max_concurrent_plans,omitempty"`
	DebounceSeconds      int            `json:"debounce_seconds,omitempty"`       // REACT-02 / ADR-0062
	ConditionPayloadKeys []string       `json:"condition_payload_keys,omitempty"` // REACT-03 / ADR-0063
	Approved             bool           `json:"approved,omitempty"`               // REACT-03 / ADR-0063
}

// ContradictionResolutionRecord is the raw JSON shape stored in the bbolt contradiction_resolutions bucket.
type ContradictionResolutionRecord struct {
	ResolutionID           string  `json:"resolution_id"`
	DocAID                 string  `json:"doc_a_id"`
	DocBID                 string  `json:"doc_b_id"`
	WinnerID               string  `json:"winner_id"`
	DocAAS                 float64 `json:"doc_a_as"`
	DocBAS                 float64 `json:"doc_b_as"`
	DocAAccessCount        int     `json:"doc_a_access_count"`
	DocBAccessCount        int     `json:"doc_b_access_count"`
	DocAAgeDays            int     `json:"doc_a_age_days"`
	DocBAgeDays            int     `json:"doc_b_age_days"`
	SemanticSimilarity     float64 `json:"semantic_similarity"`
	ConsolidatorAgentTrust float64 `json:"consolidator_agent_trust,omitempty"`
	VerifiedA              bool    `json:"verified_a,omitempty"`
	VerifiedB              bool    `json:"verified_b,omitempty"`
	Timestamp              string  `json:"timestamp"`
}
