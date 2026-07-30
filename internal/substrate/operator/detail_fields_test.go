package operator

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type stubToolCatalog struct{ tools []domain.SystemTool }

func (s stubToolCatalog) AllTools() []domain.SystemTool { return s.tools }

type stubSkillLister struct{ skills []domain.Skill }

func (s stubSkillLister) All() []domain.Skill { return s.skills }

// Contract 0057 removed the per-entity getters AND the detail fields, leaving six
// panes half-rendered. These pin the fields back on, so a future projection
// trim has to break a test rather than a screen.

// The tool schema is the exact menu entry the model sees. It is the only thing
// that answers "why did the agent call this with those arguments?", and nothing
// else on this plane carries it.
func TestListTools_CarriesSchemaAndResourceArgs(t *testing.T) {
	s := &Service{}
	s.SetReadSources(stubToolCatalog{tools: []domain.SystemTool{{
		Name:        "write_file",
		Description: "writes a file",
		Schema:      []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		PathArgs:    []string{"path"},
		URLArgs:     []string{"source"},
		CommandArgs: []string{"cmd"},
	}}}, nil, nil)

	resp, err := s.ListTools(context.Background(), &pb.ListToolsOpRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(resp.GetTools()) != 1 {
		t.Fatalf("got %d tools, want 1", len(resp.GetTools()))
	}
	got := resp.GetTools()[0]
	if got.GetSchemaJson() == "" {
		t.Fatal("schema_json is empty — a tool pane cannot show what the model was offered")
	}
	// The resource-arg lists say WHICH arguments a resource policy binds. Without
	// them a grant's policy reads as applying to the whole tool.
	if len(got.GetPathArgs()) != 1 || got.GetPathArgs()[0] != "path" {
		t.Fatalf("path_args = %v, want [path]", got.GetPathArgs())
	}
	if len(got.GetUrlArgs()) != 1 || len(got.GetCommandArgs()) != 1 {
		t.Fatalf("url_args = %v command_args = %v", got.GetUrlArgs(), got.GetCommandArgs())
	}
}

// A skill IS its instructions. Listing name and grants without the body describes
// the wrapper and never the thing.
func TestListSkills_CarriesTheInstructionBody(t *testing.T) {
	s := &Service{}
	s.SetReadSources(nil, stubSkillLister{skills: []domain.Skill{{
		Name:         "triage",
		Description:  "triage an incident",
		Instructions: "1. read the alert\n2. check the runbook",
		ToolGrants:   []string{"read_file"},
	}}}, nil)

	resp, err := s.ListSkills(context.Background(), &pb.ListSkillsOpRequest{})
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	got := resp.GetSkills()[0]
	if got.GetBody() == "" {
		t.Fatal("body is empty — the skill's actual instructions are the artefact")
	}
	if got.GetBody() != "1. read the alert\n2. check the runbook" {
		t.Fatalf("body = %q", got.GetBody())
	}
}

// The agent detail fields ride the READY event, so the projection folds them by
// agent id and a detail pane fills from the ordinary feed.
func TestAgentReady_CarriesTheDetailFields(t *testing.T) {
	ev := domain.AgentReadyEvent{
		AgentID:            "analyst",
		Capabilities:       []string{"analysis"},
		Description:        "analyses things",
		Trait:              "specialist",
		Runtime:            "python",
		ExecPath:           "/agents/analyst.py",
		ManifestVersion:    "2",
		Provisional:        true,
		System:             false,
		ClassificationTags: []string{"internal"},
		LastError:          "boot timed out",
	}

	out := toOperatorEvent(domain.SequencedEvent{Event: ev})
	got := out.GetAgentReady()
	if got == nil {
		t.Fatal("no AgentReadyOp payload")
	}
	if got.GetDescription() != "analyses things" || got.GetTrait() != "specialist" {
		t.Fatalf("description=%q trait=%q", got.GetDescription(), got.GetTrait())
	}
	if got.GetExecPath() == "" || got.GetManifestVersion() == "" {
		t.Fatalf("exec_path=%q manifest_version=%q", got.GetExecPath(), got.GetManifestVersion())
	}
	// provisional is the difference between "this agent SAYS it can do X" and "we
	// have seen it do X", and routing treats the two differently.
	if !got.GetProvisional() {
		t.Fatal("provisional was lost — an unverified agent would render as verified")
	}
	if got.GetLastError() != "boot timed out" {
		t.Fatalf("last_error = %q — a failing agent must not look merely idle", got.GetLastError())
	}
	if len(got.GetClassificationTags()) != 1 {
		t.Fatalf("classification_tags = %v", got.GetClassificationTags())
	}
}
