package domain

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The approval policy port (ADR-0142 D11 + owner acceptance 2026-11-06, item 1).
//
// An approval is not one shape. The policy answers, for one raised request:
// WHO may answer it (approver rules, any-of), whether a standing approval may
// pre-approve its action class, and whether the class is never self-approvable.
// The hub (approval_hub.go) enforces the answer at ANSWER TIME: the answering
// person must satisfy one of the rules the policy resolved, or the answer is
// refused — an operator included, unless a rule admits operators.
//
// Core holds the port and the basic rule kinds (self, a named person, a role,
// operator). The relationship kind (e.g. "the requester's supervisor") is
// DECLARED here and resolved by the premium hierarchy in stage 2; until a
// resolver is installed a relationship rule is unsatisfiable and a request whose
// only rules are relationships fails closed in the visible "needs_hierarchy"
// state — never approving, never falling back to self.
//
// Policies are data (Zero-Hardcode Rule): the OSS default policy reads the
// `approvals` config block, whose defaults are DefaultApprovalPolicyConfig.

// ApprovalRuleKind is the closed set of approver rule kinds.
type ApprovalRuleKind string

const (
	// ApprovalRuleSelf: the request's beneficiary (the person the action is for),
	// for effects on their own resources. Dropped for never-self-approvable classes.
	ApprovalRuleSelf ApprovalRuleKind = "self"
	// ApprovalRulePerson: one specific named person (Value = person id).
	ApprovalRulePerson ApprovalRuleKind = "person"
	// ApprovalRuleRole: anyone holding a plane role (Value = role name).
	ApprovalRuleRole ApprovalRuleKind = "role"
	// ApprovalRuleOperator: anyone holding the operator role.
	ApprovalRuleOperator ApprovalRuleKind = "operator"
	// ApprovalRuleRelationship: a person standing in a relation to the
	// beneficiary (Value = relation, e.g. "supervisor"). Resolved only by an
	// installed ApprovalRelationships (stage 2); otherwise unsatisfiable.
	ApprovalRuleRelationship ApprovalRuleKind = "relationship"
)

// ApprovalRule is one approver rule. Rendered (and configured) as "self",
// "operator", "person:<id>", "role:<name>" or "relationship:<relation>".
type ApprovalRule struct {
	Kind  ApprovalRuleKind
	Value string
}

// String renders the rule in its config spelling.
func (r ApprovalRule) String() string {
	if r.Value == "" {
		return string(r.Kind)
	}
	return string(r.Kind) + ":" + r.Value
}

// ParseApprovalRule parses a rule's config spelling.
func ParseApprovalRule(s string) (ApprovalRule, error) {
	s = strings.TrimSpace(s)
	kind, value, _ := strings.Cut(s, ":")
	r := ApprovalRule{Kind: ApprovalRuleKind(strings.TrimSpace(kind)), Value: strings.TrimSpace(value)}
	switch r.Kind {
	case ApprovalRuleSelf, ApprovalRuleOperator:
		if r.Value != "" {
			return ApprovalRule{}, fmt.Errorf("approval rule %q takes no value", s)
		}
	case ApprovalRulePerson, ApprovalRuleRole, ApprovalRuleRelationship:
		if r.Value == "" {
			return ApprovalRule{}, fmt.Errorf("approval rule %q needs a value (%s:<name>)", s, r.Kind)
		}
	default:
		return ApprovalRule{}, fmt.Errorf("unknown approval rule kind in %q (want self|operator|person:<id>|role:<name>|relationship:<relation>)", s)
	}
	return r, nil
}

// RulesString renders a rule set the way the console shows it: "self | operator".
func RulesString(rules []ApprovalRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, " | ")
}

// Approval kinds beyond the pre-existing "" (dangerous tool), ApprovalKindSkillAccess
// and ApprovalKindSchedule. The set is closed (ADR-0142 D11).
const (
	// ApprovalKindProactivePlan: a signal-born plan waiting to run. The wire Kind
	// stays "" for console compatibility (the watcher raises it as a tool-shaped
	// approval); the ACTION CLASS is this value.
	ApprovalKindProactivePlan = "proactive_plan"
	// ApprovalKindMachineConsent: "approve <tool> on <machine>?" (ADR-0127 D7).
	ApprovalKindMachineConsent = "machine_consent"
	// ApprovalKindChooseMachine: "which machine?" (ADR-0127 D6 ladder rung 4).
	ApprovalKindChooseMachine = "choose_machine"
	// ApprovalKindAccountConfirm: a person confirms a pending connected account
	// (ADR-0141 phishing defence). Raised asynchronously by the accounts plane.
	ApprovalKindAccountConfirm = "account_confirm"
	// ApprovalKindPublish: sharing private material to team/org knowledge
	// (ADR-0142 D12).
	ApprovalKindPublish = "publish"
)

// ApprovalClassTool is the action class of a dangerous-tool approval (wire kind "").
const ApprovalClassTool = "tool"

// DefaultActionClass maps a wire kind to its default action class.
func DefaultActionClass(kind string) string {
	if kind == "" {
		return ApprovalClassTool
	}
	return kind
}

