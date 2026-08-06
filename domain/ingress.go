package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Ingress — every point at which the outside world enters Cambrian (ADR-0090).
//
// Telegram, a webhook receiver, a websocket listener, an inbound REST API. Chat
// is one payload type riding through one; the concept is the entry point, not
// the payload.
//
// The rule the whole design turns on: an ingress HAS a surface, it is not one,
// and it never asserts its own. The mapping from ingress to surface is
// registered out of band by an operator; the kernel attaches it. A daemon is a
// black box (ADR-0033), and a black box asserting its own privilege level is not
// a security boundary (INV-5).
//
// This file is the PORT only. The kernel applies a registration; it never
// composes or stores one. The registry itself is a premium concern, the same
// split ADR-0085 draws for the Authorizer: OSS holds the seam, the plugin holds
// the answer. With no registry wired, nothing is an ingress and surface
// resolution falls back to the transport exactly as it does today.
// ─────────────────────────────────────────────────────────────────────────────

// IngressRegistration is what an operator registered about one ingress daemon.
type IngressRegistration struct {
	// AgentID is the ingress daemon's agent id — the principal the kernel sees on
	// the connection. This is the lookup key, and it is why the daemon cannot
	// choose its own registration: it does not get to pick who it authenticates as.
	AgentID string

	// Surface is stamped on every session opened through this ingress. It is the
	// clamp: an outsider-facing entry point carries a surface whose policy forbids
	// internal tags, and that holds even when identity resolution is wrong.
	Surface SurfaceRef

	// Namespace bounds which external identities this ingress may speak for, as a
	// set of prefixes ("tg:" for a Telegram bridge). Borrowed from Matrix
	// application services, where the homeserver — not the bridge — enforces which
	// users a bridge may act as.
	//
	// Empty means unrestricted, which is the honest default for a single-ingress
	// deployment: there is nobody to impersonate. It becomes load-bearing the
	// moment a second ingress exists, because without it a compromised Telegram
	// bridge could inject signals claiming to be a Slack user.
	Namespace []string

	// Schema is what this ingress DECLARES its items carry (ADR-0117): the
	// fields, typed, that flow into a pipeline triggered by it. Declared at
	// registration because the daemon is the one party that knows its own
	// payload contract a priori — a Telegram bridge does not need fifty
	// captures to learn it forwards `text`.
	//
	// Empty means undeclared, and everything downstream says "not declared"
	// rather than inventing fields — the same discipline as an unprofiled
	// studio ingress. The declaration must describe the ITEMS the pipeline
	// receives, not the wire payload the daemon consumes upstream: declaring
	// the whole Telegram Update here when only `text` reaches the item would
	// be a schema for data that never arrives.
	Schema []IngressSchemaField
}

// IngressSchemaField is one declared field of an ingress's items. Tagged for
// storage: declarations persist as JSON on the registration row, and stored
// JSON speaks snake_case like every other column in the deployment.
type IngressSchemaField struct {
	// Path is dotted, item-rooted, `*` for array members — the field
	// projection's own notation.
	Path string `json:"path"`
	// Type is one of: string, number, boolean, object, array.
	Type string `json:"type"`
	// Format optionally refines (identifier, datetime_utc, …). Informational.
	Format string `json:"format,omitempty"`
}

// MaySpeakFor reports whether externalID falls inside this ingress's namespace.
//
// An empty namespace permits everything. An empty externalID is always refused:
// "no sender" is not an identity, and treating it as one would let an ingress
// open conversations that can never be delivered to.
func (r IngressRegistration) MaySpeakFor(externalID string) bool {
	if strings.TrimSpace(externalID) == "" {
		return false
	}
	if len(r.Namespace) == 0 {
		return true
	}
	for _, prefix := range r.Namespace {
		if prefix != "" && strings.HasPrefix(externalID, prefix) {
			return true
		}
	}
	return false
}

// IsZero reports whether this is the empty registration (no ingress).
func (r IngressRegistration) IsZero() bool { return r.AgentID == "" }

// ErrOutsideNamespace is returned when an ingress tries to act for an external
// identity it was not registered to speak for.
var ErrOutsideNamespace = errors.New("ingress: external id is outside this ingress's namespace")

// DeliveryFor builds the delivery address for one external identity, refusing
// anything outside this ingress's namespace.
//
// This is the enforcement point for the namespace bound. Checking at BIND time is
// what matters: once an address is stored, later deliveries resolve it from the
// conversation rather than from anything a caller supplies, so an id that never
// gets bound can never be delivered to.
func (r IngressRegistration) DeliveryFor(externalID string) (DeliveryAddress, error) {
	if r.IsZero() {
		return DeliveryAddress{}, errors.New("ingress: no registration")
	}
	if !r.MaySpeakFor(externalID) {
		return DeliveryAddress{}, fmt.Errorf("%w: %q not permitted for %q", ErrOutsideNamespace, externalID, r.AgentID)
	}
	return DeliveryAddress{IngressAgentID: r.AgentID, ExternalID: externalID}, nil
}

