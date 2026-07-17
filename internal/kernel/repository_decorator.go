package kernel

import (
	"context"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/mapper"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/supervision/aggregator"
	session "github.com/cambrian-sh/core/internal/substrate/session"
	subsynaptic "github.com/cambrian-sh/core/internal/substrate/synaptic"
)

// Compile-time interface assertions.
var (
	_ domain.AgentRegistry              = (*AgentRepoDecorator)(nil)
	_ domain.AgentUpdater               = (*AgentRepoDecorator)(nil)
	_ domain.AgentPruner                = (*AgentRepoDecorator)(nil)
	_ aggregator.TaskEventReader        = (*AgentRepoDecorator)(nil)
	_ executer.TaskEventWriter          = (*AgentRepoDecorator)(nil)
	_ session.SessionRepository         = (*AgentRepoDecorator)(nil)
	_ subsynaptic.EventStore            = (*AgentRepoDecorator)(nil)
	_ domain.PlanEventWriter            = (*AgentRepoDecorator)(nil)
	_ domain.RetrievalSessionLogger     = (*AgentRepoDecorator)(nil)
	_ domain.TraversalLogger            = (*AgentRepoDecorator)(nil)
	_ domain.ContradictionLogger        = (*AgentRepoDecorator)(nil)
)

// AgentRepoDecorator wraps storage.BBoltAdapter and maps DTOs to domain entities.
// It is the interface layer that completely isolates storage from domain knowledge.
type AgentRepoDecorator struct {
	store  *storage.BBoltAdapter
	mapper *mapper.AgentMapper
}

// NewAgentRepoDecorator creates a decorator around the raw bbolt store.
func NewAgentRepoDecorator(store *storage.BBoltAdapter) *AgentRepoDecorator {
	return &AgentRepoDecorator{
		store:  store,
		mapper: mapper.NewAgentMapper(),
	}
}

// ── domain.AgentRegistry ─────────────────────────────────────────────────────

func (d *AgentRepoDecorator) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	rec, err := d.store.GetAgentRecord(name)
	if err != nil {
		return nil, err
	}
	agent := d.mapper.ToDomain(*rec)
	return &agent, nil
}

// SaveArtifact persists an artifact's metadata record (incl. ADR-0034 tags).
func (d *AgentRepoDecorator) SaveArtifact(a domain.Artifact) error {
	return d.store.SaveArtifactRecord(d.mapper.ArtifactToRecord(a))
}

// GetArtifact reads an artifact's metadata record by content hash (nil if absent).
func (d *AgentRepoDecorator) GetArtifact(hash string) (*domain.Artifact, error) {
	rec, err := d.store.GetArtifactRecord(hash)
	if err != nil || rec == nil {
		return nil, err
	}
	a := d.mapper.ArtifactToDomain(*rec)
	return &a, nil
}

// ListStepArtifacts returns all artifacts produced by a session+step.
func (d *AgentRepoDecorator) ListStepArtifacts(sessionID string, stepIndex int) ([]domain.Artifact, error) {
	recs, err := d.store.ListArtifactRecordsByStep(sessionID, stepIndex)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Artifact, 0, len(recs))
	for _, rec := range recs {
		out = append(out, d.mapper.ArtifactToDomain(rec))
	}
	return out, nil
}

// HasAgent reports whether agentID is a registered agent. Used by the ADR-0034
// ScopeResolver to distinguish an unprofiled registered agent (unrestricted) from
// an unknown principal (fail-closed). Satisfies scope.AgentExister.
func (d *AgentRepoDecorator) HasAgent(agentID string) bool {
	rec, err := d.store.GetAgentRecord(agentID)
	return err == nil && rec != nil
}

func (d *AgentRepoDecorator) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	recs, err := d.store.GetAllAgentRecords()
	if err != nil {
		return nil, err
	}
	agents := make([]domain.AgentDefinition, 0, len(recs))
	for _, rec := range recs {
		agents = append(agents, d.mapper.ToDomain(rec))
	}
	return agents, nil
}

func (d *AgentRepoDecorator) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	rec, err := d.store.GetManifestRecord(agentID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return &domain.AgentManifest{}, nil
	}
	m := d.mapper.ManifestToDomain(*rec)
	return &m, nil
}

// GetManifestBatch returns manifests for all given agent IDs in a single bbolt Tx.
func (d *AgentRepoDecorator) GetManifestBatch(ids []string) (map[string]*domain.AgentManifest, error) {
	recs, err := d.store.GetManifestRecordBatch(ids)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*domain.AgentManifest, len(recs))
	for id, rec := range recs {
		m := d.mapper.ManifestToDomain(rec)
		result[id] = &m
	}
	return result, nil
}

// ── domain.AgentUpdater ──────────────────────────────────────────────────────

func (d *AgentRepoDecorator) SetProvisional(agentID string, provisional bool) error {
	return d.store.SetProvisional(agentID, provisional)
}

