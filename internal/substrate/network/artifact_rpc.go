package network

import (
	"context"
	"errors"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

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

// artifactPrincipal is the identity the artifact plane acts as. It comes from the
// authenticated gRPC metadata, never from the request body (INV-5).
func artifactPrincipal(ctx context.Context) domain.PrincipalRef {
	return domain.AgentPrincipal(callerAgentID(ctx))
}

// artifactSurface is the surface the artifact plane presents to the decision
// point: the agent-facing gRPC plane.
var artifactSurface = domain.SurfaceRef{Kind: domain.SurfaceAgent}

// authorizer returns the server's decision point, defaulting to the OSS allow-all.
// The kernel always asks; only the answer differs.
func (s *Server) authorizer() domain.Authorizer {
	if s.Authz == nil {
		return domain.AllowAllAuthorizer{}
	}
	return s.Authz
}

func callerAgentID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-agent-id"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// UploadArtifact derives the artifact's authoritative classification from the
// decision point (the agent's operator-configured write classification, narrowed
// only by req.Tags), stamps provenance, and stores bytes (CAS) + metadata. The
// agent cannot choose its own classification — only narrow. ADR-0035 (C2) /
// REQ-SDK-007c.
func (s *Server) UploadArtifact(ctx context.Context, req *pb.UploadArtifactRequest) (*pb.UploadArtifactResponse, error) {
	if s.ArtifactBytes == nil || s.ArtifactMeta == nil {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured")
	}
	principal := artifactPrincipal(ctx)
	tags, err := authz.ClassifyArtifactWrite(ctx, s.authorizer(), principal, req.GetTags())
	if err != nil {
		if errors.Is(err, authz.ErrWriteDenied) {
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

// GetArtifact returns artifact bytes only when the caller's predicate permits the
// artifact's tags. A denied artifact is reported as found=false — indistinguishable
// from absent, so the existence of out-of-reach data does not leak.
func (s *Server) GetArtifact(ctx context.Context, req *pb.GetArtifactRequest) (*pb.GetArtifactResponse, error) {
	if s.ArtifactBytes == nil || s.ArtifactMeta == nil {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured")
	}
	principal := artifactPrincipal(ctx)
	eff, dec := s.authorizer().ReadFilter(ctx, principal, artifactSurface)
	if eff == nil {
		return nil, status.Error(codes.PermissionDenied, dec.Explain())
	}
	art, err := s.ArtifactMeta.GetArtifact(req.GetHash())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if art == nil || !authz.ArtifactReadable(eff, *art) {
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

// ListStepArtifacts returns the filtered metadata of artifacts for a session+step.
// Out-of-reach artifacts are silently omitted.
func (s *Server) ListStepArtifacts(ctx context.Context, req *pb.ListStepArtifactsRequest) (*pb.ListStepArtifactsResponse, error) {
	if s.ArtifactMeta == nil {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured")
	}
	principal := artifactPrincipal(ctx)
	eff, dec := s.authorizer().ReadFilter(ctx, principal, artifactSurface)
	if eff == nil {
		return nil, status.Error(codes.PermissionDenied, dec.Explain())
	}
	arts, err := s.ArtifactMeta.ListStepArtifacts(req.GetSessionId(), int(req.GetStepIndex()))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	visible := authz.FilterArtifacts(eff, arts)
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
