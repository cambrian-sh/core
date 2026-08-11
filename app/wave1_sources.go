package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Contract-0072 read sources. Each adapts a kernel surface the operator plane
// must not import directly, in the same shape as the existing A2 read wiring.

// ── checkpoints ──────────────────────────────────────────────────────────────

// checkpointSource is the storage surface ListSessionCheckpoints needs. Both the
// Postgres session store and the bbolt adapter satisfy it.
type checkpointSource interface {
	ListRunsForSession(sessionID domain.SessionID) ([]domain.Run, error)
	ListCheckpoints(runID domain.RunID) ([]domain.CheckpointMeta, error)
}

type checkpointLister struct{ src checkpointSource }

// CheckpointsForSession fans a session out over its runs.
//
// The fan-out is the whole adapter: checkpoints are stored per RUN, and an
// operator holds a session id. Flattening the result would lose which plan each
// step index belongs to, so every row keeps its run id and the caller groups.
func (c checkpointLister) CheckpointsForSession(sessionID string) ([]domain.CheckpointMeta, error) {
	runs, err := c.src.ListRunsForSession(domain.SessionID(sessionID))
	if err != nil {
		return nil, err
	}
	var out []domain.CheckpointMeta
	for _, r := range runs {
		metas, err := c.src.ListCheckpoints(r.ID)
		if err != nil {
			// One unreadable run must not blank the whole tab: the other runs'
			// recovery points are still true and still useful.
			continue
		}
		out = append(out, metas...)
	}
	return out, nil
}

// ResumableAt reports whether a resume from this point is still valid.
//
// Conservative by construction: it answers true only when the checkpoint's
// context is actually loadable. A checkpoint the kernel would refuse to resume
// from is worse than no checkpoint, because the operator plans a recovery that
// will not happen — so the failure direction here must be "says no when it could
// have said yes", never the reverse.
func (c checkpointLister) ResumableAt(runID string, stepIndex int) bool {
	type loader interface {
		LoadCheckpoint(runID domain.RunID, stepIndex int) (map[string]string, error)
	}
	l, ok := c.src.(loader)
	if !ok {
		return false
	}
	ctxMap, err := l.LoadCheckpoint(domain.RunID(runID), stepIndex)
	return err == nil && ctxMap != nil
}

// ── MCP registry ─────────────────────────────────────────────────────────────

type mcpLister struct {
	// runtime is the LIVE list (contract 0097): boot config plus every runtime
	// save. Reading a boot-time slice here would show a console the state it
	// just changed.
	runtime   *mcpRuntime
	connector *mcp.Connector
	// toolsFor counts the tools currently attributed to a server id.
	toolsFor func(serverID string) int
	// secrets answers "is a token installed, and where from?" without carrying
	// it. nil ⇒ report env-var presence only.
	secrets *storage.BoltConfigStore
}

// MCPConfigured is true whenever the runtime exists — since contract 0097 the
// MCP substrate is always constructed, and "zero servers" is an ordinary state
// the console renders with its add affordance, not an absent subsystem.
func (m mcpLister) MCPConfigured() bool { return m.runtime != nil }

func (m mcpLister) MCPServers() []operator.MCPServerInfo {
	var servers []mcp.ServerConfig
	if m.runtime != nil {
		servers = m.runtime.list()
	}
	out := make([]operator.MCPServerInfo, 0, len(servers))
	for _, s := range servers {
		info := operator.MCPServerInfo{
			Name:               s.ID,
			Transport:          s.Transport,
			Endpoint:           s.Endpoint,
			Args:               s.Args,
			AuthType:           s.AuthType,
			AuthHeader:         s.AuthHeader,
			ClassificationTags: s.DefaultClassificationTags,
		}
		info.TokenConfigured, info.TokenSource = m.tokenFacts(s)
		// Endpoint carries a URL for http/sse and a command for stdio. Split them
		// so the console never renders a shell command as a link.
		if strings.EqualFold(s.Transport, "http") || strings.EqualFold(s.Transport, "sse") {
			info.URL = s.Endpoint
		} else {
			info.Command = strings.TrimSpace(s.Endpoint + " " + strings.Join(s.Args, " "))
		}
		if m.connector != nil {
			_, connected := m.connector.Session(s.ID)
			info.Connected = connected
			if !connected {
				// Distinguishable from a server that is simply idle: an operator
				// reading "connected: false" with no reason cannot tell a
				// misconfiguration from a server that has not been reached yet.
				info.LastError = "no live session — the server has not connected on this kernel"
			}
		}
		if m.toolsFor != nil {
			info.ToolCount = m.toolsFor(s.ID)
		}
		out = append(out, info)
	}
	return out
}

