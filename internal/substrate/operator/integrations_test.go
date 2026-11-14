package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type stubIntegrations struct {
	calls     int
	gotSecret string
}

func (s *stubIntegrations) Catalog() []*pb.IntegrationCatalogEntryOp {
	return []*pb.IntegrationCatalogEntryOp{{Id: "brave-search"}}
}
func (s *stubIntegrations) Categories() []string { return []string{"web-search"} }
func (s *stubIntegrations) Integrations() []*pb.IntegrationOp {
	return []*pb.IntegrationOp{{Id: "brave", Enabled: true}}
}
func (s *stubIntegrations) Install(_ context.Context, r *pb.InstallIntegrationOpRequest) (ConfigWriteOutcome, *pb.IntegrationOp, error) {
	s.calls++
	s.gotSecret = r.GetSecrets()["BRAVE_API_KEY"]
	return ConfigWriteOutcome{Key: "mcp.server_overrides.brave", Set: true, Effect: EffectLive}, &pb.IntegrationOp{Id: "brave"}, nil
}
func (s *stubIntegrations) Update(context.Context, *pb.UpdateIntegrationOpRequest) (ConfigWriteOutcome, *pb.IntegrationOp, error) {
	s.calls++
	return ConfigWriteOutcome{Effect: EffectLive}, nil, nil
}
func (s *stubIntegrations) SetSecret(_ context.Context, _, _, v string) (ConfigWriteOutcome, *pb.IntegrationOp, error) {
	s.calls++
	s.gotSecret = v
	return ConfigWriteOutcome{Set: true, Effect: EffectLive}, nil, nil
}
func (s *stubIntegrations) ClearSecret(context.Context, string, string) (ConfigWriteOutcome, *pb.IntegrationOp, error) {
	s.calls++
	return ConfigWriteOutcome{Effect: EffectLive}, nil, nil
}
func (s *stubIntegrations) SetEnabled(context.Context, string, bool) (ConfigWriteOutcome, *pb.IntegrationOp, error) {
	s.calls++
	return ConfigWriteOutcome{Effect: EffectLive}, nil, nil
}
func (s *stubIntegrations) Remove(context.Context, string, bool) (ConfigWriteOutcome, error) {
	s.calls++
	return ConfigWriteOutcome{Effect: EffectLive}, nil
}
func (s *stubIntegrations) RevertToFile(_ context.Context, id string) (ConfigWriteOutcome, *pb.IntegrationOp, error) {
	s.calls++
	return ConfigWriteOutcome{Key: "mcp.server_overrides." + id, Set: true, Effect: EffectLive}, &pb.IntegrationOp{Id: id, Source: "file"}, nil
}
func (s *stubIntegrations) Test(context.Context, string, bool) (*pb.IntegrationTestResultOp, error) {
	return &pb.IntegrationTestResultOp{Ok: true}, nil
}

