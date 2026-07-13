package network

import (
	"context"
	"errors"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ArtifactByteStore is the content-addressable byte store (ArtifactVault).
type ArtifactByteStore interface {
	Store(content []byte) (string, error)
	Load(hash string) ([]byte, error)
}

// ArtifactMetaStore persists/reads artifact metadata records (incl. tags).
type ArtifactMetaStore interface {
	SaveArtifact(a domain.Artifact) error
	GetArtifact(hash string) (*domain.Artifact, error)
	ListStepArtifacts(sessionID string, stepIndex int) ([]domain.Artifact, error)
}

// ArtifactScopeResolver resolves an agent's effective READ scope and its
// operator-configured write classification (ADR-0035 C2) for artifact access.
type ArtifactScopeResolver interface {
	EffectiveForAgent(ctx context.Context, agentID string) (*domain.EffectiveScope, bool)
	EffectiveForCaller(ctx context.Context, agentID string, caller domain.ScopeConfig) (*domain.EffectiveScope, bool)
	DefaultWriteTags(ctx context.Context, agentID string) []string
}

// ArtifactSessionScopes returns the non-forgeable caller_scope persisted on a
// session, for Phase-2 re-derivation parity with QueryMemory. Optional.
type ArtifactSessionScopes interface {
	CallerScope(ctx context.Context, sessionID string) domain.ScopeConfig
}

// effectiveReadScope resolves the caller's effective READ scope, honoring Phase-2
// session caller_scope when present (mirrors QueryService.resolveScope).
func (s *Server) effectiveReadScope(ctx context.Context, agentID string) (*domain.EffectiveScope, bool) {
	if s.ArtifactSessions != nil {
		if sid, ok := domain.SessionIDFromContext(ctx); ok {
			if caller := s.ArtifactSessions.CallerScope(ctx, sid); !caller.IsZero() {
				return s.ArtifactScopes.EffectiveForCaller(ctx, agentID, caller)
			}
		}
	}
	return s.ArtifactScopes.EffectiveForAgent(ctx, agentID)
}

func callerAgentID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-agent-id"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// UploadArtifact derives the artifact's kernel-authoritative classification from
// the agent's operator-configured DefaultWriteTags (narrowed only by req.Tags),
// kernel-stamps provenance, and stores bytes (CAS) + metadata. The agent cannot
// choose its own classification — only narrow. ADR-0035 (C2) / REQ-SDK-007c.
func (s *Server) UploadArtifact(ctx context.Context, req *pb.UploadArtifactRequest) (*pb.UploadArtifactResponse, error) {
	if s.ArtifactBytes == nil || s.ArtifactMeta == nil || s.ArtifactScopes == nil {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured")
	}
	agentID := callerAgentID(ctx)
	if _, ok := s.ArtifactScopes.EffectiveForAgent(ctx, agentID); !ok {
		return nil, status.Error(codes.PermissionDenied, "unknown principal: "+agentID)
	}

	// Kernel-derived classification: DefaultWriteTags narrowed by req.Tags (hint).
	defaultWriteTags := s.ArtifactScopes.DefaultWriteTags(ctx, agentID)
	tags, err := scope.AuthorizeArtifactWrite(
		scope.WriterScope{WriterID: agentID, DefaultWriteTags: defaultWriteTags}, s.ArtifactVocab, req.GetTags())
	if err != nil {
		if errors.Is(err, scope.ErrUnknownClassification) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	hash, err := s.ArtifactBytes.Store(req.GetContent())
	if err != nil {
		return nil, status.Error(codes.Internal, "vault store: "+err.Error())
	}
	art := domain.Artifact{
		Hash:            hash,
		ContentType:     req.GetContentType(),
		SizeBytes:       int64(len(req.GetContent())),
		SessionID:       req.GetSessionId(),
		StepIndex:       int(req.GetStepIndex()),
		SemanticSummary: req.GetSemanticSummary(),
		Tags:            tags,
	}
	if err := s.ArtifactMeta.SaveArtifact(art); err != nil {
		return nil, status.Error(codes.Internal, "artifact record: "+err.Error())
	}
	return &pb.UploadArtifactResponse{Hash: hash, Tags: tags}, nil
}

// GetArtifact returns artifact bytes only when the caller's effective scope permits
// the artifact's tags. A scope-denied artifact is reported as found=false —
// indistinguishable from absent, so the existence of out-of-scope data does not
// leak. ADR-0034 (D12).
func (s *Server) GetArtifact(ctx context.Context, req *pb.GetArtifactRequest) (*pb.GetArtifactResponse, error) {
	if s.ArtifactBytes == nil || s.ArtifactMeta == nil || s.ArtifactScopes == nil {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured")
	}
	agentID := callerAgentID(ctx)
	eff, ok := s.effectiveReadScope(ctx, agentID)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "unknown principal: "+agentID)
	}
	art, err := s.ArtifactMeta.GetArtifact(req.GetHash())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if art == nil || !scope.ArtifactReadable(eff, *art) {
		return &pb.GetArtifactResponse{Found: false}, nil // fail-closed / not found
	}
	content, err := s.ArtifactBytes.Load(art.Hash)
	if err != nil {
		return nil, status.Error(codes.Internal, "vault load: "+err.Error())
	}
	return &pb.GetArtifactResponse{
		Content:     content,
		ContentType: art.ContentType,
		Tags:        art.Tags,
		Found:       true,
	}, nil
}

// ListStepArtifacts returns the scope-filtered metadata of artifacts for a
// session+step. Out-of-scope artifacts are silently omitted. ADR-0034 (D12).
func (s *Server) ListStepArtifacts(ctx context.Context, req *pb.ListStepArtifactsRequest) (*pb.ListStepArtifactsResponse, error) {
	if s.ArtifactMeta == nil || s.ArtifactScopes == nil {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured")
	}
	agentID := callerAgentID(ctx)
	eff, ok := s.effectiveReadScope(ctx, agentID)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "unknown principal: "+agentID)
	}
	arts, err := s.ArtifactMeta.ListStepArtifacts(req.GetSessionId(), int(req.GetStepIndex()))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	visible := scope.FilterArtifactsByScope(eff, arts)
	out := make([]*pb.ArtifactMeta, 0, len(visible))
	for _, a := range visible {
		out = append(out, &pb.ArtifactMeta{
			Hash:            a.Hash,
			ContentType:     a.ContentType,
			SizeBytes:       a.SizeBytes,
			Tags:            a.Tags,
			SemanticSummary: a.SemanticSummary,
		})
	}
	return &pb.ListStepArtifactsResponse{Artifacts: out}, nil
}
