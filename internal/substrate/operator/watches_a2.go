package operator

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// Watch CRUD is PREMIUM capability-gated (ADR-0047 D14 / Amendment A2.6). The
// proto lives in OSS in full; the handlers delegate to domain.WatchConfigHandler,
// an OSS-owned port that is nil in an OSS build (the ApprovalHub pattern, D14
// rule 3) — so these RPCs return Unimplemented and the WatchTriggered event class
// never publishes. The premium build injects a real handler via the existing
// Options.NewSignalReceiver seam (ADR-0057), never a //go:build premium handler
// inside this package — excisability is preserved by construction.

// SetWatchHandler wires the premium watch CRUD surface. nil (OSS) ⇒ the four watch
// RPCs return Unimplemented; the capability handshake omits "watches-*".
func (s *Service) SetWatchHandler(h domain.WatchConfigHandler) { s.watches = h }

// SetDeadLetterReader wires the reactive dead-letter read surface (REACT-01 /
// ADR-0061). nil ⇒ ListWatchDeadLetters returns Unimplemented.
func (s *Service) SetDeadLetterReader(r domain.WatchDeadLetterReader) { s.deadletters = r }

// SetWatchObservability wires the REACT-05 watch-metrics reader + backtester. nil ⇒ the
// GetWatchMetrics / BacktestWatch RPCs return Unimplemented.
func (s *Service) SetWatchObservability(m domain.WatchMetricsReader, b domain.WatchBacktester) {
	s.watchMetrics = m
	s.watchBacktest = b
}

// GetWatchMetrics returns per-watch observability counters (REACT-05 / ADR-0071). Read
// RPC (any authenticated role).
func (s *Service) GetWatchMetrics(_ context.Context, _ *pb.GetWatchMetricsOpRequest) (*pb.GetWatchMetricsOpResponse, error) {
	if s.watchMetrics == nil {
		return nil, status.Error(codes.Unimplemented, "watch observability is a premium capability")
	}
	ms := s.watchMetrics.WatchMetrics()
	out := make([]*pb.WatchMetricsOp, 0, len(ms))
	for _, m := range ms {
		out = append(out, &pb.WatchMetricsOp{
			WatchId:                m.WatchID,
			SignalsSeen:            m.SignalsSeen,
			ConditionFired:         m.ConditionFired,
			ConditionSuppressed:    m.ConditionSuppressed,
			DryRunWouldFire:        m.DryRunWouldFire,
			ActionFailed:           m.ActionFailed,
			DeadLettered:           m.DeadLettered,
			MeanConditionLatencyMs: m.MeanConditionLatencyMs(),
		})
	}
	return &pb.GetWatchMetricsOpResponse{Metrics: out}, nil
}

// BacktestWatch replays a candidate watch over the signal journal and reports would-fires
// without acting (REACT-05 / ADR-0071). Read RPC.
func (s *Service) BacktestWatch(ctx context.Context, req *pb.BacktestWatchOpRequest) (*pb.BacktestWatchOpResponse, error) {
	if s.watchBacktest == nil {
		return nil, status.Error(codes.Unimplemented, "watch backtesting is a premium capability")
	}
	cfg := fromWatchConfigOp(req.GetConfig())
	verdicts, err := s.watchBacktest.Backtest(ctx, cfg, req.GetAfterSeq())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backtest: %v", err)
	}
	out := make([]*pb.WatchBacktestVerdictOp, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, &pb.WatchBacktestVerdictOp{
			Seq: v.Seq, StreamId: v.StreamID, RawText: v.RawText,
			WouldFire: v.WouldFire, EvalError: v.EvalError,
		})
	}
	return &pb.BacktestWatchOpResponse{Verdicts: out}, nil
}

