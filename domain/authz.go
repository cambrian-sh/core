package domain

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// The authorization PORT (ADR-0085).
//
// This file is the WHOLE of access control in the OSS kernel. It declares the
// question the kernel asks before it acts, the vocabulary of the answer, and an
// allow-everything default. It contains NO policy: no groups, no links, no
// precedence, no tag algebra, no vocabulary validation, no resolution. Those are
// the decision point (PDP) and live in a premium plugin.
//
// The split follows the Windows Security Reference Monitor: AccessCheck lives in
// the NT kernel and cannot be replaced; the DACL it evaluates arrives from
// outside as data. Here the ENFORCEMENT POINTS are in the kernel and are not
// pluggable — what is pluggable is the DECISION, which is data.
//
// Consequence, stated once because getting it backwards is a security bug: the
// OSS kernel fails OPEN (unrestricted is the correct and only semantics for a
// single-tenant open-source deployment). Fail-CLOSED is a property of the
// plugin, which denies whenever it cannot resolve a principal.
// ─────────────────────────────────────────────────────────────────────────────

// ResourceKind names the four securable resource kinds. One algebra governs all
// four; what differs per kind is only which tags the resource presents.
type ResourceKind string

const (
	KindMemory   ResourceKind = "memory"   // documents, facts, scenes
	KindSkill    ResourceKind = "skill"    // authored procedural capabilities
	KindAgent    ResourceKind = "agent"    // invocation of an agent
	KindTool     ResourceKind = "tool"     // invocation of a system/MCP tool
	KindArtifact ResourceKind = "artifact" // files produced by a run
)

// PrincipalKind distinguishes the sorts of identity that can hold access.
type PrincipalKind string

const (
	PrincipalAgent  PrincipalKind = "agent"
	PrincipalUser   PrincipalKind = "user"
	PrincipalDaemon PrincipalKind = "daemon"
	// PrincipalSystem is the kernel itself acting on behalf of no one (maintenance
	// reads: temporal decay & GC, spreading activation, episodic indexing).
	PrincipalSystem PrincipalKind = "system"
	// PrincipalMachine is a consumer-owned worker machine on the contribution
	// lane (ADR-0127 D1) — a broker holding an outbound connection, contributing
	// tools to the agents attending ITS OWNER's tasks. Its own kind, not an
	// agent: policy must be able to target "every worker" as one class, and a
	// machine principal the scope store has never heard of fails closed like
	// any other.
	PrincipalMachine PrincipalKind = "machine"
)

// PrincipalRef identifies WHO is asking. It is established by the kernel from the
// authenticated connection or session and is never read from a request payload or
// from a daemon's claim about itself (INV-5).
type PrincipalRef struct {
	ID   string
	Kind PrincipalKind
}

// AgentPrincipal is the common case: an agent acting under its own identity.
func AgentPrincipal(agentID string) PrincipalRef {
	return PrincipalRef{ID: agentID, Kind: PrincipalAgent}
}

// UserPrincipal is a human operator acting under their own identity, established
// from the authenticated operator session — never from a request payload (INV-5).
//
// It exists because the operator plane had no way to say who it was: its handlers
// resolved the operator's name for the AUDIT entry and then called downstream with a
// bare context, so every write they performed reached the authorization chokepoint as
// "<none>". The agent plane had the same defect and was fixed; this is its counterpart.
func UserPrincipal(userID string) PrincipalRef {
	return PrincipalRef{ID: userID, Kind: PrincipalUser}
}

// MachinePrincipal is a named worker machine (ADR-0127 D1), established from
// its authenticated machine credential — never from a request payload (INV-5).
// String() renders it as `machine:<name>`, the id the ADR's vocabulary uses.
func MachinePrincipal(machine string) PrincipalRef {
	return PrincipalRef{ID: machine, Kind: PrincipalMachine}
}

