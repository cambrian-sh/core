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
	res, err := s.watchBacktest.Backtest(ctx, cfg, req.GetAfterSeq())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backtest: %v", err)
	}
	out := make([]*pb.WatchBacktestVerdictOp, 0, len(res.Verdicts))
	for _, v := range res.Verdicts {
		out = append(out, &pb.WatchBacktestVerdictOp{
			Seq: v.Seq, StreamId: v.StreamID, RawText: v.RawText,
			WouldFire: v.WouldFire, EvalError: v.EvalError,
		})
	}
	// The window rides with the verdicts (GOV-02): journal GC shortens replayable
	// history, and "would have fired twice" means nothing without how much history
	// was searched.
	return &pb.BacktestWatchOpResponse{
		Verdicts:          out,
		RetainedOldestSeq: res.RetainedOldestSeq,
		RetainedNewestSeq: res.RetainedNewestSeq,
		RetainedCount:     int32(res.RetainedCount),
	}, nil
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
		op := toWatchConfigOp(w)
		s.enrichWatchConfig(op)
		filtered = append(filtered, op)
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
		ID:          c.GetId(),
		Name:        c.GetName(),
		Description: c.GetDescription(),
		Source: domain.WatchSource{
			Type:     c.GetSourceType(),
			StreamID: c.GetSourceStreamId(),
			Cron:     c.GetSourceCron(),
			Timezone: c.GetSourceTimezone(),
		},
		Condition:            c.GetCondition(),
		ConditionType:        c.GetConditionType(),
		Action:               action,
		Active:               c.GetActive(),
		ResponseMode:         c.GetResponseMode(),
		DaemonParams:         stringMapToAny(c.GetDaemonParams()),
		MaxConcurrentPlans:   int(c.GetMaxConcurrentPlans()),
		DebounceSeconds:      int(c.GetDebounceSeconds()),
		ConditionPayloadKeys: c.GetConditionPayloadKeys(),
		Approved:             c.GetApproved(),
		DryRun:               c.GetDryRun(),
		MissedFirePolicy:     c.GetMissedFirePolicy(),
		Actions:              watchActionsFromOp(c.GetActions()),
	}
}

// watchActionsToOp maps arms 2..N onto the wire.
func watchActionsToOp(in []domain.WatchAction) []*pb.WatchActionOp {
	if len(in) == 0 {
		return nil
	}
	out := make([]*pb.WatchActionOp, 0, len(in))
	for _, a := range in {
		out = append(out, &pb.WatchActionOp{
			Type: a.Type, TargetType: a.TargetType, Target: a.Target, Payload: a.Payload,
		})
	}
	return out
}

// watchActionsFromOp maps arms 2..N off the wire.
func watchActionsFromOp(in []*pb.WatchActionOp) []domain.WatchAction {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.WatchAction, 0, len(in))
	for _, a := range in {
		out = append(out, domain.WatchAction{
			Type: a.GetType(), TargetType: a.GetTargetType(), Target: a.GetTarget(), Payload: a.GetPayload(),
		})
	}
	return out
}

func toWatchConfigOp(c domain.WatchConfig) *pb.WatchConfigOp {
	return &pb.WatchConfigOp{
		Id:             c.ID,
		Name:           c.Name,
		Description:    c.Description,
		SourceType:     c.Source.Type,
		SourceStreamId: c.Source.StreamID,
		SourceCron:     c.Source.Cron,
		SourceTimezone: c.Source.Timezone,
		Condition:      c.Condition,
		ConditionType:  c.ConditionType,
		Action: &pb.WatchActionOp{
			Type: c.Action.Type, TargetType: c.Action.TargetType, Target: c.Action.Target, Payload: c.Action.Payload,
		},
		Actions:              watchActionsToOp(c.Actions),
		Active:               c.Active,
		ResponseMode:         c.ResponseMode,
		DaemonParams:         anyMapToString(c.DaemonParams),
		MaxConcurrentPlans:   int32(c.MaxConcurrentPlans),
		DebounceSeconds:      int32(c.DebounceSeconds),
		ConditionPayloadKeys: c.ConditionPayloadKeys,
		Approved:             c.Approved,
		DryRun:               c.DryRun,
		// MissedFirePolicy was declared on the wire by REACT-06 and dropped here,
		// so a schedule watch round-tripped through the console silently lost its
		// catch-up behaviour and reverted to "skip". Fixed with contract 0074.
		MissedFirePolicy: c.MissedFirePolicy,
		// Contract 0074: "unknown" until a stream registry says otherwise, and -1
		// for an uncounted refcount. Both are filled in by enrichWatchConfig when
		// the reactive plane can answer; neither may default to a healthy-looking
		// value, because a watch whose stream has died still reads "active".
		SourceStreamState:    domain.StreamUnknown,
		SourceStreamRefcount: -1,
	}
}