// tokenFacts mirrors generatorRegistry.keyFacts: it reports the source the
// connector will actually use (env first, store second — the mcp.tokenFor
// precedence), never the credential.
func (m mcpLister) tokenFacts(s mcp.ServerConfig) (configured bool, source string) {
	if s.AuthTokenEnv != "" && os.Getenv(s.AuthTokenEnv) != "" {
		return true, "env:" + s.AuthTokenEnv
	}
	if m.secrets != nil && m.secrets.Configured(mcp.TokenSecretName(s.ID)) {
		return true, "store"
	}
	return false, ""
}

// ── embedding ────────────────────────────────────────────────────────────────

type embeddingReporter struct {
	cfg config.EmbedderConfig
	// counter returns the stored vector count, or -1 when it cannot be counted.
	counter func(ctx context.Context) int64
}

func (e embeddingReporter) EmbeddingConfig() (provider, model, endpoint string, dimensions int) {
	return e.cfg.Provider, e.cfg.Model, e.cfg.Endpoint, e.cfg.Dimensions
}

func (e embeddingReporter) VectorCount(ctx context.Context) int64 {
	if e.counter == nil {
		// -1, never 0. "0 vectors" tells an operator the corpus is empty, which
		// is a different and far more alarming claim than "this kernel cannot
		// count them".
		return -1
	}
	return e.counter(ctx)
}

// ── input classification ─────────────────────────────────────────────────────

type inputClassifier struct{ router domain.InputRouter }

// Classify runs the ADR-0031 router and returns its decision WITHOUT acting.
//
// The classification string is passed through verbatim — all five values, never
// collapsed. `ingest` in particular must survive: it WRITES to memory, and a
// console that renders it as a read makes an operator approve a write believing
// they approved a question.
func (c inputClassifier) Classify(ctx context.Context, text, surface string) (operator.ClassifiedInput, error) {
	if c.router == nil {
		return operator.ClassifiedInput{}, fmt.Errorf("no router configured")
	}
	// surface travels as SourceType, never as Intent. Intent is the Layer-0
	// pre-classification hint and would short-circuit the router entirely — the
	// RPC would then echo back the caller's own guess as the kernel's decision,
	// which is exactly the invisible-disagreement failure this plane avoids.
	dec, err := c.router.Resolve(ctx, domain.RouterInput{Body: text, SourceType: surface})
	if err != nil {
		return operator.ClassifiedInput{}, err
	}
	out := operator.ClassifiedInput{
		Classification: string(dec.Type),
		Why:            dec.Why,
		Confidence:     dec.Confidence,
		Question:       dec.ClarificationQuestion,
	}
	for _, o := range dec.ClarificationOptions {
		out.Options = append(out.Options, o.Label)
	}
	return out, nil
}

// ── generator registry ───────────────────────────────────────────────────────

type generatorRegistry struct {
	cfg      config.LLMProviderConfig
	provider *llm.Provider
	// secrets resolves whether a credential is installed and where it comes
	// from, WITHOUT returning it. nil ⇒ report env-var presence only.
	secrets *storage.BoltConfigStore
}

