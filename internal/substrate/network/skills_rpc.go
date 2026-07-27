package network

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
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

// ListSkills returns the system skills the calling agent (x-agent-id) may load
// (ADR-0046 D2/D4). The principal's effective scope gates visibility (D9):
//   - names present  → Tier-2 fetch for those named, scope-gated (use_skill)
//   - query present  → relevance-ranked Tier-1 menu within scope (push / find_skills)
//   - neither        → all scope-permitted skills, Tier-1 (full menu)
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
	eff, _ := s.authorizer().ReadFilter(ctx, domain.AgentPrincipal(agentID), domain.SurfaceRef{Kind: domain.SurfaceAgent})
	if eff == nil {
		return &pb.ListSkillsResponse{}, nil
	}

	full := skillFullFromMetadata(ctx)
	var skills []domain.Skill
	switch {
	case len(skillNamesFromMetadata(ctx)) > 0:
		// use_skill Tier-2 fetch: only the named skills the agent's scope permits.
		// ADR-0046 D6: on the full=true load, a system skill confers its bundled
		// grants run-scoped (keyed by the session token) — the operator-authorized
		// widening. Dangerous tools still require approval at execute time.
		session := mdValue(ctx, "x-session-token")
		for _, n := range skillNamesFromMetadata(ctx) {
			if sk, ok := s.SkillRegistry.Get(n); ok && domain.SkillVisible(eff, sk) {
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
				skills = append(skills, sk)
			}
		}
	case skillQueryFromMetadata(ctx) != "" && s.SkillRetriever != nil:
		// push / find_skills: relevance-ranked within scope (store enforces scope).
		ranked, err := s.SkillRetriever.Rank(ctx, skillQueryFromMetadata(ctx), eff, skillKFromMetadata(ctx))
		if err == nil {
			for _, n := range ranked {
				if sk, ok := s.SkillRegistry.Get(n); ok {
					skills = append(skills, sk)
				}
			}
		}
	default:
		// full menu: every scope-permitted system skill.
		for _, sk := range s.SkillRegistry.All() {
			if domain.SkillVisible(eff, sk) {
				skills = append(skills, sk)
			}
		}
	}

	out := make([]*pb.SkillDescriptor, 0, len(skills))
	for _, sk := range skills {
		desc, instr, grants := domain.SkillDisclosure(sk, full)
		out = append(out, &pb.SkillDescriptor{
			Name:         sk.Name,
			Description:  desc,
			Instructions: instr,
			ToolGrants:   grants,
		})
	}
	return &pb.ListSkillsResponse{Skills: out}, nil
}
