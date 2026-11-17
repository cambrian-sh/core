package network

import (
	"context"
	"encoding/json"
	"errors"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/ledger"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The artifact plane (REQ-SDK-007c) over the run ledger — ADR-0157 D5.2: the
// ledger is the ONE artifact registry. UploadArtifact records a kind=artifact
// entry through the ledger's classified writer (the vault keeps the bytes,
// content-addressed and stored once); GetArtifact and ListStepArtifacts read
// through the ENFORCED ledger reader under the caller's own predicate. The
// retired bbolt metadata bucket keyed artifacts by content hash, so a second
// upload of the same bytes overwrote the first owner's tags
// (T-20261115-artifact-metadata-is-keyed-by-content-hash-so); a ledger entry's id
// includes its provenance, so two owners' uploads of the same bytes are two
// entries with their own tags.
//
// Wire contract (Q5: unary, no proto change): UploadArtifactResponse.hash and
// GetArtifactRequest.hash carry the artifact REFERENCE — a ledger id (led_…) —
// not a content hash. Bytes are capped by execution.ledger.artifacts.max_file_bytes
// (≤ the vault's 100 MiB ceiling); streaming stays out until a model file needs more.

// artifactToolName is the ledger tool name of an UploadArtifact entry.
const artifactToolName = "upload_artifact"

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

// artifactProvenance is where an uploaded artifact comes from: the lease the
// caller presented (x-lease-id), else the last lease the calling INSTANCE
// presented on this plane (instanceWork) — never the request's session_id /
// step_index, which an agent writes (INV-5). Without either it is attributed to
// the caller alone (StepIndex -1).
func (s *Server) artifactProvenance(ctx context.Context) domain.LedgerProvenance {
	if lease := callerLeaseID(ctx, nil); lease != "" {
		if _, known := s.resolveLeaseFor(ctx, lease); known {
			return s.toolCallProvenance(ctx, lease)
		}
	}
	if b, ok := s.instanceWork().lastFor(domain.AgentInstanceFromContext(ctx)); ok {
		return s.provenanceOfBinding(ctx, b)
	}
	return domain.LedgerProvenance{StepIndex: -1, Owner: domain.ConversationOwnerFromContext(ctx),
		ConversationID: domain.ConversationIDFromContext(ctx)}
}

// UploadArtifact records the bytes as a kind=artifact run-ledger entry. The
// ledger writer derives the classification from the decision point (the agent's
// operator-configured write classification, narrowed only by req.Tags; ADR-0035
// C2), stamps the conversation owner (ADR-0142 D12), and attributes the entry to
// the calling agent and its instance from the verified context (ADR-0157 D5.2).
// The agent cannot choose its own classification — only narrow.
func (s *Server) UploadArtifact(ctx context.Context, req *pb.UploadArtifactRequest) (*pb.UploadArtifactResponse, error) {
	if !s.ledgerOn() {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured (execution.ledger.enabled is off)")
	}
	// ADR-0142 D12: an artifact uploaded while working a person's conversation
	// is private to that person (the owner comes from the lease, never the request).
	ctx = s.withConversationOwner(ctx)
	principal := artifactPrincipal(ctx)
	// A hint the decision point refuses (a coined or broadening tag) is refused
	// HERE with InvalidArgument: the ledger writer would otherwise drop the hint and
	// file the bytes under the writer's full — broader — classification, the
	// opposite of what the agent asked for.
	if _, err := authz.ClassifyArtifactWrite(ctx, s.authorizer(), principal, req.GetTags()); err != nil {
		if errors.Is(err, authz.ErrWriteDenied) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	args, _ := json.Marshal(map[string]string{"summary": req.GetSemanticSummary()})
	ent, err := s.Ledger.Writer.Record(ctx, domain.LedgerRecord{
		Kind:               domain.LedgerArtifact,
		Provenance:         s.artifactProvenance(ctx),
		ToolName:           artifactToolName,
		ArgsJSON:           args,
		Writer:             principal,
		InstanceID:         domain.AgentInstanceFromContext(ctx),
		ClassificationHint: req.GetTags(),
		ContentType:        req.GetContentType(),
		Body:               req.GetContent(),
	})
	switch {
	case errors.Is(err, domain.ErrArtifactTooLarge):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ledger.ErrUnclassifiable):
		// The decision point refused the writer (or its hint) outright.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, "artifact record: "+err.Error())
	}
	return &pb.UploadArtifactResponse{Hash: ent.ID, Tags: ent.Tags}, nil
}

// artifactReadCtx puts the caller's read predicate on ctx for the enforced
// ledger reader (AGENTS.md §2.4: a by-id read's only scope channel). nil ⇒ the
// decision point denied the caller outright.
func (s *Server) artifactReadCtx(ctx context.Context) (context.Context, error) {
	principal := artifactPrincipal(ctx)
	eff, dec := s.authorizer().ReadFilter(ctx, principal, artifactSurface)
	if eff == nil {
		return nil, status.Error(codes.PermissionDenied, dec.Explain())
	}
	return domain.WithScope(ctx, eff), nil
}

// GetArtifact returns an artifact's bytes only when the caller's predicate
// permits the entry (its own tags and every classification it was derived
// from). A denied artifact is reported as found=false — indistinguishable from
// absent, so the existence of out-of-reach data does not leak.
func (s *Server) GetArtifact(ctx context.Context, req *pb.GetArtifactRequest) (*pb.GetArtifactResponse, error) {
	if !s.ledgerOn() {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured (execution.ledger.enabled is off)")
	}
	rctx, err := s.artifactReadCtx(ctx)
	if err != nil {
		return nil, err
	}
	id := req.GetHash()
	if !domain.IsLedgerID(id) {
		return &pb.GetArtifactResponse{Found: false}, nil
	}
	e, err := s.Ledger.Reader.Get(rctx, id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if e == nil || e.Kind != domain.LedgerArtifact {
		return &pb.GetArtifactResponse{Found: false}, nil // fail-closed / not found
	}
	return &pb.GetArtifactResponse{
		Content:     e.Body,
		ContentType: e.ContentType,
		Tags:        e.Tags,
		Found:       true,
	}, nil
}

// ListStepArtifacts returns the artifact entries recorded under a task session
// and step, scope-filtered for the caller by the enforced reader. Out-of-reach
// artifacts are silently omitted.
func (s *Server) ListStepArtifacts(ctx context.Context, req *pb.ListStepArtifactsRequest) (*pb.ListStepArtifactsResponse, error) {
	if !s.ledgerOn() {
		return nil, status.Error(codes.Unimplemented, "artifact storage not configured (execution.ledger.enabled is off)")
	}
	if req.GetSessionId() == "" {
		return &pb.ListStepArtifactsResponse{}, nil
	}
	rctx, err := s.artifactReadCtx(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.Ledger.Reader.Find(rctx, domain.LedgerQuery{SessionID: req.GetSessionId(),
		StepIndex: int(req.GetStepIndex()), HasStep: true, Kind: domain.LedgerArtifact})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*pb.ArtifactMeta, 0, len(rows))
	for _, e := range rows {
		out = append(out, &pb.ArtifactMeta{
			Hash:            e.ID,
			ContentType:     e.ContentType,
			SizeBytes:       e.SizeBytes,
			Tags:            e.Tags,
			SemanticSummary: e.Preview,
		})
	}
	return &pb.ListStepArtifactsResponse{Artifacts: out}, nil
}