// ApprovalClassRule is one policy row: the approver rules (any-of) for an action
// class, and whether a standing approval may cover it. Class "*" is the fallback.
type ApprovalClassRule struct {
	Class         string
	Approvers     []string
	AllowStanding bool
	// WaitOffline parks a BLOCKING request whose eligible approvers have no
	// connected surface for its window, instead of failing it at once as
	// unroutable; the person answers it from their inbox when they connect
	// (owner decision 2026-11-15, ENG-653). Default false: the historical
	// "nobody can answer ⇒ deny now" stays for every rule that does not opt in.
	WaitOffline bool
}

// ApprovalPolicyConfig is the data the default (config) policy reads.
type ApprovalPolicyConfig struct {
	Rules []ApprovalClassRule
	// NeverSelfApprovable lists action classes no person may approve for
	// themselves (e.g. public_record_release, payment): the self rule is dropped
	// for them, and a standing approval for them must be granted by someone other
	// than the beneficiary.
	NeverSelfApprovable []string
	// ToolClasses maps a tool name to the action class its approvals carry, so
	// e.g. a payment tool's approval is the "payment" class, not generic "tool".
	ToolClasses map[string]string
	// StandingMaxTTL caps a standing approval's life (0 = until revoked).
	StandingMaxTTL time.Duration
}

// DefaultApprovalPolicyConfig is the shipped policy data (ADR-0142 D11 defaults:
// self for actions on one's own resources, operator for everything else — which
// for the pre-existing kinds is exactly today's behaviour). It is data the
// config layer defaults to, not Go branching.
func DefaultApprovalPolicyConfig() ApprovalPolicyConfig {
	return ApprovalPolicyConfig{
		Rules: []ApprovalClassRule{
			{Class: ApprovalClassTool, Approvers: []string{"operator"}, AllowStanding: true},
			{Class: ApprovalKindSkillAccess, Approvers: []string{"operator"}},
			{Class: ApprovalKindProactivePlan, Approvers: []string{"operator"}},
			// A person's own recurring task is their own resource (D11.1); an
			// operator may still answer, as before.
			{Class: ApprovalKindSchedule, Approvers: []string{"self", "operator"}, AllowStanding: true},
			{Class: ApprovalKindMachineConsent, Approvers: []string{"self"}, AllowStanding: true},
			{Class: ApprovalKindChooseMachine, Approvers: []string{"self"}},
			{Class: ApprovalKindAccountConfirm, Approvers: []string{"self"}},
			{Class: ApprovalKindPublish, Approvers: []string{"self"}},
			// A knowledge commit (domain/knowledge_commit.go) is the person the
			// proposal is for confirming it; a kind routed under its own
			// CommitClass falls to "*" (operator) until the deployment names its
			// approvers in the approvals config.
			{Class: ApprovalKindKnowledgeCommit, Approvers: []string{"self"}},
			{Class: "public_record_release", Approvers: []string{"role:compliance", "operator"}},
			{Class: "payment", Approvers: []string{"operator"}},
			{Class: "*", Approvers: []string{"operator"}},
		},
		NeverSelfApprovable: []string{"public_record_release", "payment"},
		StandingMaxTTL:      30 * 24 * time.Hour,
	}
}

// ApprovalRoute is what the policy resolved for one request.
type ApprovalRoute struct {
	ActionClass    string
	Rules          []ApprovalRule
	AllowStanding  bool
	WaitOffline    bool
	NeverSelf      bool
	StandingMaxTTL time.Duration
}

// ApprovalPolicy resolves who may answer a request. Implementations are pure
// over policy data; the hub calls Route once per raised request and stores the
// result on the item, so the rule shown and the rule enforced are the same.
type ApprovalPolicy interface {
	Route(ctx context.Context, item ApprovalItem) (ApprovalRoute, error)
}

// ApprovalRelationships resolves relationship rules (stage 2: the premium
// hierarchy). Approvers returns the person ids standing in `relation` to the
// beneficiary. nil on the hub ⇒ every relationship rule is unsatisfiable
// ("needs hierarchy").
type ApprovalRelationships interface {
	Approvers(ctx context.Context, beneficiary PrincipalRef, relation string) ([]string, error)
}

// ConfigApprovalPolicy is the OSS default ApprovalPolicy over ApprovalPolicyConfig.
type ConfigApprovalPolicy struct {
	byClass   map[string]ApprovalClassRule
	rules     map[string][]ApprovalRule
	neverSelf map[string]bool
	tools     map[string]string
	maxTTL    time.Duration
}

