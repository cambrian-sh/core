package app

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/integrations"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// End to end for the outbound identifier guard (ENG-917): the default config, the
// shipped catalog's policies, the one real detector (internal/identifiers), the real
// approval hub and the agent-plane ExecuteTool RPC — only the MCP server is a fake.

type countingSend struct{ n atomic.Int32 }

func (h *countingSend) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	h.n.Add(1)
	return []byte(`{"sent":true}`), nil
}

// catalogTool registers name with the policy the shipped catalog entry gives it
// (mcp.DescribeToolPolicy: per-tool entry, server default, else fail-closed).
func catalogTool(t *testing.T, entryID, tool string) domain.SystemTool {
	t.Helper()
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cat.Get(entryID)
	if !ok {
		t.Fatalf("catalog has no %s", entryID)
	}
	return policyTool(e.ID, e.Server.Tools, e.Server.DefaultToolPolicy, tool)
}

func policyTool(server string, tools []config.MCPToolConfig, def *config.MCPToolConfig, tool string) domain.SystemTool {
	sc := mcp.ServerConfig{ID: server, Tools: map[string]mcp.ToolPolicy{}}
	for _, p := range tools {
		sc.Tools[p.Name] = mcp.ToolPolicy{Dangerous: p.Dangerous, Effects: p.Effects}
	}
	if def != nil {
		sc.DefaultPolicy = &mcp.ToolPolicy{Dangerous: def.Dangerous, Effects: def.Effects}
	}
	rep := mcp.DescribeToolPolicy(sc, tool)
	st := domain.SystemTool{Name: "mcp:" + server + "/" + tool, Dangerous: rep.Dangerous}
	for _, e := range rep.Effects {
		st.Effects = append(st.Effects, domain.ToolEffect(e))
	}
	return st
}

type guardRig struct {
	srv  *subnetwork.Server
	hub  *domain.AddressedHub
	evs  <-chan domain.ApprovalEvent
	send *countingSend
	stop func()
}

func newGuardRig(t *testing.T, tools ...domain.SystemTool) *guardRig {
	t.Helper()
	cfg := config.DefaultConfig().Execution
	if !cfg.Tools.OutboundIdentifierGuard.Enabled {
		t.Fatal("execution.tools.outbound_identifier_guard must default to on")
	}
	guard, err := outboundIdentifierGuard(cfg.Tools.OutboundIdentifierGuard)
	if err != nil || guard == nil {
		t.Fatalf("guard from the default config: %v %v", guard, err)
	}
	reg := domain.NewInMemoryToolRegistry()
	grants := domain.NewInMemoryGrantsStore()
	var gs []domain.ToolGrant
	for _, st := range tools {
		reg.Register(st)
		gs = append(gs, domain.ToolGrant{Tool: st.Name, Policy: domain.ToolResourcePolicy{AllowAll: true}})
	}
	grants.Set("agent-a", gs)
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: 5 * time.Second})
	evs, cancel := hub.Watch(domain.ApprovalWatch{For: domain.OperatorApprover("op"), Resolutions: true})
	send := &countingSend{}
	te, err := domain.NewToolExecutor(evidenceToolControls(cfg, nil, domain.ToolExecutorControls{
		Registry: reg, Grants: grants, Authz: domain.AllowAllAuthorizer{}, Approval: hub,
		OutboundIdentifiers: guard,
	}), domain.ToolExecutor{Handler: send, MCPHandler: send, InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return &guardRig{srv: &subnetwork.Server{ToolExecutor: te}, hub: hub, evs: evs, send: send, stop: cancel}
}

func (r *guardRig) call(tool, args string) <-chan *pb.ExecuteToolResponse {
	out := make(chan *pb.ExecuteToolResponse, 1)
	go func() {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "agent-a"))
		resp, err := r.srv.ExecuteTool(ctx, &pb.ExecuteToolRequest{ToolName: tool, ArgsJson: args})
		if err != nil {
			resp = &pb.ExecuteToolResponse{Error: err.Error()}
		}
		out <- resp
	}()
	return out
}

func (r *guardRig) nextRaised(t *testing.T) domain.ApprovalItem {
	t.Helper()
	for {
		select {
		case ev := <-r.evs:
			if ev.Type == domain.ApprovalEventRaised {
				return ev.Item
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no approval was raised")
		}
	}
}

func waitResp(t *testing.T, ch <-chan *pb.ExecuteToolResponse) *pb.ExecuteToolResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("call did not finish")
		return nil
	}
}

// A deployment's case-response tool, declared egress_send and NOT dangerous: the
// shape of a worldsim cases_respond_request a deployment classifies by policy.
var caseRespond = policyTool("cases", []config.MCPToolConfig{{Name: "respond_request", Effects: []string{"write", "egress_send"}}}, nil, "respond_request")

