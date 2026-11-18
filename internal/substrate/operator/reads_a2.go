package operator

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/llmusage"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
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
// (tool → which agents hold it). Satisfied by *domain.LayeredGrantsStore and
// *domain.InMemoryGrantsStore (AllGrants). When s.grants does not implement it,
// ToolOp.grants is left empty (best-effort); a store error fails ListTools.
type grantsEnumerator interface {
	AllGrants(ctx context.Context) (map[string][]domain.ToolGrant, error)
}

const defaultReadPageSize = 50

// ListDocuments enumerates ingested documents by row (read RPC; any authenticated
// role, no command_id).
//
// The counterpart to QueryMemory, not a variant of it. Search answers "find the
// document that says X"; this answers "which of my documents have no labels?" —
// a question with no query text, because the operator does not yet know what those
// documents say. Access policy acts only on labels, so an unlabelled document is
// not denied, it is invisible to the policy model, and until this existed the
// console could not enumerate the invisible set at all.
func (s *Service) ListDocuments(ctx context.Context, req *pb.ListDocumentsOpRequest) (*pb.ListDocumentsOpResponse, error) {
	if req.GetUploadName() != "" {
		return s.matchUpload(ctx, req) // contract 0100, ADR-0138 B1.3
	}
	if s.documents == nil {
		return nil, status.Error(codes.Unimplemented, "document listing not configured")
	}
	page, next, total, err := s.documents.ListDocuments(ctx, memory.DocumentFilter{
		Limit:          int(req.GetLimit()),
		Cursor:         req.GetCursor(),
		UnlabelledOnly: req.GetUnlabelledOnly(),
		IDPrefix:       req.GetIdPrefix(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list documents: %v", err)
	}
	out := make([]*pb.DocumentSummaryOp, 0, len(page))
	for _, d := range page {
		out = append(out, &pb.DocumentSummaryOp{
			Id:              d.ID,
			Title:           d.Title,
			SourceType:      d.SourceType,
			Tags:            d.Tags,
			ChunkCount:      int32(d.ChunkCount),
			CreatedAtUnixMs: d.CreatedAt.UnixMilli(),
		})
	}
	return &pb.ListDocumentsOpResponse{
		Documents:     out,
		NextCursor:    next,
		TotalMatching: int32(total),
	}, nil
}

// ListTools returns the whole tool catalog the operator governs, with per-tool
// grant reverse-index and MCP-vs-builtin source labelling (A2.3). Paged.
func (s *Service) ListTools(ctx context.Context, req *pb.ListToolsOpRequest) (*pb.ListToolsOpResponse, error) {
	if s.tools == nil {
		return &pb.ListToolsOpResponse{Page: pageOf(req.GetPage())}, nil
	}
	rev, err := s.toolGrantsIndex(ctx) // tool name → []ToolGrantOp
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list tool grants: %v", err)
	}

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
			// ADR-0085/0086: what the tool is about, and what it does. Surfacing
			// effects_inferred is what makes the strict-mode migration actionable —
			// otherwise an operator has no way to know which manifests still need
			// classifying before flipping the flag.
			ClassificationTags: t.ClassificationTags,
			Effects:            effectNames(t.Effects),
			EffectsInferred:    t.EffectsInferred,
			Grants:             rev[t.Name],
			// Contract 0074: the detail fields 0057 removed. SchemaJson is the
			// exact menu entry the model sees, so a tool pane can answer "why did
			// the agent call this with those arguments?"; the resource-arg lists
			// say WHICH arguments a resource policy actually binds, without which
			// a grant's policy reads as applying to the whole tool.
			SchemaJson:  string(t.Schema),
			PathArgs:    t.PathArgs,
			UrlArgs:     t.URLArgs,
			CommandArgs: t.CommandArgs,
		})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	page, lo, hi := paginate(len(filtered), req.GetPage(), req.GetPageSize())
	return &pb.ListToolsOpResponse{Tools: filtered[lo:hi], Total: int32(len(filtered)), Page: page}, nil
}

