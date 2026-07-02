package domain

import (
	"context"
	"fmt"
	"strings"
)

// DiscoveryReport is the Scout agent's pre-plan observation of the world, handed to the
// Planner as the `<DiscoveryLTM>` block (ADR-0051 D9). It has two faces: the structured
// Entities are deterministic world-state — TRUSTED (they cannot carry instructions); the
// Interpretation is a thin advisory pattern read from Scout's LLM. The trust boundary
// (injection-scanning the Interpretation before it reaches the Planner) is enforced when
// the block is rendered — see ADR-0051 D13 / issue-005.
type DiscoveryReport struct {
	Entities       []DiscoveredEntity // structured, trusted state deltas
	Interpretation string             // thin LLM pattern read (advisory; untrusted text — scanned at render, issue-005)
	Unobserved     []string           // canonical kind:id entities Scout could not reach within the cap (issue-003)
	Environment    *EnvFacts          // deterministic OS/path facts — always present when Scout ran (trusted)
}

// EnvFacts are deterministic, trusted facts about the runtime environment the plan will run
// in (ADR-0051). They exist so the Planner/agents emit CORRECT, ABSOLUTE paths instead of
// guessing a Unix `~/Desktop` on a Windows host — the kind of cross-OS path bug a one-shot
// planner produces because it never grounds in where it actually is. Computed without an LLM.
type EnvFacts struct {
	OS      string // runtime.GOOS: "windows" | "linux" | "darwin"
	Home    string // the user's home directory (absolute)
	Desktop string // the user's Desktop directory (absolute)
	Cwd     string // the process working directory (absolute)
}

// DiscoveredEntity is one structured observation. The raw observed content lives behind
// ContentCID (ADR-0048 offload) and is NEVER inlined into the Planner prompt — only the
// compact Summary is.
type DiscoveredEntity struct {
	Kind       string // file | dir | api | url | service | db
	ID         string // canonical id
	Exists     bool
	Summary    string // compact, decision-relevant gist ("10 sections, 3 written, 7 missing")
	ContentCID string // reference to the raw observation in the ContentStore (never inlined)
}

// IsEmpty reports whether the report carries nothing worth injecting — Scout observed
// no entities and produced no interpretation. An empty report means "degrade to one-shot":
// the Planner sees no `<DiscoveryLTM>` block and behaves exactly as before Scout existed.
func (r *DiscoveryReport) IsEmpty() bool {
	return r == nil || (len(r.Entities) == 0 && strings.TrimSpace(r.Interpretation) == "" &&
		len(r.Unobserved) == 0 && r.Environment == nil)
}

// RenderDiscoveryBlock renders the `<DiscoveryLTM>` Planner-prompt section (ADR-0051 D9/D13).
// The trust boundary: STRUCTURED facts (kind/id/exists/content_cid) are trusted; the
// GENERATIVE text (entity summaries + the interpretation) is UNTRUSTED and passes
// sanitizeUntrusted before entering the prompt — it cannot break out of its tag or carry an
// instruction the Planner trusts. The raw body is referenced by content_cid, never inlined.
// Returns "" for an empty/nil report (the degrade signal).
func RenderDiscoveryBlock(r *DiscoveryReport) string {
	if r.IsEmpty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<DiscoveryLTM>\n")
	if e := r.Environment; e != nil {
		// Trusted deterministic facts. Plain quotes (NOT %q — that would Go-escape the
		// backslashes in a Windows path); XML-escaped for attribute safety.
		fmt.Fprintf(&sb, "  <environment os=\"%s\" home=\"%s\" desktop=\"%s\" cwd=\"%s\"/>\n",
			escapeXMLContent(e.OS), escapeXMLContent(e.Home), escapeXMLContent(e.Desktop), escapeXMLContent(e.Cwd))
	}
	for _, e := range r.Entities {
		cidAttr := ""
		if e.ContentCID != "" {
			cidAttr = fmt.Sprintf(" content_cid=%q", e.ContentCID)
		}
		// kind/id/exists/content_cid: trusted structural facts. summary: untrusted prose.
		fmt.Fprintf(&sb, "  <entity kind=%q id=%q exists=%q%s>%s</entity>\n",
			e.Kind, e.ID, fmt.Sprintf("%t", e.Exists), cidAttr, sanitizeUntrusted(e.Summary))
	}
	if s := strings.TrimSpace(r.Interpretation); s != "" {
		fmt.Fprintf(&sb, "  <interpretation>%s</interpretation>\n", sanitizeUntrusted(s))
	}
	for _, u := range r.Unobserved {
		fmt.Fprintf(&sb, "  <unobserved>%s</unobserved>\n", escapeXMLContent(u))
	}
	sb.WriteString("</DiscoveryLTM>\n")
	return sb.String()
}

// discoveryCtxKey carries the Scout report from Server.Execute (where Scout runs) into the
// Planner's GetExecutionPlan (where it is rendered), without changing the planner signature
// — mirroring WithScope/WithSessionID (scope_context.go).
type discoveryCtxKey struct{}

// WithDiscovery attaches Scout's report to ctx for the Planner to consume.
func WithDiscovery(ctx context.Context, report *DiscoveryReport) context.Context {
	return context.WithValue(ctx, discoveryCtxKey{}, report)
}

// DiscoveryFromContext returns the attached Scout report, or (nil, false) when none was set
// (no Scout ran, or Scout degraded to empty) — the Planner then plans one-shot as before.
func DiscoveryFromContext(ctx context.Context) (*DiscoveryReport, bool) {
	r, ok := ctx.Value(discoveryCtxKey{}).(*DiscoveryReport)
	return r, ok && !r.IsEmpty()
}
