package operator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// MemorySharer applies the ADR-0142 D12 publish: a person's private
// (owner:<person>) document is re-classified to team/org tags. Plan checks
// without changing anything; Apply performs it. Both refuse a document the
// person does not own (ErrShareNotOwner) and target tags outside what the
// person may write (ErrShareNotPermitted).
type MemorySharer interface {
	PlanShare(ctx context.Context, person domain.PrincipalRef, docID string, target []string) ([]string, error)
	ApplyShare(ctx context.Context, person domain.PrincipalRef, docID string, target []string) ([]string, error)
}

// Share refusals.
var (
	// ErrShareNotOwner: no such document, or it is not the caller's private
	// material (one answer for both: no existence leak).
	ErrShareNotOwner = errors.New("no private document of yours with that id")
	// ErrShareNotPermitted: the target classification is outside what the
	// person may write.
	ErrShareNotPermitted = errors.New("the target tags are not within what you may write")
)

// SetMemorySharer wires the publish effect. nil ⇒ ShareToTeam is Unimplemented.
func (s *Service) SetMemorySharer(m MemorySharer) { s.sharer = m }

// ShareRequestPreview encodes a share's target tags on its approval item, and
// ShareTargetsOf decodes them (the hub's publish handler reads it back).
func ShareRequestPreview(target []string) string {
	b, _ := json.Marshal(map[string][]string{"target_tags": target})
	return string(b)
}

// ShareTargetsOf decodes ShareRequestPreview.
func ShareTargetsOf(preview string) []string {
	var v map[string][]string
	if err := json.Unmarshal([]byte(preview), &v); err != nil {
		return nil
	}
	return v["target_tags"]
}

// ShareToTeam publishes one of the caller's private documents (ADR-0142 D12).
// The approval policy for class "publish" decides: under the shipped default
// (self) the caller's own request is the approval and it applies at once; under
// a stricter policy (role:compliance, …) it waits as an addressed approval and
// applies when an approver answers. Audited either way.
func (s *Service) ShareToTeam(ctx context.Context, req *pb.ShareToTeamOpRequest) (*pb.ShareToTeamOpResponse, error) {
	if s.sharer == nil || s.hitl == nil {
		return nil, status.Error(codes.Unimplemented, "sharing is not wired on this kernel")
	}
	me, ok := callerApprover(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "sharing requires an authenticated caller")
	}
	target := cleanTags(req.GetTargetTags())
	if req.GetCommandId() == "" || req.GetReason() == "" || req.GetDocumentId() == "" || len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason, document_id and target_tags are required")
	}
	planned, err := s.sharer.PlanShare(ctx, me.Principal, req.GetDocumentId(), target)
	if err != nil {
		return nil, shareStatus(err)
	}
	actor, role, _ := PrincipalFromContext(ctx)
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "share_to_team",
		TargetType: "document", TargetID: req.GetDocumentId(), After: strings.Join(target, ","),
		Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	resp := &pb.ShareToTeamOpResponse{CommandId: req.GetCommandId(), Deduped: deduped}
	it, err := s.hitl.Raise(ctx, domain.ApprovalRequest{
		Kind:        domain.ApprovalKindPublish,
		ToolName:    "share_to_team",
		Object:      req.GetDocumentId(),
		Resource:    req.GetDocumentId(),
		ArgsPreview: ShareRequestPreview(target),
		Beneficiary: me.Principal,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "share approval: %v", err)
	}
	if it.State == domain.ApprovalPending {
		if ok, _ := s.hitl.Answerable(ctx, it, me); ok {
			// The caller is an approver of their own publish (default: self): the
			// explicit request IS the approval, recorded on the hub like any other.
			if it, err = s.hitl.Answer(ctx, it.ID, domain.ApprovalAnswer{Approved: true, By: me, Reason: req.GetReason()}); err != nil && !errors.Is(err, domain.ErrApprovalResolved) {
				return nil, status.Errorf(codes.Unavailable, "share approval: %v", err)
			}
		}
	}
	resp.ApprovalId = it.ID
	cur, _ := s.hitl.Get(ctx, it.ID)
	switch {
	case cur.State == domain.ApprovalApproved && strings.HasPrefix(cur.Detail, "approved, but applying it failed"):
		return nil, status.Error(codes.Internal, cur.Detail)
	case cur.State == domain.ApprovalApproved:
		resp.State, resp.Tags = "shared", planned
	case cur.State == domain.ApprovalPending:
		resp.State = "pending_approval"
	default:
		resp.State = string(cur.State)
	}
	return resp, nil
}

func cleanTags(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func shareStatus(err error) error {
	switch {
	case errors.Is(err, ErrShareNotOwner):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrShareNotPermitted):
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Errorf(codes.Unavailable, "share: %v", err)
}