// Generators projects the configured generator list plus live breaker state.
func (g generatorRegistry) Generators() []operator.GeneratorInfo {
	out := make([]operator.GeneratorInfo, 0, len(g.cfg.Generators))
	for _, gen := range g.cfg.Generators {
		info := operator.GeneratorInfo{
			ID:        gen.ID,
			Provider:  gen.Provider,
			Model:     gen.Model,
			Endpoint:  gen.Endpoint,
			TimeoutMs: int64(gen.TimeoutMs),
			IsDefault: gen.ID == g.cfg.Default,
			// Per-generator daily token accounting is not tracked by this kernel.
			// -1, not 0: "0 calls today" is a claim about traffic, and reporting
			// it for an untracked counter would make a busy generator look idle.
			TokensInToday:  -1,
			TokensOutToday: -1,
			CallsToday:     -1,
			// The declared half, so an edit in the console round-trips instead of
			// being reconstructed from defaults on save.
			Capabilities:    gen.Capabilities,
			NativeTools:     gen.NativeTools,
			DisableThinking: gen.DisableThinking,
			APIKeyEnv:       gen.APIKeyEnv,
		}
		if g.provider != nil {
			info.BreakerState = g.provider.BreakerState(gen.ID)
		}
		info.KeyConfigured, info.KeyLastFour, info.KeySource = g.keyFacts(gen)
		out = append(out, info)
	}
	return out
}

// keyFacts answers "is a credential installed, and where from?" without ever
// returning the credential. It mirrors the ADR-0101 D5 precedence the kernel
// actually applies at call time — env var first, store second — so the console
// names the source the kernel will really use rather than the one configured.
func (g generatorRegistry) keyFacts(gen config.GeneratorConfig) (configured bool, lastFour, source string) {
	if gen.APIKeyEnv != "" {
		if v := os.Getenv(gen.APIKeyEnv); v != "" {
			if len(v) >= 4 {
				lastFour = v[len(v)-4:]
			}
			return true, lastFour, "env:" + gen.APIKeyEnv
		}
	}
	if g.secrets != nil {
		name := llm.GeneratorKeySecretName(gen.ID)
		if g.secrets.Configured(name) {
			return true, g.secrets.LastFour(name), "store"
		}
	}
	return false, "", ""
}

// RoleAssignments reports which generator serves each system organ.
//
// Read from the PROVIDER's live map when one exists, not the boot config: a
// SetRoleAssignment hot-applies there (contract 0096), and a console that
// re-reads after saving must see the assignment it just made, not the boot
// state it just changed.
func (g generatorRegistry) RoleAssignments() []operator.RoleAssignment {
	roles := g.cfg.Roles
	if g.provider != nil {
		roles = g.provider.Roles()
	}
	out := make([]operator.RoleAssignment, 0, len(roles))
	// Sorted so the console renders a stable order rather than Go's map order.
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		ra := operator.RoleAssignment{Role: r, GeneratorID: roles[r]}
		if g.provider != nil {
			ra.Resolved = g.provider.KnowsGenerator(roles[r])
		}
		out = append(out, ra)
	}
	return out
}

// TestGenerator makes one real call against the generator's live endpoint.
func (g generatorRegistry) TestGenerator(ctx context.Context, id string) (operator.GeneratorTestResult, error) {
	for _, gen := range g.cfg.Generators {
		if gen.ID != id {
			continue
		}
		r := llm.ProbeGenerator(ctx, gen)
		return operator.GeneratorTestResult{
			OK:             r.OK,
			ModelServed:    r.ModelServed,
			ModelRequested: r.ModelRequested,
			LatencyMs:      r.LatencyMs,
			Error:          r.Err,
			Sample:         r.Sample,
		}, nil
	}
	return operator.GeneratorTestResult{}, fmt.Errorf("no generator with id %q is configured", id)
}