// enrichWatchConfig fills the contract-0074 liveness and history fields.
//
// Split from toWatchConfigOp because the mapper is a pure translation and these
// need live sources — and because a nil source must leave "unknown" in place
// rather than overwrite it with a default that reads as healthy.
func (s *Service) enrichWatchConfig(op *pb.WatchConfigOp) {
	if op == nil {
		return
	}
	if s.streams != nil && op.GetSourceStreamId() != "" {
		op.SourceStreamState = s.streams.StreamState(op.GetSourceStreamId())
		op.SourceStreamRefcount = int32(s.streams.StreamRefcount(op.GetSourceStreamId()))
	}
	if s.watchFires != nil {
		fires := s.watchFires.RecentFires(op.GetId(), watchFireHistoryLimit)
		out := make([]*pb.WatchFireOp, 0, len(fires))
		for _, f := range fires {
			out = append(out, &pb.WatchFireOp{
				AtUnixMs:  f.At.UnixMilli(),
				Outcome:   f.Outcome,
				Error:     f.Error,
				LatencyMs: f.LatencyMs,
			})
		}
		op.LastFires = out
	}
}

// watchFireHistoryLimit bounds the per-watch history on a LIST response. The
// design asks for a 20-bar sparkline; fetching more would cost a page of rows
// nothing renders.
const watchFireHistoryLimit = 20

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

// DeadLetterRetrier replays a dead-lettered reactive action.
//
// Satisfied by the premium reactive engine; nil in OSS. Kept separate from
// WatchDeadLetterReader because reading the queue is safe on any build and
// replaying is not — an OSS kernel that could list entries must not thereby be
// able to fire them.
type DeadLetterRetrier interface {
	// RetryDeadLetter replays one entry under the idempotency key recorded with
	// it (REACT-01), so a repeated call is a no-op rather than a second fire.
	RetryDeadLetter(ctx context.Context, deadLetterID string) error
}

// SetDeadLetterRetrier wires the replay surface. nil ⇒ RetryWatchDeadLetter
// returns Unimplemented, which is what the console renders as "this contract has
// no retry RPC" rather than offering a button that fails.
func (s *Service) SetDeadLetterRetrier(r DeadLetterRetrier) { s.deadletterRetry = r }

// RetryWatchDeadLetter replays one dead letter. Mutating: command_id + reason,
// audited, idempotent.
//
// Two layers of idempotency, deliberately. command_id dedupes the OPERATOR's
// retry (a double-click, a client resend); the REACT-01 key dedupes the ACTION
// itself (a replay of something that in fact already ran). They protect against
// different mistakes and neither subsumes the other.
func (s *Service) RetryWatchDeadLetter(ctx context.Context, req *pb.RetryWatchDeadLetterOpRequest) (*pb.CommandAck, error) {
	if s.deadletterRetry == nil {
		return nil, status.Error(codes.Unimplemented, "replaying a dead letter is a premium capability")
	}
	if req.GetDeadLetterId() == "" {
		return nil, status.Error(codes.InvalidArgument, "dead_letter_id is required")
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"retry_watch_dead_letter", "watch_dead_letter", req.GetDeadLetterId(), req.GetDeadLetterId(),
		func() error { return s.deadletterRetry.RetryDeadLetter(ctx, req.GetDeadLetterId()) })
}

// SetWatchLiveness wires the contract-0074 stream registry, fire history and
// plane-budget readers. Any may be nil, and a nil source leaves its fields at
// the "cannot tell" value rather than a default that reads as healthy.
func (s *Service) SetWatchLiveness(streams domain.StreamRegistry, fires domain.WatchFireReader, budget domain.ReactiveBudgetReader) {
	s.streams = streams
	s.watchFires = fires
	s.planeBudget = budget
}

// GetReactiveBudget reports the plane's running totals against its caps.
//
// The dead-letter count comes from the existing reader, so the screen's one
// already-real figure keeps working even on a kernel that cannot report budgets.
func (s *Service) GetReactiveBudget(_ context.Context, _ *pb.GetReactiveBudgetOpRequest) (*pb.GetReactiveBudgetOpResponse, error) {
	if s.planeBudget == nil && s.deadletters == nil {
		return nil, status.Error(codes.Unimplemented, "the reactive plane is not configured on this kernel")
	}

	resp := &pb.GetReactiveBudgetOpResponse{DeadLetterCount: -1}
	if s.deadletters != nil {
		if entries, err := s.deadletters.ListDeadLetters(0); err == nil {
			resp.DeadLetterCount = int64(len(entries))
		}
	}
	if s.planeBudget == nil {
		// Every counter absent rather than zero: a console must be able to say
		// "not reported" instead of drawing a plane that looks idle.
		resp.Budget = &pb.ReactivePlaneBudgetOp{
			GateEvaluationsThisHour: -1, GateEvaluationsCap: -1,
			PlansStartedThisHour: -1, PlansStartedCap: -1,
			SignalsShedThisHour: -1,
		}
		return resp, nil
	}

	b := s.planeBudget.PlaneBudget()
	resp.Budget = &pb.ReactivePlaneBudgetOp{
		GateEvaluationsThisHour: b.GateEvaluationsThisHour,
		GateEvaluationsCap:      b.GateEvaluationsCap,
		PlansStartedThisHour:    b.PlansStartedThisHour,
		PlansStartedCap:         b.PlansStartedCap,
		SignalsShedThisHour:     b.SignalsShedThisHour,
	}
	if !b.WindowStarted.IsZero() {
		resp.Budget.WindowStartedUnixMs = b.WindowStarted.UnixMilli()
	}
	return resp, nil
}
