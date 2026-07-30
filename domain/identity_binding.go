package domain

import (
	"context"
	"time"
)

// Identity bindings (contract 0077).
//
// Without them THE SURFACE IS THE IDENTITY: a policy links to
// `surface:chat:telegram`, so every sender who finds the bot has identical
// reach. `@unknown_4471` is governed by the same rule as `@afsin` — not because
// they are different people, but because nobody is.
//
// A binding is the hop that makes them different people.

// Binding targets. The third is a DELEGATION and is deliberately its own kind:
// anyone an admin adds to that room inherits the group the moment they mention
// the bot, so an audit must be able to tell a delegated room apart from a named
// person.
const (
	// BindPrincipal is the safe default — their own reach, inherited by nobody.
	BindPrincipal = "principal"
	// BindGroup gives someone a team's reach rather than their own.
	BindGroup = "group"
	// BindRoomGroup hands the membership decision to the external platform.
	BindRoomGroup = "room_group"
)

// IdentityBinding maps one external identity on one surface to a principal or a
// group.
type IdentityBinding struct {
	Surface string
	// ExternalID is the STABLE NUMERIC id from the platform — never the username
	// and never the phone number.
	//
	// A username changes, so binding on one binds on something the person can
	// alter at will, which is an impersonation vector rather than an identity. A
	// phone number arrives only if someone deliberately shares a contact card, so
	// binding on it makes the binding depend on an act unrelated to identity.
	ExternalID string
	// Username is a LABEL, kept so a console can show a recognisable name. It is
	// never matched on.
	Username    string
	DisplayName string
	// BoundToKind is BindPrincipal, BindGroup or BindRoomGroup.
	BoundToKind string
	BoundToID   string
	BoundAt     time.Time
	// UsernameChangedFrom records a previous username, so a console can show
	// "the binding held, because it is on the id" rather than leaving an operator
	// to wonder whether a renamed account is still the same person.
	UsernameChangedFrom string
	// Blocked drops this sender AT THE INGRESS: a blocked sender never reaches a
	// policy or a plan, which is a stronger statement than "every policy denies
	// them" and does not depend on the policy set being right.
	Blocked bool
}

// IsZero reports an absent binding.
func (b IdentityBinding) IsZero() bool { return b.Surface == "" && b.ExternalID == "" }

// Stranger policy modes — what happens to a sender with no binding.
const (
	// StrangerSurfaceDefault is today's behaviour: the surface IS the identity, so
	// an unbound sender gets whatever the surface's own policy allows.
	StrangerSurfaceDefault = "surface_default"
	// StrangerGuestPrincipal binds unbound senders to one named guest principal,
	// so their reach is deliberate rather than inherited.
	StrangerGuestPrincipal = "guest_principal"
	// StrangerRefuseUntilBound refuses, REPLYING ONCE rather than ignoring.
	// Silent ignoring is indistinguishable from a broken bot, and the person on
	// the other side retries instead of asking an operator to bind them.
	StrangerRefuseUntilBound = "refuse_until_bound"
)

// StrangerPolicy is what an unbound sender gets on one surface.
type StrangerPolicy struct {
	Surface          string
	Mode             string
	GuestPrincipalID string
	// ReadableDocuments is THE reach — how many documents an unbound sender can
	// read right now. -1 when it cannot be computed.
	//
	// It is what makes the panel a decision rather than a preference: "whatever
	// you set here is what a person you have never heard of gets" is abstract,
	// "right now a stranger can read 1204 documents" is not. Only the kernel can
	// compute it — a console would have to evaluate the whole policy set against
	// a principal that does not exist yet.
	ReadableDocuments int64
}

// SenderProfile is what the entry point knows about whoever just spoke.
//
// Everything except ExternalID is a CLAIM the bridge relays from the platform,
// and none of it is ever matched on — it exists so a console can show a row a
// human recognises. An operator deciding whether to grant reach to
// "6484759603" is being asked a question they cannot answer; the same row
// reading "@afsin · Afsin" is a decision they can actually make.
type SenderProfile struct {
	// ExternalID is the addressed identity — the thing the ingress namespace was
	// checked against and the thing a binding is made on.
	ExternalID string
	// SpeakerID is WHO TYPED, which is not always who was addressed. In a 1:1 they
	// are the same number; in a group the external id is the room and this is the
	// person in it. Recorded separately rather than substituted, because the
	// binding must stay on the id the delivery path can actually reach.
	SpeakerID string
	// Username is the platform handle, WITHOUT any leading sigil. A label only.
	Username string
	// DisplayName is the human name the platform reports. A label only.
	DisplayName string
}

// IsZero reports a profile carrying nothing to record.
func (p SenderProfile) IsZero() bool {
	return p.ExternalID == "" && p.SpeakerID == "" && p.Username == "" && p.DisplayName == ""
}

// IdentityResolver answers "who is this external sender?" on the inbound path.
//
// nil ⇒ no bindings exist and the surface remains the identity, which is the
// pre-0077 behaviour rather than a failure.
type IdentityResolver interface {
	// ResolveIdentity returns the binding for a sender on a surface. The second
	// return is false when the sender is unbound.
	//
	// It takes the whole PROFILE rather than just the id because resolution is
	// also the only moment the system learns a sender exists: an operator cannot
	// bind someone they have never been told about, and a worklist of bare
	// numbers is one they cannot act on. Implementations are expected to record
	// the sighting.
	ResolveIdentity(ctx context.Context, surface string, p SenderProfile) (IdentityBinding, bool)
	// StrangerPolicyFor returns what an unbound sender gets on this surface.
	StrangerPolicyFor(ctx context.Context, surface string) StrangerPolicy
}

// ErrSenderBlocked is returned by the inbound path for a blocked sender. It is a
// distinct error so the ingress can drop the message without opening a
// conversation, and so a log can tell a block apart from a policy denial.
type blockedSenderError struct{ externalID string }

func (e blockedSenderError) Error() string {
	return "ingress: sender " + e.externalID + " is blocked"
}

// NewBlockedSenderError builds the sentinel for a blocked sender.
func NewBlockedSenderError(externalID string) error {
	return blockedSenderError{externalID: externalID}
}

// IsBlockedSender reports whether err is a blocked-sender refusal.
func IsBlockedSender(err error) bool {
	_, ok := err.(blockedSenderError)
	return ok
}
