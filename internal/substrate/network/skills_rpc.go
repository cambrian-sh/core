package network

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/access"
)

// skillQueryFromMetadata reads the ADR-0046 relevance query (x-skill-query).
func skillQueryFromMetadata(ctx context.Context) string { return mdValue(ctx, "x-skill-query") }

// skillKFromMetadata reads the ADR-0046 menu size (x-skill-k); default 3.
func skillKFromMetadata(ctx context.Context) int {
	if v := mdValue(ctx, "x-skill-k"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			return k
		}
	}
	return 3
}

// skillFullFromMetadata reads the ADR-0046 Tier-2 flag (x-skill-full). "true" ⇒
// serve full instructions + tool grants (the use_skill path); else Tier-1.
func skillFullFromMetadata(ctx context.Context) bool { return mdValue(ctx, "x-skill-full") == "true" }

// skillNamesFromMetadata reads the ADR-0046 target skill names (x-skill-names,
// comma-separated) — the use_skill Tier-2 fetch.
func skillNamesFromMetadata(ctx context.Context) []string {
	v := mdValue(ctx, "x-skill-names")
	if v == "" {
		return nil
	}
	var names []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// SkillAccess is per-agent skill access (decisions/skill-access-grants.md) as the
// agent plane uses it. *access.Service satisfies it. nil = no agent is bounded
// (every agent keeps scope-gated access, the pre-grant behaviour).
type SkillAccess interface {
	Effective(ctx context.Context, agentID string) (set map[string]string, bound bool)
	Request(ctx context.Context, in access.RequestInput) access.Outcome
}

// skillRationaleFromMetadata reads the optional use_skill request rationale
// (x-skill-rationale), shown to the operator who decides the request.
func skillRationaleFromMetadata(ctx context.Context) string { return mdValue(ctx, "x-skill-rationale") }

// servedSkill is one skill ListSkills answers with: the skill, its access for the
// caller ("" = unbounded agent), and whether its Tier-2 body may be disclosed.
type servedSkill struct {
	sk     domain.Skill
	access string
	detail string
	body   bool
}

// skillInSet reports whether an access value is inside the effective set ("" =
// the agent is unbounded, so everything visible is).
func skillInSet(a string) bool {
	return a == "" || a == domain.SkillAccessProfile || a == domain.SkillAccessGranted
}

// ListSkills returns the system skills the calling agent (x-agent-id) may load
// (ADR-0046 D2/D4). The principal's effective scope gates visibility (D9):
//   - names present  → Tier-2 fetch for those named, scope-gated (use_skill)
//   - query present  → relevance-ranked Tier-1 menu within scope (push / find_skills)
//   - neither        → all scope-permitted skills, Tier-1 (full menu)
//
// Per-agent skill access (decisions/skill-access-grants.md) bounds a PROFILE
// agent further: its effective set is its profile's skills plus its grants, and
// a skill body is never served outside it (fail closed). The full menu lists only
// the set; find_skills may still DISCOVER other visible skills (Tier-1, marked
// "requestable"); a use_skill on one opens a skill_access request on the operator
// approval hub and waits (execution.skills.access_request_timeout_seconds): an
// approval becomes a durable grant and the body is served in this same call; no
// decision answers "pending", a denial "denied" — both without a body.
//
// Fail-closed: a missing registry, or an unknown principal with no resolvable
// scope, yields an empty menu. Agent-local skills are NOT served here.
func (s *Server) ListSkills(ctx context.Context, _ *pb.ListSkillsRequest) (*pb.ListSkillsResponse, error) {
	if s.SkillRegistry == nil {
		return &pb.ListSkillsResponse{}, nil
	}
	agentID := agentIDFromMetadata(ctx)

	// Resolve the agent's read predicate. A nil predicate means the decision point
	// could not resolve the principal → empty menu (fail-closed in the plugin;
	// never reached in OSS, where every principal resolves).
	// The turn's surface (ADR-0085 D7, re-stamped by withTurnSurface): a skill
	// menu answers to a surface-linked policy as recall and tool calls do.
	ctx = s.withTurnSurface(ctx, s.resolveCallerSession(ctx))
	eff, _ := s.authorizer().ReadFilter(ctx, domain.AgentPrincipal(agentID),
		domain.SurfaceFromContextOr(ctx, domain.SurfaceRef{Kind: domain.SurfaceAgent}))
	if eff == nil {
		return &pb.ListSkillsResponse{}, nil
	}

	// The caller's effective skill set; bound=false (no profile, or no access
	// service) leaves the agent unbounded.
	var set map[string]string
	bound := false
	if s.SkillAccess != nil {
		set, bound = s.SkillAccess.Effective(ctx, agentID)
	}
	// ADR-0145 D12: a composed worker's skill index is its composition. A skill
	// outside it reads as denied, is never served and never opens an access
	// request: the model may narrow what the kernel composed, never widen it.
	compIn, composedBound := s.composedSkillAccess(ctx, agentID)
	accessOf := func(name string) string {
		if composedBound {
			if compIn(name) {
				return domain.SkillAccessProfile
			}
			return SkillAccessComposedOut
		}
		if !bound {
			return ""
		}
		if a, ok := set[name]; ok {
			return a
		}
		return domain.SkillAccessRequestable
	}

	full := skillFullFromMetadata(ctx)
	var skills []servedSkill
	switch {
	case len(skillNamesFromMetadata(ctx)) > 0:
		// use_skill Tier-2 fetch: only the named skills the agent's scope permits.
		// ADR-0046 D6: on the full=true load, a system skill confers its bundled
		// grants run-scoped (keyed by the session token) — the operator-authorized
		// widening. Dangerous tools still require approval at execute time.
		session := mdValue(ctx, "x-session-token")
		for _, n := range skillNamesFromMetadata(ctx) {
			sk, ok := s.SkillRegistry.Get(n)
			if !ok || !domain.SkillVisible(eff, sk) {
				continue
			}
			acc, detail := accessOf(n), ""
			if composedBound && !skillInSet(acc) {
				detail = "outside the skills the kernel composed for this step"
			}
			if !skillInSet(acc) && full && !composedBound {
				// Outside the effective set: ask the operator. Only a Tier-2 load
				// asks; a Tier-1 lookup just reports the skill as requestable.
				out := s.SkillAccess.Request(ctx, access.RequestInput{
					AgentID: agentID, Skill: n, RunID: s.skillRunID(ctx), Rationale: skillRationaleFromMetadata(ctx),
				})
				acc, detail = out.Access, out.Detail
			}
			if !skillInSet(acc) {
				skills = append(skills, servedSkill{sk: sk, access: acc, detail: detail})
				continue
			}
			// ADR-0085 D4: the skill's grants are INTERSECTED with what policy
			// permits this principal, never unioned. The skill still loads; the
			// tools it may not confer are clipped and reported, so the agent is
			// told what it actually has rather than what the skill wished for.
			if full && session != "" && s.ToolExecutor != nil {
				granted, clipped := s.ToolExecutor.ConferSkillGrants(ctx, session, agentID, sk.ToolGrants)
				sk.ToolGrants = granted
				for _, c := range clipped {
					slog.WarnContext(ctx, "ADR-0085 D4: skill grant clipped",
						slog.String("event", "skill_grant_clipped"),
						slog.String("skill", sk.Name),
						slog.String("agent_id", agentID),
						slog.String("tool", c.Resource.ID),
						slog.String("detail", c.Detail))
				}
			}
			// ADR-0139 D13: a full load IS a use (the curator's input).
			if full {
				s.recordSkillUse(ctx, sk.Name)
			}
			skills = append(skills, servedSkill{sk: sk, access: acc, detail: detail, body: full})
		}
	case skillQueryFromMetadata(ctx) != "" && s.SkillRetriever != nil:
		// push / find_skills: relevance-ranked within scope (store enforces scope).
		// Discovery is Tier-1, so a bounded agent may see requestable skills here.
		ranked, err := s.SkillRetriever.Rank(ctx, skillQueryFromMetadata(ctx), eff, skillKFromMetadata(ctx))
		if err == nil {
			for _, n := range ranked {
				if sk, ok := s.SkillRegistry.Get(n); ok {
					acc := accessOf(n)
					if composedBound && !skillInSet(acc) {
						continue // discovery does not reach past a composition
					}
					skills = append(skills, servedSkill{sk: sk, access: acc, body: full && skillInSet(acc)})
				}
			}
		}
	default:
		// full menu: every scope-permitted system skill — for a bounded agent, only
		// its effective set.
		for _, sk := range s.SkillRegistry.All() {
			if !domain.SkillVisible(eff, sk) {
				continue
			}
			acc := accessOf(sk.Name)
			if !skillInSet(acc) {
				continue
			}
			skills = append(skills, servedSkill{sk: sk, access: acc, body: full})
		}
	}

	out := make([]*pb.SkillDescriptor, 0, len(skills))
	for _, sv := range skills {
		desc, instr, grants := domain.SkillDisclosure(sv.sk, sv.body)
		out = append(out, &pb.SkillDescriptor{
			Name:         sv.sk.Name,
			Description:  desc,
			Instructions: instr,
			ToolGrants:   grants,
			Access:       sv.access,
			AccessDetail: sv.detail,
		})
	}
	return &pb.ListSkillsResponse{Skills: out}, nil
}
