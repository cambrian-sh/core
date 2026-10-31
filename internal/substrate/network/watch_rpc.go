package network

import (
	"context"

	pb "github.com/cambrian-sh/core/api/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Orchestrator watch-CRUD RPCs are RETIRED IN PLACE (owner decision
// 2026-10-31, T-20261031-orchestrator-watch-crud-registerwatch).
//
// They predate the operator plane (ADR-0032) and duplicated its watch commands
// on the agent-facing service with no authentication, no audit row, no reason
// and no command-id dedup: any client that reached the port could list, delete,
// deactivate or register watches. Nothing calls them — the console and the
// benchmark harness use OperatorConsole/{Register,List,Delete}Watch and
// SetWatchActive (ADR-0047 A2), and the SDK uses Orchestrator only for memory,
// artifacts and SignalStream.
//
// They stay declared in cambrian.proto because the proto surface is a stable
// v0.x contract (ADR-0057 D8); removing them waits for a contract bump. Until
// then the methods are operator-gated (authz.operatorGatedAgentServiceMethods)
// AND refuse every call, so there is exactly one way to change a watch, and it
// is audited. A second, unaudited operator door would be a duplicate pathway
// (AGENTS.md §4.3).

// errWatchCRUDRetired names the replacement so a caller knows where to go.
func errWatchCRUDRetired(method string) error {
	return status.Errorf(codes.FailedPrecondition,
		"Orchestrator/%s is retired: use OperatorConsole/%s, which is authenticated and audited", method, method)
}

// RegisterWatch is retired; see the file comment.
func (s *Server) RegisterWatch(context.Context, *pb.RegisterWatchRequest) (*pb.RegisterWatchResponse, error) {
	return nil, errWatchCRUDRetired("RegisterWatch")
}

// ListWatches is retired; see the file comment.
func (s *Server) ListWatches(context.Context, *pb.ListWatchesRequest) (*pb.ListWatchesResponse, error) {
	return nil, errWatchCRUDRetired("ListWatches")
}

// DeleteWatch is retired; see the file comment.
func (s *Server) DeleteWatch(context.Context, *pb.DeleteWatchRequest) (*pb.DeleteWatchResponse, error) {
	return nil, errWatchCRUDRetired("DeleteWatch")
}

// SetWatchActive is retired; see the file comment.
func (s *Server) SetWatchActive(context.Context, *pb.SetWatchActiveRequest) (*pb.SetWatchActiveResponse, error) {
	return nil, errWatchCRUDRetired("SetWatchActive")
}
