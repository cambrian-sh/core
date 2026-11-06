package network

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/access"
)

// Per-agent skill access on the agent plane (decisions/skill-access-grants.md).

type fakeSkillAccess struct {
	own      map[string][]string // profile agents
	granted  map[string]map[string]bool
	answer   access.Outcome // what a request answers
	requests []access.RequestInput
}

func (f *fakeSkillAccess) Effective(_ context.Context, agentID string) (map[string]string, bool) {
	own, ok := f.own[agentID]
	if !ok {
		return nil, false
	}
	set := map[string]string{}
	for n := range f.granted[agentID] {
		set[n] = domain.SkillAccessGranted
	}
	for _, n := range own {
		set[n] = domain.SkillAccessProfile
	}
	return set, true
}

func (f *fakeSkillAccess) Request(_ context.Context, in access.RequestInput) access.Outcome {
	f.requests = append(f.requests, in)
	if f.answer.Access == domain.SkillAccessGranted {
		if f.granted == nil {
			f.granted = map[string]map[string]bool{}
		}
		if f.granted[in.AgentID] == nil {
			f.granted[in.AgentID] = map[string]bool{}
		}
		f.granted[in.AgentID][in.Skill] = true
	}
	return f.answer
}

type fixedRetriever []string

func (r fixedRetriever) Rank(context.Context, string, *domain.TagPredicate, int) ([]string, error) {
	return r, nil
}

func accessServer(fa SkillAccess) *Server {
	reg := domain.NewInMemorySkillRegistry()
	for _, n := range []string{"file-report", "summarise", "proposed-x"} {
		reg.Register(domain.Skill{Name: n, Description: "Does " + n + ".", Instructions: "BODY-" + n})
	}
	s := lifecycleServer(&fakeSkillLifecycle{})
	s.SkillRegistry = reg
	s.SkillRetriever = fixedRetriever{"proposed-x", "summarise"}
	s.SkillAccess = fa
	return s
}

