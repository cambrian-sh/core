package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type fakeCatalog struct{ tools []domain.SystemTool }

func (f fakeCatalog) AllTools() []domain.SystemTool { return f.tools }

type fakeSkills struct{ skills []domain.Skill }

func (f fakeSkills) All() []domain.Skill { return f.skills }

type fakeMemory struct {
	results []domain.SearchResult
	err     error
}

func (f fakeMemory) SearchSystem(context.Context, string) ([]domain.SearchResult, error) {
	return f.results, f.err
}

// ListTools returns the whole catalog with builtin/mcp source labels and the
// tool→agents reverse index built from the grants store. A2.3.
func TestListTools_SourceLabelAndGrantsIndex(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetReadSources(fakeCatalog{tools: []domain.SystemTool{
		{Name: "web_search", Description: "search the web", Dangerous: false, DataReadKinds: []string{"web"}},
		{Name: "shell_exec", Description: "run a shell command", Dangerous: true},
		{Name: "mcp:jira/create_issue", Description: "make a jira ticket"},
	}}, nil, nil)

	// Grant web_search to agent-1 so the reverse index has an entry.
	if _, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "g1", Reason: "r", AgentId: "agent-1", ToolName: "web_search", Granted: true,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if resp.GetTotal() != 3 {
		t.Fatalf("want 3 tools, got %d", resp.GetTotal())
	}
	byName := map[string]*pb.ToolOp{}
	for _, tl := range resp.GetTools() {
		byName[tl.GetName()] = tl
	}
	if byName["web_search"].GetSource() != "builtin" {
		t.Errorf("web_search source = %q, want builtin", byName["web_search"].GetSource())
	}
	if byName["mcp:jira/create_issue"].GetSource() != "mcp:jira" {
		t.Errorf("mcp tool source = %q, want mcp:jira", byName["mcp:jira/create_issue"].GetSource())
	}
	if !byName["shell_exec"].GetDangerous() {
		t.Errorf("shell_exec should be dangerous")
	}
	grants := byName["web_search"].GetGrants()
	if len(grants) != 1 || grants[0].GetAgentId() != "agent-1" {
		t.Fatalf("web_search grants = %+v, want [agent-1]", grants)
	}
	if len(byName["shell_exec"].GetGrants()) != 0 {
		t.Errorf("shell_exec should have no grants")
	}
}

// dangerous_only and source filters narrow the catalog.
func TestListTools_Filters(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetReadSources(fakeCatalog{tools: []domain.SystemTool{
		{Name: "web_search"},
		{Name: "shell_exec", Dangerous: true},
		{Name: "mcp:jira/create_issue"},
		{Name: "mcp:slack/post"},
	}}, nil, nil)

	dang, _ := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{DangerousOnly: true})
	if dang.GetTotal() != 1 || dang.GetTools()[0].GetName() != "shell_exec" {
		t.Fatalf("dangerous_only = %+v", dang.GetTools())
	}
	mcp, _ := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{Source: "mcp"})
	if mcp.GetTotal() != 2 {
		t.Fatalf("source=mcp want 2, got %d", mcp.GetTotal())
	}
	one, _ := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{Source: "mcp:slack"})
	if one.GetTotal() != 1 || one.GetTools()[0].GetName() != "mcp:slack/post" {
		t.Fatalf("source=mcp:slack want 1, got %+v", one.GetTools())
	}
}

// ListTools paginates over a sorted catalog.
func TestListTools_Pagination(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetReadSources(fakeCatalog{tools: []domain.SystemTool{
		{Name: "c"}, {Name: "a"}, {Name: "b"}, {Name: "d"},
	}}, nil, nil)

	p1, _ := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{Page: 1, PageSize: 2})
	if p1.GetTotal() != 4 || len(p1.GetTools()) != 2 || p1.GetTools()[0].GetName() != "a" || p1.GetTools()[1].GetName() != "b" {
		t.Fatalf("page1 = %+v (total %d)", p1.GetTools(), p1.GetTotal())
	}
	p2, _ := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{Page: 2, PageSize: 2})
	if len(p2.GetTools()) != 2 || p2.GetTools()[0].GetName() != "c" {
		t.Fatalf("page2 = %+v", p2.GetTools())
	}
	p3, _ := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{Page: 3, PageSize: 2})
	if len(p3.GetTools()) != 0 {
		t.Fatalf("page3 should be empty, got %+v", p3.GetTools())
	}
}