// vectorCounterFor adapts a vector store to the embedding panel's count, when it
// can answer one.
//
// Returns nil — not a zero-returning function — when the store cannot count.
// EmbeddingConfigOp.vector_count then reports -1, because "0 vectors" tells an
// operator their corpus is empty, which is a different and far more alarming
// claim than "this kernel cannot count them".
func vectorCounterFor(v any) func(ctx context.Context) int64 {
	type counter interface {
		CountVectors(ctx context.Context) (int64, error)
	}
	c, ok := v.(counter)
	if !ok || c == nil {
		return nil
	}
	return func(ctx context.Context) int64 {
		n, err := c.CountVectors(ctx)
		if err != nil {
			return -1
		}
		return n
	}
}

// ── operator-authored plans (contract 0074) ──────────────────────────────────

// planSubmitter runs a plan the operator wrote, through the SAME execution path
// a planner-produced plan takes (see network.AuthoredPlanMetadataKey).
type planSubmitter struct {
	sessions func(ctx context.Context, goal string) (string, error)
	execute  func(sessionID, planJSON string)
	known    func(id string) bool
}

func (p planSubmitter) KnownAgent(id string) bool {
	if p.known == nil {
		return true // cannot verify ⇒ do not warn; a false alarm on every pin is worse
	}
	return p.known(id)
}

// SubmitPlan attaches the plan to a session and starts it.
//
// Execution is fire-and-forget, matching SendMessage: the operator watches
// progress on the feed rather than holding a gRPC call open for the length of a
// multi-step plan.
func (p planSubmitter) SubmitPlan(ctx context.Context, sessionID, subject string, steps []domain.Step) (string, string, error) {
	if subject == "" && len(steps) > 0 {
		subject = steps[0].Query
	}
	plan := domain.ExecutionPlan{Steps: steps, Subject: subject}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return "", "", fmt.Errorf("encode authored plan: %w", err)
	}

	if sessionID == "" {
		if p.sessions == nil {
			return "", "", fmt.Errorf("no session manager is wired")
		}
		// The session's goal is the plan's subject: an operator-authored plan has
		// no natural-language goal behind it, and leaving it blank would make the
		// session unidentifiable in every list that shows one.
		sid, serr := p.sessions(ctx, subject)
		if serr != nil {
			return "", "", serr
		}
		sessionID = sid
	}
	if p.execute == nil {
		return "", "", fmt.Errorf("no execution path is wired")
	}
	p.execute(sessionID, string(planJSON))

	// The run id is minted inside the execution path, so it is not available to
	// return here. The session id is what the console needs to follow the plan on
	// the feed; returning a fabricated plan id would be worse than an empty one.
	return sessionID, "", nil
}

// ── propose-only planning (contract 0075) ────────────────────────────────────

// planProposer plans WITHOUT committing: no session, no run, no spend.
//
// It reaches the planner directly rather than through Server.Execute, because
// Execute is the COMMITTING path — it binds a session, persists a run and
// dispatches agents. Routing a "just show me" request through it and trying to
// suppress the side effects afterwards would make the promise depend on every
// future change to that function remembering this caller.
type planProposer struct {
	planner   func(ctx context.Context, goal string) (*domain.ExecutionPlan, error)
	clarifier *awareness.Clarifier
	// tags lists the classification labels a goal might touch, for the access
	// consequence. nil ⇒ the consequence says the deployment is unclassified.
	tags func(ctx context.Context) []string
}

func (p planProposer) Clarify(ctx context.Context, goal string, answers []string) ([]operator.ProposedQuestion, error) {
	if p.clarifier == nil {
		return nil, nil
	}
	qs, err := p.clarifier.Clarify(ctx, goal, answers)
	if err != nil {
		return nil, err
	}
	out := make([]operator.ProposedQuestion, 0, len(qs))
	for _, q := range qs {
		pq := operator.ProposedQuestion{
			Question:              q.Question,
			Kind:                  q.Kind,
			WhyItChangesTheAnswer: q.WhyItChangesTheAnswer,
		}
		for _, o := range q.Options {
			pq.Options = append(pq.Options, operator.ProposedOption{
				Label:         o.Label,
				DocumentCount: o.DocumentCount,
				Detail:        o.Detail,
			})
		}
		out = append(out, pq)
	}
	return out, nil
}