func TestE2E_EgressSendWithIdentifierWaitsForApprovalWithHighlights(t *testing.T) {
	r := newGuardRig(t, caseRespond)
	defer r.stop()
	args := `{"case":"BE-2026/0001","letter":"Sayın başvuru sahibi, Merve Güneş (T.C. 10304949101, tel 0 (509) 803 65 52) ile ilgili belgeler ektedir."}`
	done := r.call(caseRespond.Name, args)
	it := r.nextRaised(t)
	if n := r.send.n.Load(); n != 0 {
		t.Fatalf("the call ran (%d) before anyone approved it", n)
	}
	for _, want := range []string{domain.OutboundIdentifierNoticePrefix, "national ID number «10304949101» in letter", "phone number «0 (509) 803 65 52» in letter"} {
		if !strings.Contains(it.DuplicateNotice, want) {
			t.Fatalf("approver notice lacks %q:\n%s", want, it.DuplicateNotice)
		}
	}
	// The listing the console reads carries it too.
	got, err := r.hub.Get(context.Background(), it.ID)
	if err != nil || got.DuplicateNotice != it.DuplicateNotice {
		t.Fatalf("stored item notice = %q, %v", got.DuplicateNotice, err)
	}
	if _, err := r.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")}); err != nil {
		t.Fatal(err)
	}
	resp := waitResp(t, done)
	if resp.Denied || resp.Error != "" || r.send.n.Load() != 1 {
		t.Fatalf("approved call: %+v ran=%d", resp, r.send.n.Load())
	}
}

func TestE2E_EgressSendWithoutIdentifierRaisesNoApproval(t *testing.T) {
	r := newGuardRig(t, caseRespond)
	defer r.stop()
	resp := waitResp(t, r.call(caseRespond.Name, `{"case":"BE-2026/0001","letter":"Belgeler kişisel veriler çıkarılarak ektedir. Ref E-74183520-310.04-1113, 24.09.2026."}`))
	if resp.Denied || resp.Error != "" || r.send.n.Load() != 1 {
		t.Fatalf("no identifier ⇒ runs with no approval: %+v", resp)
	}
	select {
	case ev := <-r.evs:
		if ev.Type == domain.ApprovalEventRaised {
			t.Fatalf("an approval was raised: %+v", ev.Item)
		}
	default:
	}
}

// A shipped web-read tool (egress_read, the brave-search server default) is not
// guarded: an identifier in a search query raises no approval.
func TestE2E_CatalogEgressReadToolIsNotGuarded(t *testing.T) {
	search := catalogTool(t, "brave-search", "brave_web_search")
	if domain.SendsOutside(search) {
		t.Fatalf("brave-search must be egress_read, got %v", search.Effects)
	}
	r := newGuardRig(t, search)
	defer r.stop()
	resp := waitResp(t, r.call(search.Name, `{"query":"10304949101"}`))
	if resp.Denied || resp.Error != "" || r.send.n.Load() != 1 {
		t.Fatalf("egress_read call: %+v", resp)
	}
}

// The shipped send tool is egress_send and dangerous: one approval, carrying the
// identifiers; an undeclared tool of a fail-closed server is dangerous
// read+write+egress and its approval carries them too.
func TestE2E_CatalogSendAndUndeclaredToolApprovalsCarryHighlights(t *testing.T) {
	send := catalogTool(t, "google-workspace-personal-write", "send_gmail_message")
	undeclared := catalogTool(t, "google-workspace-personal-write", "some_future_tool")
	if !send.Dangerous || !send.HasEffect(domain.EffectEgressSend) {
		t.Fatalf("send_gmail_message policy = %+v", send)
	}
	if !undeclared.Dangerous || !undeclared.HasEffect(domain.EffectEgress) || !undeclared.HasEffect(domain.EffectWrite) {
		t.Fatalf("undeclared tool must fail closed: %+v", undeclared)
	}
	r := newGuardRig(t, send, undeclared)
	defer r.stop()
	for _, st := range []domain.SystemTool{send, undeclared} {
		done := r.call(st.Name, `{"to":"requester@example.org","body":"Kimlik no 55879350776"}`)
		it := r.nextRaised(t)
		if !strings.Contains(it.DuplicateNotice, "«55879350776» in body") || !strings.Contains(it.DuplicateNotice, "«requester@example.org» in to") {
			t.Fatalf("%s approval notice:\n%s", st.Name, it.DuplicateNotice)
		}
		if _, err := r.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: false, By: domain.OperatorApprover("op")}); err != nil {
			t.Fatal(err)
		}
		if resp := waitResp(t, done); !resp.Denied {
			t.Fatalf("%s: denied approval must stop the call: %+v", st.Name, resp)
		}
	}
	if r.send.n.Load() != 0 {
		t.Fatalf("denied calls ran %d times", r.send.n.Load())
	}
}

func TestOutboundIdentifierGuard_OffAndKinds(t *testing.T) {
	if g, err := outboundIdentifierGuard(config.OutboundIdentifierGuardConfig{Enabled: false}); g != nil || err != nil {
		t.Fatalf("off ⇒ nil, got %v %v", g, err)
	}
	if _, err := outboundIdentifierGuard(config.OutboundIdentifierGuardConfig{Enabled: true, Kinds: []string{"passport"}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	g, err := outboundIdentifierGuard(config.OutboundIdentifierGuardConfig{Enabled: true, Kinds: []string{"national_id"}})
	if err != nil {
		t.Fatal(err)
	}
	if hits := g.Scan([]byte(`{"to":"a@example.org","body":"10304949101"}`)); len(hits) != 1 || hits[0].Kind != domain.IdentifierNationalID {
		t.Fatalf("kinds narrowing: %+v", hits)
	}
}
