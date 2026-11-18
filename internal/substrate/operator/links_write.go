package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// The operator's REVIEW surface over the identity plane (contract 0098;
// five-planes step 2, FIVE-PLANES-BUILD.md).
//
// The plane splits minting from asserting on purpose: an Entity carries no
// belief, and every epistemic burden — who said it, by what means, on what
// evidence — lives on the Link. That split only pays off if a HUMAN can act on
// the links, and until this file existed nothing in the product could. Producers
// whose mechanism sits below the trust ceiling (derived, scored, correlation)
// write `candidate` and can never promote themselves; with no promotion path at
// all, every one of those proposals was write-only.
//
// It follows generators_write.go exactly: a port declared beside the handler, a
// DTO of its own rather than the domain type, a Set…Writer that leaves the RPCs
// Unimplemented until a kernel binds them, and runMutation for the audit-first,
// command_id-deduplicated body.

// LinkWriter is the write half of the identity plane's review lane. The kernel
// binds it to domain.LinkStore; nothing here knows that type, for the reason
// GeneratorWriter does not know config.GeneratorConfig — the operator plane
// speaks its own shapes so a store change is not a contract change.
type LinkWriter interface {
	// ConfirmLink promotes a candidate by APPENDING a new human-mechanism row.
	// The original assertion is left exactly as it was: what the machine
	// proposed and what the reviewer decided are two separate claims, and an
	// overwrite would destroy the producer-behaviour record the lane exists to
	// accumulate. Re-confirming is idempotent and returns the existing row.
	ConfirmLink(ctx context.Context, namespace, linkID, actor string) (LinkRecord, error)

	// RetractLink stamps state + retracted_at and NOTHING else — never a DELETE
	// (ADR-0093 D6). A rejected candidate must stay queryable, or the producer
	// that proposed it proposes it again forever.
	RetractLink(ctx context.Context, namespace, linkID, actor, reason string) error

	// RetractLinksByProducer revokes every unretracted row one producer wrote and
	// returns the count. This is the reason the producer column exists: a pass
	// that turns out to be wrong is undone wholesale rather than row by row.
	RetractLinksByProducer(ctx context.Context, namespace, producer, actor string) (int, error)

	// ListLinkCandidates is the review inbox: unretracted proposals, highest
	// confidence first.
	ListLinkCandidates(ctx context.Context, namespace string, limit int) ([]LinkRecord, error)

	// ListLinkPairs is the review inbox grouped by claim (contract 0109): each
	// pair once, with only the rows the operator plane may read.
	ListLinkPairs(ctx context.Context, namespace string, limit int) ([]LinkPairRecord, error)

	// ConfirmLinkPair confirms every open candidate row of the pair the operator
	// plane may read — one confirmation per row, each as ConfirmLink — and
	// returns the confirmations. A pair with no readable candidate is refused as
	// an absent one (domain.ErrLinkRefused).
	ConfirmLinkPair(ctx context.Context, pair LinkPairKey, actor string) ([]LinkRecord, error)

	// RejectLinkPair retracts every open readable candidate row of the pair and
	// records the rejection of the claim; returns how many rows it retracted.
	RejectLinkPair(ctx context.Context, pair LinkPairKey, actor, reason string) (int, error)
}

// LinkPairKey names one claim as the operator plane speaks it.
type LinkPairKey struct {
	NamespaceID string
	Family      string
	FromRef     string
	ToRef       string
	Relation    string
}

// LinkPairRecord is one claim in the review inbox.
type LinkPairRecord struct {
	Key LinkPairKey
	// Confidence is the best candidate's.
	Confidence    float64
	Candidates    []LinkRecord
	Confirmations []LinkRecord
	// AlreadyConfirmed: a readable confirmation holds the claim — "already
	// confirmed via another source; extend the confirmation?".
	AlreadyConfirmed bool
}

// LinkRecord is one assertion as the operator plane speaks it — the read shape
// for a review card. Times are UTC; a nil-equivalent bound is the zero time,
// which the mapper renders as 0 rather than year 1.
type LinkRecord struct {
	ID          string
	NamespaceID string
	Family      string
	FromRef     string
	ToRef       string
	Relation    string
	State       string
	Mechanism   string
	Producer    string
	Confidence  float64
	EvidenceID  string
	AssertedBy  string
	AssertedAt  time.Time
	RecordedAt  time.Time
	ValidFrom   time.Time
	ValidTo     time.Time
	RetractedAt time.Time
	SourceRef   string
	// SupportingEvidenceIDs are the other evidence rows the assertion was derived
	// from (ENG-2032).
	SupportingEvidenceIDs []string
}