// Propose builds a candidate plan and estimates it.
func (p planProposer) Propose(ctx context.Context, goal string, answers []string) (operator.ProposedPlan, error) {
	if p.planner == nil {
		return operator.ProposedPlan{}, fmt.Errorf("no planner is configured")
	}
	// Answers are appended to the goal rather than held server-side. A
	// half-answered interview kept in kernel memory would leak whenever an
	// operator closed the tab, and this way the request stays stateless.
	input := goal
	if len(answers) > 0 {
		input = goal + "\n\nThe operator answered: " + strings.Join(answers, "; ")
	}

	plan, err := p.planner(ctx, input)
	if err != nil {
		return operator.ProposedPlan{}, err
	}
	return operator.ProposedPlan{
		Steps:           plan.Steps,
		EstimatedTokens: estimatePlanTokens(plan.Steps),
		EstimatedWallMs: estimatePlanWallMs(plan.Steps),
		MaxParallel:     maxParallelWidth(plan.Steps),
	}, nil
}

// AccessConsequence names what a goal would touch, before anything runs.
func (p planProposer) AccessConsequence(ctx context.Context, goal string) string {
	if p.tags == nil {
		return ""
	}
	labels := p.tags(ctx)
	if len(labels) == 0 {
		// Honest rather than reassuring: an unclassified corpus is not a safe one,
		// it is one where policy has nothing to act on.
		return "This deployment has no classification vocabulary, so nothing here is label-restricted — a plan reaches whatever the caller's scope allows."
	}
	return fmt.Sprintf(
		"This deployment classifies memory with %d labels (%s). A plan reads only what your own scope reaches, and anything it writes back into memory inherits the labels of what it read.",
		len(labels), strings.Join(labels, ", "))
}

// estimatePlanTokens is a STRUCTURAL estimate — step count and query size — not a
// prediction. It exists so a proposal card can say "roughly this big" rather than
// nothing; presenting it as an accurate forecast would be worse than silence,
// which is why the wire field is named estimated_*.
func estimatePlanTokens(steps []domain.Step) int64 {
	const perStepOverhead = 1200 // priming + tool menu + context, order-of-magnitude
	var total int64
	for _, st := range steps {
		total += perStepOverhead + int64(len(st.Query))/4
	}
	return total
}

// estimatePlanWallMs estimates along the CRITICAL PATH, not the step count:
// independent steps run in parallel, so summing them would overstate a wide plan
// badly enough to make the number useless.
func estimatePlanWallMs(steps []domain.Step) int64 {
	const perStepMs = 4000
	depth := make([]int, len(steps))
	longest := 0
	for i, st := range steps {
		d := 0
		for _, dep := range st.DependsOn {
			if dep >= 0 && dep < len(depth) && depth[dep] > d {
				d = depth[dep]
			}
		}
		depth[i] = d + 1
		if depth[i] > longest {
			longest = depth[i]
		}
	}
	return int64(longest) * perStepMs
}

// maxParallelWidth is the widest set of steps that could run at once — the same
// depth layering, counted across instead of down.
func maxParallelWidth(steps []domain.Step) int {
	depth := make([]int, len(steps))
	byDepth := map[int]int{}
	widest := 0
	for i, st := range steps {
		d := 0
		for _, dep := range st.DependsOn {
			if dep >= 0 && dep < len(depth) && depth[dep] > d {
				d = depth[dep]
			}
		}
		depth[i] = d + 1
		byDepth[depth[i]]++
		if byDepth[depth[i]] > widest {
			widest = byDepth[depth[i]]
		}
	}
	return widest
}

