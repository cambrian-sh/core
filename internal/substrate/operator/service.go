package operator

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/forget"
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
	idp        domain.Authenticator
	// principalNames gives WhoAmI the caller's display name (contract 0102).
	principalNames PrincipalNames
	// logins is the per-source failure window Login consults BEFORE the
	// credential is evaluated (ADR-0130 1f). Built in NewService — always on,
	// never attached later.
	logins   *loginLimiter
	loginObs LoginObserver // nil ⇒ only the structured log line records an attempt
	audit    AuditStore
	grants   GrantsStore
	// grantsMu serialises the grant read-modify-writes (SetToolGrant,
	// SetToolPolicy) so two concurrent commands for one agent cannot lose an
	// update between reading the set and replacing it (ENG-1116).
	grantsMu sync.Mutex
	controls *ExecutionControlHub
	// logs is the kernel's in-process retention window (contract 0082). nil ⇒
	// QueryLogs/TailLogs answer Unimplemented rather than an empty window, which
	// would read as "the kernel has been silent".
	logs    *util.LogRing
	hitl    domain.ApprovalHub
	effects CommandEffects
	// sharer applies the ADR-0142 D12 publish (ShareToTeam). nil ⇒ Unimplemented.
	sharer MemorySharer

	// ADR-0047 Amendment A2 read sources (CORE-OPS-1).
	tools      ToolCatalog
	skills     SkillLister
	memory     MemoryQuerier
	documents  memory.DocumentLister // nil ⇒ ListDocuments Unimplemented
	docGetter  memory.DocumentGetter // nil ⇒ GetDocument Unimplemented
	answerer   MemoryAnswerer        // ADR-0081; nil ⇒ AnswerMemory Unimplemented
	toolRunner ToolRunner
	ingestor   MemoryIngestor
	// uploads answers the ADR-0138 B1.3 replace-by-name lookup (ListDocuments
	// upload_name). nil ⇒ that lookup is Unimplemented and "upload-replace" is withheld.
	uploads UploadMatcher
	// eventTimeTolerance is the ADR-0134 future-time clamp tolerance; nil = default.
	eventTimeTolerance *time.Duration
	// sourceMetaCaps bounds IngestMemory's descriptive fields (ADR-0136 A3.5).
	sourceMetaCaps    SourceMetaCaps
	watches           domain.WatchConfigHandler
	pipelines         domain.PipelineLister
	pipelineDryRun    domain.PipelineDryRunner
	pipelineAuthor    domain.PipelineAuthor
	pipelineWriter    domain.PipelineWriter
	pipelineLifecycle domain.PipelineLifecycle
	deadletters       domain.WatchDeadLetterReader // REACT-01 / ADR-0061
	watchMetrics      domain.WatchMetricsReader    // REACT-05 / ADR-0071
	watchBacktest     domain.WatchBacktester       // REACT-05 / ADR-0071
	routePreview      RoutePreviewer               // ROUTE-07 / ADR-0077
	policy            domain.PolicyAdmin           // ADR-0085; nil ⇒ access-policy RPCs Unimplemented

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
	// mcpWriter is the write half of the MCP server surface (contract 0097).
	// nil ⇒ Save/Remove/SetToken/Test return Unimplemented, which a console
	// renders as "this kernel's MCP servers are file-configured".
	mcpWriter MCPWriter
	// integrations is the integrations surface (capability "integrations").
	integrations IntegrationManager
	// linkWriter is the identity plane's review lane (contract 0098, five-planes
	// step 2). nil ⇒ Confirm/Retract/ListLinkCandidates return Unimplemented,
	// which a console renders as "this kernel holds no assertions about identity"
	// rather than an empty review queue — two very different states.
	linkWriter LinkWriter
	// notes is the ADR-0139 D14 memory-notes surface (memory_notes.go). nil ⇒
	// the five notes RPCs answer Unimplemented and "memory-notes" is not advertised.
	notes MemoryNotesAdmin
	// skillLifecycle is the ADR-0139 D13 skill lifecycle (skills_lifecycle.go).
	// nil ⇒ the five lifecycle RPCs answer Unimplemented, ListSkills carries no
	// lifecycle fields, and "skills-lifecycle" is not advertised.
	skillLifecycle SkillLifecycleAdmin
	// skillGrants is per-agent skill access (skill_grants.go,
	// decisions/skill-access-grants.md). nil = the RPCs answer Unimplemented and
	// "skill-grants" is not advertised.
	skillGrants SkillGrantAdmin

	sessionOps SessionOps
	convOps    ConversationOps // ADR-0084 D9: OSS chat lane
	// principals answers "is this a principal the deployment knows?" for act-for
	// conversations (actfor.go). nil ⇒ act-for is refused (fail closed).
	principals domain.PrincipalDirectory
	// schedules is the operator view of principal-owned scheduled tasks
	// (scheduled_tasks.go). nil ⇒ ListScheduledTasks answers Unimplemented.
	schedules domain.PrincipalSchedules
	// facts is the read-only fact inspection (contract 0103, facts.go); nil ⇒
	// ListFacts / GetFactChain / GetFactCoverage answer Unimplemented.
	facts domain.FactInspector
	// forget is the ADR-0137 forget service (contract 0105); nil or disabled ⇒ the
	// erasure RPCs answer Unimplemented.
	forget *forget.Service
	// factStats is the fact extractor whose counters the Snapshot reports (ENG-431); nil = absent.
	factStats FactStatsSource

	kernelVersion   string
	contractVersion string
	capabilities    []string
	plugins         []PluginInfo // ADR-0089: plugin identity on the handshake

	// unwired names the optional seams New was given nil (Unwired, ENG-1305).
	unwired []string
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