// toolGrantsIndex builds the tool→agents reverse index from the grants store,
// when it supports enumeration. Empty otherwise (documented best-effort, A2.3).
func (s *Service) toolGrantsIndex(ctx context.Context) (map[string][]*pb.ToolGrantOp, error) {
	enum, ok := s.grants.(grantsEnumerator)
	if !ok || enum == nil {
		return nil, nil
	}
	all, err := enum.AllGrants(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]*pb.ToolGrantOp{}
	for agentID, grants := range all {
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
	return out, nil
}

// ListSkills returns the registered system skills, filtered + paged (A2.1).
//
// ADR-0139 D13: with the skill lifecycle wired, each skill carries its lifecycle
// fields (version, source, state, lock, usage) and the D13 filters apply;
// archived skills, which the catalog no longer serves, are listed from their
// stored content when the request asks for them. One listing, not a second one.
func (s *Service) ListSkills(ctx context.Context, req *pb.ListSkillsOpRequest) (*pb.ListSkillsOpResponse, error) {
	if s.skills == nil {
		return &pb.ListSkillsOpResponse{Page: pageOf(req.GetPage())}, nil
	}
	var records map[string]lifecycle.Skill
	if s.skillLifecycle != nil {
		recs, err := s.skillLifecycle.Skills(ctx)
		if err != nil {
			return nil, lifecycleErr(err)
		}
		records = make(map[string]lifecycle.Skill, len(recs))
		for _, r := range recs {
			records[r.Name] = r
		}
	}
	q := strings.ToLower(strings.TrimSpace(req.GetQuery()))
	var filtered []*pb.SkillOp
	keep := func(sk domain.Skill, sourcePath string) {
		if q != "" && !strings.Contains(strings.ToLower(sk.Name), q) && !strings.Contains(strings.ToLower(sk.Description), q) {
			return
		}
		op := &pb.SkillOp{
			Name:        sk.Name,
			Description: sk.Description,
			ToolGrants:  sk.ToolGrants,
			ScopeTags:   sk.ScopeTags,
			// Contract 0074: a skill IS its instructions. Without the body a detail
			// pane can list a skill's name and grants and never show what it
			// actually tells an agent to do.
			Body:       sk.Instructions,
			SourcePath: sourcePath,
		}
		if records != nil {
			if r, ok := records[sk.Name]; ok {
				skillOpLifecycle(op, r)
			} else {
				// Served but not lifecycle-managed (registered over the wire).
				op.LifecycleState = domain.SkillStateActive
			}
			if !skillListFilter(req, op) {
				return
			}
		}
		filtered = append(filtered, op)
	}
	served := map[string]bool{}
	for _, sk := range s.skills.All() {
		served[sk.Name] = true
		keep(sk, sk.SourcePath)
	}
	for name, r := range records {
		if !served[name] && r.State == domain.SkillStateArchived {
			c := r.Content
			c.Name = name
			keep(c, "")
		}
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
	// ADR-0134 D4 (contract 0099): event-time parameters, checked before any work.
	tq, err := opEventTimeQuery(req.GetReferenceTime(), req.GetEventAfter(), req.GetEventBefore())
	if err != nil {
		return nil, err
	}
	ctx = domain.WithEventTimeQuery(ctx, tq)
	// ENG-534: report the kernel's own LLM calls for this request in the
	// cambrian-llm-usage trailer, as the agent plane does (llmusage.Trailer).
	// Deferred, so every return path — errors included — reports what was spent.
	ctx, sendLLMUsage := llmusage.WithTally(ctx)
	defer sendLLMUsage()
	results, err := s.memory.SearchSystem(ctx, req.GetQuery())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query memory: %v", err)
	}
	resp := &pb.QueryMemoryResponse{}
	// INV-3: an empty operator result must be attributable. The operator reads at
	// ScopeSystem, so policy normally plays no part and the note stays absent —
	// but if it ever does (a misconfigured bypass, an unsatisfiable clamp), the
	// operator learns it here rather than concluding the corpus is empty.
	if s.policy != nil && len(results) == 0 {
		resp.PolicyNote = policyNote(s.policy.ExplainAccess(ctx, domain.AccessRequest{
			Principal: domain.SystemPrincipal,
			Surface:   domain.SurfaceRef{Kind: domain.SurfaceOperator},
			Resource:  domain.ResourceRef{Kind: domain.KindMemory},
		}))
	}
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
		if f := req.GetSession(); f != "" && domain.DocSessionID(r.Document.Metadata) != f {
			continue
		}
		op := &pb.MemoryOp{
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
		}
		setHitTime(r.Document, &op.EventTime, &op.EventTimeEnd, &op.TimeSource, &op.TimePrecision)
		setFactAnnotations(r.Document.Metadata, op)
		resp.Results = append(resp.Results, op)
		if topK > 0 && len(resp.Results) >= topK {
			break
		}
	}
	return resp, nil
}

