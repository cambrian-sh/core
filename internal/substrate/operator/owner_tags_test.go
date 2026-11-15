package operator

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
)

// ENG-1046 (owner decision 2026-11-15, ADR-0146 D5): an operator-authored
// descriptor tag list may not carry an owner tag. Each write RPC answers
// INVALID_ARGUMENT naming the tag, before the audit and before any effect.

type recordingMCPWriter struct{ saved []MCPServerSpec }

func (w *recordingMCPWriter) SaveMCPServer(spec MCPServerSpec) (ConfigWriteOutcome, error) {
	w.saved = append(w.saved, spec)
	return ConfigWriteOutcome{Set: true, Effect: EffectLive}, nil
}
func (w *recordingMCPWriter) RemoveMCPServer(string) (ConfigWriteOutcome, error) {
	return ConfigWriteOutcome{}, nil
}
func (w *recordingMCPWriter) SetMCPServerToken(context.Context, string, string) (ConfigWriteOutcome, error) {
	return ConfigWriteOutcome{}, nil
}
func (w *recordingMCPWriter) ClearMCPServerToken(context.Context, string) error { return nil }
func (w *recordingMCPWriter) TestMCPServer(context.Context, MCPServerSpec) MCPTestResult {
	return MCPTestResult{}
}

func wantOwnerTagRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if codeOf(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "owner:alice") {
		t.Fatalf("%s: want INVALID_ARGUMENT naming owner:alice, got %v", what, err)
	}
}

func TestRegisterSkill_RefusesOwnerScopeTag(t *testing.T) {
	s := newWriteService(nil, nil)
	var registered []string
	s.SetCommandEffects(CommandEffectsFuncs{RegisterSkillFn: func(_ context.Context, name, _, _ string, _, _ []string) error {
		registered = append(registered, name)
		return nil
	}})
	ctx := ContextWithPrincipal(context.Background(), "op", RoleOperator)
	_, err := s.RegisterSkill(ctx, &pb.RegisterSkillRequest{CommandId: "c1", Reason: "r", Name: "mine",
		ScopeTags: []string{"ops", "owner:alice"}})
	wantOwnerTagRefusal(t, "RegisterSkill", err)
	if len(registered) != 0 {
		t.Fatalf("refused skill reached the effect: %v", registered)
	}
	if _, err := s.RegisterSkill(ctx, &pb.RegisterSkillRequest{CommandId: "c2", Reason: "r", Name: "ok",
		ScopeTags: []string{"ops"}}); err != nil || len(registered) != 1 {
		t.Fatalf("clean skill: err=%v registered=%v", err, registered)
	}
}

func TestSaveMCPServer_RefusesOwnerClassificationTag(t *testing.T) {
	s := newWriteService(nil, nil)
	w := &recordingMCPWriter{}
	s.SetMCPWriter(w)
	ctx := ContextWithPrincipal(context.Background(), "op", RoleOperator)
	_, err := s.SaveMCPServer(ctx, &pb.SaveMCPServerOpRequest{CommandId: "c1", Reason: "r", Server: &pb.MCPServerSpecOp{
		Id: "crm", Transport: "http", Endpoint: "https://crm.example/mcp", ClassificationTags: []string{"owner:alice"}}})
	wantOwnerTagRefusal(t, "SaveMCPServer", err)
	if len(w.saved) != 0 {
		t.Fatal("refused server reached the writer")
	}
	if _, err := s.SaveMCPServer(ctx, &pb.SaveMCPServerOpRequest{CommandId: "c2", Reason: "r", Server: &pb.MCPServerSpecOp{
		Id: "crm", Transport: "http", Endpoint: "https://crm.example/mcp", ClassificationTags: []string{"crm"}}}); err != nil || len(w.saved) != 1 {
		t.Fatalf("clean server: err=%v saved=%d", err, len(w.saved))
	}
}

func TestIntegrations_RefuseOwnerClassificationTags(t *testing.T) {
	s := newWriteService(nil, nil)
	m := &stubIntegrations{}
	s.SetIntegrationManager(m)
	ctx := ContextWithPrincipal(context.Background(), "op", RoleOperator)

	_, err := s.InstallIntegration(ctx, &pb.InstallIntegrationOpRequest{CommandId: "c1", Reason: "r",
		CatalogId: "brave-search", ClassificationTags: []string{"owner:alice"}})
	wantOwnerTagRefusal(t, "InstallIntegration tags", err)
	_, err = s.InstallIntegration(ctx, &pb.InstallIntegrationOpRequest{CommandId: "c2", Reason: "r", Id: "x",
		Custom: &pb.IntegrationCustomSpecOp{Transport: "http", Endpoint: "https://x.example", ClassificationTags: []string{"owner:alice"}}})
	wantOwnerTagRefusal(t, "InstallIntegration custom", err)
	_, err = s.UpdateIntegration(ctx, &pb.UpdateIntegrationOpRequest{CommandId: "c3", Reason: "r", Id: "brave",
		SetTags: true, ClassificationTags: []string{"owner:alice"}})
	wantOwnerTagRefusal(t, "UpdateIntegration", err)
	if m.calls != 0 {
		t.Fatalf("a refused write reached the manager (%d calls)", m.calls)
	}
	if _, err := s.UpdateIntegration(ctx, &pb.UpdateIntegrationOpRequest{CommandId: "c4", Reason: "r", Id: "brave",
		SetTags: true, ClassificationTags: []string{"search"}}); err != nil || m.calls != 1 {
		t.Fatalf("clean update: err=%v calls=%d", err, m.calls)
	}
}
