package operator

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// DeliveryIntentSource is the two-phase delivery journal the operator plane reads
// and resolves (ENG-1657, ADR-0090 D8.1; ingress.DeliveryService).
type DeliveryIntentSource interface {
	ListIntents(ctx context.Context, states []domain.DeliveryIntentState, limit int) ([]domain.DeliveryIntent, error)
	ResolveAmbiguous(ctx context.Context, key, action, reason string) error
}

const (
	deliveryIntentsDefault = 100
	deliveryIntentsMax     = 1000
)

// ListDeliveryIntents lists outbound delivery intents (contract 0118, capability
// "delivery-intents"): by default the ambiguous and failed ones a person may need
// to look at. Identifiers, state, attempts and the error only — never the message
// text (only its length): the conversation is its owner's.
func (s *Service) ListDeliveryIntents(ctx context.Context, req *pb.ListDeliveryIntentsOpRequest) (*pb.ListDeliveryIntentsOpResponse, error) {
	if s.deliveryIntents == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel runs no delivery journal")
	}
	var states []domain.DeliveryIntentState
	for _, st := range req.GetStates() {
		switch domain.DeliveryIntentState(st) {
		case domain.DeliveryPrepared, domain.DeliveryDispatched, domain.DeliveryConfirmed, domain.DeliveryAmbiguous, domain.DeliveryFailed:
			states = append(states, domain.DeliveryIntentState(st))
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown delivery state %q", st)
		}
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = deliveryIntentsDefault
	}
	if limit > deliveryIntentsMax {
		limit = deliveryIntentsMax
	}
	intents, err := s.deliveryIntents.ListIntents(ctx, states, limit)
	if err != nil {
		if errors.Is(err, domain.ErrDeliveryJournalOff) {
			return nil, status.Error(codes.Unimplemented, err.Error())
		}
		return nil, status.Errorf(codes.Unavailable, "delivery intents: %v", err)
	}
	resp := &pb.ListDeliveryIntentsOpResponse{}
	for _, in := range intents {
		op := &pb.DeliveryIntentOp{
			Key: in.Key, ConversationId: in.ConversationID, TxnId: in.TxnID,
			IngressAgentId: in.Address.IngressAgentID, ExternalId: in.Address.ExternalID,
			State: string(in.State), Attempts: int32(in.Attempts), Error: in.Err,
			TextChars: int32(len([]rune(in.Text))),
		}
		if !in.CreatedAt.IsZero() {
			op.CreatedAtUnixMs = in.CreatedAt.UnixMilli()
		}
		if !in.UpdatedAt.IsZero() {
			op.UpdatedAtUnixMs = in.UpdatedAt.UnixMilli()
		}
		resp.Intents = append(resp.Intents, op)
	}
	return resp, nil
}

// ResolveDeliveryIntent settles one AMBIGUOUS intent (contract 0118): "confirm" (it
// arrived) or "resend" (send it once more, then settle by the outcome). Mutating:
// command_id + reason, audited.
func (s *Service) ResolveDeliveryIntent(ctx context.Context, req *pb.ResolveDeliveryIntentOpRequest) (*pb.CommandAck, error) {
	if s.deliveryIntents == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel runs no delivery journal")
	}
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}
	action := req.GetAction()
	if action != domain.DeliveryResolveConfirm && action != domain.DeliveryResolveResend {
		return nil, status.Errorf(codes.InvalidArgument, "action must be %q or %q", domain.DeliveryResolveConfirm, domain.DeliveryResolveResend)
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"resolve_delivery_intent", "delivery_intent", req.GetKey(), action,
		func() error {
			err := s.deliveryIntents.ResolveAmbiguous(ctx, req.GetKey(), action, req.GetReason())
			switch {
			case err == nil:
				return nil
			case errors.Is(err, domain.ErrDeliveryIntentNotFound):
				return status.Error(codes.NotFound, err.Error())
			case errors.Is(err, domain.ErrDeliveryIntentNotAmbiguous):
				return status.Error(codes.FailedPrecondition, err.Error())
			case errors.Is(err, domain.ErrDeliveryJournalOff):
				return status.Error(codes.Unimplemented, err.Error())
			case errors.Is(err, domain.ErrDeliveryPermanent):
				return status.Errorf(codes.FailedPrecondition, "the far side refused it for good; recorded as failed: %v", err)
			default:
				return status.Errorf(codes.Unavailable, "resolve: %v", err)
			}
		})
}
