package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/pkg/util"
)

// SessionLister supplies persistent session state for the Snapshot fan-in.
// Satisfied by the kernel's SessionManager. nil ⇒ Snapshot returns no sessions.
type SessionLister interface {
	ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error)
}

// Service implements the OperatorConsole gRPC service. It embeds the generated
// Unimplemented base so the build still satisfies the interface as later slices
// (commands, auth) add RPCs in premium/separate files — and so the premium tree
// stays excisable from the OSS repo (ADR-0047 D14).
type Service struct {
	pb.UnimplementedOperatorConsoleServer
	feed       *Spool
	projection *Projection
	sessions   SessionLister
	idp        OperatorIdentity
	audit      AuditStore
	grants     GrantsStore
	controls   *ExecutionControlHub
	// logs is the kernel's in-process retention window (contract 0082). nil ⇒
	// QueryLogs/TailLogs answer Unimplemented rather than an empty window, which
	// would read as "the kernel has been silent".
	logs    *util.LogRing
	hitl    domain.ApprovalHub
	effects CommandEffects

	// ADR-0047 Amendment A2 read sources (CORE-OPS-1).
	tools         ToolCatalog
	skills        SkillLister
	memory        MemoryQuerier
	documents     memory.DocumentLister // nil ⇒ ListDocuments Unimplemented
	answerer      MemoryAnswerer        // ADR-0081; nil ⇒ AnswerMemory Unimplemented
	toolRunner    ToolRunner
	ingestor      MemoryIngestor
	watches       domain.WatchConfigHandler
	deadletters   domain.WatchDeadLetterReader // REACT-01 / ADR-0061
	watchMetrics  domain.WatchMetricsReader    // REACT-05 / ADR-0071
	watchBacktest domain.WatchBacktester       // REACT-05 / ADR-0071
	routePreview  RoutePreviewer               // ROUTE-07 / ADR-0077
	policy        domain.PolicyAdmin           // ADR-0085; nil ⇒ access-policy RPCs Unimplemented

	// Contract 0072 (Wave 1). Each is nil-able and its RPC then answers
	// Unimplemented — never an empty success, so a console can distinguish
	// "nothing to report" from "this kernel cannot report it".
	checkpoints CheckpointLister
	mcpServers  MCPServerLister
	embedding   EmbeddingReporter
	classifier  InputClassifier
	generators  GeneratorRegistry
	// deadletterRetry is premium-only: reading the dead-letter queue is safe on
	// any build, replaying from it is not.
	deadletterRetry DeadLetterRetrier
	// configSchema is the ADR-0101 read half of the runtime-config surface.
	configSchema ConfigSchemaReporter
	// configWriter / secretWriter are the ADR-0101 durable write halves. nil ⇒
	// the write RPCs answer Unimplemented rather than accepting a save that
	// cannot persist.
	// Contract 0074 reactive liveness. nil ⇒ the fields stay at their
	// "cannot tell" values rather than defaulting to something healthy-looking.
	streams     domain.StreamRegistry
	watchFires  domain.WatchFireReader
	planeBudget domain.ReactiveBudgetReader
	// blastRadius previews a scope/grant mutation's effect (contract 0076).
	blastRadius BlastRadiusEstimator
	// tokenSeries backs the spend sparkline (contract 0075).
	tokenSeries domain.TokenSeriesReader
	// planProposer plans WITHOUT committing (contract 0075). Side-effect free.
	planProposer PlanProposer
	// planSubmitter runs operator-authored plans (contract 0074).
	planSubmitter PlanSubmitter
	configWriter  ConfigWriter
	secretWriter  SecretWriter
	// generatorWriter is the write half of the generator surface (contract
	// 0083). nil ⇒ SaveGenerator/RemoveGenerator return Unimplemented, which a
	// console renders as "this kernel's generators are file-configured" rather
	// than offering a Save button that does nothing.
	generatorWriter GeneratorWriter

	sessionOps SessionOps
	convOps    ConversationOps // ADR-0084 D9: OSS chat lane

	kernelVersion   string
	contractVersion string
	capabilities    []string
	plugins         []PluginInfo // ADR-0089: plugin identity on the handshake
}

// PluginInfo is one declared plugin as reported on the handshake (ADR-0089).
//
// It is declared HERE rather than reusing app.PluginStatus because app imports
// this package; the composition root maps its status into this shape. That keeps
// the operator plane free of any knowledge of how plugins are composed, which is
// the same reason the kernel never interprets a capability string (ADR-0082 D2).
type PluginInfo struct {
	ID           string
	DisplayName  string
	Version      string
	State        string
	Capabilities []string
	Panels       []PluginPanel
	Reason       string
	Missing      []string
	ExpiresAt    string // RFC3339, empty when not applicable
}

