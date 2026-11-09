package network

import (
	"context"
	"log/slog"
	"strconv"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// LedgerAccess is what the server holds of the run ledger (ADR-0145 D1): the
// classified writer and the ENFORCED reader built by kernel.NewLedgerStack. The
// raw store never reaches the server.
type LedgerAccess struct {
	Writer domain.LedgerWriter
	Reader domain.LedgerReader
}

// ledgerOn reports whether the ledger is wired (execution.ledger.enabled).
func (s *Server) ledgerOn() bool {
	return s.Ledger != nil && s.Ledger.Writer != nil && s.Ledger.Reader != nil
}

// turnOfSession is the conversation message that opened the turn a task session
// was ordered by (ADR-0084 D2 link), "" when none.
func (s *Server) turnOfSession(ctx context.Context, sid domain.SessionID) (conv, turn string) {
	if sid == "" || s.SessionMgr == nil {
		return "", ""
	}
	ses, err := s.SessionMgr.GetSession(ctx, sid)
	if err != nil || ses == nil {
		return "", ""
	}
	return ses.ConversationID, ses.OriginMessageID
}

// toolCallProvenance is where a tool call's ledger entry comes from, resolved
// from the caller's LEASE (never a header or payload, INV-5): the run and step
// the lease was minted for, the conversation and turn that ordered it, and the
// conversation owner (withCallerSession already seeded it on ctx).
func (s *Server) toolCallProvenance(ctx context.Context, lease domain.LeaseID) domain.LedgerProvenance {
	p := domain.LedgerProvenance{StepIndex: -1, Owner: domain.ConversationOwnerFromContext(ctx),
		ConversationID: domain.ConversationIDFromContext(ctx)}
	b, known := s.resolveLeaseFor(ctx, lease)
	if !known {
		return p
	}
	// A replanned plan's steps record under their generation's run id
	// (domain.LedgerRunID), so step indices never collide across replans.
	p.RunID = domain.LedgerRunID(string(b.RunID), b.PlanGeneration)
	if p.RunID != "" {
		p.StepIndex = b.StepIndex
	}
	p.TurnID = b.OriginMessageID
	if b.ConversationID != "" {
		p.ConversationID = b.ConversationID
	}
	if p.TurnID == "" || p.ConversationID == "" {
		conv, turn := s.turnOfSession(ctx, b.SessionID)
		if p.ConversationID == "" {
			p.ConversationID = conv
		}
		if p.TurnID == "" {
			p.TurnID = turn
		}
	}
	return p
}

// runProvenance is the ledger provenance of an Execute's run: the ordering
// conversation turn (from the caller's lease binding, header first, then the
// handoff's _session_token_id — resolveBindingFromHandoff), the session's
// conversation link when the binding carries none, and the conversation owner.
// StepIndex is -1; the DAG executor sets each step's.
func (s *Server) runProvenance(ctx context.Context, meta map[string]string, run domain.RunID, sid domain.SessionID) domain.LedgerProvenance {
	p := domain.LedgerProvenance{RunID: string(run), StepIndex: -1}
	if b, known := s.resolveBindingFromHandoff(ctx, meta); known {
		p.ConversationID = b.ConversationID
		p.TurnID = b.OriginMessageID
		if s.ConversationOwner != nil {
			p.Owner = s.ConversationOwner(ctx, b)
		}
	}
	if p.ConversationID == "" || p.TurnID == "" {
		conv, turn := s.turnOfSession(ctx, sid)
		if p.ConversationID == "" {
			p.ConversationID = conv
		}
		if p.TurnID == "" {
			p.TurnID = turn
		}
	}
	if p.Owner == "" {
		p.Owner = domain.ConversationOwnerFromContext(ctx)
	}
	return p
}

// readLedgerNode serves a led_… id through GetContextNode (the SDK's resolve_cid
// and assemble_context read references there), via the ENFORCED reader under the
// caller's own predicate — the same resolution GetArtifact uses. Forbidden and
// absent both answer as an unknown cid.
func (s *Server) readLedgerNode(ctx context.Context, id string) *pb.ContextNodeResponse {
	principal := artifactPrincipal(ctx)
	eff, _ := s.authorizer().ReadFilter(ctx, principal, artifactSurface)
	if eff == nil {
		return &pb.ContextNodeResponse{Cid: id}
	}
	e, err := s.Ledger.Reader.Get(domain.WithScope(ctx, eff), id)
	if err != nil {
		slog.Warn("ADR-0145: ledger read for GetContextNode failed", "id", id, "err", err)
		return &pb.ContextNodeResponse{Cid: id}
	}
	if e == nil {
		return &pb.ContextNodeResponse{Cid: id}
	}
	labels := []string{string(e.Kind)}
	if e.RunID != "" && e.StepIndex >= 0 {
		labels = append(labels, "step_"+strconv.Itoa(e.StepIndex))
	}
	if e.ToolName != "" {
		labels = append(labels, e.ToolName)
	}
	return &pb.ContextNodeResponse{Cid: id, Type: "ledger_" + string(e.Kind), Data: e.Body, Labels: labels}
}

// runLedgerScope is the predicate the DAG executor reads its own run's entries
// under: the kernel acting for the run's owner (the explicit, greppable bypass of
// AGENTS.md §2.4, with the owner fence still applied).
func runLedgerScope(owner string) *domain.TagPredicate {
	p := &domain.TagPredicate{Bypass: true}
	if owner != "" {
		p.Owners = []string{owner}
	}
	return p
}

// planContentStore is the content store a plan's DAG executor gets: none when the
// ledger is on (ADR-0145 D1, one pathway — step results are ledger entries, not
// plan-scoped content-store scratch, and there is nothing for the plan-end GC to
// evict), the server's otherwise (the legacy path).
func (s *Server) planContentStore() domain.ContentStore {
	if s.ledgerOn() {
		return nil
	}
	return s.ContentStore
}
