package signal

import (
	"context"
	"errors"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/metadata"
)

// ManagerAccess is what signal needs from the agent manager to look up instances
// by auth token. Both OSS Watcher and premium ReactiveEngine supply one. ADR-0032.
//
// An ALIAS of the domain port, not a copy of its method set (AGENTS.md §3.1): it
// uses every method of domain.AgentManager, so a restated interface would only
// drift from the port silently.
type ManagerAccess = domain.AgentManager

// ValidateToken extracts the Bearer token from gRPC metadata and validates it
// against the manager's instance registry. Returns the matching Instance or
// an error describing the validation failure.
func ValidateToken(ctx context.Context, manager ManagerAccess) (*domain.Instance, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, errors.New("token: missing gRPC metadata")
	}

	authHeader := ""
	if vals := md.Get("authorization"); len(vals) > 0 {
		authHeader = vals[0]
	}

	if authHeader == "" {
		return nil, errors.New("token: missing authorization header")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == authHeader || token == "" {
		return nil, errors.New("token: invalid Bearer token format")
	}

	inst := manager.FindInstanceByToken(token)
	if inst == nil {
		return nil, errors.New("token: unknown instance")
	}
	return inst, nil
}