// AnswerMemory (ADR-0081) runs the agentic answer lane and returns a grounded,
// [n]-cited answer plus the evidence each marker resolves to. source / session /
// min_importance and the top_k pool cap narrow the evidence BEFORE synthesis
// (memory.WithAnswerEvidence, ENG-223), so citation marker n is evidence[n-1] —
// exactly the synthesizer's [n] label — and the answer never cites a filtered row.
func (s *Service) AnswerMemory(ctx context.Context, req *pb.AnswerMemoryRequest) (*pb.AnswerMemoryResponse, error) {
	if s.answerer == nil {
		return nil, status.Error(codes.Unimplemented, "operator answer lane not configured (memory-answer)")
	}
	// ADR-0134 D4 (contract 0099): the event-time parameters ride the context into
	// every search the agentic loop issues for this answer.
	tq, err := opEventTimeQuery(req.GetReferenceTime(), req.GetEventAfter(), req.GetEventBefore())
	if err != nil {
		return nil, err
	}
	ctx = domain.WithEventTimeQuery(ctx, tq)
	// ENG-534: report the kernel's own LLM calls for this request in the
	// cambrian-llm-usage trailer, as the agent plane does (llmusage.Trailer).
	// Deferred, so every return path — errors included — reports what was spent.
	ctx, sendLLMUsage := llmusage.WithTally(ctx)
	defer sendLLMUsage()
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
	ctx = memory.WithAnswerEvidence(ctx, memory.AnswerEvidence{Keep: answerPoolFilter(req), Max: int(req.GetTopK())})
	st, answer, evidence, err := s.answerer.AnswerSystem(ctx, req.GetQuery())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "answer memory: %v", err)
	}
	resp := &pb.AnswerMemoryResponse{Status: st, Answer: answer}
	// INV-3: an abstention that access policy CAUSED must say so. "The corpus does
	// not answer that" and "you are not permitted to see the answer" are very
	// different statements, and without this they render identically.
	if s.policy != nil && len(evidence) == 0 {
		resp.PolicyNote = policyNote(s.policy.ExplainAccess(ctx, domain.AccessRequest{
			Principal: domain.SystemPrincipal,
			Surface:   domain.SurfaceRef{Kind: domain.SurfaceOperator},
			Resource:  domain.ResourceRef{Kind: domain.KindMemory},
		}))
	}
	// The pool was filtered and capped before synthesis, so every returned row is
	// a citation and its 1-based position is the synthesizer's [n] label.
	for i, r := range evidence {
		c := &pb.MemoryCitation{
			Marker:      int32(i + 1),
			DocId:       r.Document.ID,
			Text:        r.Document.Text,
			SectionPath: r.Document.SectionPath,
			Source:      metaString(r.Document.Metadata, "source"),
			Score:       r.Score,
			Importance:  docImportance(r.Document),
			Tags:        metaStringSlice(r.Document.Metadata, "tags"),
		}
		setHitTime(r.Document, &c.EventTime, &c.EventTimeEnd, &c.TimeSource, &c.TimePrecision)
		resp.Citations = append(resp.Citations, c)
	}
	return resp, nil
}

// answerPoolFilter is AnswerMemory's source / session / min_importance filter
// over the evidence pool (nil when the request sets none). The session test is
// domain.DocSessionID, the same one QueryMemory uses.
func answerPoolFilter(req *pb.AnswerMemoryRequest) func(domain.SearchResult) bool {
	minImp, src, sess := req.GetMinImportance(), req.GetSource(), req.GetSession()
	if minImp <= 0 && src == "" && sess == "" {
		return nil
	}
	return func(r domain.SearchResult) bool {
		if minImp > 0 && docImportance(r.Document) < minImp {
			return false
		}
		if src != "" && metaString(r.Document.Metadata, "source") != src {
			return false
		}
		return sess == "" || domain.DocSessionID(r.Document.Metadata) == sess
	}
}

// ── mapping / pagination helpers ──────────────────────────────────────────────