// loginSource is the limiter key: the peer address the kernel observed on the
// connection, host only (a port changes per connection and would make every
// attempt a new source). "unknown" when there is no peer info, which is only
// the in-process test path — those attempts share one bucket.
func loginSource(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil && host != "" {
		return host
	}
	return p.Addr.String()
}

// Login authenticates a human operator and returns a token bound to its role.
// The interceptor lets this RPC through unauthenticated, which is why the
// per-source limiter runs here, before the credential is looked at.
func (s *Service) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	if s.idp == nil {
		return nil, status.Error(codes.Unimplemented, "operator identity not configured")
	}
	source := loginSource(ctx)
	if ok, wait := s.logins.Allow(source); !ok {
		s.observeLogin(LoginAttempt{Username: req.GetUsername(), Source: source, LockedOut: true,
			Failures: loginMaxFailures, At: time.Now()})
		return nil, status.Errorf(codes.ResourceExhausted,
			"too many failed logins from this source; retry in %s", wait.Round(time.Second))
	}
	pl, ok := s.idp.(domain.PasswordLogin)
	if !ok {
		// An OIDC-only kernel: the person signs in at the issuer (DescribeAuth says
		// which) and presents its token; there is no password to check here.
		return nil, status.Error(codes.FailedPrecondition,
			"password login is not enabled on this kernel; sign in through the identity provider (DescribeAuth)")
	}
	token, id, err := pl.Login(ctx, req.GetUsername(), req.GetPassword())
	if err != nil && errors.Is(err, domain.ErrAuthUnavailable) {
		// The password was never checked (the credential store or registry did
		// not answer): retryable, and NOT a failed attempt — an outage must not
		// lock a source out (ENG-2120).
		return nil, status.Error(codes.Unavailable, "sign-in temporarily unavailable; retry")
	}
	if err != nil {
		n := s.logins.Fail(source)
		s.observeLogin(LoginAttempt{Username: req.GetUsername(), Source: source, Failures: n, At: time.Now()})
		if errors.Is(err, domain.ErrUnauthenticated) {
			return nil, status.Error(codes.Unauthenticated, "invalid credentials")
		}
		return nil, status.Error(codes.Internal, "login failed")
	}
	s.logins.Reset(source)
	s.observeLogin(LoginAttempt{Username: req.GetUsername(), Source: source, Success: true, At: time.Now()})
	return &pb.LoginResponse{Token: token, Role: string(RoleFromRoles(id.Roles)),
		Principal: id.Principal.String(), Roles: id.Roles}, nil
}

// DescribeAuth tells a client how to sign in (ADR-0142 D1). Unauthenticated by
// design (the interceptor lets it through); it carries no secret.
func (s *Service) DescribeAuth(_ context.Context, _ *pb.DescribeAuthRequest) (*pb.DescribeAuthResponse, error) {
	out := &pb.DescribeAuthResponse{}
	if s.idp == nil {
		return out, nil
	}
	if src, ok := s.idp.(domain.AuthMetadataSource); ok {
		m := src.AuthMetadata()
		out.PasswordLogin, out.OidcIssuer, out.OidcClientId = m.PasswordLogin, m.OIDCIssuer, m.OIDCClientID
	} else {
		_, out.PasswordLogin = s.idp.(domain.PasswordLogin)
	}
	return out, nil
}