// ── executer.TaskEventWriter ─────────────────────────────────────────────────

func (d *AgentRepoDecorator) WriteTaskEvent(event domain.TaskEvent) error {
	rec := d.mapper.TaskEventToRecord(event)
	return d.store.WriteTaskEventRecord(rec)
}

// ── aggregator.TaskEventReader ────────────────────────────────────────────────

func (d *AgentRepoDecorator) ReadTaskEvent(taskID string) (*domain.TaskEvent, error) {
	rec, err := d.store.ReadTaskEventRecord(taskID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	event := d.mapper.TaskEventToDomain(*rec)
	return &event, nil
}

func (d *AgentRepoDecorator) ReadTaskEvents(agentID, sourceHash string) ([]domain.TaskEvent, error) {
	recs, err := d.store.ReadTaskEventRecords(agentID, sourceHash)
	if err != nil {
		return nil, err
	}
	events := make([]domain.TaskEvent, 0, len(recs))
	for _, rec := range recs {
		events = append(events, d.mapper.TaskEventToDomain(rec))
	}
	return events, nil
}

func (d *AgentRepoDecorator) ReadAllAgentIDs() ([]string, error) {
	return d.store.ReadAllAgentIDs()
}

// SetAgent persists an AgentDefinition to the agents bucket.
func (d *AgentRepoDecorator) SetAgent(def domain.AgentDefinition) error {
	rec := d.mapper.ToRecord(def)
	return d.store.WriteAgentRecord(rec)
}

// SetAgentWithManifest registers an agent AND persists its manifest (ADR-0075) — the
// persist path an AgentSource uses to carry the manifest EXTRAS (PythonDeps for PLAT-01,
// MemoryLimitMB for SEC-01, schemas) that plain SetAgent drops. It is **idempotent by
// SourceHash**: an unchanged agent keeps its existing record (and its post-interview
// Provisional state) — so registering the built-in filesystem agents through a source
// behaves exactly like the old in-Seed scan, not a blind re-provisioning on every boot.
// A nil manifest carries an empty manifest record (matching the built-in scan, which
// always wrote one).
func (d *AgentRepoDecorator) SetAgentWithManifest(def domain.AgentDefinition, manifest *domain.AgentManifest) error {
	da := storage.DiscoveredAgent{Agent: d.mapper.ToRecord(def)}
	if manifest != nil {
		da.Manifest = d.mapper.ManifestToRecord(*manifest)
	}
	return d.store.UpsertDiscoveredAgent(da)
}

// DeleteAgent removes an agent (and its manifest) from the registry. It is the
// eviction half of the startup reconcile (domain.AgentPruner): an orphan that
// is no longer declared by its source must stop appearing as an auction
// candidate, since GetAllAgents reads the same bucket. Idempotent.
func (d *AgentRepoDecorator) DeleteAgent(agentID string) error {
	return d.store.DeleteAgentRecord(agentID)
}

// ── clusterer.ClusterStore ───────────────────────────────────────────────────

// SetCapabilities updates the Capabilities slice on an AgentRecord in the agents bucket.
func (d *AgentRepoDecorator) SetCapabilities(agentID string, caps []string) error {
	return d.store.SetCapabilities(agentID, caps)
}

// SetClusterName persists a cluster name keyed by representative agent ID in
// the capability_clusters bucket.
func (d *AgentRepoDecorator) SetClusterName(repID, name string) error {
	return d.store.SetClusterName(repID, name)
}

// GetClusterName retrieves the cluster name for a representative agent ID.
// Returns "", nil when no entry exists.
func (d *AgentRepoDecorator) GetClusterName(repID string) (string, error) {
	return d.store.GetClusterName(repID)
}

// ── session.SessionRepository ─────────────────────────────────────────────────

func (d *AgentRepoDecorator) SaveSession(_ context.Context, ses domain.Session) error {
	rec := d.mapper.SessionToRecord(ses)
	return d.store.SaveSessionRecord(rec)
}

func (d *AgentRepoDecorator) GetSession(_ context.Context, id string) (*domain.Session, error) {
	rec, err := d.store.GetSessionRecord(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	s := d.mapper.SessionToDomain(*rec)
	return &s, nil
}

func (d *AgentRepoDecorator) ListSessions(_ context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	recs, err := d.store.ListSessionRecords(string(status))
	if err != nil {
		return nil, err
	}
	sessions := make([]domain.Session, 0, len(recs))
	for _, rec := range recs {
		sessions = append(sessions, d.mapper.SessionToDomain(rec))
	}
	return sessions, nil
}

// ── subsynaptic.EventStore ───────────────────────────────────────────────────

func (d *AgentRepoDecorator) LogEvent(_ context.Context, ev domain.SessionEvent) error {
	rec := d.mapper.SessionEventToRecord(ev)
	return d.store.WriteEventRecord(rec)
}

func (d *AgentRepoDecorator) GetEvents(_ context.Context, sessionID string, limit int) ([]domain.SessionEvent, error) {
	recs, err := d.store.ListEventRecords(sessionID, limit)
	if err != nil {
		return nil, err
	}
	events := make([]domain.SessionEvent, 0, len(recs))
	for _, rec := range recs {
		events = append(events, d.mapper.SessionEventToDomain(rec))
	}
	return events, nil
}

func (d *AgentRepoDecorator) GetEventsByType(_ context.Context, sessionID string, types ...domain.SessionEventType) ([]domain.SessionEvent, error) {
	typeStrs := make([]string, len(types))
	for i, t := range types {
		typeStrs[i] = string(t)
	}
	recs, err := d.store.ListEventRecordsByType(sessionID, typeStrs)
	if err != nil {
		return nil, err
	}
	events := make([]domain.SessionEvent, 0, len(recs))
	for _, rec := range recs {
		events = append(events, d.mapper.SessionEventToDomain(rec))
	}
	return events, nil
}

func (d *AgentRepoDecorator) GetAllRecentEvents(_ context.Context, since time.Time, limit int) ([]domain.SessionEvent, error) {
	recs, err := d.store.ListAllEventRecordsSince(since, limit)
	if err != nil {
		return nil, err
	}
	events := make([]domain.SessionEvent, 0, len(recs))
	for _, rec := range recs {
		events = append(events, d.mapper.SessionEventToDomain(rec))
	}
	return events, nil
}

// ── domain.PlanEventWriter ───────────────────────────────────────────────────

func (d *AgentRepoDecorator) WritePlanEvent(event domain.PlanEvent) error {
	rec := d.mapper.PlanEventToRecord(event)
	return d.store.WritePlanEventRecord(rec)
}

// ── domain.RetrievalSessionLogger ────────────────────────────────────────────

func (d *AgentRepoDecorator) LogRetrieval(session domain.RetrievalSession) error {
	rec := d.mapper.RetrievalSessionToRecord(session)
	return d.store.WriteRetrievalSessionRecord(rec)
}

func (d *AgentRepoDecorator) LinkToPlanOutcome(sessionID string, planID string, outcome domain.PlanOutcome) error {
	return d.store.UpdateRetrievalSessionPlanOutcome(sessionID, planID, string(outcome))
}

// ── domain.TraversalLogger ───────────────────────────────────────────────────

func (d *AgentRepoDecorator) LogTraversal(entry domain.TraversalLogEntry) error {
	rec := d.mapper.TraversalLogEntryToRecord(entry)
	return d.store.WriteTraversalLogEntryRecord(rec)
}

func (d *AgentRepoDecorator) UpdatePlanOutcome(entryID string, planID string, outcome domain.PlanOutcome) error {
	return d.store.UpdateTraversalLogPlanOutcome(entryID, planID, string(outcome))
}

// ── domain.ContradictionLogger ───────────────────────────────────────────────

func (d *AgentRepoDecorator) LogContradiction(res domain.ContradictionResolution) error {
	rec := d.mapper.ContradictionResolutionToRecord(res)
	return d.store.WriteContradictionResolutionRecord(rec)
}

// ── WatchConfig persistence (ADR-0032) ────────────────────────────────────────

// WriteWatchConfig persists a WatchConfig to BBolt. ADR-0032.
func (d *AgentRepoDecorator) WriteWatchConfig(cfg domain.WatchConfig) error {
	rec := d.mapper.WatchConfigToRecord(cfg)
	return d.store.WriteWatchConfig(rec)
}

// ReadWatchConfig retrieves a WatchConfig by ID. ADR-0032.
func (d *AgentRepoDecorator) ReadWatchConfig(id string) (domain.WatchConfig, error) {
	rec, err := d.store.ReadWatchConfig(id)
	if err != nil {
		return domain.WatchConfig{}, err
	}
	return d.mapper.WatchConfigFromRecord(*rec), nil
}

// ReadAllWatchConfigs returns all persisted WatchConfigs. Used at startup
// to populate the ReactiveEngine's in-memory fan-out registry. ADR-0032.
func (d *AgentRepoDecorator) ReadAllWatchConfigs() ([]domain.WatchConfig, error) {
	recs, err := d.store.ReadAllWatchConfigs()
	if err != nil {
		return nil, err
	}
	out := make([]domain.WatchConfig, 0, len(recs))
	for _, rec := range recs {
		r := rec
		out = append(out, d.mapper.WatchConfigFromRecord(r))
	}
	return out, nil
}

// DeleteWatchConfig removes a WatchConfig by ID. ADR-0032.
func (d *AgentRepoDecorator) DeleteWatchConfig(id string) error {
	return d.store.DeleteWatchConfig(id)
}

// SetWatchConfigActive toggles the Active field of a WatchConfig. ADR-0032.
func (d *AgentRepoDecorator) SetWatchConfigActive(id string, active bool) error {
	return d.store.SetWatchConfigActive(id, active)
}