// IngressResolver answers "is this principal a registered ingress, and what did
// the operator register about it?".
//
// nil is a valid value and means no registry is configured — nothing is an
// ingress, and every surface comes from the transport as before. That is the
// OSS default and it is deliberately fail-OPEN in the same sense the OSS
// Authorizer is: an unscoped deployment is correct, not broken.
type IngressResolver interface {
	ResolveIngress(ctx context.Context, principal PrincipalRef) (IngressRegistration, bool)
}

// IngressSurface returns the surface an ingress-opened session should carry, and
// whether the caller is an ingress at all.
//
// Only a REGISTERED ingress produces a surface here, which is the property that
// keeps this safe. `ResolveSurface` prefers a session's stored surface over the
// transport one on the assumption that the session's is NARROWER — true for an
// outsider-facing ingress, and false for, say, an operator-created session whose
// surface is the widest there is. Stamping every session with whatever surface
// happened to open it would therefore widen access for ordinary sessions. Only
// ingress-opened sessions are stamped, so the assumption holds by construction.
func IngressSurface(ctx context.Context, r IngressResolver, principal PrincipalRef) (SurfaceRef, bool) {
	if r == nil || principal.IsZero() {
		return SurfaceRef{}, false
	}
	reg, ok := r.ResolveIngress(ctx, principal)
	if !ok || reg.IsZero() {
		return SurfaceRef{}, false
	}
	if reg.Surface.Kind == "" && reg.Surface.ID == "" {
		return SurfaceRef{}, false
	}
	return reg.Surface, true
}

// IngressLister enumerates the registered ingresses (ADR-0090).
//
// `IngressResolver` answers "is this principal an ingress, and which surface
// does it carry" — the authorization question, asked one principal at a time.
// This answers "what ingresses exist", which is the operator question, and the
// two are not the same lookup: a console cannot resolve what it cannot already
// name.
//
// The registry itself stays a premium concern, exactly as IngressResolver does:
// OSS holds the seam, the plugin holds the registry.
type IngressLister interface {
	ListIngresses(ctx context.Context) ([]IngressRegistration, error)
}

// IngressDeregistrar withdraws an entry organ's registration.
//
// Deliberately a separate interface from IngressLister: reading who may enter
// and removing an entry are different powers, and bundling them would hand the
// second to every caller that needs the first.
type IngressDeregistrar interface {
	DeregisterIngress(ctx context.Context, agentID string) error
}

// IngressSchemaDeclarer records what a REGISTERED ingress's items carry
// (ADR-0117). A third separate power, for the deregistrar's reason: declaring
// a schema neither reads the registry nor withdraws from it, and the plugin
// that owns an entry point should hold exactly this and nothing wider.
// Declaring on an unregistered agent is refused — the registration itself
// stays the operator's act, because registering mints a surface (ADR-0090 D2).
type IngressSchemaDeclarer interface {
	DeclareIngressSchema(ctx context.Context, agentID string, fields []IngressSchemaField) error
}

// TurnFunc runs one admitted conversational turn in a conversation.
//
// Handed to a router so the router can decide WHEN and UNDER WHAT SHAPE the turn
// happens without owning the turn itself. What a turn does — history, the worker
// pool, the LLM lease — stays the chat tier's business.
type TurnFunc func(ctx context.Context, conversationID, text string) error

// TurnMessage is the admitted turn as the router receives it: the text, plus
// the sender facts the ingress TRANSPORTED alongside it (ADR-0117).
//
// The sender block exists because the item a pipeline gates on used to be
// `{text}` alone while the wire carried the speaker's identity the whole way —
// the graph could not say "route support-chat messages by who wrote them"
// about facts the kernel was already holding. Every field except Text is a
// relayed CLAIM (checked against the ingress's namespace where checkable,
// never matched on for authorization) and may be absent on a bridge that does
// not report it — absent, not empty-string-pretending-to-be-a-value.
type TurnMessage struct {
	Text string
	// SenderExternalID is the namespace-checked external identity ("tg:123").
	SenderExternalID string
	// SpeakerID is who WROTE the message — distinct from the external id in a
	// group chat, where the conversation is the group and the speaker is one
	// member of it.
	SpeakerID string
	// Username and DisplayName are naming claims, carried so a graph (and a
	// person reading its output) can name a sender instead of citing a number.
	Username    string
	DisplayName string
}

// TurnRouter shapes what happens around an admitted turn.
//
// It is reached ONLY after admission: the ingress daemon authenticated the sender
// at the external surface, and the kernel has already checked the namespace, the
// identity binding, blocked senders and the stranger policy. None of that is
// visible here, and deliberately so (ADR-0090 D2) — a router is operator-authored
// and must not be able to reorder or skip a security check.
//
// `handled=false` means "not mine": the caller runs the turn directly, which is
// what keeps a deployment with no router behaving exactly as it does today.
type TurnRouter interface {
	RouteTurn(ctx context.Context, ingressAgentID, conversationID string, msg TurnMessage, run TurnFunc) (handled bool, err error)
}