// PluginPanel is one operator surface a plugin contributes.
type PluginPanel struct {
	ID         string
	Title      string
	Capability string
}

// NewService wires the OperatorConsole over a Spool feed. The projection and
// session lister are optional (set via SetSnapshotSources) — without them
// Snapshot still returns a consistent as_of_seq with empty state.
func NewService(feed *Spool) *Service {
	return &Service{feed: feed, projection: NewProjection()}
}

// SetIdentity wires the OperatorIdentity backing the Login RPC (ADR-0047 D13).
func (s *Service) SetIdentity(idp OperatorIdentity) { s.idp = idp }

// Login authenticates a human operator and returns a token bound to its role.
// The interceptor lets this RPC through unauthenticated.
func (s *Service) Login(_ context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	if s.idp == nil {
		return nil, status.Error(codes.Unimplemented, "operator identity not configured")
	}
	token, _, role, err := s.idp.Login(req.GetUsername(), req.GetPassword())
	if err != nil {
		return nil, err
	}
	return &pb.LoginResponse{Token: token, Role: string(role)}, nil
}

// SetSnapshotSources wires the live-plan projection and the session lister used
// by Snapshot. The projection must be the one fed by SubscribeProjection.
func (s *Service) SetSnapshotSources(projection *Projection, sessions SessionLister) {
	if projection != nil {
		s.projection = projection
	}
	s.sessions = sessions
}

// SetHandshake configures the capability + version handshake reported by
// Snapshot (ADR-0047 D14). The capability set reflects which surfaces this
// kernel build supports; the UI hides the rest and warns on version skew.
func (s *Service) SetHandshake(kernelVersion, contractVersion string, capabilities []string) {
	s.kernelVersion = kernelVersion
	s.contractVersion = contractVersion
	s.capabilities = capabilities
}

// SetPlugins records which plugins this build declared, for the handshake
// (ADR-0089). Every DECLARED plugin is reported, including one that failed
// entitlement or has unmet dependencies: a console that cannot tell "this
// deployment has no reactive engine" from "the reactive engine declined to
// register" cannot explain a missing surface to the operator who paid for it.
func (s *Service) SetPlugins(plugins []PluginInfo) { s.plugins = plugins }

// Snapshot returns bounded live operational state stamped with a lower-bound
// as_of_seq captured BEFORE any source read (ADR-0047 D6) — so an event landing
// mid-read is re-delivered on resume rather than lost. The client resumes
// StreamEvents from as_of_seq+1.
func (s *Service) Snapshot(ctx context.Context, _ *pb.SnapshotRequest) (*pb.SnapshotResponse, error) {
	asOf := s.feed.Head() // lower bound: captured before reads

	resp := &pb.SnapshotResponse{
		AsOfSeq:         asOf,
		KernelVersion:   s.kernelVersion,
		ContractVersion: s.contractVersion,
		Capabilities:    s.capabilities,
	}
	for _, p := range s.plugins {
		info := &pb.PluginInfoOp{
			Id:           p.ID,
			DisplayName:  p.DisplayName,
			Version:      p.Version,
			State:        p.State,
			Capabilities: p.Capabilities,
			Reason:       p.Reason,
			Missing:      p.Missing,
			ExpiresAt:    p.ExpiresAt,
		}
		for _, pan := range p.Panels {
			info.Panels = append(info.Panels, &pb.PluginPanelOp{
				Id: pan.ID, Title: pan.Title, Capability: pan.Capability,
			})
		}
		resp.Plugins = append(resp.Plugins, info)
	}
	for _, p := range s.projection.PlansInFlight() {
		resp.Plans = append(resp.Plans, &pb.PlanInFlightOp{
			SessionId:   p.SessionID,
			PlanId:      p.PlanID,
			ActiveStep:  int32(p.ActiveStep),
			Status:      p.Status,
			ActiveAgent: p.ActiveAgent,
			CostSoFar:   p.CostSoFar,
		})
	}
	if s.sessions != nil {
		// Active + paused sessions are the operationally-live set.
		for _, st := range []domain.SessionStatus{domain.SessionActive, domain.SessionPaused} {
			list, err := s.sessions.ListSessions(ctx, st)
			if err != nil {
				continue // best-effort; a snapshot omits an unreachable source rather than failing
			}
			for _, se := range list {
				resp.Sessions = append(resp.Sessions, &pb.SessionSummaryOp{
					Id:     string(se.ID),
					Goal:   se.Goal,
					Status: string(se.Status),
				})
			}
		}
	}
	return resp, nil
}