// Logout revokes the bearer token this call carries (ADR-0130 D5 / 1f, contract
// 0099). The auth interceptor has already resolved that token — an unknown or
// expired one never reaches here — so the principal is known and the revoke is of
// a live token. Afterwards every call with it, on the gRPC plane and the HTTP admin
// routes alike (both resolve through the same OperatorIdentity), is
// Unauthenticated.
func (s *Service) Logout(ctx context.Context, _ *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	if s.idp == nil {
		return nil, status.Error(codes.Unimplemented, "operator identity not configured")
	}
	token := bearerToken(ctx)
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "no bearer token to revoke")
	}
	principal, role, _ := PrincipalFromContext(ctx)
	if err := s.idp.Revoke(ctx, token); err != nil {
		return nil, status.Errorf(codes.Internal, "revoke token: %v", err)
	}
	slog.Info("operator logout", "user", principal, "role", string(role))
	return &pb.LogoutResponse{}, nil
}

// observeLogin always writes the structured log line, and additionally hands
// the attempt to the observer when one is wired. Successes are INFO; failures
// WARN with the running count, so a guessing run is visible in the log of every
// deployment. The observer (the premium receipts lane, ADR-0130 1f) is the
// durable, tamper-evident record ON TOP of the log, not a replacement for it:
// a log line that disappears the moment a plugin is installed would make the
// plugin's own failure modes silent.
func (s *Service) observeLogin(a LoginAttempt) {
	if s.loginObs != nil {
		s.loginObs.ObserveLogin(a)
	}
	switch {
	case a.LockedOut:
		slog.Warn("ADR-0130: operator login refused — source locked out", "user", a.Username, "source", a.Source,
			"window", loginFailureWindow.String())
	case !a.Success:
		slog.Warn("ADR-0130: operator login failed", "user", a.Username, "source", a.Source,
			"failures", a.Failures, "lockout_at", loginMaxFailures)
	default:
		slog.Info("operator login", "user", a.Username, "source", a.Source)
	}
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
	if s.factStats != nil {
		resp.FactExtractor = factExtractorStatsOp(s.factStats.Stats())
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
		bus.Subscribe(t, func(_ context.Context, e domain.DomainEvent) { feed.Emit(e) })
	}
}

// SubscribeProjection folds plan-state events into the live-plan projection
// (ADR-0047 D7), alongside the feed. Both read the same synchronous bus, so the
// projection and feed stay consistent; Snapshot's lower-bound as_of_seq covers
// any in-flight race.
func SubscribeProjection(bus domain.EventBus, projection *Projection) {
	bus.Subscribe(domain.EventTypePlanState, func(_ context.Context, e domain.DomainEvent) { projection.Apply(e) })
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
	// ADR-0070 D2: quarantine/recovery reach the console as audit entries (see
	// the mapper); without these two lines they were published and dropped.
	domain.EventTypeDaemonQuarantined,
	domain.EventTypeDaemonRecovered,
	domain.EventTypeWatchTriggered,
	domain.EventTypeMemoryWritten,
	domain.EventTypeHITLRaised,
	// ADR-0142 D11: every resolution (approved, denied, expired, withdrawn, …)
	// leaves the pending set on every console, not only the one that answered.
	domain.EventTypeHITLResolved,
	domain.EventTypeVerifierRound,
	domain.EventTypeLLMHealth,
	domain.EventTypePlanState,
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
	// ADR-0114 D37: live per-node pipeline readings. The ROUTE-08.A and
	// retention notes above are not decoration — this event had a publisher, a
	// mapper case, a fold and a rendered column, and stayed blank because it was
	// missing from THIS list. Third time.
	domain.EventTypePipelineMeters,
	// ADR-0114 D38: the per-item flow view. Bridged the moment the mapper case
	// exists — the feed_bridge test asserts exactly this pairing, because three
	// features have shipped blank by missing it.
	domain.EventTypePipelineStep,
	// Integrations: MCP server health (stopped/starting/ready/error) so the
	// console shows a server going down or coming up without polling.
	domain.EventTypeMCPServerHealth,
}
