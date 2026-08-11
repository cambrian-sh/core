package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// MCPWriter is the write half of the MCP server surface (contract 0097).
//
// Like the generator writer it lands on the ADR-0101 store — the whole server
// list under one key, because koanf replaces lists wholesale — but unlike
// generators the implementation also applies LIVE: the connector can arm and
// drop servers at runtime, so a save's effect is `live`, not restart_required.
type MCPWriter interface {
	// SaveMCPServer creates or replaces one server by id, stores the whole
	// effective list, and arms the server's health/reconnect loop.
	SaveMCPServer(spec MCPServerSpec) (ConfigWriteOutcome, error)
	// RemoveMCPServer drops one server: store, session, watch loop and tools.
	// Removing an id the kernel does not have is an error, for the same reason
	// RemoveGenerator's is — silence hides a typo that leaves the real server
	// serving.
	RemoveMCPServer(id string) (ConfigWriteOutcome, error)
	// SetMCPServerToken stores the server's credential and bounces its
	// connection so the new token is actually used.
	SetMCPServerToken(id, token string) (ConfigWriteOutcome, error)
	// ClearMCPServerToken removes the stored credential (and bounces).
	ClearMCPServerToken(id string) error
	// TestMCPServer dials the spec once, ephemeral, never touching a live
	// session for the same id.
	TestMCPServer(ctx context.Context, spec MCPServerSpec) MCPTestResult
}

// MCPServerSpec is the DECLARED half of an MCP server — what an operator
// authors. Connection state, tool counts and credentials never appear here.
type MCPServerSpec struct {
	ID                 string
	Transport          string // "stdio" | "http" | "sse"
	Endpoint           string
	Args               []string
	AuthType           string // "none" | "bearer" | "header"
	AuthHeader         string
	ClassificationTags []string
}

// MCPTestResult is what one ephemeral connection attempt learned.
type MCPTestResult struct {
	OK        bool
	LatencyMs int64
	ToolNames []string
	Error     string
}

// SetMCPWriter wires the MCP write path. nil ⇒ Unimplemented.
func (s *Service) SetMCPWriter(w MCPWriter) { s.mcpWriter = w }

func mcpSpecFromOp(g *pb.MCPServerSpecOp) MCPServerSpec {
	return MCPServerSpec{
		ID:                 g.GetId(),
		Transport:          g.GetTransport(),
		Endpoint:           g.GetEndpoint(),
		Args:               g.GetArgs(),
		AuthType:           g.GetAuthType(),
		AuthHeader:         g.GetAuthHeader(),
		ClassificationTags: g.GetClassificationTags(),
	}
}

// SaveMCPServer creates or replaces an MCP server. Mutating: command_id +
// reason, audited, idempotent on command_id.
func (s *Service) SaveMCPServer(ctx context.Context, req *pb.SaveMCPServerOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.mcpWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot persist MCP server changes")
	}
	g := req.GetServer()
	if g == nil || g.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "server.id is required")
	}
	// Validated here rather than left to the store, for the generator reason: a
	// server missing an endpoint is accepted by the config loader and fails at
	// connect time, far from the edit that caused it.
	if g.GetTransport() == "" {
		return nil, status.Error(codes.InvalidArgument, "server.transport is required (stdio|http|sse)")
	}
	if g.GetEndpoint() == "" {
		return nil, status.Error(codes.InvalidArgument, "server.endpoint is required")
	}

	spec := mcpSpecFromOp(g)
	var outcome ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"save_mcp_server", "mcp_server", spec.ID,
		"mcp server "+spec.ID+" → "+spec.Transport+" "+spec.Endpoint,
		func() error {
			var applyErr error
			outcome, applyErr = s.mcpWriter.SaveMCPServer(spec)
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp([]ConfigWriteOutcome{outcome}),
	}, nil
}

// RemoveMCPServer drops a server from the stored list and the running kernel.
func (s *Service) RemoveMCPServer(ctx context.Context, req *pb.RemoveMCPServerOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.mcpWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot persist MCP server changes")
	}
	if req.GetServerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "server_id is required")
	}

	var outcome ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"remove_mcp_server", "mcp_server", req.GetServerId(),
		"mcp server "+req.GetServerId()+" removed",
		func() error {
			var applyErr error
			outcome, applyErr = s.mcpWriter.RemoveMCPServer(req.GetServerId())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp([]ConfigWriteOutcome{outcome}),
	}, nil
}

// SetMCPServerToken stores a server credential. The token never appears in the
// audit record — only the fact that one was set.
func (s *Service) SetMCPServerToken(ctx context.Context, req *pb.SetMCPServerTokenOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.mcpWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot store MCP credentials")
	}
	if req.GetServerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "server_id is required")
	}
	if req.GetToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "token is required; use ClearMCPServerToken to remove one")
	}

	var outcome ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"set_mcp_server_token", "mcp_server", req.GetServerId(),
		"credential stored for mcp server "+req.GetServerId(),
		func() error {
			var applyErr error
			outcome, applyErr = s.mcpWriter.SetMCPServerToken(req.GetServerId(), req.GetToken())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp([]ConfigWriteOutcome{outcome}),
	}, nil
}

// ClearMCPServerToken removes a stored credential.
func (s *Service) ClearMCPServerToken(ctx context.Context, req *pb.ClearMCPServerTokenOpRequest) (*pb.CommandAck, error) {
	if s.mcpWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot store MCP credentials")
	}
	if req.GetServerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "server_id is required")
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"clear_mcp_server_token", "mcp_server", req.GetServerId(),
		"credential cleared for mcp server "+req.GetServerId(),
		func() error { return s.mcpWriter.ClearMCPServerToken(req.GetServerId()) })
}

// TestMCPServer dials the submitted spec once. A failed probe is a SUCCESSFUL
// RPC carrying ok=false — the operator asked "does this work?" and got an
// answer (the TestGenerator precedent).
func (s *Service) TestMCPServer(ctx context.Context, req *pb.TestMCPServerOpRequest) (*pb.MCPServerTestResultOp, error) {
	if s.mcpWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot probe MCP servers")
	}
	g := req.GetServer()
	if g == nil || g.GetTransport() == "" || g.GetEndpoint() == "" {
		return nil, status.Error(codes.InvalidArgument, "server.transport and server.endpoint are required")
	}
	res := s.mcpWriter.TestMCPServer(ctx, mcpSpecFromOp(g))
	return &pb.MCPServerTestResultOp{
		Ok:        res.OK,
		LatencyMs: res.LatencyMs,
		ToolNames: res.ToolNames,
		Error:     res.Error,
	}, nil
}
