package operator

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// IntegrationManager is the integrations surface (capability "integrations",
// T-20261106-integrations-manage-mcp-servers-from-the): the curated MCP catalog
// and the installed servers, with credentials, live lifecycle and health.
//
// The operator service owns the RPC contract — validation, command_id dedupe,
// audit — and the manager owns the effect. It is implemented in the composition
// root on the SAME store entries and live connector as the contract-0097
// MCPWriter, so there is one MCP server pathway, not two.
//
// Mutations return (outcome, integration-after, error). A rejected change is an
// outcome with Effect "rejected", not an error: the operator asked and got an
// answer. error is reserved for the kernel being unable to act at all.
type IntegrationManager interface {
	Catalog() []*pb.IntegrationCatalogEntryOp
	Categories() []string
	Integrations(ctx context.Context) []*pb.IntegrationOp
	Install(ctx context.Context, req *pb.InstallIntegrationOpRequest) (ConfigWriteOutcome, *pb.IntegrationOp, error)
	Update(ctx context.Context, req *pb.UpdateIntegrationOpRequest) (ConfigWriteOutcome, *pb.IntegrationOp, error)
	SetSecret(ctx context.Context, id, name, value string) (ConfigWriteOutcome, *pb.IntegrationOp, error)
	ClearSecret(ctx context.Context, id, name string) (ConfigWriteOutcome, *pb.IntegrationOp, error)
	SetEnabled(ctx context.Context, id string, enabled bool) (ConfigWriteOutcome, *pb.IntegrationOp, error)
	Remove(ctx context.Context, id string, keepSecrets bool) (ConfigWriteOutcome, error)
	// RevertToFile drops the stored override or tombstone for a server a config
	// file declares, so the file entry applies again, and restarts it live.
	RevertToFile(ctx context.Context, id string) (ConfigWriteOutcome, *pb.IntegrationOp, error)
	Test(ctx context.Context, id string, callProbe bool) (*pb.IntegrationTestResultOp, error)
	// SetTrustTier sets (or, with "", clears) an integration's sandbox trust
	// tier and returns the tier in effect BEFORE the change, for the audit.
	SetTrustTier(ctx context.Context, id, tier string) (ConfigWriteOutcome, *pb.IntegrationOp, string, error)
}

func (s *Service) needIntegrations() error {
	if s.integrations == nil {
		return status.Error(codes.Unimplemented, "this kernel cannot manage integrations")
	}
	return nil
}

// IntegrationHealthToOp maps a health event to its wire shape (feed + reads).
func IntegrationHealthToOp(e domain.MCPServerHealthEvent) *pb.IntegrationHealthOp {
	op := &pb.IntegrationHealthOp{
		ServerId:     e.ServerID,
		State:        e.State,
		ToolCount:    int32(e.ToolCount),
		LastError:    e.LastError,
		RestartCount: int32(e.Restarts),
	}
	if !e.LastHandshake.IsZero() {
		op.LastHandshakeUnixMs = e.LastHandshake.UnixMilli()
	}
	if !e.ChangedAt.IsZero() {
		op.ChangedAtUnixMs = e.ChangedAt.UnixMilli()
	}
	return op
}

// ListIntegrationCatalog returns the installable catalog.
func (s *Service) ListIntegrationCatalog(_ context.Context, _ *pb.ListIntegrationCatalogOpRequest) (*pb.ListIntegrationCatalogOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	return &pb.ListIntegrationCatalogOpResponse{
		Entries:    s.integrations.Catalog(),
		Categories: s.integrations.Categories(),
	}, nil
}

// ListIntegrations returns every installed server with state and health.
func (s *Service) ListIntegrations(ctx context.Context, _ *pb.ListIntegrationsOpRequest) (*pb.ListIntegrationsOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	return &pb.ListIntegrationsOpResponse{Integrations: s.integrations.Integrations(ctx), Configured: true}, nil
}

