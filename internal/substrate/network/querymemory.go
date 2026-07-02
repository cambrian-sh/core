package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"time"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"

	"google.golang.org/grpc/metadata"
)

// QueryMemory handles the QueryMemory RPC: delegates to the MemorySearcher (which
// owns embedding, ACL filtering, and vector search) and translates results to proto.
func (s *Server) QueryMemory(ctx context.Context, req *pb.MemoryRequest) (*pb.MemoryResponse, error) {
	callerID := ""
	sessionID := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-agent-id"); len(vals) > 0 {
			callerID = vals[0]
		}
		// ADR-0034 (D13): the session ID lets the MemorySearcher look up the
		// non-forgeable caller_scope from the session record server-side. It is
		// taken from authenticated gRPC metadata, never from request payload.
		if vals := md.Get("x-session-id"); len(vals) > 0 {
			sessionID = vals[0]
		}
	}
	if sessionID != "" {
		ctx = domain.WithSessionID(ctx, sessionID)
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

	return &pb.MemoryResponse{Results: pbResults}, nil
}
