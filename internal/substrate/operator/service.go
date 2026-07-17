package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
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
	hitl       domain.ApprovalHub
	effects    CommandEffects

	// ADR-0047 Amendment A2 read sources (CORE-OPS-1).
	tools      ToolCatalog
	skills     SkillLister
	memory     MemoryQuerier
	toolRunner ToolRunner
	ingestor    MemoryIngestor
	watches       domain.WatchConfigHandler
	deadletters   domain.WatchDeadLetterReader // REACT-01 / ADR-0061
	watchMetrics  domain.WatchMetricsReader    // REACT-05 / ADR-0071
	watchBacktest domain.WatchBacktester       // REACT-05 / ADR-0071
	routePreview  RoutePreviewer               // ROUTE-07 / ADR-0077

	sessionOps SessionOps

	kernelVersion   string
	contractVersion string
	capabilities    []string
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
					Id:     se.ID,
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
	domain.EventTypeSessionDormant,
	domain.EventTypeSessionCompleted,
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
}