func TestIntegrations_UnwiredIsUnimplemented(t *testing.T) {
	s := newWriteService(nil, nil)
	if _, err := s.ListIntegrations(context.Background(), &pb.ListIntegrationsOpRequest{}); codeOf(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
	if s.HasIntegrationManager() {
		t.Fatal("capability must not be advertised when unwired")
	}
}

// Mutations are audited with the secret NAMES, never their values, and dedupe
// on command_id (the second call does not reach the manager).
func TestIntegrations_AuditedDedupedAndNeverAuditSecrets(t *testing.T) {
	s := newWriteService(nil, nil)
	m := &stubIntegrations{}
	s.SetIntegrationManager(m)
	ctx := context.Background()
	req := &pb.InstallIntegrationOpRequest{
		CommandId: "c1", Reason: "connect search", CatalogId: "brave-search", Id: "brave", Enabled: true,
		Secrets: map[string]string{"BRAVE_API_KEY": "BSA-super-secret-key"},
	}
	resp, err := s.InstallIntegration(ctx, req)
	if err != nil || resp.GetOutcome().GetEffect() != EffectLive || resp.GetIntegration().GetId() != "brave" {
		t.Fatalf("install: %+v %v", resp, err)
	}
	if m.gotSecret != "BSA-super-secret-key" {
		t.Fatal("the secret did not reach the manager")
	}
	again, err := s.InstallIntegration(ctx, req)
	if err != nil || !again.GetDeduped() || m.calls != 1 {
		t.Fatalf("command_id dedupe failed: %+v calls=%d", again, m.calls)
	}
	if _, err := s.SetIntegrationSecret(ctx, &pb.SetIntegrationSecretOpRequest{
		CommandId: "c2", Reason: "rotate", Id: "brave", Name: "BRAVE_API_KEY", Value: "BSA-rotated-secret",
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.audit.Query(ctx, AuditFilter{})
	if len(rows) != 2 {
		t.Fatalf("want 2 audit rows, got %d", len(rows))
	}
	for _, r := range rows {
		blob := r.After + r.Before + r.Reason + r.TargetID
		if strings.Contains(blob, "BSA-super-secret-key") || strings.Contains(blob, "BSA-rotated-secret") {
			t.Fatalf("SECRET LEAK in audit: %+v", r)
		}
		if r.TargetType != "integration" {
			t.Errorf("target type %q", r.TargetType)
		}
	}
	if !strings.Contains(rows[0].After+rows[1].After, "BRAVE_API_KEY") {
		t.Error("the audit should name which secret was set")
	}
}

func TestIntegrations_RequireCommandIDReasonAndArgs(t *testing.T) {
	s := newWriteService(nil, nil)
	s.SetIntegrationManager(&stubIntegrations{})
	ctx := context.Background()
	if _, err := s.SetIntegrationEnabled(ctx, &pb.SetIntegrationEnabledOpRequest{Id: "x", Enabled: true}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("missing command_id/reason: %v", err)
	}
	if _, err := s.SetIntegrationSecret(ctx, &pb.SetIntegrationSecretOpRequest{CommandId: "c", Reason: "r", Id: "x", Name: "N"}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("empty secret value: %v", err)
	}
	if _, err := s.InstallIntegration(ctx, &pb.InstallIntegrationOpRequest{CommandId: "c", Reason: "r", Id: "x"}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("no catalog_id and no custom: %v", err)
	}
}

// Reads are open to a Viewer; every mutation and TestIntegration (it can launch
// a process) is Operator-only.
func TestIntegrations_RoleGating(t *testing.T) {
	for _, m := range []string{"ListIntegrationCatalog", "ListIntegrations"} {
		if isOperatorOnly("/cambrian.operator.OperatorConsole/"+m, nil) {
			t.Errorf("%s should be readable by a Viewer", m)
		}
	}
	for _, m := range []string{"InstallIntegration", "UpdateIntegration", "SetIntegrationSecret", "ClearIntegrationSecret",
		"SetIntegrationEnabled", "TestIntegration", "RemoveIntegration", "RevertIntegrationToFile"} {
		if !isOperatorOnly("/cambrian.operator.OperatorConsole/"+m, nil) {
			t.Errorf("%s must be Operator-only", m)
		}
	}
}

// Health reaches the operator feed.
func TestIntegrations_HealthEventMapsToFeed(t *testing.T) {
	now := time.Now()
	ev := toOperatorEvent(domain.SequencedEvent{Seq: 7, At: now, Event: domain.MCPServerHealthEvent{
		ServerID: "brave", State: "error", LastError: "boom", ToolCount: 0, Restarts: 2, ChangedAt: now,
	}})
	h := ev.GetIntegrationHealth()
	if h == nil || h.GetServerId() != "brave" || h.GetState() != "error" || h.GetRestartCount() != 2 || h.GetChangedAtUnixMs() == 0 {
		t.Fatalf("mapped: %+v", ev)
	}
	found := false
	for _, et := range feedEventTypes {
		if et == domain.EventTypeMCPServerHealth {
			found = true
		}
	}
	if !found {
		t.Fatal("the health event is not bridged onto the feed (feedEventTypes)")
	}
}

// RevertIntegrationToFile is a mutation like the others: command_id + reason
// required, audited under the integration, deduped on command_id.
func TestIntegrations_RevertToFileIsAuditedAndDeduped(t *testing.T) {
	s := newWriteService(nil, nil)
	m := &stubIntegrations{}
	s.SetIntegrationManager(m)
	ctx := context.Background()
	if _, err := s.RevertIntegrationToFile(ctx, &pb.RevertIntegrationToFileOpRequest{Id: "fetch"}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("missing command_id/reason: %v", err)
	}
	if _, err := s.RevertIntegrationToFile(ctx, &pb.RevertIntegrationToFileOpRequest{CommandId: "c", Reason: "r"}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("missing id: %v", err)
	}
	req := &pb.RevertIntegrationToFileOpRequest{CommandId: "rv1", Reason: "back to mcp.json", Id: "fetch"}
	resp, err := s.RevertIntegrationToFile(ctx, req)
	if err != nil || resp.GetOutcome().GetEffect() != EffectLive || resp.GetIntegration().GetSource() != "file" {
		t.Fatalf("revert: %+v %v", resp, err)
	}
	again, err := s.RevertIntegrationToFile(ctx, req)
	if err != nil || !again.GetDeduped() || m.calls != 1 {
		t.Fatalf("command_id dedupe failed: %+v calls=%d", again, m.calls)
	}
	rows, _ := s.audit.Query(ctx, AuditFilter{})
	if len(rows) != 1 || rows[0].TargetType != "integration" || rows[0].TargetID != "fetch" ||
		!strings.Contains(rows[0].After, "reverted") {
		t.Fatalf("audit rows: %+v", rows)
	}
}