// SystemPrincipal is the kernel-internal principal for maintenance reads. It is
// the identity counterpart of the ScopeSystem predicate.
var SystemPrincipal = PrincipalRef{ID: "kernel", Kind: PrincipalSystem}

// IsZero reports whether no principal could be established. Callers must treat
// this fail-closed in the plugin and fail-open in OSS — the Authorizer decides,
// not the call site.
func (p PrincipalRef) IsZero() bool { return p.ID == "" }

// String renders "kind:id" for logs and decision records.
func (p PrincipalRef) String() string {
	if p.ID == "" {
		return "<none>"
	}
	if p.Kind == "" {
		return p.ID
	}
	return string(p.Kind) + ":" + p.ID
}

// SurfaceRef identifies the entry point a request arrived through — the operator
// console, an internal chat daemon, an outsider-facing chat daemon, the reactive
// engine acting unattended. The surface can CLAMP what may be done regardless of
// who is asking (the Windows loopback analogue). Like PrincipalRef it is bound to
// the authenticated session, never to the message (INV-5).
type SurfaceRef struct {
	ID   string
	Kind string
}

// Well-known surface kinds. The kernel attaches these; what any of them permits
// is entirely the plugin's business.
const (
	SurfaceOperator = "operator" // the operator console / CLI
	SurfaceAgent    = "agent"    // the agent-facing gRPC plane
	SurfaceChat     = "chat"     // a conversation ingress
	SurfaceReactive = "reactive" // unattended reactive/daemon execution
	SurfaceMCP      = "mcp"      // the inbound Cambrian MCP endpoint (ADR-0126)
	SurfaceInternal = "internal" // in-process kernel call path
)

// String renders "kind:id" for logs and decision records.
func (s SurfaceRef) String() string {
	if s.ID == "" && s.Kind == "" {
		return "<none>"
	}
	if s.Kind == "" {
		return s.ID
	}
	if s.ID == "" {
		return s.Kind
	}
	return s.Kind + ":" + s.ID
}

// ResourceRef identifies WHAT is being reached for.
type ResourceRef struct {
	Kind ResourceKind
	ID   string
}

// String renders "kind/id" for logs and decision records.
func (r ResourceRef) String() string {
	if r.ID == "" {
		return string(r.Kind)
	}
	return string(r.Kind) + "/" + r.ID
}

// Taggable is the interface by which a resource presents its classification tags
// to the decision point. Every securable kind implements it; nothing else about
// the resource crosses the port. Tags are opaque strings to the kernel.
type Taggable interface {
	// AuthzRef identifies the resource for the decision record.
	AuthzRef() ResourceRef
	// AuthzTags returns the resource's classification tags.
	AuthzTags() []string
}

// ToolEffect is a CLOSED set of verb classes describing what an invocation DOES,
// as opposed to what it is about. A tag answers "what is this about"; it cannot
// answer "what am I doing to it" — reading a CRM contact and deleting one carry
// the same tag and vastly different risk (ADR-0086).
//
// Closed, not an open string namespace: wildcard action strings are how people
// accidentally grant far more than they intended.
type ToolEffect string

const (
	EffectRead   ToolEffect = "read"   // observes state, no mutation
	EffectWrite  ToolEffect = "write"  // mutates internal state
	EffectEgress ToolEffect = "egress" // transmits data outside the deployment
	EffectSpend  ToolEffect = "spend"  // incurs cost or moves money
	EffectAdmin  ToolEffect = "admin"  // alters the system's own configuration
)

// AllToolEffects is the complete closed set, in escalating-risk order. Used for
// registration validation and for operator vocabulary listings.
var AllToolEffects = []ToolEffect{EffectRead, EffectWrite, EffectEgress, EffectSpend, EffectAdmin}

// ValidToolEffect reports whether e is a member of the closed set.
func ValidToolEffect(e ToolEffect) bool {
	for _, x := range AllToolEffects {
		if x == e {
			return true
		}
	}
	return false
}

