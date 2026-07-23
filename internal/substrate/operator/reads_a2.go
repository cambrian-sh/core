package operator

import (
	"context"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ADR-0047 Amendment A2 (CORE-OPS-1): the operator-plane paged reads. These are
// any-authenticated-role reads (the interceptor's Query*/List* gate) and never
// fold into Snapshot (D8). Distinct messages from the agent-plane equivalents
// (A2.1): the operator plane carries a Bearer principal, never an x-agent-id.

// ToolCatalog is the whole registered tool catalog, independent of any agent's
// grants. Satisfied by *domain.ToolExecutor (AllTools). nil ⇒ ListTools empty.
type ToolCatalog interface {
	AllTools() []domain.SystemTool
}

// SkillLister enumerates the registered system skills. Satisfied by
// domain.SkillRegistry (All). nil ⇒ ListSkills empty.
type SkillLister interface {
	All() []domain.Skill
}

// MemoryQuerier is the operator's ScopeSystem memory read (A2.4/D13 — sees all
// data). Satisfied by *memory.QueryService (SearchSystem). nil ⇒ Unimplemented.
type MemoryQuerier interface {
	SearchSystem(ctx context.Context, query string) ([]domain.SearchResult, error)
}

// MemoryAnswerer is the optional operator answer lane (ADR-0081): the agentic
// retrieval loop producing a grounded, [n]-cited answer plus the evidence each
// marker resolves to. Satisfied by *memory.QueryService (AnswerSystem) only when
// the agentic path is wired; nil ⇒ AnswerMemory returns Unimplemented and the
// "memory-answer" capability is not advertised.
type MemoryAnswerer interface {
	AnswerSystem(ctx context.Context, query string) (status, answer string, evidence []domain.SearchResult, err error)
}

// grantsEnumerator is the optional reverse-index source for ListTools.grants
// (tool → which agents hold it). Satisfied by *domain.InMemoryGrantsStore (All).
// When s.grants does not implement it, ToolOp.grants is left empty (best-effort).
type grantsEnumerator interface {
	All() map[string][]domain.ToolGrant
}

const defaultReadPageSize = 50

// SetReadSources wires the operator-plane read RPCs (ADR-0047 A2). Any source may
// be nil; its RPC then returns an empty page (tools/skills) or Unimplemented
// (memory) rather than failing.
func (s *Service) SetReadSources(tools ToolCatalog, skills SkillLister, memory MemoryQuerier) {
	s.tools = tools
	s.skills = skills
	s.memory = memory
}

// SetMemoryAnswerer wires the ADR-0081 answer lane. nil (the default) leaves
// AnswerMemory returning Unimplemented and withholds the "memory-answer"
// capability.
func (s *Service) SetMemoryAnswerer(a MemoryAnswerer) { s.answerer = a }

// HasMemoryAnswerer reports whether the answer lane is wired, so app.go can gate
// the "memory-answer" capability on it.
func (s *Service) HasMemoryAnswerer() bool { return s.answerer != nil }

// ListTools returns the whole tool catalog the operator governs, with per-tool
// grant reverse-index and MCP-vs-builtin source labelling (A2.3). Paged.
func (s *Service) ListTools(_ context.Context, req *pb.ListToolsOpRequest) (*pb.ListToolsOpResponse, error) {
	if s.tools == nil {
		return &pb.ListToolsOpResponse{Page: pageOf(req.GetPage())}, nil
	}
	rev := s.toolGrantsIndex() // tool name → []ToolGrantOp

	q := strings.ToLower(strings.TrimSpace(req.GetQuery()))
	var filtered []*pb.ToolOp
	for _, t := range s.tools.AllTools() {
		if req.GetDangerousOnly() && !t.Dangerous {
			continue
		}
		src := toolSource(t.Name)
		if f := req.GetSource(); f != "" && !sourceMatches(src, f) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(t.Name), q) && !strings.Contains(strings.ToLower(t.Description), q) {
			continue
		}
		filtered = append(filtered, &pb.ToolOp{
			Name:           t.Name,
			Description:    t.Description,
			Dangerous:      t.Dangerous,
			Source:         src,
			DataReadKinds:  t.DataReadKinds,
			DataWriteKinds: t.DataWriteKinds,
			Grants:         rev[t.Name],
		})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	page, lo, hi := paginate(len(filtered), req.GetPage(), req.GetPageSize())
	return &pb.ListToolsOpResponse{Tools: filtered[lo:hi], Total: int32(len(filtered)), Page: page}, nil
}

// toolGrantsIndex builds the tool→agents reverse index from the grants store,
// when it supports enumeration. Empty otherwise (documented best-effort, A2.3).
func (s *Service) toolGrantsIndex() map[string][]*pb.ToolGrantOp {
	enum, ok := s.grants.(grantsEnumerator)
	if !ok || enum == nil {
		return nil
	}
	out := map[string][]*pb.ToolGrantOp{}
	for agentID, grants := range enum.All() {
		for _, g := range grants {
			out[g.Tool] = append(out[g.Tool], &pb.ToolGrantOp{
				AgentId: agentID,
				Policy:  toToolPolicyOp(g.Policy),
			})
		}
	}
	// Stable agent ordering within each tool for deterministic pages.
	for _, gs := range out {
		sort.Slice(gs, func(i, j int) bool { return gs[i].AgentId < gs[j].AgentId })
	}
	return out
}

// ListSkills returns the registered system skills, filtered + paged (A2.1).
func (s *Service) ListSkills(_ context.Context, req *pb.ListSkillsOpRequest) (*pb.ListSkillsOpResponse, error) {
	if s.skills == nil {
		return &pb.ListSkillsOpResponse{Page: pageOf(req.GetPage())}, nil
	}
	q := strings.ToLower(strings.TrimSpace(req.GetQuery()))
	var filtered []*pb.SkillOp
	for _, sk := range s.skills.All() {
		if q != "" && !strings.Contains(strings.ToLower(sk.Name), q) && !strings.Contains(strings.ToLower(sk.Description), q) {
			continue
		}
		filtered = append(filtered, &pb.SkillOp{
			Name:        sk.Name,
			Description: sk.Description,
			ToolGrants:  sk.ToolGrants,
			ScopeTags:   sk.ScopeTags,
		})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	page, lo, hi := paginate(len(filtered), req.GetPage(), req.GetPageSize())
	return &pb.ListSkillsOpResponse{Skills: filtered[lo:hi], Total: int32(len(filtered)), Page: page}, nil
}

// QueryMemory runs a ScopeSystem fact recall (A2.4/D13). top_k caps the returned
// set; source/session/min_importance are post-filters over document metadata.
func (s *Service) QueryMemory(ctx context.Context, req *pb.QueryMemoryRequest) (*pb.QueryMemoryResponse, error) {
	if s.memory == nil {
		return nil, status.Error(codes.Unimplemented, "operator memory query not configured")
	}
	results, err := s.memory.SearchSystem(ctx, req.GetQuery())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query memory: %v", err)
	}
	resp := &pb.QueryMemoryResponse{}
	topK := int(req.GetTopK())
	for _, r := range results {
		imp := docImportance(r.Document)
		if req.GetMinImportance() > 0 && imp < req.GetMinImportance() {
			continue
		}
		src := metaString(r.Document.Metadata, "source")
		if f := req.GetSource(); f != "" && src != f {
			continue
		}
		if f := req.GetSession(); f != "" && metaString(r.Document.Metadata, "session_id") != f {
			continue
		}
		resp.Results = append(resp.Results, &pb.MemoryOp{
			DocId:      r.Document.ID,
			Summary:    docSummary(r.Document),
			Score:      r.Score,
			Source:     src,
			Importance: imp,
			Tags:       metaStringSlice(r.Document.Metadata, "tags"),
			// ADR-0060 citation surface: the structural breadcrumb and the verbatim
			// chunk body. `summary` is a <=200-char preview — a citation UI needs the
			// passage itself to quote, and the breadcrumb to name where it came from.
			SectionPath: r.Document.SectionPath,
			Text:        r.Document.Text,
		})
		if topK > 0 && len(resp.Results) >= topK {
			break
		}
	}
	return resp, nil
}

// AnswerMemory (ADR-0081) runs the agentic answer lane and returns a grounded,
// [n]-cited answer plus the evidence each marker resolves to. Evidence marker n is
// 1-based over the (filtered) evidence order, matching the synthesizer's labels.
func (s *Service) AnswerMemory(ctx context.Context, req *pb.AnswerMemoryRequest) (*pb.AnswerMemoryResponse, error) {
	if s.answerer == nil {
		return nil, status.Error(codes.Unimplemented, "operator answer lane not configured (memory-answer)")
	}
	// The synthesis LLM call reads its timeout from the remaining ctx deadline. An
	// operator answer runs retrieval + one grounded synthesis, so give it a
	// generous, well-defined budget: an unbounded inbound ctx would otherwise leave
	// the agent's deadline ill-defined and the LLM stream could be cut short
	// (DEADLINE_EXCEEDED). Only shortens if the caller already imposed something
	// tighter.
	const answerBudget = 90 * time.Second
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > answerBudget {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, answerBudget)
		defer cancel()
	}
	st, answer, evidence, err := s.answerer.AnswerSystem(ctx, req.GetQuery())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "answer memory: %v", err)
	}
	resp := &pb.AnswerMemoryResponse{Status: st, Answer: answer}
	topK := int(req.GetTopK())
	marker := int32(0)
	for _, r := range evidence {
		imp := docImportance(r.Document)
		if req.GetMinImportance() > 0 && imp < req.GetMinImportance() {
			continue
		}
		src := metaString(r.Document.Metadata, "source")
		if f := req.GetSource(); f != "" && src != f {
			continue
		}
		if f := req.GetSession(); f != "" && metaString(r.Document.Metadata, "session_id") != f {
			continue
		}
		marker++
		resp.Citations = append(resp.Citations, &pb.MemoryCitation{
			Marker:      marker,
			DocId:       r.Document.ID,
			Text:        r.Document.Text,
			SectionPath: r.Document.SectionPath,
			Source:      src,
			Score:       r.Score,
			Importance:  imp,
			Tags:        metaStringSlice(r.Document.Metadata, "tags"),
		})
		if topK > 0 && int(marker) >= topK {
			break
		}
	}
	return resp, nil
}

