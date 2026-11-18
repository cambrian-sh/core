package app

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
)

// End to end for ENG-1046 (owner decision 2026-11-15, ADR-0146 D5: owner tags
// are kernel-only): over a real gRPC connection, through the operator bearer
// interceptor, onto the real config store, MCP connector and skill index —
// every operator-authored descriptor carrying an owner: tag is refused with
// INVALID_ARGUMENT naming the tag and changes nothing; the same descriptor
// without one is applied. A stored override that already carries one fails the
// next boot naming the server.

type e2eOperatorIdP struct{}

func (e2eOperatorIdP) Resolve(_ context.Context, token string) (domain.Identity, error) {
	if token != "op-token" {
		return domain.Identity{}, domain.ErrUnauthenticated
	}
	return domain.Identity{Principal: domain.PersonPrincipal("op"), Roles: []string{"operator"}, Issuer: "local"}, nil
}
func (e2eOperatorIdP) Revoke(context.Context, string) error { return nil }

func TestDescriptorOwnerTags_RefusedOnTheOperatorPlane(t *testing.T) {
	acme := newAuthServer(t)
	rig := newIntegrationRig(t, acme.url)

	// The RegisterSkill effect exactly as bootstrapKernel wires it: the kernel
	// skill registry plus the skill index.
	skillReg := domain.NewInMemorySkillRegistry()
	skillStore := &sinkFakeStore{}
	skillIndexer := &domain.SkillIndexer{Store: skillStore, Embedder: sinkFakeEmb{}}

	svc := operatortest.New(t, nil, func(d *operator.Deps, o *operator.Options) {
		o.MCPWriter = rig.src
		o.IntegrationManager = rig.m
		d.Effects = operator.CommandEffectsFuncs{
			RegisterSkillFn: func(ctx context.Context, name, description, instructions string, toolGrants, scopeTags []string) error {
				sk := domain.Skill{Name: name, Description: description, Instructions: instructions, ToolGrants: toolGrants, ScopeTags: scopeTags}
				skillReg.Register(sk)
				return skillIndexer.Index(ctx, sk)
			},
		}
	})

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnaryInterceptor(operator.UnaryAuthInterceptor(e2eOperatorIdP{}, nil, nil)))
	pb.RegisterOperatorConsoleServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewOperatorConsoleClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer op-token")

	refused := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), `"owner:alice"`) {
			t.Fatalf("%s: want INVALID_ARGUMENT naming owner:alice, got %v", what, err)
		}
	}
	hasServer := func(id string) bool {
		for _, s := range rig.src.effectiveMCPServers() {
			if s.ID == id {
				return true
			}
		}
		return false
	}

	// Skill: refused with owner:, applied without.
	_, err = client.RegisterSkill(ctx, &pb.RegisterSkillRequest{CommandId: "s1", Reason: "r", Name: "mine",
		Description: "d", Instructions: "i", ScopeTags: []string{"ops", "owner:alice"}})
	refused("RegisterSkill", err)
	if _, ok := skillReg.Get("mine"); ok || skillStore.saved["mine"] {
		t.Fatal("a refused skill was registered or indexed")
	}
	if _, err := client.RegisterSkill(ctx, &pb.RegisterSkillRequest{CommandId: "s2", Reason: "r", Name: "ops-skill",
		Description: "d", Instructions: "i", ScopeTags: []string{"ops"}}); err != nil {
		t.Fatalf("clean skill: %v", err)
	}
	if _, ok := skillReg.Get("ops-skill"); !ok || !skillStore.saved["ops-skill"] {
		t.Fatal("a clean skill must be registered and indexed")
	}

	// MCP server (contract-0097 MCPServerSpecOp).
	_, err = client.SaveMCPServer(ctx, &pb.SaveMCPServerOpRequest{CommandId: "m1", Reason: "r", Server: &pb.MCPServerSpecOp{
		Id: "crm", Transport: "http", Endpoint: "https://crm.example/mcp", ClassificationTags: []string{"owner:alice"}}})
	refused("SaveMCPServer", err)
	if hasServer("crm") {
		t.Fatal("a refused MCP server was stored")
	}
	resp, err := client.SaveMCPServer(ctx, &pb.SaveMCPServerOpRequest{CommandId: "m2", Reason: "r", Server: &pb.MCPServerSpecOp{
		Id: "crm", Transport: "http", Endpoint: "https://crm.example/mcp", ClassificationTags: []string{"crm"}}})
	if err != nil || len(resp.GetOutcomes()) != 1 || resp.GetOutcomes()[0].GetEffect() == operator.EffectRejected || !hasServer("crm") {
		t.Fatalf("clean MCP server: %+v %v", resp, err)
	}

	// Integration install (catalog with tags, and a custom spec) and update.
	_, err = client.InstallIntegration(ctx, &pb.InstallIntegrationOpRequest{CommandId: "i1", Reason: "r",
		CatalogId: "acme", Id: "acme", ClassificationTags: []string{"crm", "owner:alice"}})
	refused("InstallIntegration catalog", err)
	_, err = client.InstallIntegration(ctx, &pb.InstallIntegrationOpRequest{CommandId: "i2", Reason: "r", Id: "custom1",
		Custom: &pb.IntegrationCustomSpecOp{Transport: "http", Endpoint: "https://x.example/mcp", ClassificationTags: []string{"owner:alice"}}})
	refused("InstallIntegration custom", err)
	if hasServer("acme") || hasServer("custom1") {
		t.Fatal("a refused integration was stored")
	}
	_, err = client.UpdateIntegration(ctx, &pb.UpdateIntegrationOpRequest{CommandId: "u1", Reason: "r", Id: "crm",
		SetTags: true, ClassificationTags: []string{"owner:alice"}})
	refused("UpdateIntegration", err)
	if _, err := client.UpdateIntegration(ctx, &pb.UpdateIntegrationOpRequest{CommandId: "u2", Reason: "r", Id: "crm",
		SetTags: true, ClassificationTags: []string{"crm", "sales"}}); err != nil {
		t.Fatalf("clean update: %v", err)
	}
	for _, s := range rig.src.effectiveMCPServers() {
		if s.ID == "crm" && (len(s.ClassificationTags) != 2 || s.ClassificationTags[1] != "sales") {
			t.Fatalf("clean update did not land: %v", s.ClassificationTags)
		}
	}
}

// A stored console override that already carries an owner tag (written before
// this rule) fails the next boot's config load naming the server — refuse, not
// skip (decision logged on ENG-1046).
func TestDescriptorOwnerTags_StoredOverrideFailsBoot(t *testing.T) {
	dir := bundle(t, map[string]string{})
	storePath := filepath.Join(t.TempDir(), "config.db")
	store, err := storage.OpenConfigStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfgPath := filepath.Join(dir, "config.json")
	if _, _, err := config.LoadConfigWithStore(cfgPath, store); err != nil {
		t.Fatalf("clean boot: %v", err)
	}
	stored := config.StoredMCPServer{MCPServerConfig: config.MCPServerConfig{ID: "crm", Transport: "http",
		Endpoint: "https://crm.example/mcp", ClassificationTags: []string{"owner:alice"}}}
	if err := store.SetOverride(config.MCPServerOverrideKey("crm"), stored); err != nil {
		t.Fatal(err)
	}
	_, _, err = config.LoadConfigWithStore(cfgPath, store)
	if err == nil || !strings.Contains(err.Error(), "mcp.servers[crm].classification_tags") || !strings.Contains(err.Error(), "owner:alice") {
		t.Fatalf("want the boot to fail naming the stored server, got %v", err)
	}
}