// documentTagCounter adapts the document lister to the clarifier's counter.
//
// The count comes from the SAME listing the console reads, filtered by tag, so
// "61 documents" on an interview option and "61" on the documents screen cannot
// disagree. nil lister ⇒ nil counter ⇒ options report -1, which the clarifier
// renders as "document count unavailable" rather than as zero.
func documentTagCounter(l memory.DocumentLister) awareness.DocumentCounter {
	if l == nil {
		return nil
	}
	return tagCounter{lister: l}
}

type tagCounter struct{ lister memory.DocumentLister }

func (t tagCounter) CountByTag(ctx context.Context, tag string) int {
	// Limit 1: only totalMatching is wanted, and pulling a full page to count it
	// would read the corpus to answer a number the query already returns.
	_, _, total, err := t.lister.ListDocuments(ctx, memory.DocumentFilter{
		Tags:  []string{tag},
		Limit: 1,
	})
	if err != nil {
		return -1
	}
	return total
}

// ── blast radius (contract 0076) ─────────────────────────────────────────────

// blastRadiusEstimator answers what a scope or grant mutation would do to agents
// and in-flight plans.
//
// Distinct from the policy blast radius the scope screens already compute from
// the document listing: that answers "how many documents does this rule touch?",
// this answers "whose reach changes, and which running plans must be
// re-evaluated?".
type blastRadiusEstimator struct {
	// agents lists registered agents, for the reach comparison.
	agents func(ctx context.Context) []domain.AgentDefinition
	// scopeOf renders an agent's CURRENT effective scope. nil ⇒ the preview is
	// marked incomplete rather than reporting "unchanged" for everything, which
	// would understate the radius.
	scopeOf func(ctx context.Context, agentID string) string
	// inFlight lists running plans. nil ⇒ incomplete, same reasoning.
	inFlight func() []operator.PlanImpact
}

// blastRadiusTTL is how long a preview stays meaningful.
//
// Short on purpose: a blast radius is a statement about live state, and agents
// register and plans finish continuously. A stale preview UNDERSTATES, which is
// the direction that misleads.
const blastRadiusTTL = 30 * time.Second

func (b blastRadiusEstimator) EstimateBlastRadius(ctx context.Context, m operator.BlastRadiusMutation) (operator.BlastRadius, error) {
	out := operator.BlastRadius{CacheTTL: blastRadiusTTL, Complete: true}

	if b.agents == nil || b.scopeOf == nil {
		// Reported as INCOMPLETE rather than as an empty (and therefore reassuring)
		// radius. "Nothing is affected" and "I could not look" must not render the
		// same when the answer gates a boundary change.
		out.Complete = false
		out.IncompleteReason = "this kernel cannot resolve agent scopes, so the effect on agents is unknown"
		return out, nil
	}

	for _, a := range b.agents(ctx) {
		before := b.scopeOf(ctx, a.ID)
		after := b.projectedScope(m, a, before)
		if after == before {
			continue // unchanged agents are omitted; the list is the radius
		}
		out.Agents = append(out.Agents, operator.AgentImpact{
			AgentID:   a.ID,
			Before:    before,
			After:     after,
			Direction: scopeDirection(before, after),
		})
	}

	if b.inFlight == nil {
		out.Complete = false
		out.IncompleteReason = "this kernel does not track in-flight plans, so running work may be affected without appearing here"
		return out, nil
	}
	// A plan is affected when it is running on an agent whose reach changed: its
	// remaining steps were planned against the OLD boundary.
	changed := map[string]bool{}
	for _, a := range out.Agents {
		changed[a.AgentID] = true
	}
	for _, p := range b.inFlight() {
		if p.ReEvaluationRequired || changed[p.PlanID] {
			out.Plans = append(out.Plans, p)
		}
	}
	return out, nil
}