func listAs(t *testing.T, s *Server, agent string, kv ...string) []*pb.SkillDescriptor {
	t.Helper()
	md := metadata.Pairs(append([]string{"x-agent-id", agent}, kv...)...)
	resp, err := s.ListSkills(skillAgentCtx(agent, md), &pb.ListSkillsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetSkills()
}

func names(ds []*pb.SkillDescriptor) map[string]*pb.SkillDescriptor {
	m := map[string]*pb.SkillDescriptor{}
	for _, d := range ds {
		m[d.GetName()] = d
	}
	return m
}

// The full menu of a profile agent is its effective set; an unbounded agent
// still sees every scope-visible skill, unannotated.
func TestListSkills_MenuIsEffectiveSet(t *testing.T) {
	fa := &fakeSkillAccess{own: map[string][]string{"analyst": {"file-report", "summarise"}}}
	s := accessServer(fa)
	m := names(listAs(t, s, "analyst"))
	if len(m) != 2 || m["proposed-x"] != nil || m["file-report"].GetAccess() != domain.SkillAccessProfile {
		t.Fatalf("menu = %v", m)
	}
	fa.granted = map[string]map[string]bool{"analyst": {"proposed-x": true}}
	if m := names(listAs(t, s, "analyst")); len(m) != 3 || m["proposed-x"].GetAccess() != domain.SkillAccessGranted {
		t.Fatalf("menu with grant = %v", m)
	}
	if m := names(listAs(t, s, "legacy")); len(m) != 3 || m["proposed-x"].GetAccess() != "" {
		t.Fatalf("unbounded menu = %v", m)
	}
}

// A non-granted skill's body is never served to a bound agent: not on a Tier-1
// names lookup, not on find_skills even with full=true, not on a denied or
// pending use_skill.
func TestListSkills_NonGrantedBodyNeverServed(t *testing.T) {
	for _, ans := range []access.Outcome{
		{Access: domain.SkillAccessPending, Detail: "no operator decision yet"},
		{Access: domain.SkillAccessDenied, Detail: "an operator denied access"},
	} {
		fa := &fakeSkillAccess{own: map[string][]string{"analyst": {"file-report"}}, answer: ans}
		s := accessServer(fa)
		for _, kv := range [][]string{
			{"x-skill-names", "proposed-x"},
			{"x-skill-query", "anything", "x-skill-full", "true"},
			{"x-skill-names", "proposed-x", "x-skill-full", "true", "x-session-token", "lease-1"},
		} {
			for _, d := range listAs(t, s, "analyst", kv...) {
				if d.GetName() == "proposed-x" && (d.GetInstructions() != "" || len(d.GetToolGrants()) > 0) {
					t.Fatalf("%v: body served outside the set: %v", kv, d)
				}
			}
		}
		d := names(listAs(t, s, "analyst", "x-skill-names", "proposed-x", "x-skill-full", "true"))["proposed-x"]
		if d.GetAccess() != ans.Access || d.GetAccessDetail() != ans.Detail {
			t.Fatalf("use_skill answer = %v, want %v", d, ans)
		}
	}
}

// find_skills discovers skills outside the set, marked requestable (Tier-1).
func TestListSkills_FindSkillsMarksRequestable(t *testing.T) {
	fa := &fakeSkillAccess{own: map[string][]string{"analyst": {"summarise"}}}
	m := names(listAs(t, accessServer(fa), "analyst", "x-skill-query", "report"))
	if m["proposed-x"].GetAccess() != domain.SkillAccessRequestable || m["summarise"].GetAccess() != domain.SkillAccessProfile {
		t.Fatalf("find_skills = %v", m)
	}
	if len(fa.requests) != 0 {
		t.Fatal("discovery must not open a request")
	}
}

// use_skill on a requestable skill opens a request carrying the lease's run and
// the rationale; an approval serves the body in the same call and counts a use;
// the next menu lists the skill as granted.
func TestListSkills_RequestApprovedServesSameCall(t *testing.T) {
	fa := &fakeSkillAccess{own: map[string][]string{"agent-a": {"summarise"}}, answer: access.Outcome{Access: domain.SkillAccessGranted}}
	s := accessServer(fa)
	lc := s.SkillLifecycle.(*fakeSkillLifecycle)
	d := names(listAs(t, s, "agent-a", "x-skill-names", "proposed-x", "x-skill-full", "true",
		"x-session-token", "lease-1", "x-skill-rationale", "the task needs a report"))["proposed-x"]
	if d.GetInstructions() != "BODY-proposed-x" || d.GetAccess() != domain.SkillAccessGranted {
		t.Fatalf("descriptor = %v", d)
	}
	if len(fa.requests) != 1 || fa.requests[0].RunID != "run-1" || fa.requests[0].Rationale != "the task needs a report" || fa.requests[0].AgentID != "agent-a" {
		t.Fatalf("requests = %+v", fa.requests)
	}
	if len(lc.uses) != 1 || lc.uses[0][0] != "proposed-x" {
		t.Fatalf("an approved load is a use: %v", lc.uses)
	}
	if m := names(listAs(t, s, "agent-a")); m["proposed-x"].GetAccess() != domain.SkillAccessGranted {
		t.Fatalf("next menu = %v", m)
	}
	// In-set loads never ask.
	listAs(t, s, "agent-a", "x-skill-names", "summarise", "x-skill-full", "true")
	if len(fa.requests) != 1 {
		t.Fatalf("an in-set load opened a request: %+v", fa.requests)
	}
}

// End to end over the real access service and the real approval hub: an operator
// approving live gives the body in the same ListSkills call.
func TestListSkills_LiveApprovalOverHub(t *testing.T) {
	hub := domain.NewInMemoryApprovalController(time.Minute)
	ch, cancel := hub.Watch()
	defer cancel()
	go func() {
		if r, ok := <-ch; ok {
			hub.Submit(r.ID, true, "op")
		}
	}()
	svc, err := access.New(&oneGrantStore{}, func(id string) ([]string, bool) {
		return []string{"summarise"}, id == "analyst"
	}, nil, hub, access.Config{RequestTimeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := accessServer(svc)
	d := names(listAs(t, s, "analyst", "x-skill-names", "proposed-x", "x-skill-full", "true"))["proposed-x"]
	if d.GetInstructions() != "BODY-proposed-x" {
		t.Fatalf("descriptor = %v", d)
	}
}

// oneGrantStore is the smallest durable store the live test needs.
type oneGrantStore struct{ grants []domain.SkillGrant }

func (o *oneGrantStore) Grant(_ context.Context, g domain.SkillGrant, _ string) (bool, error) {
	o.grants = append(o.grants, g)
	return true, nil
}
func (o *oneGrantStore) Revoke(context.Context, string, string, string, string, string, time.Time) error {
	return nil
}
func (o *oneGrantStore) ActiveGrants(context.Context, string) ([]domain.SkillGrant, error) {
	return o.grants, nil
}
func (o *oneGrantStore) ListGrants(context.Context, domain.SkillGrantFilter) ([]domain.SkillGrant, error) {
	return o.grants, nil
}
func (o *oneGrantStore) OpenRequest(context.Context, domain.SkillAccessRequest) error { return nil }
func (o *oneGrantStore) DenyRequest(context.Context, string, string, string, time.Time) error {
	return nil
}
func (o *oneGrantStore) ListRequests(context.Context, domain.SkillGrantFilter) ([]domain.SkillAccessRequest, error) {
	return nil, nil
}
func (o *oneGrantStore) EventByCommand(context.Context, string) (domain.SkillGrantEvent, bool, error) {
	return domain.SkillGrantEvent{}, false, nil
}
func (o *oneGrantStore) ListEvents(context.Context, domain.SkillGrantFilter, int) ([]domain.SkillGrantEvent, error) {
	return nil, nil
}
