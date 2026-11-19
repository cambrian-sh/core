package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The outbound identifier guard (owner decision 2026-11-15,
// T-20261108-outbound-tool-writes-have-no-deterministic, ENG-917).
//
// The chat disclosure guard (internal/chat/disclosure.go) checks a chat reply;
// text an agent writes INTO an outside system through a tool — a case response
// letter, a mail draft, a post — used to pass the executor with no deterministic
// check. This guard runs the SAME identifier detector over the text arguments of
// every tool call that can transmit outside (effects include egress_send; plain
// egress counts as send, the safe direction; a tool with no effects at all is
// treated as sending). A hit never rewrites or masks the call: it sends the call
// to approval, and the approval request names each detected identifier and the
// argument it sits in, so the approver sees exactly what would leave.
//
// Effects come from the operator/catalog policy (an MCP server's own
// annotations are never read for this), and an undeclared tool is dangerous
// with read+write+egress (mcp.UnlistedToolPolicy), so it already needs approval
// and the guard adds the identifiers to that approval.

// IdentifierKind is one category of personal identifier the detector recognises.
type IdentifierKind string

const (
	IdentifierNationalID IdentifierKind = "national_id"
	IdentifierPhone      IdentifierKind = "phone"
	IdentifierEmail      IdentifierKind = "email"
	IdentifierIBAN       IdentifierKind = "iban"
	IdentifierCard       IdentifierKind = "card"
)

// AllIdentifierKinds is the closed set, in the fixed order texts list them in.
var AllIdentifierKinds = []IdentifierKind{IdentifierNationalID, IdentifierPhone, IdentifierEmail, IdentifierIBAN, IdentifierCard}

// ValidIdentifierKind reports whether k is in the closed set.
func ValidIdentifierKind(k IdentifierKind) bool {
	for _, x := range AllIdentifierKinds {
		if x == k {
			return true
		}
	}
	return false
}

// identifierNouns name each kind for a person (the approval notice).
var identifierNouns = map[IdentifierKind]string{
	IdentifierNationalID: "national ID number",
	IdentifierPhone:      "phone number",
	IdentifierEmail:      "e-mail address",
	IdentifierIBAN:       "bank account number",
	IdentifierCard:       "card number",
}

// IdentifierMatch is one identifier found in a text, by byte offsets.
type IdentifierMatch struct {
	Start, End int
	Kind       IdentifierKind
	Value      string
}

// IdentifierDetector finds personal identifiers in a text, left to right,
// without overlaps. There is one implementation (internal/identifiers), shared
// by the chat disclosure guard and this guard — never a second detector.
type IdentifierDetector interface {
	FindIdentifiers(text string) []IdentifierMatch
}

// OutboundIdentifier is one identifier the guard found in a call's arguments.
type OutboundIdentifier struct {
	// Arg is the argument it sits in, as a JSON path ("body", "to[0]",
	// "message.text"); "" when the arguments were not a JSON value.
	Arg   string
	Kind  IdentifierKind
	Value string
}

// OutboundIdentifierGuard is the executor control (ToolExecutorControls.
// OutboundIdentifiers; nil ⇒ off, execution.tools.outbound_identifier_guard).
type OutboundIdentifierGuard struct {
	detector IdentifierDetector
	kinds    map[IdentifierKind]bool
}

// NewOutboundIdentifierGuard builds the guard over the one detector. kinds
// narrows which categories count as a hit; empty ⇒ every kind.
func NewOutboundIdentifierGuard(d IdentifierDetector, kinds []IdentifierKind) (*OutboundIdentifierGuard, error) {
	if d == nil {
		return nil, fmt.Errorf("outbound identifier guard: a detector is required")
	}
	if len(kinds) == 0 {
		kinds = AllIdentifierKinds
	}
	set := make(map[IdentifierKind]bool, len(kinds))
	for _, k := range kinds {
		if !ValidIdentifierKind(k) {
			return nil, fmt.Errorf("outbound identifier guard: unknown identifier kind %q", k)
		}
		set[k] = true
	}
	return &OutboundIdentifierGuard{detector: d, kinds: set}, nil
}

// SendsOutside reports whether a call of t can transmit to a person or change
// state outside the deployment: its ceiling effects include egress_send (plain
// egress expands to it; egress that was only inferred counts as send). A tool
// with no effects bypassed registration and is treated as sending (fail closed).
func SendsOutside(t SystemTool) bool {
	if len(CeilingEffects(t)) == 0 {
		return true
	}
	return Exercises(t, EffectEgressSend)
}

// Check returns the identifiers in the text arguments of a call of tool, or
// nil when the tool does not send outside or the arguments carry none.
func (g *OutboundIdentifierGuard) Check(tool SystemTool, args []byte) []OutboundIdentifier {
	if g == nil || !SendsOutside(tool) {
		return nil
	}
	return g.Scan(args)
}

// Scan returns the identifiers in every string value of args (a JSON value),
// in argument-path order. Arguments that are not JSON are scanned as one text.
func (g *OutboundIdentifierGuard) Scan(args []byte) []OutboundIdentifier {
	if g == nil || len(args) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return g.scanText("", string(args), nil)
	}
	var out []OutboundIdentifier
	walkStrings(v, "", func(path, s string) { out = g.scanText(path, s, out) })
	return out
}

func (g *OutboundIdentifierGuard) scanText(path, text string, out []OutboundIdentifier) []OutboundIdentifier {
	for _, m := range g.detector.FindIdentifiers(text) {
		if g.kinds[m.Kind] {
			out = append(out, OutboundIdentifier{Arg: path, Kind: m.Kind, Value: m.Value})
		}
	}
	return out
}

// walkStrings visits every string leaf of a decoded JSON value, object keys in
// sorted order so the result is deterministic.
func walkStrings(v any, path string, visit func(path, s string)) {
	switch x := v.(type) {
	case string:
		visit(path, x)
	case []any:
		for i, e := range x {
			walkStrings(e, path+"["+strconv.Itoa(i)+"]", visit)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walkStrings(x[k], p, visit)
		}
	}
}

// OutboundIdentifierNoticePrefix opens every identifier notice, so a surface
// (or a test) can recognise it.
const OutboundIdentifierNoticePrefix = "Personal identifiers in outbound text"

// RenderOutboundIdentifierNotice words the hits for the approver: each
// identifier, its category and the argument it is in — highlighted, never
// masked, because the approver must see what would leave. "" when none.
func RenderOutboundIdentifierNotice(ids []OutboundIdentifier) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		p := identifierNouns[id.Kind] + " «" + id.Value + "»"
		if id.Arg != "" {
			p += " in " + id.Arg
		}
		parts = append(parts, p)
	}
	return OutboundIdentifierNoticePrefix + " (check before approving; this call sends them outside): " +
		strings.Join(parts, "; ") + "."
}

// OutboundIdentifierDenyReason is the refusal when a call needs approval only
// because of the guard and no approval controller is configured.
func OutboundIdentifierDenyReason(ids []OutboundIdentifier) string {
	return "approval required but unavailable: " + RenderOutboundIdentifierNotice(ids)
}