// SetLinkWriter wires the identity-plane review path. nil ⇒ Unimplemented.
func (s *Service) SetLinkWriter(w LinkWriter) { s.linkWriter = w }

// HasLinkWriter reports whether this kernel can serve the review lane, so the
// composition root advertises the `links` capability only when the RPCs behind
// it lead somewhere. A console gates the whole review screen on that string —
// without it, an operator would be shown a queue with buttons that answer
// Unimplemented.
func (s *Service) HasLinkWriter() bool { return s.linkWriter != nil }

// linkOpTime renders an optional instant. 0 means UNBOUNDED (or "not retracted"),
// which is a different claim from "the epoch" — the obsRow convention on the
// query plane, for the same reason.
func linkOpTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func linkToOp(l LinkRecord) *pb.LinkOp {
	return &pb.LinkOp{
		Id: l.ID, NamespaceId: l.NamespaceID, Family: l.Family,
		FromRef: l.FromRef, ToRef: l.ToRef, Relation: l.Relation,
		State: l.State, Mechanism: l.Mechanism, Producer: l.Producer,
		Confidence: l.Confidence, EvidenceId: l.EvidenceID,
		AssertedBy:            l.AssertedBy,
		AssertedAtUnixMs:      linkOpTime(l.AssertedAt),
		RecordedAtUnixMs:      linkOpTime(l.RecordedAt),
		ValidFromUnixMs:       linkOpTime(l.ValidFrom),
		ValidToUnixMs:         linkOpTime(l.ValidTo),
		RetractedAtUnixMs:     linkOpTime(l.RetractedAt),
		SourceRef:             l.SourceRef,
		SupportingEvidenceIds: l.SupportingEvidenceIDs,
	}
}

func linksToOps(in []LinkRecord) []*pb.LinkOp {
	out := make([]*pb.LinkOp, 0, len(in))
	for _, l := range in {
		out = append(out, linkToOp(l))
	}
	return out
}

// pairKeyFrom reads a request's pair, refusing one that names no claim.
func pairKeyFrom(k *pb.LinkPairKeyOp) (LinkPairKey, error) {
	key := LinkPairKey{
		NamespaceID: k.GetNamespaceId(), Family: k.GetFamily(),
		FromRef: k.GetFromRef(), ToRef: k.GetToRef(), Relation: k.GetRelation(),
	}
	if key.Family == "" || key.FromRef == "" || key.ToRef == "" || key.Relation == "" {
		return LinkPairKey{}, status.Error(codes.InvalidArgument, "pair.family, pair.from_ref, pair.to_ref and pair.relation are required")
	}
	return key, nil
}

func pairTarget(k LinkPairKey) string {
	return k.Family + ":" + k.FromRef + " " + k.Relation + " " + k.ToRef
}

// ConfirmLink promotes one candidate. Mutating: command_id + reason, audited,
// idempotent on command_id.
func (s *Service) ConfirmLink(ctx context.Context, req *pb.ConfirmLinkOpRequest) (*pb.ConfirmLinkOpResponse, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	if req.GetLinkId() == "" {
		return nil, status.Error(codes.InvalidArgument, "link_id is required")
	}
	actor, _, _ := PrincipalFromContext(ctx)
	var confirmed LinkRecord
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"confirm_link", "link", req.GetLinkId(),
		"link "+req.GetLinkId()+" confirmed",
		func() error {
			var applyErr error
			confirmed, applyErr = s.linkWriter.ConfirmLink(ctx, req.GetNamespaceId(), req.GetLinkId(), actor)
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	resp := &pb.ConfirmLinkOpResponse{CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped()}
	// A deduplicated command applied nothing this time round, so there is no
	// confirmation row in hand to report. Returning a zero LinkOp would be worse
	// than returning none: it reads as a link with an empty id.
	if confirmed.ID != "" {
		resp.Link = linkToOp(confirmed)
	}
	return resp, nil
}

// RetractLink rejects one assertion. Mutating: command_id + reason, audited.
//
// The reason is not decoration here. A retraction is the record that a human
// looked at a proposal and said no, and the audit entry runMutation writes is
// the only place that judgement is preserved — the link row itself never gains a
// field, because a row that can be rewritten is a row whose history nobody can
// trust.
func (s *Service) RetractLink(ctx context.Context, req *pb.RetractLinkOpRequest) (*pb.CommandAck, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	if req.GetLinkId() == "" {
		return nil, status.Error(codes.InvalidArgument, "link_id is required")
	}
	actor, _, _ := PrincipalFromContext(ctx)
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"retract_link", "link", req.GetLinkId(),
		"link "+req.GetLinkId()+" retracted",
		func() error {
			return s.linkWriter.RetractLink(ctx, req.GetNamespaceId(), req.GetLinkId(), actor, req.GetReason())
		})
}