// integrationMutation runs one audited, deduped mutation and shapes the reply.
func (s *Service) integrationMutation(ctx context.Context, commandID, reason, action, id, after string,
	apply func() (ConfigWriteOutcome, *pb.IntegrationOp, error)) (*pb.IntegrationMutationOpResponse, error) {
	var (
		outcome ConfigWriteOutcome
		after2  *pb.IntegrationOp
	)
	ack, err := s.runMutation(ctx, commandID, reason, action, "integration", id, after, func() error {
		var aerr error
		outcome, after2, aerr = apply()
		return aerr
	})
	if err != nil {
		return nil, err
	}
	resp := &pb.IntegrationMutationOpResponse{CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Integration: after2}
	if !ack.GetDeduped() {
		resp.Outcome = outcomesToOp([]ConfigWriteOutcome{outcome})[0]
	}
	return resp, nil
}

// refuseIntegrationOwnerTags refuses an owner-namespace tag in an integration's
// classification tags — the request's and a custom spec's — with
// INVALID_ARGUMENT naming the tag (ADR-0146 D5, owner decision 2026-11-15,
// ENG-1046). config.ValidateMCPServer refuses the same at boot and on every
// store write; this answers the console with the right code before the audit.
func refuseIntegrationOwnerTags(tags []string, custom *pb.IntegrationCustomSpecOp) error {
	if err := domain.RefuseOwnerTags("classification_tags", tags); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := domain.RefuseOwnerTags("custom.classification_tags", custom.GetClassificationTags()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// SetIntegrationTrustTier sets an integration's ADR-0139 D8 sandbox tier from
// the console (owner decision 2026-11-15: any tier, raise or lower, audited).
// The audit entry records the actor and role (runMutation), the reason, the
// tier before and the tier after
// (T-20261115-console-custom-mcp-server-install-cannot-set).
func (s *Service) SetIntegrationTrustTier(ctx context.Context, req *pb.SetIntegrationTrustTierOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	after := req.GetTrustTier()
	if after == "" {
		after = "(cleared: catalog/default tier)"
	}
	var (
		outcome ConfigWriteOutcome
		op      *pb.IntegrationOp
	)
	// The tier before the change is read inside the mutation, after the
	// command_id dedupe, and written onto the audit entry with the outcome.
	ack, err := s.runMutationBefore(ctx, req.GetCommandId(), req.GetReason(), "set_integration_trust_tier", "integration",
		req.GetId(), "trust_tier "+after, func() (string, error) {
			var before string
			var aerr error
			outcome, op, before, aerr = s.integrations.SetTrustTier(ctx, req.GetId(), req.GetTrustTier())
			if before == "" {
				before = "(catalog/default tier)"
			}
			return "trust_tier " + before, aerr
		})
	if err != nil {
		return nil, err
	}
	resp := &pb.IntegrationMutationOpResponse{CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Integration: op}
	if !ack.GetDeduped() {
		resp.Outcome = outcomesToOp([]ConfigWriteOutcome{outcome})[0]
	}
	return resp, nil
}

// sortedKeys lists map keys (names only — never values) for audit summaries.
func sortedKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// InstallIntegration installs a catalog entry or a custom server.
func (s *Service) InstallIntegration(ctx context.Context, req *pb.InstallIntegrationOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	id := req.GetId()
	if id == "" {
		id = req.GetCatalogId()
	}
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id or catalog_id is required")
	}
	if req.GetCatalogId() == "" && req.GetCustom() == nil {
		return nil, status.Error(codes.InvalidArgument, "catalog_id or custom is required")
	}
	if err := refuseIntegrationOwnerTags(req.GetClassificationTags(), req.GetCustom()); err != nil {
		return nil, err
	}
	src := "catalog " + req.GetCatalogId()
	if req.GetCatalogId() == "" {
		src = "custom " + req.GetCustom().GetTransport() + " " + req.GetCustom().GetEndpoint()
	}
	// The audit record names the settings and secrets that were supplied — the
	// NAMES only. A secret value never reaches the audit log.
	after := "install " + id + " from " + src + " enabled=" + tf(req.GetEnabled()) +
		" settings=[" + sortedKeys(req.GetSettings()) + "] secrets=[" + sortedKeys(req.GetSecrets()) + "]"
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "install_integration", id, after,
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) { return s.integrations.Install(ctx, req) })
}