var _ pb.OperatorConsoleServer = (*Service)(nil)

// StreamEvents drains the sequenced feed from the client's cursor and pushes new
// events as they arrive. It captures the update channel before each ReadFrom so
// an Emit racing the wait is never missed. A slow client only delays its own
// stream — it never back-pressures the publisher (ADR-0047 D2/D9). Proper
// RESYNC_REQUIRED signalling is issue 0047-02; here a resync simply serves the
// current retained window.
func (s *Service) StreamEvents(req *pb.SubscribeRequest, stream pb.OperatorConsole_StreamEventsServer) error {
	ctx := stream.Context()
	cursor := req.GetLastSeq()

	// Live-only lane for token chunks (ADR-0047 D12): delivered as they arrive,
	// never replayed.
	ephCh, ephCancel := s.feed.SubscribeEphemeral()
	defer ephCancel()

	for {
		updated := s.feed.Updates() // capture before reading to avoid a missed wakeup

		events, resync := s.feed.ReadFrom(cursor)
		if resync {
			// The client's cursor has aged out of the retained window. Signal
			// RESYNC_REQUIRED (the client must Snapshot + resubscribe) and resume
			// live from the current head — no silent gap. ADR-0047 D6 (0047-02).
			head := s.feed.Head()
			if err := stream.Send(&pb.OperatorEvent{
				Seq:     head,
				Payload: &pb.OperatorEvent_Resync{Resync: &pb.ResyncRequired{LatestSeq: head}},
			}); err != nil {
				return err
			}
			cursor = head
			events = nil
		}
		for _, se := range events {
			if err := stream.Send(toOperatorEvent(se)); err != nil {
				return err
			}
			cursor = se.Seq
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-updated:
			// new retained event(s) available — loop and drain
		case se := <-ephCh:
			// live-only token chunk — deliver immediately, do not advance cursor
			if err := stream.Send(toOperatorEvent(se)); err != nil {
				return err
			}
		}
	}
}

// SubscribeBridge wires the spool to the EventBus: every published DomainEvent of
// a feed-relevant type is Emitted into the spool. This is the EventBus→feed
// bridge (ADR-0047 D2); it keeps the synchronous bus decoupled from the network.
func SubscribeBridge(bus domain.EventBus, feed *Spool) {
	for _, t := range feedEventTypes {
		bus.Subscribe(t, func(e domain.DomainEvent) { feed.Emit(e) })
	}
}

// SubscribeProjection folds plan-state events into the live-plan projection
// (ADR-0047 D7), alongside the feed. Both read the same synchronous bus, so the
// projection and feed stay consistent; Snapshot's lower-bound as_of_seq covers
// any in-flight race.
func SubscribeProjection(bus domain.EventBus, projection *Projection) {
	bus.Subscribe(domain.EventTypePlanState, func(e domain.DomainEvent) { projection.Apply(e) })
}

// feedEventTypes is the set of existing DomainEvent types the operator feed
// surfaces today (ADR-0047 0047-01). Later slices add the new event types.
var feedEventTypes = []string{
	domain.EventTypeAuctionEvent,
	domain.EventTypeAgentReady,
	// Phase 2: the absolute lifecycle state, covering all five transitions. The two
	// below remain for consumers that predate it.
	domain.EventTypeSessionState,
	domain.EventTypeSessionDormant,
	domain.EventTypeMemoryPressure,
	domain.EventTypeDaemonCrashed,
	domain.EventTypeWatchTriggered,
	domain.EventTypeMemoryWritten,
	domain.EventTypeHITLRaised,
	domain.EventTypeVerifierRound,
	domain.EventTypeLLMHealth,
	domain.EventTypePlanState,
	// ROUTE-08.A: ScoutUsefulnessEvent is Published to the EventBus (server.Execute)
	// and has a mapper case, but was missing from this bridge list — so it never
	// reached the feed. Added here so the per-session scout signal is actually visible.
	domain.EventTypeScoutUsefulness,
	// REACT-02 / ADR-0062: reactive backpressure shed events.
	domain.EventTypeReactiveBudget,
	// Agent-loop observability: per-memory_query thrash + poisoning-provenance events.
	domain.EventTypeAgentStep,
	// ADR-0102 A1: retention/compaction passes. Present here rather than only on the
	// owning plugin's plane because a deletion an operator has to go looking for is
	// not meaningfully auditable. Note the ROUTE-08.A precedent above — a mapper case
	// without an entry in THIS list is an event that is built, published and silently
	// never delivered.
	domain.EventTypeRetentionRun,
}
