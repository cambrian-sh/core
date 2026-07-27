package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

	"google.golang.org/grpc/metadata"
)

// QueryMemory handles the QueryMemory RPC: delegates to the MemorySearcher (which
// owns embedding, ACL filtering, and vector search) and translates results to proto.
func (s *Server) QueryMemory(ctx context.Context, req *pb.MemoryRequest) (*pb.MemoryResponse, error) {
	callerID := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-agent-id"); len(vals) > 0 {
			callerID = vals[0]
		}
	}
	// ADR-0034 (D13): the session ID lets the MemorySearcher look up the
	// non-forgeable caller_scope from the session record server-side.
	//
	// Phase 0: it is RESOLVED from the caller's opaque BudgetLease rather than read
	// straight off the header. The header used to carry the lease (agents) or a session
	// ID (operator) interchangeably, so this lookup was handed a lease, missed, and fell
	// through to agent-only scope — Phase-2 caller-scope enforcement never engaged. See
	// resolveCallerSession.
	sessionID := s.resolveCallerSession(ctx)
	if sessionID != "" {
		ctx = domain.WithSessionID(ctx, sessionID)
		// ADR-0085 D7: a session's recorded surface OVERRIDES the transport-derived
		// one. It is the narrower, more specific fact — a conversation opened on an
		// outsider ingress stays an outsider conversation even when a later turn
		// arrives over an internal path. Widening on the way in is exactly the
		// escalation the clamp exists to prevent.
		ctx = domain.WithSurface(ctx, authz.ResolveSurface(ctx, s.SessionMgr))
	}

	slog.Info("QueryMemory called", "caller", callerID, "query", req.GetQuery())

	if s.MemorySearcher == nil {
		return &pb.MemoryResponse{Results: []*pb.MemoryResult{}}, nil
	}

	// ADR-0049 D4: x-lane="actions" routes to the "what did I do" lane (action
	// records); anything else is the default fact lane ("what do I know").
	var results []domain.SearchResult
	var err error
	lane := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("x-lane")) > 0 {
		lane = md.Get("x-lane")[0]
	}
	switch lane {
	case "actions":
		results, err = s.MemorySearcher.SearchActions(ctx, req.GetQuery(), callerID)
	case "scenes":
		results, err = s.MemorySearcher.SearchScenes(ctx, req.GetQuery(), callerID)
	case "entity":
		// ADR-0049 Issue 012: exact entity lookup — the query is a canonical kind:id.
		results, err = s.MemorySearcher.SearchEntities(ctx, req.GetQuery(), callerID)
	case "precedents":
		// ADR-0049 Issue 014: the world-model precedent pull lane (transitions).
		results, err = s.MemorySearcher.SearchPrecedents(ctx, req.GetQuery(), callerID)
	default:
		results, err = s.MemorySearcher.Search(ctx, req.GetQuery(), callerID)
	}
	if err != nil {
		return nil, fmt.Errorf("querymemory: %w", err)
	}

	// Step-2 observability (diagnostic, zero behavior change): emit an AgentStepEvent so
	// the harness can measure query-thrash (how many/how similar an agent's queries are)
	// and context poisoning from retrieval provenance — SelfHits are results the caller
	// itself authored (self-referential feedback), CrossSessionHits are results written in
	// another session bleeding in. Only for agent callers (callerID set).
	if s.EventBus != nil && callerID != "" {
		self, cross := 0, 0
		for _, r := range results {
			if r.Document.Metadata == nil {
				continue
			}
			if r.Document.Metadata["source_agent"] == callerID {
				self++
			}
			if sid := domain.DocSessionID(r.Document.Metadata); sid != "" && domain.SessionID(sid) != sessionID {
				cross++
			}
		}
		_ = s.EventBus.Publish(domain.AgentStepEvent{
			SessionID:        string(sessionID),
			AgentID:          callerID,
			Action:           "memory_query",
			Query:            req.GetQuery(),
			Hits:             len(results),
			SelfHits:         self,
			CrossSessionHits: cross,
		})
	}

	pbResults := make([]*pb.MemoryResult, 0, len(results))
	for _, r := range results {
		// ADR-0048 A1 (D10): fold the provenance/freshness facts the SDK renders into
		// the metadata payload. source_agent + session_id (D9) are already in Metadata
		// (kernel-stamped at write, ADR-0048 D1); the TEMPORAL facts (created_at,
		// last_accessed, activation) live on the Document STRUCT, not in Metadata, so
		// without this they never reach the agent. Reserved underscore keys avoid any
		// collision with real metadata keys. A rendering fact, not value-routing
		// (Zero-Hardcode-clean — the agent's LLM decides whether a stale fact warrants
		// re-verification; this never gates routing).
		meta := make(map[string]any, len(r.Document.Metadata)+3)
		maps.Copy(meta, r.Document.Metadata)
		meta["_activation_strength"] = r.Document.ActivationStrength
		if !r.Document.CreatedAt.IsZero() {
			meta["_created_at"] = r.Document.CreatedAt.UTC().Format(time.RFC3339)
		}
		if !r.Document.LastAccessedAt.IsZero() {
			meta["_last_accessed_at"] = r.Document.LastAccessedAt.UTC().Format(time.RFC3339)
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			metaJSON = []byte("{}")
		}
		// ADR-0048 #1: represent a fact by its one-line Summary when present (the
		// agent reads the gist, not the full body); the full content is reachable via
		// metadata["content_cid"] (carried in metaJSON) through get_context_node.
		text := r.Document.Text
		if r.Document.Summary != "" {
			text = r.Document.Summary
		}
		pbResults = append(pbResults, &pb.MemoryResult{
			Text:     text,
			Score:    float32(r.Score),
			Metadata: string(metaJSON),
		})
	}

	// Honor the caller's requested window. The MemorySearcher returns the
	// server-side recall window (config recall_top_k); results are already
	// blend-ranked best-first, so the prefix IS the top-k. This only ever
	// returns FEWER (never fabricates), and k==0 (unset) keeps the full window
	// — without this, req.TopK was silently dropped and every caller got the
	// config window regardless of what it asked for.
	if k := int(req.GetTopK()); k > 0 && len(pbResults) > k {
		pbResults = pbResults[:k]
	}

	return &pb.MemoryResponse{Results: pbResults, PolicyNote: s.policyNote(ctx, callerID, len(pbResults))}, nil
}