// NewConfigApprovalPolicy validates cfg and builds the policy. An invalid rule,
// or a never-self class whose ONLY approver is self (it could never be answered),
// is a configuration error.
func NewConfigApprovalPolicy(cfg ApprovalPolicyConfig) (*ConfigApprovalPolicy, error) {
	p := &ConfigApprovalPolicy{
		byClass:   map[string]ApprovalClassRule{},
		rules:     map[string][]ApprovalRule{},
		neverSelf: map[string]bool{},
		tools:     map[string]string{},
		maxTTL:    cfg.StandingMaxTTL,
	}
	for _, c := range cfg.NeverSelfApprovable {
		if c = strings.TrimSpace(c); c != "" {
			p.neverSelf[c] = true
		}
	}
	for tool, class := range cfg.ToolClasses {
		if strings.TrimSpace(tool) != "" && strings.TrimSpace(class) != "" {
			p.tools[strings.TrimSpace(tool)] = strings.TrimSpace(class)
		}
	}
	for _, row := range cfg.Rules {
		class := strings.TrimSpace(row.Class)
		if class == "" {
			return nil, fmt.Errorf("approvals: a rule row has no class")
		}
		if _, dup := p.byClass[class]; dup {
			return nil, fmt.Errorf("approvals: class %q is listed twice", class)
		}
		if len(row.Approvers) == 0 {
			return nil, fmt.Errorf("approvals: class %q lists no approvers", class)
		}
		var parsed []ApprovalRule
		usable := 0
		for _, s := range row.Approvers {
			r, err := ParseApprovalRule(s)
			if err != nil {
				return nil, fmt.Errorf("approvals: class %q: %w", class, err)
			}
			if r.Kind == ApprovalRuleSelf && p.neverSelf[class] {
				continue // dropped: never self-approvable
			}
			parsed = append(parsed, r)
			usable++
		}
		if usable == 0 {
			return nil, fmt.Errorf("approvals: class %q is never self-approvable but lists only self as approver", class)
		}
		p.byClass[class] = row
		p.rules[class] = parsed
	}
	return p, nil
}

// Route implements ApprovalPolicy.
func (p *ConfigApprovalPolicy) Route(_ context.Context, item ApprovalItem) (ApprovalRoute, error) {
	class := item.ActionClass
	if class == "" {
		if c, ok := p.tools[item.ToolName]; ok && item.Kind == "" {
			class = c
		} else {
			class = DefaultActionClass(item.Kind)
		}
	}
	key := class
	if _, ok := p.byClass[key]; !ok {
		key = "*"
	}
	row, ok := p.byClass[key]
	if !ok {
		// No row and no fallback: unroutable, which the hub fails closed.
		return ApprovalRoute{ActionClass: class, NeverSelf: p.neverSelf[class]}, nil
	}
	rules := append([]ApprovalRule(nil), p.rules[key]...)
	if p.neverSelf[class] {
		// The fallback row may list self; the class itself never allows it.
		kept := rules[:0]
		for _, r := range rules {
			if r.Kind != ApprovalRuleSelf {
				kept = append(kept, r)
			}
		}
		rules = kept
	}
	return ApprovalRoute{
		ActionClass:    class,
		Rules:          rules,
		AllowStanding:  row.AllowStanding,
		WaitOffline:    row.WaitOffline,
		NeverSelf:      p.neverSelf[class],
		StandingMaxTTL: p.maxTTL,
	}, nil
}

// Classes lists the configured classes (sorted), for diagnostics.
func (p *ConfigApprovalPolicy) Classes() []string {
	out := make([]string, 0, len(p.byClass))
	for c := range p.byClass {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Approver is who answers (or would answer) an approval: an authenticated
// person and the plane roles they hold. Built by the surface from verified
// facts only (the operator interceptor, a bound chat sender) — never a payload.
type Approver struct {
	Principal PrincipalRef
	Roles     []string
}

// ApprovalRoleOperator is the plane role the operator rule admits.
const ApprovalRoleOperator = "operator"

// OperatorApprover is an authenticated console operator.
func OperatorApprover(id string) Approver {
	return Approver{Principal: UserPrincipal(id), Roles: []string{ApprovalRoleOperator}}
}

// HasRole reports whether the approver holds role r.
func (a Approver) HasRole(r string) bool {
	for _, x := range a.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// IsZero reports an unidentified approver (never satisfies any rule).
func (a Approver) IsZero() bool { return a.Principal.IsZero() }

// principalRolesCtxKey carries the authenticated person's FULL role set, stamped
// by the operator-plane interceptor from the verified Identity (never a payload).
type principalRolesCtxKey struct{}

// WithPrincipalRoles returns a child context carrying the caller's role set.
func WithPrincipalRoles(ctx context.Context, roles []string) context.Context {
	return context.WithValue(ctx, principalRolesCtxKey{}, append([]string(nil), roles...))
}

// PrincipalRolesFromContext returns the caller's role set (nil when none was stamped).
func PrincipalRolesFromContext(ctx context.Context) []string {
	r, _ := ctx.Value(principalRolesCtxKey{}).([]string)
	return r
}

// SamePerson reports whether two principal refs name the same person. During
// the ADR-0142 stage-1 kind migration a person may still arrive as `agent:<id>`
// on the MCP/chat doors and as `user:<id>` on the console; both are a person
// with the same registry id. Machines, daemons and system never match.
func SamePerson(a, b PrincipalRef) bool {
	if a.ID == "" || b.ID == "" || a.ID != b.ID {
		return false
	}
	return personish(a.Kind) && personish(b.Kind)
}

func personish(k PrincipalKind) bool { return k == PrincipalUser || k == PrincipalAgent }
