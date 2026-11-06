package agentcard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// Remote card verification (ADR-0139 D4: remote A2A agents are dispatched at
// the third-party tier; their cards are verified when signed).
//
// Trust comes from a key set the deployment pins (a2a.trusted_jwks_file), never
// from the card: a card's `jku` points at keys served by the same party that
// served the card, so fetching it would let whoever controls the card's origin
// vouch for themselves. That is the reason the jku is ignored, not an oversight.

// Signature policies (a2a.signature_policy).
const (
	// PolicyOff ignores card signatures entirely.
	PolicyOff = "off"
	// PolicyOptional verifies a card when it is signed by a trusted key and
	// refuses it when such a signature fails. An unsigned card, or one signed
	// only by keys outside the trusted set, is accepted with a WARN.
	PolicyOptional = "optional"
	// PolicyRequired accepts only cards that verify against a trusted key.
	PolicyRequired = "required"
)

// Policies lists the valid a2a.signature_policy values.
var Policies = []string{PolicyOff, PolicyOptional, PolicyRequired}

// RemoteFetcher wraps the A2A HTTP client's card fetch with signature
// verification. It satisfies domain.CardFetcher, so the registration path and
// the profile seeder see only cards the policy admitted.
type RemoteFetcher struct {
	Inner  domain.CardFetcher
	Policy string
	Keys   KeySet
}

// FetchCard fetches the card at endpoint and applies the policy.
func (f *RemoteFetcher) FetchCard(ctx context.Context, endpoint string) (*domain.AgentCard, error) {
	if f == nil || f.Inner == nil {
		return nil, errors.New("agentcard: remote fetcher has no client")
	}
	card, err := f.Inner.FetchCard(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if err := f.Check(endpoint, card); err != nil {
		return nil, err
	}
	return card, nil
}

// Check applies the policy to one fetched card. It is exported so a test, or a
// future operator "verify this card" action, runs the exact rule registration runs.
func (f *RemoteFetcher) Check(endpoint string, card *domain.AgentCard) error {
	policy := f.Policy
	if policy == "" {
		policy = PolicyOptional
	}
	if policy == PolicyOff {
		return nil
	}
	if card == nil || len(card.Raw) == 0 {
		// Without the bytes as served there is nothing to verify against.
		if policy == PolicyRequired {
			return fmt.Errorf("a2a card from %s: signature required but the card bytes are unavailable", endpoint)
		}
		return nil
	}
	v, err := Verify(card.Raw, f.Keys)
	switch {
	case err == nil:
		card.Verified = true
		card.SignerKeyID = v.KeyID
		slog.Info("ADR-0139 D4: remote agent card signature verified", "endpoint", endpoint, "kid", v.KeyID, "alg", v.Algorithm)
		return nil
	case !errors.Is(err, ErrUnsigned) && !errors.Is(err, ErrUnknownKey):
		// Refused under BOTH policies: a broken signature by a key we trust
		// means the card was altered after signing, and a signed card the
		// canonicaliser refuses (a duplicate member name) is one two parsers
		// could read differently.
		return fmt.Errorf("a2a card from %s refused: %w", endpoint, err)
	case policy == PolicyRequired:
		return fmt.Errorf("a2a card from %s refused (a2a.signature_policy=required): %w", endpoint, err)
	default:
		slog.Warn("ADR-0139 D4: remote agent card accepted UNVERIFIED (a2a.signature_policy=optional)",
			"endpoint", endpoint, "reason", err)
		return nil
	}
}
