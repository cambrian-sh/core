package authz

import (
	"context"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Fail-closed write classification (ENG-2115, owner decision 2026-11-18).
//
// A decision point answers ClassifyWrite with the classification it will stamp. The
// premium decision point derives it as `write ceiling ∩ hint` (ADR-0035 D8′-2), and
// until this check a requested tag outside the ceiling was silently DROPPED: a writer
// with no ceiling that asked for `confidential` had its document stored unclassified
// — readable by every unrestricted reader — with no error and no signal (the
// access-policy smoke's 8 leaks, readiness run ENG-1995 block 9).
//
// The rule now: a write is stored WITH every classification tag it requested, or it
// is not stored. When the decision point's answer lacks a requested tag (no write
// ceiling, the tag is outside the ceiling) or it refuses a tagged write outright (an
// unknown tag, a closed-vocabulary violation), the write is refused with an error
// naming the tags. A writer that requests NO tags is unaffected: it gets the decision
// point's default (the writer's ceiling, or unclassified without one).
//
// ENG-2161 (owner decision 2026-11-19) adds the untagged half: a writer that requests
// NO tags while its write ceiling holds CLOSED tags is refused too — the decision
// point answers ReasonClassificationRequired — because taking the whole ceiling would
// stamp every closed tag on the row and hide it from everyone but a reader granted
// all of them. The refusal is a *ClassificationRequiredError naming the closed tags
// and the ceiling to choose from. A writer whose ceiling holds only open tags (or
// that has none) still gets the default.
//
// Narrowing still works: a hint that names a subset of the ceiling is honoured in
// full — every requested tag is in the answer — and the answer carries exactly it.
//
// This is the ONE place the rule is applied (AGENTS.md §2.5): every write chokepoint
// that asks ClassifyWrite on a writer's behalf calls ClassifyWriteStrict, so no
// caller and no decision point can reintroduce the silent drop. In OSS
// (AllowAllAuthorizer) the answer is the hint itself, so the check always holds.

// UnhonouredClassificationError is the refusal of a write whose requested
// classification cannot be stored. It unwraps to ErrWriteDenied, so every boundary
// that maps ErrWriteDenied (InvalidArgument at the RPC planes) maps it unchanged.
//
// It names only the tags the WRITER sent (and the writer itself) — never the
// deployment vocabulary or the writer's ceiling — so it cannot be used to probe
// either.
type UnhonouredClassificationError struct {
	// Principal is the writer, rendered for the message.
	Principal string
	// Tags are the requested classification tags the write could not carry.
	Tags []string
	// Reason says why: the decision point's refusal reason, or outsideCeiling.
	Reason string
}

// outsideCeiling is the Reason when the decision point allowed the write but its
// classification lacks requested tags (they are outside the writer's write ceiling,
// or the writer has none).
const outsideCeiling = "not_in_write_ceiling"

func (e *UnhonouredClassificationError) Error() string {
	if e.Reason == outsideCeiling {
		return fmt.Sprintf("authz: write refused: requested classification [%s] cannot be honoured — "+
			"the tags are not in the write classification (write ceiling) of %s, so the write would be stored "+
			"without them; nothing was stored (give the writer a write ceiling that includes them, or do not request them)",
			strings.Join(e.Tags, ", "), e.Principal)
	}
	return fmt.Sprintf("authz: write refused: requested classification [%s] was refused for %s (%s); nothing was stored",
		strings.Join(e.Tags, ", "), e.Principal, e.Reason)
}

// Unwrap makes errors.Is(err, ErrWriteDenied) hold.
func (e *UnhonouredClassificationError) Unwrap() error { return ErrWriteDenied }

// ClassificationRequiredError is the refusal of an UNTAGGED write whose writer's
// write ceiling holds closed tags (ENG-2161, ADR-0035 C2 as amended). It unwraps to
// ErrWriteDenied, so every boundary that maps ErrWriteDenied (InvalidArgument at the
// RPC planes) maps it unchanged.
//
// Unlike UnhonouredClassificationError it names the writer's ceiling: the owner
// decided the refusal must say which tags to choose from, and the ceiling is the
// writer's own — the operator set it for this writer — so naming it to that writer
// discloses nothing about anyone else's reach.
type ClassificationRequiredError struct {
	// Principal is the writer, rendered for the message.
	Principal string
	// Closed are the closed tags in the writer's ceiling.
	Closed []string
	// Ceiling is the writer's write ceiling: what a classification may name.
	Ceiling []string
}

func (e *ClassificationRequiredError) Error() string {
	return fmt.Sprintf("authz: write refused: %s sent no classification, and its write ceiling holds the closed tags [%s] — "+
		"an unclassified write would carry every one of them and be readable only by a reader granted all of them; "+
		"nothing was stored (send the tags this write should carry, chosen from [%s])",
		e.Principal, strings.Join(e.Closed, ", "), strings.Join(e.Ceiling, ", "))
}

// Unwrap makes errors.Is(err, ErrWriteDenied) hold.
func (e *ClassificationRequiredError) Unwrap() error { return ErrWriteDenied }

// RequestedClassification is the part of a hint that is a classification REQUEST:
// the hint less the kernel-stamped identity namespaces — provenance stamps (never
// the writer's to set; the decision point strips and re-stamps them, ADR-0035
// D8′-5) and owner tags (kernel-stamped, ADR-0146 D5; the decision point may drop a
// foreign one). Neither is a tag the writer can ask for, so neither can be
// "unhonoured".
func RequestedClassification(hint []string) []string {
	var out []string
	for _, t := range hint {
		if t == "" || strings.HasPrefix(t, "provenance:") || domain.IsOwnerTag(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ClassifyWriteStrict asks the decision point to classify a write and refuses it
// when the answer does not carry every requested classification tag (see the file
// comment). It returns the decision point's classification and decision unchanged
// when the write may proceed; on refusal the error is an
// *UnhonouredClassificationError, a *ClassificationRequiredError for an untagged
// write the decision point refused for want of a classification (ENG-2161), or
// ErrWriteDenied for an untagged write it denied otherwise (no principal). The decision is returned either way, so a
// caller can log its reason.
//
// A nil authorizer is the OSS allow-all default.
func ClassifyWriteStrict(ctx context.Context, a domain.Authorizer, principal domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision, error) {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	final, dec := a.ClassifyWrite(ctx, principal, hint)
	requested := RequestedClassification(hint)
	if !dec.Allowed {
		if len(requested) == 0 {
			if dec.Reason == domain.ReasonClassificationRequired {
				var closed, ceiling []string
				if c := dec.ClassificationChoice; c != nil {
					closed, ceiling = dedup(c.Closed), dedup(c.Ceiling)
				}
				return nil, dec, &ClassificationRequiredError{Principal: principal.String(), Closed: closed, Ceiling: ceiling}
			}
			return nil, dec, ErrWriteDenied
		}
		reason := string(dec.Reason)
		if reason == "" {
			reason = "denied"
		}
		return nil, dec, &UnhonouredClassificationError{Principal: principal.String(), Tags: dedup(requested), Reason: reason}
	}
	if len(requested) == 0 {
		return final, dec, nil
	}
	have := make(map[string]struct{}, len(final))
	for _, t := range final {
		have[t] = struct{}{}
	}
	var missing []string
	for _, t := range requested {
		if _, ok := have[t]; !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return nil, dec, &UnhonouredClassificationError{Principal: principal.String(), Tags: dedup(missing), Reason: outsideCeiling}
	}
	return final, dec, nil
}

func dedup(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
