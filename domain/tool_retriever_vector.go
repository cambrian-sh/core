package domain

import (
	"context"
	"strings"
)

// VectorToolRetriever is the pgvector-backed ToolRetriever (ADR-0044 D3/D4/D6).
// It embeds the need with the nomic `search_query:` prefix, runs the store's
// cosine search over DocTypeTool documents, restricts the candidate set to the
// granted tools via an in-query Filter (authorized-by-construction), and applies
// a relevance Floor — returning up to k tool names, or none when nothing clears
// the floor. It depends only on the VectorStore + Embedder ports, so the
// ToolExecutor never imports the vector store (the hexagon holds).
//
// Tool documents are stored with Document.ID = the tool's identity (native name
// or mcp:<server>/<tool>), which is what the search returns and the Filter matches.
type VectorToolRetriever struct {
	Store    VectorStore
	Embedder Embedder
	Floor    float64 // minimum cosine (RawScore) to include; <=0 disables the floor
}

// Rank implements ToolRetriever.
func (r VectorToolRetriever) Rank(ctx context.Context, query string, grantedNames []string, k int) ([]string, error) {
	vec, err := r.Embedder.Embed(ctx, "search_query: "+query)
	if err != nil {
		return nil, err
	}
	opts := SearchOptions{DocumentType: DocTypeTool, TopK: k}
	if f := toolIDFilter(grantedNames); f != "" {
		opts.Filter = f // restrict ranking to the authorized set (grant filter in the query)
	}
	results, err := r.Store.Search(ctx, vec, opts)
	if err != nil {
		return nil, err
	}
	granted := make(map[string]struct{}, len(grantedNames))
	for _, n := range grantedNames {
		granted[n] = struct{}{}
	}
	out := make([]string, 0, k)
	for _, res := range results {
		if r.Floor > 0 && res.RawScore < r.Floor {
			continue // relevance floor: below this, "no tool fits" (empty allowed)
		}
		name := res.Document.ID
		// Defense in depth: the Filter already excludes ungranted tools, but never
		// emit one even if a backend ignores the Filter.
		if len(granted) > 0 {
			if _, ok := granted[name]; !ok {
				continue
			}
		}
		out = append(out, name)
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

// toolIDFilter builds a SQL predicate restricting the search to the granted tool
// identities (their Document.ID). Empty when no names are given (unrestricted ⇒
// rank over all tools). Tool identities are operator/discovery-controlled, but
// single quotes are escaped defensively.
func toolIDFilter(names []string) string {
	if len(names) == 0 {
		return ""
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + strings.ReplaceAll(n, "'", "''") + "'"
	}
	return "id IN (" + strings.Join(quoted, ", ") + ")"
}

var _ ToolRetriever = VectorToolRetriever{}