// ── mapping / pagination helpers ──────────────────────────────────────────────

// toolSource labels a tool builtin vs mcp:<serverID>, derived from the ADR-0043
// name prefix "mcp:<server>/<tool>" (A2.3) — no SystemTool schema field needed.
func toolSource(name string) string {
	const p = "mcp:"
	if !strings.HasPrefix(name, p) {
		return "builtin"
	}
	rest := name[len(p):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return p + rest[:i]
	}
	return p + rest
}

// sourceMatches supports both an exact source ("builtin", "mcp:srv") and the
// bare "mcp" prefix filter (all MCP tools).
func sourceMatches(src, filter string) bool {
	if filter == "mcp" {
		return strings.HasPrefix(src, "mcp:")
	}
	return src == filter
}

func toToolPolicyOp(p domain.ToolResourcePolicy) *pb.ToolPolicyOp {
	return &pb.ToolPolicyOp{
		AllowedPaths:    p.Filesystem.AllowRoots,
		AllowedUrls:     p.Network.AllowDomains,
		AllowedCommands: p.Command.AllowCommands,
	}
}

func docSummary(d domain.Document) string {
	if d.Summary != "" {
		return d.Summary
	}
	return firstLineMax(d.Text, 200)
}

func docImportance(d domain.Document) float64 {
	if v, ok := d.Metadata["importance"].(float64); ok {
		return v
	}
	return d.ActivationStrength
}

func metaString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func metaStringSlice(m map[string]interface{}, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func firstLineMax(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 && i < max {
		return s[:i]
	}
	if len(s) > max {
		return s[:max]
	}
	return s
}

// pageOf normalizes a 1-based page number (0/negative ⇒ 1).
func pageOf(p int32) int32 {
	if p <= 0 {
		return 1
	}
	return p
}

// paginate returns the normalized 1-based page and the [lo,hi) slice bounds for a
// collection of size n, given the requested page and page_size (defaults applied).
func paginate(n int, page, pageSize int32) (normPage int32, lo, hi int) {
	size := int(pageSize)
	if size <= 0 {
		size = defaultReadPageSize
	}
	normPage = pageOf(page)
	lo = (int(normPage) - 1) * size
	if lo > n {
		lo = n
	}
	hi = lo + size
	if hi > n {
		hi = n
	}
	return normPage, lo, hi
}