// AccessRequest is one question put to the decision point.
type AccessRequest struct {
	Principal PrincipalRef
	Surface   SurfaceRef
	Resource  ResourceRef
	// Tags are the resource's classification tags (from Taggable, or read from
	// the document's metadata).
	Tags []string
	// Effects are the effect classes the invocation declares. Meaningful for
	// KindTool; empty for the other kinds.
	Effects []ToolEffect
	// Session correlates the decision with the caller's session, so a decision
	// record can be joined to a run.
	Session SessionID
}

// DecisionReason is the controlled vocabulary of WHY. An administrator must be
// able to act on any of these without reading code (ADR-0085 D8).
type DecisionReason string

const (
	// ReasonAllowed — permitted.
	ReasonAllowed DecisionReason = "allowed"
	// ReasonBypass — the kernel-internal ScopeSystem path; filtering skipped.
	ReasonBypass DecisionReason = "bypass"
	// ReasonForbiddenTag — a specific deny tag matched. Names the tag and the
	// policy that contributed it.
	ReasonForbiddenTag DecisionReason = "forbidden_tag"
	// ReasonMissingRequiredTag — the resource lacks a required tag. Names it.
	ReasonMissingRequiredTag DecisionReason = "missing_required_tag"
	// ReasonAnyOfUnsatisfied — no tag from a required OR-clause was present.
	ReasonAnyOfUnsatisfied DecisionReason = "anyof_unsatisfied"
	// ReasonEffectNotPermitted — tags passed; the declared effect class is not granted.
	ReasonEffectNotPermitted DecisionReason = "effect_not_permitted"
	// ReasonUnsatisfiablePolicy — the effective scope can never match anything.
	// This is a SAFE state (zero rows) but must surface to the operator, not just
	// the log, or it is indistinguishable from "there is no data".
	ReasonUnsatisfiablePolicy DecisionReason = "unsatisfiable_policy"
	// ReasonNoPrincipal — fail-closed default: identity could not be established.
	ReasonNoPrincipal DecisionReason = "no_principal"
	// ReasonSkillGrantClipped — NOT a denial. A skill's tool grant was narrowed to
	// the principal's existing privilege (ADR-0085 D4).
	ReasonSkillGrantClipped DecisionReason = "skill_grant_clipped"
	// ReasonNotAuthorized is the generic denial for a decision point that declines
	// to explain further. A plugin returning this is losing the property that makes
	// the whole mechanism usable; it exists so the enum is total.
	ReasonNotAuthorized DecisionReason = "not_authorized"

	// ReasonNotAParty: the resource carries a PARTY-SCOPED tag and the reader is
	// not one of its parties (ADR-0121). Distinct from missing_required_tag on
	// purpose — the reader holds every tag the policy asks for, and was refused
	// by a relationship rather than by a label, which is a different sentence to
	// put in front of an operator.
	//
	// Detail names the party-scoped tag responsible, never the reader's
	// identities: a denial that listed who you would have to BE is a denial that
	// enumerates other people.
	ReasonNotAParty DecisionReason = "not_a_party"

	// ReasonWorkerNotOwned: a contributed local:<machine>/<tool> step named a
	// machine that is not in the task beneficiary's fleet (ADR-0127 D1/D9 —
	// authority owner must equal task beneficiary, per call). An unknown
	// machine, a foreign machine, and a task with no beneficiary all refuse
	// under this one reason on purpose: distinguishing them would let a caller
	// probe whose fleet a machine is in, the same enumeration ReasonNotAParty
	// declines.
	ReasonWorkerNotOwned DecisionReason = "worker_not_owned"

	// The CL-2 contributed-step reasons (ADR-0127 D6/D7). Every consent outcome
	// and every parking event lands on the decision seam under its own reason —
	// receipts are the point, so none of these collapse into a generic one.
	//
	// Consent (D7): the pre-dispatch gate's outcome for a contributed step.
	// The first two are ALLOWS (recorded, not refused); the rest are refusals.
	//
	// ReasonConsentAuto: a read-only step under the `auto` knob dispatched
	// silently — receipted, never prompted (owner ruling 2026-08-20).
	ReasonConsentAuto DecisionReason = "consent_auto"
	// ReasonConsentApproved: a surface answered the approve prompt yes; Detail
	// names the approver.
	ReasonConsentApproved DecisionReason = "consent_approved"
	// ReasonConsentOnMachine: the machine's knob is on-machine-only — the step
	// dispatched carrying the consent marker; the broker prompts locally and a
	// consent-denied report is a recorded refusal (ReasonConsentDeniedOnMachine).
	ReasonConsentOnMachine DecisionReason = "consent_on_machine"
	// ReasonConsentDenied: a surface answered the approve prompt no.
	ReasonConsentDenied DecisionReason = "consent_denied"
	// ReasonConsentDeniedOnMachine: the worker's report said consent was denied
	// at the machine (on-machine-only knob) — a refusal, not a worker error.
	ReasonConsentDeniedOnMachine DecisionReason = "consent_denied_on_machine"
	// ReasonConsentTimeout: the approve prompt went unanswered within the
	// consent window (or the caller gave up waiting) — fail-closed.
	ReasonConsentTimeout DecisionReason = "consent_timeout"
	// ReasonConsentUnroutable: consent was required but there was no way to ask
	// — no consent controller wired, or no surface subscribed — fail-closed.
	ReasonConsentUnroutable DecisionReason = "consent_unroutable"

	// Parking (D6): a step targeting an owned-but-offline machine. The first
	// two are events on an eventually-dispatched step; the last two end it.
	//
	// ReasonStepParked: the step parked awaiting the machine; Detail names the
	// deadline.
	ReasonStepParked DecisionReason = "step_parked"
	// ReasonParkDispatched: the machine polled back in and the parked step
	// proceeded through the normal consent-checked dispatch path.
	ReasonParkDispatched DecisionReason = "park_dispatched"
	// ReasonParkExpired: the deadline passed with the machine still offline —
	// the step fails VISIBLY (named error), never silently.
	ReasonParkExpired DecisionReason = "park_expired"
	// ReasonParkAbandoned: the caller (plan/context) gave up while the step was
	// parked; the step never dispatched.
	ReasonParkAbandoned DecisionReason = "park_abandoned"
	// ReasonRelayFailed: the step reached a live machine and the relay failed
	// anyway — the machine stopped answering, the call deadline passed, the
	// transport broke. Recorded because a machine that vanishes MID-STEP is the
	// lane failure an operator most needs to see, and without this the journal
	// showed only the consent decision that preceded it.
	ReasonRelayFailed DecisionReason = "relay_failed"

	// The selection ladder (D6) on a bare local:<capability> step.
	//
	// ReasonMachineSelected: the ladder resolved a machine (sole capable,
	// configured default, or a surface's answer); Detail names it and the rung.
	ReasonMachineSelected DecisionReason = "machine_selected"
	// ReasonWorkerUnresolved: the ladder ended without an answer (no candidate,
	// unanswered/denied "which machine?" prompt, no way to ask) — refused,
	// never guessed.
	ReasonWorkerUnresolved DecisionReason = "worker_unresolved"
)

