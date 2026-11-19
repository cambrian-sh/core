package domain

import (
	"strings"
)

// Content provenance: the source class (ENG-531, owner decision 2026-11-19,
// T-20261104-no-content-provenance-reaches-tool).
//
// A source class answers "where did this text come from?" for content an agent
// consumes: a recalled memory chunk, a tool result. It is the label the
// Rule-of-Two gate (rule_of_two.go) reads to decide whether a session has
// consumed UNTRUSTED content — which classes count as untrusted is
// configuration (execution.tools.rule_of_two.untrusted_sources), never a Go
// branch. The set is CLOSED, like the tool effect classes (ADR-0086): an
// unknown class is a configuration error, not a silently ignored string.
//
// It is a TRUST source. The payload fence's DataProvenance.Class
// (untrusted_fence.go, ENG-94) names the rendering channel instead
// (recalled_memory, ingested_document, tool_output, worker_result). Both are
// kept (ENG-2223, owner decision 2026-11-19): every fence carries its channel
// as provenance= AND its source class as source=, so the model reads the same
// origin the gate does; DefaultSourceClass is how a channel maps when the exact
// source is unknown.

// SourceClass is where a piece of consumed content came from.
type SourceClass string

const (
	// SourceUser is text the person the session serves wrote themself.
	SourceUser SourceClass = "user"
	// SourceOperator is configuration and instructions the deployment's operator
	// authored (profiles, policies, skills).
	SourceOperator SourceClass = "operator"
	// SourceWeb is content fetched from outside the deployment (a web search, a
	// page fetch, any tool whose effects include egress or egress_read).
	SourceWeb SourceClass = "web"
	// SourceTool is a tool's result that is not web content: a native tool, an
	// MCP server's answer, a contributed worker tool, a kernel tool's read.
	SourceTool SourceClass = "tool"
	// SourceOtherAgent is text another agent (or the kernel recording an agent's
	// step output) wrote: model-generated, derived from whatever that agent read.
	SourceOtherAgent SourceClass = "other-agent"
	// SourceIngested is content a connector, an ingress, a document upload or a
	// poller brought in (a source:<connector>:<id> writer, ENG-494).
	SourceIngested SourceClass = "ingested"
)

// AllSourceClasses is the closed set, in a fixed order for listings.
var AllSourceClasses = []SourceClass{SourceUser, SourceOperator, SourceWeb, SourceTool, SourceOtherAgent, SourceIngested}

// ValidSourceClass reports whether c is in the closed set.
func ValidSourceClass(c SourceClass) bool {
	for _, x := range AllSourceClasses {
		if x == c {
			return true
		}
	}
	return false
}

// MetaSourceClass is the metadata key a recalled record's source class travels
// under to the agent (QueryMemory result metadata). Underscore-prefixed like the
// other kernel-stamped rendering facts, so it can never collide with a stored key
// — and stamped by the kernel on the way out, never read from stored metadata.
const MetaSourceClass = "_source_class"

// RecordSourceClass classifies a recalled memory record from facts the kernel
// stamped at write time — the provenance:source tag (authz.EnforcingStoreWriter
// stamps it from the writer's principal, never from the writer's own metadata)
// and the ingestion keys the ingestion manager writes.
//
//	provenance:source=source:<connector>:<id>       → ingested (ENG-494 source principal)
//	ingest_thread_id / source_uri / document_id set → ingested (a corpus chunk)
//	anything else                                   → other-agent
//
// Fail closed: memory has no writer that is provably the person or the operator
// (a person's chat text reaches memory through the kernel's extraction, an
// agent's facts through the agent), so nothing recalled classifies as user or
// operator. An operator who trusts agent-written memory removes other-agent from
// untrusted_sources; the label stays honest either way.
func RecordSourceClass(meta map[string]any, tags []string) SourceClass {
	for _, t := range tags {
		if v, ok := strings.CutPrefix(t, ProvenanceSourceTagPrefix); ok {
			if strings.HasPrefix(v, string(PrincipalSourceKind)+":") {
				return SourceIngested
			}
		}
	}
	if DocIngestThreadID(meta) != "" {
		return SourceIngested
	}
	for _, k := range []string{"source_uri", "document_id"} {
		if v, _ := meta[k].(string); v != "" {
			return SourceIngested
		}
	}
	return SourceOtherAgent
}

// ToolResultSourceClass classifies the result of a call of t. A tool that can
// reach outside the deployment (declared or inferred egress, egress_read or
// egress_send; an undeclared tool's fail-closed default includes egress) returns
// web content; every other tool's result is tool content. Effects come from the
// operator/catalog policy, never from an MCP server's own annotations.
func ToolResultSourceClass(t SystemTool) SourceClass {
	if len(t.Effects) == 0 {
		return SourceWeb // bypassed registration: assume the widest reach (fail closed)
	}
	for _, e := range t.Effects {
		if e == EffectEgress || e == EffectEgressRead || e == EffectEgressSend {
			return SourceWeb
		}
	}
	return SourceTool
}

// RecordTagsOf extracts a record's classification tags from its metadata,
// handling the []string and []any encodings (a JSON round trip produces the
// latter). The same reading authz.DocTags does, for domain callers.
func RecordTagsOf(meta map[string]any) []string {
	switch v := meta["tags"].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