// opEventTimeQuery converts and checks the wire event-time query (ADR-0134 D4)
// through domain.EventTimeQuery.Validate, the one check both planes share: an
// inverted or empty window is INVALID_ARGUMENT. The window is applied in SQL beside
// the read scope (ADR-0134 P2).
func opEventTimeQuery(ref, after, before *timestamppb.Timestamp) (domain.EventTimeQuery, error) {
	var q domain.EventTimeQuery
	for _, f := range []struct {
		name string
		ts   *timestamppb.Timestamp
		dst  *time.Time
	}{{"reference_time", ref, &q.ReferenceTime}, {"event_after", after, &q.EventAfter}, {"event_before", before, &q.EventBefore}} {
		t, err := opWireTime(f.name, f.ts)
		if err != nil {
			return domain.EventTimeQuery{}, err
		}
		if t != nil {
			*f.dst = *t
		}
	}
	if err := q.Validate(); err != nil {
		return domain.EventTimeQuery{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return q, nil
}

// setHitTime fills a hit's ADR-0134 event-time fields from domain.HitEventTime, the
// one reader every plane shares (MemoryOp and MemoryCitation carry the same four).
func setHitTime(d domain.Document, start, end **timestamppb.Timestamp, source, precision *string) {
	ht := domain.HitEventTime(d)
	if !ht.EventTime.IsZero() {
		*start = timestamppb.New(ht.EventTime.UTC())
	}
	if !ht.EventTimeEnd.IsZero() {
		*end = timestamppb.New(ht.EventTimeEnd.UTC())
	}
	*source = string(ht.Source)
	*precision = string(ht.Precision)
}

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

// setFactAnnotations copies the fact lane's per-hit annotations (ADR-0136,
// memory.annotateFactHits: domain.MetaFact*) onto the operator MemoryOp (contract
// 0104), so the console can say WHY a fact-injected hit is there
// (T-20261103-operator-querymemory-drops-fact-match-fact). A hit the lane did not
// touch carries none of the keys and stays unset.
func setFactAnnotations(meta map[string]interface{}, op *pb.MemoryOp) {
	op.FactMatches = metaStringSlice(meta, domain.MetaFactMatches)
	switch v := meta[domain.MetaFactMatchScore].(type) {
	case float64:
		op.FactMatch = v
	case float32:
		op.FactMatch = float64(v)
	}
	if b, ok := meta[domain.MetaFactCurrent].(bool); ok {
		op.FactCurrent = b
	}
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

// effectNames renders a tool's effect classes for the wire.
func effectNames(es []domain.ToolEffect) []string {
	if len(es) == 0 {
		return nil
	}
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = string(e)
	}
	return out
}

// GetDocument fetches one document by id, body included.
//
// The primitive the memory lane was missing. ListDocuments is keyed but returns no
// body; QueryMemory returns the body but is ranked. Resolving an id therefore meant
// searching semantically for an opaque token — the worst case for an embedder — and
// hoping it ranked. A larger top-k does not fix that, because there is no question
// to rank against.
//
// Reads at operator scope, like the rest of this plane. `found=false` is a normal
// answer, not an error: a console resolving a citation needs a dangling reference
// and a blank document to stay distinguishable, and a gRPC NotFound would make
// every unresolvable citation look like a transport failure.
func (s *Service) GetDocument(ctx context.Context, req *pb.GetDocumentOpRequest) (*pb.GetDocumentOpResponse, error) {
	if s.docGetter == nil {
		return nil, status.Error(codes.Unimplemented, "document read not configured")
	}
	id := req.GetId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	// The operator plane reads at ScopeSystem (ADR-0047 D13), like its other reads.
	// Stamped EXPLICITLY: a by-id BODY read that simply forgot a scope looks
	// identical to one that deliberately reads as system, and only one is correct.
	doc, err := s.docGetter.GetDocument(
		domain.WithScope(ctx, domain.ScopeSystem), domain.SystemPrincipal, id)
	if err != nil {
		if errors.Is(err, memory.ErrDocumentNotFound) {
			return &pb.GetDocumentOpResponse{Found: false, Id: id}, nil
		}
		return nil, status.Errorf(codes.Internal, "get document: %v", err)
	}
	resp := &pb.GetDocumentOpResponse{
		Found:        true,
		Id:           doc.ID,
		Text:         doc.Text,
		Summary:      doc.Summary,
		DocumentType: doc.DocumentType,
		Source:       metaString(doc.Metadata, "source"),
	}
	if tags, ok := doc.Metadata["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				resp.Tags = append(resp.Tags, s)
			}
		}
	}
	return resp, nil
}