// PolicyContribution records that a named policy, linked at a named container,
// contributed a specific term to the effective scope. This is what turns a denial
// into "because policy P, linked at L, contributed tag T" (the gpresult model).
type PolicyContribution struct {
	// PolicyID is the policy object that contributed the term.
	PolicyID string
	// PolicyName is its human label.
	PolicyName string
	// LinkedAt names the container the policy was linked to ("organisation",
	// "group:support", "principal:agent_x", "surface:chat-public").
	LinkedAt string
	// Term is the kind of term contributed ("required", "any_of", "forbidden",
	// "effect").
	Term string
	// Values are the tags/effects contributed by this policy at this link.
	Values []string
	// Enforced mirrors the link's Enforced flag (a downstream Block Inheritance
	// does not apply to it).
	Enforced bool
}

// String renders a contribution as one administrator-readable line.
func (c PolicyContribution) String() string {
	name := c.PolicyName
	if name == "" {
		name = c.PolicyID
	}
	return fmt.Sprintf("%s@%s: %s{%s}", name, c.LinkedAt, c.Term, strings.Join(c.Values, ","))
}

// AccessDecision is the structured, explainable outcome of one access question. Every
// decision — allow and deny — is a value; nothing about a denial is exceptional.
type AccessDecision struct {
	Allowed   bool
	Resource  ResourceRef
	Principal PrincipalRef
	Surface   SurfaceRef
	Reason    DecisionReason
	// Detail names the specific tag, clause, or effect responsible. It is what
	// makes ReasonForbiddenTag actionable rather than merely true.
	Detail string
	// DecidedBy lists which policy, linked where, contributed which term.
	DecidedBy []PolicyContribution

	// GrantedTags are the CLOSED tags policy reopened for this principal (ADR-0091).
	// Recorded on the decision because a closed-tag denial is otherwise
	// unexplainable: the operator wrote no policy forbidding the tag, so the answer
	// has to name the closure and the missing grant instead of "forbidden tag".
	GrantedTags []string
	// PolicyVersion identifies the policy snapshot this decision was computed
	// against, so a decision is reproducible after the policy changes.
	PolicyVersion string
	// ReportOnly marks a decision computed under a report-only policy: Allowed is
	// the value the caller must act on, WouldHaveDenied is what enforcement would
	// have done (ADR-0087 D9).
	ReportOnly      bool
	WouldHaveDenied bool
}