// ListTools with no catalog wired returns an empty page, never an error.
func TestListTools_NoCatalog(t *testing.T) {
	svc, _, _, _ := newCommandService()
	resp, err := svc.ListTools(context.Background(), &pb.ListToolsOpRequest{})
	if err != nil || resp.GetTotal() != 0 {
		t.Fatalf("want empty page, got total=%d err=%v", resp.GetTotal(), err)
	}
}

func TestListSkills_ListsAndFilters(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetReadSources(nil, fakeSkills{skills: []domain.Skill{
		{Name: "pdf_report", Description: "build a PDF report", ToolGrants: []string{"write_file"}, ScopeTags: []string{"docs"}},
		{Name: "web_scrape", Description: "scrape a page"},
	}}, nil)

	all, err := svc.ListSkills(context.Background(), &pb.ListSkillsOpRequest{})
	if err != nil || all.GetTotal() != 2 {
		t.Fatalf("want 2 skills, got %d err=%v", all.GetTotal(), err)
	}
	if all.GetSkills()[0].GetName() != "pdf_report" || all.GetSkills()[0].GetToolGrants()[0] != "write_file" {
		t.Fatalf("skill mapping wrong: %+v", all.GetSkills()[0])
	}
	q, _ := svc.ListSkills(context.Background(), &pb.ListSkillsOpRequest{Query: "scrape"})
	if q.GetTotal() != 1 || q.GetSkills()[0].GetName() != "web_scrape" {
		t.Fatalf("query filter = %+v", q.GetSkills())
	}
}

// QueryMemory maps results and applies min_importance / source / top_k filters.
func TestQueryMemory_MapsAndFilters(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetReadSources(nil, nil, fakeMemory{results: []domain.SearchResult{
		{Document: domain.Document{ID: "d1", Summary: "budget is 500k", Metadata: map[string]interface{}{"importance": 0.9, "source": "email", "tags": []string{"finance"}}}, Score: 0.92},
		{Document: domain.Document{ID: "d2", Text: "low signal note", Metadata: map[string]interface{}{"importance": 0.2, "source": "slack"}}, Score: 0.40},
		{Document: domain.Document{ID: "d3", Summary: "another email", Metadata: map[string]interface{}{"importance": 0.8, "source": "email"}}, Score: 0.85},
	}})

	// min_importance drops d2; source=email keeps d1,d3.
	resp, err := svc.QueryMemory(context.Background(), &pb.QueryMemoryRequest{Query: "budget", MinImportance: 0.5, Source: "email"})
	if err != nil {
		t.Fatalf("QueryMemory: %v", err)
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(resp.GetResults()), resp.GetResults())
	}
	first := resp.GetResults()[0]
	if first.GetDocId() != "d1" || first.GetSummary() != "budget is 500k" || first.GetSource() != "email" {
		t.Fatalf("mapping wrong: %+v", first)
	}
	if first.GetImportance() != 0.9 || len(first.GetTags()) != 1 || first.GetTags()[0] != "finance" {
		t.Fatalf("importance/tags wrong: %+v", first)
	}

	// top_k caps the result set.
	capped, _ := svc.QueryMemory(context.Background(), &pb.QueryMemoryRequest{Query: "x", TopK: 1})
	if len(capped.GetResults()) != 1 {
		t.Fatalf("top_k=1 want 1, got %d", len(capped.GetResults()))
	}
}

func TestQueryMemory_Unconfigured(t *testing.T) {
	svc, _, _, _ := newCommandService()
	if _, err := svc.QueryMemory(context.Background(), &pb.QueryMemoryRequest{Query: "x"}); err == nil {
		t.Fatal("expected Unimplemented when memory source not wired")
	}
}