// ListWatchDeadLetters returns reactive actions that could not be delivered
// (REACT-01 / ADR-0061). Read RPC (any authenticated role). The reader is the OSS
// bbolt journal; an OSS-only kernel never writes entries, so the list is empty.
func (s *Service) ListWatchDeadLetters(_ context.Context, req *pb.ListWatchDeadLettersOpRequest) (*pb.ListWatchDeadLettersOpResponse, error) {
	if s.deadletters == nil {
		return nil, status.Error(codes.Unimplemented, "reactive dead-letter surface is not configured")
	}
	entries, err := s.deadletters.ListDeadLetters(int(req.GetLimit()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list dead-letters: %v", err)
	}
	out := make([]*pb.WatchDeadLetterOp, 0, len(entries))
	for _, e := range entries {
		out = append(out, &pb.WatchDeadLetterOp{
			Id:             e.ID,
			WatchId:        e.WatchID,
			ActionType:     e.ActionType,
			Key:            e.Key,
			Reason:         e.Reason,
			SignalStreamId: e.Signal.StreamID,
			SignalRawText:  e.Signal.RawText,
			FailedAtUnixMs: e.FailedAt.UnixMilli(),
		})
	}
	return &pb.ListWatchDeadLettersOpResponse{Entries: out}, nil
}

// ListWatches returns the registered reactive watches, filtered + paged. Premium
// (Unimplemented in OSS). Read RPC (any authenticated role). A2.6.
func (s *Service) ListWatches(_ context.Context, req *pb.ListWatchesOpRequest) (*pb.ListWatchesOpResponse, error) {
	if s.watches == nil {
		return nil, status.Error(codes.Unimplemented, "watch surfaces are a premium capability")
	}
	all, err := s.watches.ListWatches()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list watches: %v", err)
	}
	var filtered []*pb.WatchConfigOp
	for _, w := range all {
		if req.GetActiveOnly() && !w.Active {
			continue
		}
		filtered = append(filtered, toWatchConfigOp(w))
	}
	page, lo, hi := paginate(len(filtered), req.GetPage(), req.GetPageSize())
	return &pb.ListWatchesOpResponse{Configs: filtered[lo:hi], Total: int32(len(filtered)), Page: page}, nil
}

// RegisterWatch persists a reactive watch (Operator-only, idempotent, audited).
// Premium (Unimplemented in OSS). The assigned id lands in the audit `after` and
// in a subsequent ListWatches. A2.6.
func (s *Service) RegisterWatch(ctx context.Context, req *pb.RegisterWatchOpRequest) (*pb.CommandAck, error) {
	if s.watches == nil {
		return nil, status.Error(codes.Unimplemented, "watch surfaces are a premium capability")
	}
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	cfg := fromWatchConfigOp(req.GetConfig())
	if cfg.Action.Type == "dispatch_agent" && cfg.Action.TargetType == "" {
		return nil, status.Error(codes.InvalidArgument, "action.target_type is required for a dispatch_agent watch")
	}
	// REACT-03 / ADR-0063: risk gate. An `llm` condition driving a high-risk,
	// unattended action (start_plan / dispatch_agent) lets untrusted signal content
	// decide a consequential action — it must carry the operator's explicit
	// acknowledgement. This is a deterministic security gate (ADR-0034), not routing.
	if isHighRiskLLMWatch(cfg) && !cfg.Approved {
		return nil, status.Error(codes.InvalidArgument,
			"a high-risk llm-condition watch (start_plan/dispatch_agent action) requires approved=true")
	}
	if s.audit == nil {
		return nil, status.Error(codes.Unimplemented, "operator audit store not configured")
	}

	// Idempotency: a replayed command_id does not register a second watch.
	prior, err := s.audit.Query(ctx, AuditFilter{CommandID: req.GetCommandId(), Limit: 1})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit lookup: %v", err)
	}
	if len(prior) == 1 {
		return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: true}, nil
	}

	id, err := s.watches.RegisterWatch(cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register watch: %v", err)
	}
	actor, role, _ := PrincipalFromContext(ctx)
	if _, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "register_watch",
		TargetType: "watch", TargetID: id, After: id, Reason: req.GetReason(), Result: "ok",
	}); err != nil {
		return nil, err
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: false}, nil
}

// DeleteWatch removes a watch by id (Operator-only, idempotent, audited). A2.6.
func (s *Service) DeleteWatch(ctx context.Context, req *pb.DeleteWatchOpRequest) (*pb.CommandAck, error) {
	if s.watches == nil {
		return nil, status.Error(codes.Unimplemented, "watch surfaces are a premium capability")
	}
	if req.GetWatchId() == "" {
		return nil, status.Error(codes.InvalidArgument, "watch_id is required")
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "delete_watch", "watch", req.GetWatchId(),
		req.GetWatchId(), func() error { return s.watches.DeleteWatch(req.GetWatchId()) })
}