// RetractLinksByProducer revokes a whole pass. Mutating: command_id + reason,
// audited, idempotent on command_id.
func (s *Service) RetractLinksByProducer(ctx context.Context, req *pb.RetractLinksByProducerOpRequest) (*pb.RetractLinksByProducerOpResponse, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	if req.GetProducer() == "" {
		// Refused rather than defaulted: an empty producer key that matched
		// everything would revoke the entire graph on one mistyped field.
		return nil, status.Error(codes.InvalidArgument, "producer is required")
	}
	actor, _, _ := PrincipalFromContext(ctx)
	var count int
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"retract_links_by_producer", "link_producer", req.GetProducer(),
		"producer "+req.GetProducer()+" revoked",
		func() error {
			var applyErr error
			count, applyErr = s.linkWriter.RetractLinksByProducer(ctx, req.GetNamespaceId(), req.GetProducer(), actor)
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.RetractLinksByProducerOpResponse{
		CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Retracted: int32(count),
	}, nil
}

// ListLinkCandidates reads the review inbox. Read RPC: no command_id, no audit
// entry — reading a queue is not an act on the world.
func (s *Service) ListLinkCandidates(ctx context.Context, req *pb.ListLinkCandidatesOpRequest) (*pb.ListLinkCandidatesOpResponse, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	rows, err := s.linkWriter.ListLinkCandidates(ctx, req.GetNamespaceId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &pb.ListLinkCandidatesOpResponse{Candidates: make([]*pb.LinkOp, 0, len(rows))}
	for _, l := range rows {
		out.Candidates = append(out.Candidates, linkToOp(l))
	}
	return out, nil
}

// ListLinkPairs reads the review inbox grouped by claim (contract 0109). Read RPC.
func (s *Service) ListLinkPairs(ctx context.Context, req *pb.ListLinkPairsOpRequest) (*pb.ListLinkPairsOpResponse, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	pairs, err := s.linkWriter.ListLinkPairs(ctx, req.GetNamespaceId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &pb.ListLinkPairsOpResponse{Pairs: make([]*pb.LinkPairOp, 0, len(pairs))}
	for _, p := range pairs {
		out.Pairs = append(out.Pairs, &pb.LinkPairOp{
			Pair: &pb.LinkPairKeyOp{
				NamespaceId: p.Key.NamespaceID, Family: p.Key.Family,
				FromRef: p.Key.FromRef, ToRef: p.Key.ToRef, Relation: p.Key.Relation,
			},
			Confidence:       p.Confidence,
			Candidates:       linksToOps(p.Candidates),
			Confirmations:    linksToOps(p.Confirmations),
			AlreadyConfirmed: p.AlreadyConfirmed,
		})
	}
	return out, nil
}

// ConfirmLinkPair confirms every readable open candidate row of one claim.
// Mutating: command_id + reason, audited, idempotent on command_id.
func (s *Service) ConfirmLinkPair(ctx context.Context, req *pb.ConfirmLinkPairOpRequest) (*pb.ConfirmLinkPairOpResponse, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	key, err := pairKeyFrom(req.GetPair())
	if err != nil {
		return nil, err
	}
	actor, _, _ := PrincipalFromContext(ctx)
	var confirmed []LinkRecord
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"confirm_link_pair", "link_pair", pairTarget(key),
		"link pair "+pairTarget(key)+" confirmed",
		func() error {
			var applyErr error
			confirmed, applyErr = s.linkWriter.ConfirmLinkPair(ctx, key, actor)
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.ConfirmLinkPairOpResponse{
		CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Confirmations: linksToOps(confirmed),
	}, nil
}

// RejectLinkPair rejects one claim. Mutating: command_id + reason, audited. The
// reason is the only record of the judgement (the rows never gain a field), as
// for RetractLink.
func (s *Service) RejectLinkPair(ctx context.Context, req *pb.RejectLinkPairOpRequest) (*pb.RejectLinkPairOpResponse, error) {
	if s.linkWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no identity plane to review")
	}
	key, err := pairKeyFrom(req.GetPair())
	if err != nil {
		return nil, err
	}
	actor, _, _ := PrincipalFromContext(ctx)
	var count int
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"reject_link_pair", "link_pair", pairTarget(key),
		"link pair "+pairTarget(key)+" rejected",
		func() error {
			var applyErr error
			count, applyErr = s.linkWriter.RejectLinkPair(ctx, key, actor, req.GetReason())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.RejectLinkPairOpResponse{
		CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Retracted: int32(count),
	}, nil
}