// projectedScope renders what the agent's scope WOULD read as after the mutation.
//
// Only mutations that name this agent change it. A tag_memory mutation changes a
// document's labels rather than an agent's terms, so it is reported as no change
// HERE — its radius is the document one, which the scope screens already show,
// and duplicating it would double-count the same effect on two screens.
func (b blastRadiusEstimator) projectedScope(m operator.BlastRadiusMutation, a domain.AgentDefinition, before string) string {
	if m.TargetID != a.ID {
		return before
	}
	switch m.Kind {
	case operator.MutationSetScope:
		return renderScopeTerms(m.Required, m.AnyOf, m.Forbidden)
	case operator.MutationSetWriteTags:
		return "writes: " + strings.Join(m.Tags, ", ")
	case operator.MutationSetToolGrant:
		return before + " (+tool grant " + strings.Join(m.Tags, ", ") + ")"
	}
	return before
}

func renderScopeTerms(required, anyOf, forbidden []string) string {
	var parts []string
	if len(required) > 0 {
		parts = append(parts, "requires "+strings.Join(required, "+"))
	}
	if len(anyOf) > 0 {
		parts = append(parts, "any of "+strings.Join(anyOf, "|"))
	}
	if len(forbidden) > 0 {
		parts = append(parts, "never "+strings.Join(forbidden, ", "))
	}
	if len(parts) == 0 {
		return "unrestricted"
	}
	return strings.Join(parts, "; ")
}

// scopeDirection classifies a change as widened, narrowed or unchanged.
//
// It errs toward WIDENED when it cannot tell. Narrowing an agent breaks a task
// and someone notices within minutes; widening one breaks a boundary and nobody
// does — so an ambiguous change must be flagged as the consequential one.
func scopeDirection(before, after string) string {
	switch {
	case before == after:
		return operator.DirectionUnchanged
	case after == "unrestricted" && before != "unrestricted":
		return operator.DirectionWidened
	case before == "unrestricted" && after != "unrestricted":
		return operator.DirectionNarrowed
	case len(after) < len(before):
		// Fewer terms is usually fewer restrictions, i.e. wider reach.
		return operator.DirectionWidened
	case len(after) > len(before):
		return operator.DirectionNarrowed
	default:
		return operator.DirectionWidened
	}
}

// agentScopeRenderer returns a function rendering an agent's CURRENT effective
// scope, or nil when this build cannot resolve one.
//
// nil is meaningful: the blast-radius estimator reports INCOMPLETE rather than
// claiming every agent is unaffected, because an unscoped deployment genuinely
// cannot say what a scope change would do.
func (k *Kernel) agentScopeRenderer() func(context.Context, string) string {
	if k.Authorizer == nil {
		return nil
	}
	return func(ctx context.Context, agentID string) string {
		// ReadFilter is the authoritative shape of an agent's reach — the same
		// predicate the read chokepoint pushes into the store. Rendering anything
		// else here would make the preview describe a boundary the kernel does not
		// actually enforce.
		pred, _ := k.Authorizer.ReadFilter(ctx,
			domain.AgentPrincipal(agentID), domain.SurfaceRef{Kind: "agent"})
		return renderPredicate(pred)
	}
}

// renderPredicate turns a read predicate into the sentence the preview compares.
//
// A NIL predicate means "no read is authorised at all", which is the narrowest
// possible reach — and is emphatically NOT the same as unrestricted. Collapsing
// the two would invert the direction of every comparison involving a fully
// denied agent.
func renderPredicate(p *domain.TagPredicate) string {
	if p == nil {
		return "no read authorised"
	}
	if p.Bypass {
		return "unrestricted"
	}
	var parts []string
	if len(p.RequiredTags) > 0 {
		parts = append(parts, "requires "+strings.Join(p.RequiredTags, "+"))
	}
	for _, clause := range p.AnyOfClauses {
		parts = append(parts, "any of "+strings.Join(clause, "|"))
	}
	if len(p.ForbiddenTags) > 0 {
		parts = append(parts, "never "+strings.Join(p.ForbiddenTags, ", "))
	}
	if len(parts) == 0 {
		return "unrestricted"
	}
	return strings.Join(parts, "; ")
}