// SetWatchActive toggles a watch's active flag (Operator-only, idempotent, audited). A2.6.
func (s *Service) SetWatchActive(ctx context.Context, req *pb.SetWatchActiveOpRequest) (*pb.CommandAck, error) {
	if s.watches == nil {
		return nil, status.Error(codes.Unimplemented, "watch surfaces are a premium capability")
	}
	if req.GetWatchId() == "" {
		return nil, status.Error(codes.InvalidArgument, "watch_id is required")
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "set_watch_active", "watch", req.GetWatchId(),
		boolStr(req.GetActive()), func() error { return s.watches.SetWatchActive(req.GetWatchId(), req.GetActive()) })
}

// isHighRiskLLMWatch reports whether a watch is the dangerous combination REACT-03
// gates: an `llm` condition (untrusted signal content decides the fire) driving a
// consequential, unattended action (`start_plan` / `dispatch_agent`).
func isHighRiskLLMWatch(cfg domain.WatchConfig) bool {
	if cfg.ConditionType != domain.ConditionTypeLLM {
		return false
	}
	return cfg.Action.Type == "start_plan" || cfg.Action.Type == "dispatch_agent"
}

// ── mapping ───────────────────────────────────────────────────────────────────

func fromWatchConfigOp(c *pb.WatchConfigOp) domain.WatchConfig {
	if c == nil {
		return domain.WatchConfig{}
	}
	var action domain.WatchAction
	if a := c.GetAction(); a != nil {
		action = domain.WatchAction{Type: a.GetType(), TargetType: a.GetTargetType(), Target: a.GetTarget(), Payload: a.GetPayload()}
	}
	return domain.WatchConfig{
		ID:                 c.GetId(),
		Name:               c.GetName(),
		Description:        c.GetDescription(),
		Source: domain.WatchSource{
			Type:     c.GetSourceType(),
			StreamID: c.GetSourceStreamId(),
			Cron:     c.GetSourceCron(),
			Timezone: c.GetSourceTimezone(),
		},
		Condition:          c.GetCondition(),
		ConditionType:      c.GetConditionType(),
		Action:             action,
		Active:             c.GetActive(),
		ResponseMode:         c.GetResponseMode(),
		DaemonParams:         stringMapToAny(c.GetDaemonParams()),
		MaxConcurrentPlans:   int(c.GetMaxConcurrentPlans()),
		DebounceSeconds:      int(c.GetDebounceSeconds()),
		ConditionPayloadKeys: c.GetConditionPayloadKeys(),
		Approved:             c.GetApproved(),
		DryRun:               c.GetDryRun(),
		MissedFirePolicy:     c.GetMissedFirePolicy(),
	}
}

func toWatchConfigOp(c domain.WatchConfig) *pb.WatchConfigOp {
	return &pb.WatchConfigOp{
		Id:            c.ID,
		Name:          c.Name,
		Description:   c.Description,
		SourceType:    c.Source.Type,
		SourceStreamId: c.Source.StreamID,
		SourceCron:     c.Source.Cron,
		SourceTimezone: c.Source.Timezone,
		Condition:     c.Condition,
		ConditionType: c.ConditionType,
		Action: &pb.WatchActionOp{
			Type: c.Action.Type, TargetType: c.Action.TargetType, Target: c.Action.Target, Payload: c.Action.Payload,
		},
		Active:               c.Active,
		ResponseMode:         c.ResponseMode,
		DaemonParams:         anyMapToString(c.DaemonParams),
		MaxConcurrentPlans:   int32(c.MaxConcurrentPlans),
		DebounceSeconds:      int32(c.DebounceSeconds),
		ConditionPayloadKeys: c.ConditionPayloadKeys,
		Approved:             c.Approved,
		DryRun:               c.DryRun,
	}
}

func stringMapToAny(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func anyMapToString(m map[string]any) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}
