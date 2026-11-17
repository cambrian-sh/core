package network

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Memory recalls in the run ledger (ADR-0145 D1/D8;
// T-20261109-chat-agent-answers-a-published-rule). The citizen gate showed the
// chat agent answering the council's published FOI deadline correctly from the
// kernel's seed recall of the council's own procedure, while the D8 reply check
// counted the figures as uncited: recall results were not ledger entries, so a fact
// grounded in memory looked ungrounded, and the reply had nothing to cite. Every
// QueryMemory a lease-holding caller makes (the SDK's seed recall of a chat turn or
// plan step, and every memory_query) is now recorded as ONE ledger entry of the
// pseudo-tool domain.RecallToolName under the caller's lease provenance (the turn,
// or the run and step), so the reply check and the D6 envelope accept it as
// evidence and the hits carry its id (_ledger_ref) to cite.
//
// Classification: the entry's own tags are the kernel's write classification of
// the caller (as for any tool result), and each recalled chunk's own tags are
// carried as a source classification (LedgerRecord.SourceClassifications), so the
// entry is never readable by anyone who could not read every chunk in it.

// recallEntryHit is one recalled chunk in a recall entry's body.
type recallEntryHit struct {
	Text string `json:"text"`
	// Document is the source document's title (or URI) and Section its structural
	// path ("MADDE 4 – Cevap süresi", "4. Time for compliance"), so a reply can name
	// where an organisation rule or figure comes from.
	Document string `json:"document,omitempty"`
	Section  string `json:"section,omitempty"`
	ID       string `json:"id,omitempty"`
}

type recallEntryBody struct {
	Query string           `json:"query"`
	Hits  []recallEntryHit `json:"hits"`
}

// recallSource is the title (else the URI) of the document a recalled chunk came
// from, and its section path; "" when the chunk names none.
func recallSource(d domain.Document) (document, section string) {
	section = strings.TrimSpace(d.SectionPath)
	md := d.Metadata
	if md == nil {
		return "", section
	}
	if raw, ok := md["chunk_relations"]; ok {
		var rel struct {
			SiblingContext struct {
				ParentTitle string `json:"parent_title"`
			} `json:"sibling_context"`
		}
		var b []byte
		switch v := raw.(type) {
		case json.RawMessage:
			b = v
		case []byte:
			b = v
		case string:
			b = []byte(v)
		default:
			b, _ = json.Marshal(v)
		}
		if json.Unmarshal(b, &rel) == nil && strings.TrimSpace(rel.SiblingContext.ParentTitle) != "" {
			return strings.TrimSpace(rel.SiblingContext.ParentTitle), section
		}
	}
	for _, k := range []string{"title", "source_uri"} {
		if v, ok := md[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), section
		}
	}
	return "", section
}

// recordRecall writes a recall's ledger entry and returns its id ("" when the
// ledger is off, the caller holds no lease, nothing was recalled, or the write
// failed — the recall itself is never failed by its bookkeeping).
func (s *Server) recordRecall(ctx context.Context, callerID, query string, results []domain.SearchResult) string {
	if !s.ledgerOn() || len(results) == 0 {
		return ""
	}
	lease := callerLease(ctx, s.LLMGateway)
	if lease == "" {
		return ""
	}
	if _, known := s.resolveLeaseFor(ctx, lease); !known {
		return "" // only a lease the kernel issued (and the caller holds) places an entry
	}
	body := recallEntryBody{Query: query}
	var sources [][]string
	var refs []string
	for _, r := range results {
		if r.Document.ID == domain.SubstrateCitationsID {
			continue
		}
		text := r.Document.Text
		if r.Document.Summary != "" && text == "" {
			text = r.Document.Summary
		}
		doc, sec := recallSource(r.Document)
		body.Hits = append(body.Hits, recallEntryHit{Text: text, Document: doc, Section: sec, ID: r.Document.ID})
		sources = append(sources, append([]string{}, authz.DocTags(&r.Document)...))
		refs = append(refs, recallRefs(r.Document)...)
	}
	if len(body.Hits) == 0 {
		return ""
	}
	b, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	args, _ := json.Marshal(map[string]string{"query": query})
	e, err := s.Ledger.Writer.Record(ctx, domain.LedgerRecord{
		Kind:                  domain.LedgerToolResult,
		Provenance:            s.toolCallProvenance(ctx, lease),
		ToolName:              domain.RecallToolName,
		ArgsJSON:              args,
		Writer:                domain.AgentPrincipal(callerID),
		SourceClassifications: sources,
		SourceRefs:            refs,
		ContentType:           "application/json",
		Body:                  b,
	})
	if err != nil {
		slog.WarnContext(ctx, "ADR-0145: memory recall not recorded to the ledger", "caller", callerID, "err", err)
		return ""
	}
	return e.ID
}

// recallRefs are the memory records a recalled chunk is: its own id, its
// source document and its episode. They become the entry's SourceRefs, so an
// ADR-0137 erase of any of them reaches the recall entry and everything derived
// from it (ADR-0157 D5.6).
func recallRefs(d domain.Document) []string {
	refs := []string{d.ID}
	if doc, ok := d.Metadata["document_id"].(string); ok && doc != "" {
		refs = append(refs, doc)
	}
	if d.ExperienceID != "" {
		refs = append(refs, d.ExperienceID)
	}
	return refs
}
