package authz

import (
	"context"
	"slices"

	"github.com/cambrian-sh/core/domain"
)

// ProfileNarrowingAuthorizer is the ONE kernel enforcement point for the customer
// chat posture (ADR-0084 D7; owner decision 2026-11-14, ENG-614): a conversation
// with an external customer may read memory, but only content tagged
// customer-visible (execution.chat.customer_visible_tags, default ["public"], e.g. a
// public FAQ). It wraps the resolved decision point, so every reader that asks
// ReadFilter — QueryService recall, the tool executor, the knowledge query seam, the
// document reader — is narrowed without per-site code, and a delegated plan that
// runs under the conversation's lease inherits the narrowing because the profile is
// stamped from that lease (domain.WithConversationProfile).
//
// Only the READ predicate is narrowed (the decision: "narrow the read predicate").
// Authorize, Filter and ClassifyWrite delegate unchanged: tool grants, candidate
// lists and write classification are governed elsewhere.
type ProfileNarrowingAuthorizer struct {
	domain.Authorizer
	customerTags []string
}

// NewProfileNarrowingAuthorizer wraps inner. customerTags are the tags a customer
// conversation may read; empty means it reads nothing (fail closed).
func NewProfileNarrowingAuthorizer(inner domain.Authorizer, customerTags []string) *ProfileNarrowingAuthorizer {
	if inner == nil {
		inner = domain.AllowAllAuthorizer{}
	}
	tags := make([]string, 0, len(customerTags))
	for _, t := range customerTags {
		if t != "" && !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	return &ProfileNarrowingAuthorizer{Authorizer: inner, customerTags: tags}
}

// ReadFilter is the inner predicate, AND "carries one of the customer-visible tags"
// when ctx is a customer conversation's work. The narrowed predicate:
//   - is never a bypass (a bypass would waive the clause); the owner fence the inner
//     predicate carried is kept;
//   - drops the own-rows exception and any ledger grant, so nothing reaches a
//     customer that is not customer-visible;
//   - is nil (no read at all) when no customer-visible tag is configured, or when
//     the inner decision point already granted no read.
func (a *ProfileNarrowingAuthorizer) ReadFilter(ctx context.Context, principal domain.PrincipalRef, surface domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	pred, dec := a.Authorizer.ReadFilter(ctx, principal, surface)
	if domain.ConversationProfileFromContext(ctx) != domain.ProfileCustomer || pred == nil {
		return pred, dec
	}
	if len(a.customerTags) == 0 {
		return nil, domain.AccessDecision{Principal: principal, Surface: surface, Reason: domain.ReasonAnyOfUnsatisfied,
			Detail: "customer conversation: no customer-visible tags configured (execution.chat.customer_visible_tags)"}
	}
	n := *pred
	n.Bypass = false
	n.OwnRows = nil
	n.LedgerRuns = nil
	n.AnyOfClauses = append(slices.Clone(pred.AnyOfClauses), slices.Clone(a.customerTags))
	return &n, dec
}