// policyNote explains an EMPTY result set that access policy caused, so an agent
// can say "I am not permitted to see that" instead of "I found nothing" (ADR-0085
// INV-3). It returns "" when results were returned, when no decision point is
// installed, or when policy played no part — annotating every response would
// train callers to ignore the field.
//
// It re-asks the decision point rather than threading the earlier decision back
// out of the search path: the question is cheap (a cache hit on the resolver) and
// keeping it out of the retrieval signatures is worth more than saving it.
func (s *Server) policyNote(ctx context.Context, callerID string, resultCount int) string {
	if resultCount > 0 || s.Authz == nil {
		return ""
	}
	pred, dec := s.Authz.ReadFilter(ctx, domain.AgentPrincipal(callerID), domain.SurfaceRef{Kind: domain.SurfaceAgent})
	switch {
	case pred == nil:
		// The principal did not resolve at all — the single most silent failure in
		// the system, now stated out loud.
		return dec.Explain()
	case dec.Reason == domain.ReasonUnsatisfiablePolicy:
		return dec.Explain()
	case !pred.Bypass && !pred.IsZero():
		// A real predicate applied and nothing came back. That may be an honest
		// "no data", but the caller deserves to know a boundary was in play.
		return "no results within the caller's access boundary; " + dec.Explain()
	default:
		return "" // unrestricted read, genuinely empty corpus
	}
}
