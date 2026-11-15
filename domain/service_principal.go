package domain

import "strings"

// Service principals (owner decision 2026-11-15, ENG-494 / ENG-449 / ENG-263, option A).
//
// Every write that has no person or agent behind it runs as a DETERMINISTIC service
// principal instead of no principal or one shared kernel identity:
//
//   - source:<connector>:<id> — content a connector, an ingress, a plugin or the
//     reactive lane brought in. One principal per source, so its facts supersede per
//     source (the provenance stamp is part of the fact's classification, hence of its
//     resolution key) and two connectors never merge.
//   - system:kernel — records the kernel authors itself (agent profiles, judicial
//     records). SystemPrincipal already renders as "system:kernel".
//
// Both go through the normal write chokepoint (Authorizer.ClassifyWrite) with the
// evidence classification as the hint, and the decision point stamps
// provenance:source=<WriterID>. The value is KIND-QUALIFIED for service principals so
// it can never collide with an agent id; agent and user stamps keep their bare id, as
// they always had (the fact backfill filters by them).

// PrincipalSourceKind is the kind of a source service principal. (PrincipalSource is
// the person registry's "who created this account" type, hence the longer name.)
const PrincipalSourceKind PrincipalKind = "source"

// defaultSourceID is the id part of a source principal whose source named none.
const defaultSourceID = "default"

// SourcePrincipal is the deterministic service principal of one source:
// String() is "source:<connector>:<id>". The connector part never contains ':' (it is
// replaced by '_'), so the first ':' of the id always separates connector from id. An
// empty connector yields the zero principal — a source with no name is not a source.
func SourcePrincipal(connector, id string) PrincipalRef {
	connector = strings.ReplaceAll(strings.TrimSpace(connector), ":", "_")
	if connector == "" {
		return PrincipalRef{}
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = defaultSourceID
	}
	return PrincipalRef{ID: connector + ":" + id, Kind: PrincipalSourceKind}
}

// EvidenceSourcePrincipal is the source principal of a piece of evidence: its SourceID
// ("slack:workspace-x", "erp:acme") read as <connector>:<id>; a SourceID with no ':'
// is the connector, and the namespace is the id.
func EvidenceSourcePrincipal(ev Evidence) PrincipalRef {
	connector, id, found := strings.Cut(strings.TrimSpace(ev.SourceID), ":")
	if !found {
		id = ev.NamespaceID
	}
	return SourcePrincipal(connector, id)
}

// IsService reports whether p is a service principal (system or source): a writer
// with no person or agent behind it.
func (p PrincipalRef) IsService() bool {
	return !p.IsZero() && (p.Kind == PrincipalSystem || p.Kind == PrincipalSourceKind)
}

// SourceConnector is the connector part of a source principal ("" otherwise).
func (p PrincipalRef) SourceConnector() string {
	if p.Kind != PrincipalSourceKind {
		return ""
	}
	c, _, _ := strings.Cut(p.ID, ":")
	return c
}

// WriterID is the writer-of-record id a write by p carries: the provenance:source
// stamp and the chunks' source_agent_id. Service principals are kind-qualified
// ("system:kernel", "source:slack:ws") so they cannot collide with an agent id; agents
// and users keep their bare id, unchanged from before service principals existed.
func (p PrincipalRef) WriterID() string {
	if p.IsService() {
		return p.String()
	}
	return p.ID
}

// IsReservedPrincipalID reports whether id is shaped like a service principal's
// writer id. No agent may be registered under one: its provenance stamp would be
// indistinguishable from the service principal's.
func IsReservedPrincipalID(id string) bool {
	return strings.HasPrefix(id, string(PrincipalSystem)+":") || strings.HasPrefix(id, string(PrincipalSourceKind)+":")
}

// HasProvenanceSource reports whether tags already carry a provenance:source stamp.
func HasProvenanceSource(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, ProvenanceSourceTagPrefix) {
			return true
		}
	}
	return false
}

// StampProvenanceSource returns tags with exactly one provenance:source=<writerID>,
// any stamp already present removed (a writer never sets its own provenance). An
// empty writerID returns tags unchanged.
func StampProvenanceSource(tags []string, writerID string) []string {
	if writerID == "" {
		return tags
	}
	out := make([]string, 0, len(tags)+1)
	for _, t := range tags {
		if strings.HasPrefix(t, ProvenanceSourceTagPrefix) {
			continue
		}
		out = append(out, t)
	}
	return append(out, ProvenanceSourceTagPrefix+writerID)
}