// UpdateIntegration replaces an integration's non-secret settings (and, for a
// custom one, its spec).
func (s *Service) UpdateIntegration(ctx context.Context, req *pb.UpdateIntegrationOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := refuseIntegrationOwnerTags(req.GetClassificationTags(), req.GetCustom()); err != nil {
		return nil, err
	}
	after := "update " + req.GetId() + " settings=[" + sortedKeys(req.GetSettings()) + "]"
	if req.GetCustom() != nil {
		after += " spec=" + req.GetCustom().GetTransport() + " " + req.GetCustom().GetEndpoint()
	}
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "update_integration", req.GetId(), after,
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) { return s.integrations.Update(ctx, req) })
}

// SetIntegrationSecret stores one credential (write-only).
func (s *Service) SetIntegrationSecret(ctx context.Context, req *pb.SetIntegrationSecretOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" || req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "id and name are required")
	}
	if req.GetValue() == "" {
		return nil, status.Error(codes.InvalidArgument, "value is required; use ClearIntegrationSecret to remove one")
	}
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "set_integration_secret", req.GetId(),
		"secret "+req.GetName()+" set for integration "+req.GetId(),
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) {
			return s.integrations.SetSecret(ctx, req.GetId(), req.GetName(), req.GetValue())
		})
}

// ClearIntegrationSecret removes one stored credential.
func (s *Service) ClearIntegrationSecret(ctx context.Context, req *pb.ClearIntegrationSecretOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" || req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "id and name are required")
	}
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "clear_integration_secret", req.GetId(),
		"secret "+req.GetName()+" cleared for integration "+req.GetId(),
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) {
			return s.integrations.ClearSecret(ctx, req.GetId(), req.GetName())
		})
}

// SetIntegrationEnabled starts or stops an integration.
func (s *Service) SetIntegrationEnabled(ctx context.Context, req *pb.SetIntegrationEnabledOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "set_integration_enabled", req.GetId(),
		"integration "+req.GetId()+" enabled="+tf(req.GetEnabled()),
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) {
			return s.integrations.SetEnabled(ctx, req.GetId(), req.GetEnabled())
		})
}

// RemoveIntegration uninstalls an integration.
func (s *Service) RemoveIntegration(ctx context.Context, req *pb.RemoveIntegrationOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "remove_integration", req.GetId(),
		"integration "+req.GetId()+" removed keep_secrets="+tf(req.GetKeepSecrets()),
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) {
			o, err := s.integrations.Remove(ctx, req.GetId(), req.GetKeepSecrets())
			return o, nil, err
		})
}

// RevertIntegrationToFile drops the console's override (or removal) of a
// config-file server so the file entry applies again, restarting it live.
func (s *Service) RevertIntegrationToFile(ctx context.Context, req *pb.RevertIntegrationToFileOpRequest) (*pb.IntegrationMutationOpResponse, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	return s.integrationMutation(ctx, req.GetCommandId(), req.GetReason(), "revert_integration_to_file", req.GetId(),
		"integration "+req.GetId()+" reverted to its config-file entry",
		func() (ConfigWriteOutcome, *pb.IntegrationOp, error) {
			return s.integrations.RevertToFile(ctx, req.GetId())
		})
}

// TestIntegration handshakes an installed integration. Operator-only by its
// name (not a read prefix): it can launch a process.
func (s *Service) TestIntegration(ctx context.Context, req *pb.TestIntegrationOpRequest) (*pb.IntegrationTestResultOp, error) {
	if err := s.needIntegrations(); err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	// ADR-0140 D4, as in ExecuteToolOp: an operator testing an integration
	// tests it FOR themselves, so a per_user integration is tested on the
	// caller's own connected account
	// (T-20261106-testintegration-and-the-catalog-op-do-not).
	if me, _, ok := PrincipalFromContext(ctx); ok && me != "" {
		ctx = domain.WithRunPrincipal(ctx, me)
	}
	return s.integrations.Test(ctx, req.GetId(), req.GetCallProbe())
}

func tf(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