// Explain renders the decision as a single administrator-readable sentence. It is
// the text an empty result set carries, and the text ExplainAccess returns.
func (d AccessDecision) Explain() string {
	verb := "denied"
	if d.Allowed {
		verb = "allowed"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s for %s via %s (%s", verb, d.Resource.String(), d.Principal.String(), d.Surface.String(), d.Reason)
	if d.Detail != "" {
		b.WriteString(": ")
		b.WriteString(d.Detail)
	}
	b.WriteString(")")
	if len(d.DecidedBy) > 0 {
		parts := make([]string, 0, len(d.DecidedBy))
		for _, c := range d.DecidedBy {
			parts = append(parts, c.String())
		}
		b.WriteString(" [")
		b.WriteString(strings.Join(parts, "; "))
		b.WriteString("]")
	}
	if d.ReportOnly {
		if d.WouldHaveDenied {
			b.WriteString(" (report-only: would have denied)")
		} else {
			b.WriteString(" (report-only)")
		}
	}
	return b.String()
}

// Authorizer is the decision point the kernel consults before it acts. Every
// enforcement point in the kernel calls one of these four methods; none of them
// implements policy.
//
// OSS supplies AllowAllAuthorizer. A premium plugin supplies a real one through
// app.Options, exactly as the reactive engine supplies a signal receiver.
type Authorizer interface {
	// Authorize decides one access request.
	Authorize(ctx context.Context, req AccessRequest) AccessDecision

	// Filter narrows a candidate set in a single pass. It returns the survivors in
	// input order plus a decision for EVERY candidate rejected — an empty result
	// with no decisions is a defect (INV-3).
	Filter(ctx context.Context, principal PrincipalRef, surface SurfaceRef, kind ResourceKind, candidates []Taggable) (kept []Taggable, rejected []AccessDecision)

	// ReadFilter returns the store-pushdown predicate for this principal's reads
	// on this surface, plus the decision that produced it. A nil predicate means
	// "no read is authorized at all" and the read chokepoint refuses (never runs
	// unfiltered). The accompanying AccessDecision explains an empty or impossible
	// predicate so a zero-row result is never silent.
	ReadFilter(ctx context.Context, principal PrincipalRef, surface SurfaceRef) (*TagPredicate, AccessDecision)

	// ClassifyWrite derives the authoritative classification stamped on a write.
	// The hint is the writer's requested tags, which may only NARROW the derived
	// classification — a principal can never broaden its own write. An error-free
	// denial is expressed by a AccessDecision with Allowed=false; the write chokepoint
	// refuses the write.
	ClassifyWrite(ctx context.Context, principal PrincipalRef, hint []string) ([]string, AccessDecision)
}

// Explainer is the OPTIONAL administrator-facing surface of a decision point: it
// answers "why can/can't this principal see this?" without performing the access.
// A decision point that cannot explain itself is still usable; it is just worse.
type Explainer interface {
	// ExplainAccess answers a hypothetical access question. It never mutates and
	// never performs the access.
	ExplainAccess(ctx context.Context, req AccessRequest) AccessDecision
	// Vocabulary lists the controlled classification tags an administrator may
	// select from. A free-text tag field in an admin UI is a defect (ADR-0085 D11).
	Vocabulary(ctx context.Context) []string
}

// AllowAllAuthorizer is the OSS default: it permits everything and explains that
// it did so. This is CORRECT behaviour for the open-source product, which is
// explicitly single-tenant and unscoped — not a stub and not a security hole.
//
// It is also the reason the kernel's enforcement points are unconditional: they
// always ask, and in OSS the answer is always yes.
type AllowAllAuthorizer struct{}

// allowAllPredicate is the unrestricted read filter handed out by
// AllowAllAuthorizer. It is distinct from ScopeSystem so that logs can tell
// "unscoped deployment" apart from "kernel-internal maintenance read".
var allowAllPredicate = &TagPredicate{Bypass: true}

func (AllowAllAuthorizer) Authorize(_ context.Context, req AccessRequest) AccessDecision {
	return AccessDecision{
		Allowed:   true,
		Resource:  req.Resource,
		Principal: req.Principal,
		Surface:   req.Surface,
		Reason:    ReasonAllowed,
		Detail:    "no policy plugin installed (unscoped deployment)",
	}
}

func (AllowAllAuthorizer) Filter(_ context.Context, _ PrincipalRef, _ SurfaceRef, _ ResourceKind, candidates []Taggable) ([]Taggable, []AccessDecision) {
	return candidates, nil
}

func (a AllowAllAuthorizer) ReadFilter(_ context.Context, principal PrincipalRef, surface SurfaceRef) (*TagPredicate, AccessDecision) {
	return allowAllPredicate, AccessDecision{
		Allowed:   true,
		Principal: principal,
		Surface:   surface,
		Reason:    ReasonAllowed,
		Detail:    "no policy plugin installed (unscoped deployment)",
	}
}

func (AllowAllAuthorizer) ClassifyWrite(_ context.Context, principal PrincipalRef, hint []string) ([]string, AccessDecision) {
	return hint, AccessDecision{
		Allowed:   true,
		Principal: principal,
		Reason:    ReasonAllowed,
		Detail:    "no policy plugin installed (writes keep their authored tags)",
	}
}

// authorizerCtxKey carries the Authorizer through intermediate helpers whose
// signatures must not be churned, mirroring how the effective read predicate is
// carried (WithScope).
type authorizerCtxKey struct{}

// WithAuthorizer returns a child context carrying the decision point.
func WithAuthorizer(ctx context.Context, a Authorizer) context.Context {
	return context.WithValue(ctx, authorizerCtxKey{}, a)
}

// AuthorizerFromContext returns the decision point carried by ctx, falling back
// to AllowAllAuthorizer. The fallback is deliberate and matches §4.2: in a kernel
// with no policy plugin, unrestricted IS the policy. A plugin that wants
// fail-closed behaviour installs itself; it never relies on absence.
func AuthorizerFromContext(ctx context.Context) Authorizer {
	if a, ok := ctx.Value(authorizerCtxKey{}).(Authorizer); ok && a != nil {
		return a
	}
	return AllowAllAuthorizer{}
}
