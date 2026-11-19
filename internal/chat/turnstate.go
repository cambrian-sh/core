package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/replycheck"
)

// The chat lane's own ledger entries (kind computed, stamped with the
// conversation and the turn). Tool names are the entry's call key, so each is
// found by a ledger query and never collides with a tool a worker called.
const (
	toolTurnIndex   = "chat_turn_index"
	toolSummary     = domain.LedgerToolConversationSummary
	toolReplyCheck  = "chat_reply_check"
	toolRequirement = "requirement_coverage"
	// stateQueryLimit bounds one query for the chat lane's own entries of a
	// conversation (one per delegated turn, per summary): far above any
	// conversation's count, and a query never spans conversations.
	stateQueryLimit = 1000
)

// recordState writes one chat-lane entry for the turn. derived names the ledger
// entries its content was built from, so it is never easier to read than they are
// (domain.LedgerEntryReadable). refs are the memory records the content quotes
// beyond what derived carries (the replies a summary condensed, the turn's whole
// recall), so an ADR-0137 erase of any of them deletes the entry (ENG-2155).
// Best-effort: a failed write is logged and the turn goes on.
func (s *TurnService) recordState(ctx context.Context, conv *domain.Conversation, turnID, tool string, v any, derived, refs []string) (string, bool) {
	if !s.ledgerOn() {
		return "", false
	}
	body, err := json.Marshal(v)
	if err != nil {
		slog.Warn("chat: turn state not encoded", "tool", tool, "err", err)
		return "", false
	}
	e, err := s.g.ledgerW.Record(ctx, domain.LedgerRecord{
		Kind: domain.LedgerComputed,
		Provenance: domain.LedgerProvenance{
			ConversationID: conv.ID, TurnID: turnID, StepIndex: -1, Owner: conv.OwnerID,
		},
		ToolName:    tool,
		Writer:      domain.SystemPrincipal,
		DerivedFrom: derived,
		SourceRefs:  refs,
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		slog.Warn("chat: turn state not recorded in the ledger", "tool", tool, "conversation", conv.ID, "err", err)
		return "", false
	}
	return e.ID, true
}

// findState returns the conversation's chat-lane entries of one tool, oldest
// first, with their bodies.
func (s *TurnService) findState(ctx context.Context, conv *domain.Conversation, tool string) []domain.LedgerEntry {
	if !s.ledgerOn() {
		return nil
	}
	rctx := ownerScope(ctx, conv.OwnerID)
	list, err := s.g.ledgerR.Find(rctx, domain.LedgerQuery{
		ConversationID: conv.ID, ToolName: tool, Kind: domain.LedgerComputed, Limit: stateQueryLimit,
	})
	if err != nil {
		slog.Warn("chat: turn state query failed", "tool", tool, "conversation", conv.ID, "err", err)
		return nil
	}
	return list
}

// readBody reads one entry's body through the enforced reader.
func (s *TurnService) readBody(ctx context.Context, owner, id string) (*domain.LedgerEntry, error) {
	e, err := s.g.ledgerR.Get(ownerScope(ctx, owner), id)
	if err != nil {
		return nil, fmt.Errorf("ledger get %s: %w", id, err)
	}
	return e, nil
}

// turnEntries returns the ledger entries a turn produced (every tool result, step
// output and calculation of the runs it delegated), oldest first, at most limit.
func (s *TurnService) turnEntries(ctx context.Context, conv *domain.Conversation, turnID string, limit int) []domain.LedgerEntry {
	if !s.ledgerOn() || turnID == "" {
		return nil
	}
	list, err := s.g.ledgerR.Find(ownerScope(ctx, conv.OwnerID), domain.LedgerQuery{
		ConversationID: conv.ID, TurnID: turnID, Limit: limit,
	})
	if err != nil {
		slog.Warn("chat: the turn's ledger entries could not be read", "conversation", conv.ID, "turn", turnID, "err", err)
		return nil
	}
	return list
}

// turnSourceRefs returns the memory records (chunk, document, experience ids) the
// turn's reply rests on: the union of the SourceRefs of every ledger entry the turn
// produced — its recalls (recall_ledger.go), its tool results and, transitively
// through DerivedFrom, the steps of the runs it delegated. The chat lane stores them
// with the reply (domain.Message.SourceRefs) so an ADR-0137 erase of any of them
// redacts it (owner decision 2026-11-18, ENG-2109). Kernel records only, never model
// text, and sorted: the same turn always records the same refs.
//
// The whole turn rather than only the [led_…] entries the reply cited: markers are
// optional (grounded_reply and reply_check default off) and stripped before the reply
// is stored, so a reply quoting an uncited recall would otherwise keep a forgotten
// value. The reach is bounded to replies of turns that recalled the record.
//
// Read under domain.ScopeMaintenance: the result is ids written onto the reply row,
// never content shown to a principal, and an entry a tag fence hid here would be a
// reference the erase could not follow. Scoped by conversation and turn.
func (s *TurnService) turnSourceRefs(ctx context.Context, conversationID, turnID string) []string {
	if !s.ledgerOn() || turnID == "" {
		return nil
	}
	list, err := s.g.ledgerR.Find(domain.WithScope(ctx, domain.ScopeMaintenance), domain.LedgerQuery{
		ConversationID: conversationID, TurnID: turnID, Limit: stateQueryLimit,
	})
	if err != nil {
		// Not silent (AGENTS.md §3.2): a reply stored without its sources is one a
		// later erase of them cannot redact.
		slog.Warn("chat: the reply's sources could not be read; it is stored without them and a forget of them will not redact it",
			"conversation", conversationID, "turn", turnID, "err", err)
		return nil
	}
	var sets [][]string
	for _, e := range list {
		// ENG-2165: the chat lane's own records of the turn (its summary, index entry
		// and reply-check report) carry the BROAD refs of what the turn was given, so an
		// erase deletes them; they are not what the turn recalled, and counting them as
		// the reply's own would hand every given ref on unconditionally again.
		if chatLaneTool[e.ToolName] {
			continue
		}
		sets = append(sets, e.SourceRefs)
	}
	return unionRefs(sets...)
}

// chatLaneTool names the chat lane's own ledger entries (recordState).
var chatLaneTool = map[string]bool{toolTurnIndex: true, toolSummary: true, toolReplyCheck: true}

// unionRefs is the sorted, de-duplicated union of ref sets without empty ids; nil
// when there are none.
func unionRefs(sets ...[]string) []string {
	var refs []string
	for _, set := range sets {
		for _, r := range set {
			if r != "" {
				refs = append(refs, r)
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return domain.SortedUniqueIDs(refs)
}

// messageRefs is the union of the source refs of msgs that still hold their text:
// what a turn, a summary or an emitted message given these messages may repeat
// (ENG-2155). A redacted reply holds only domain.RedactedReplyText, so its refs no
// longer travel: they would make every later reply rest on a record already gone.
func messageRefs(msgs []domain.Message) []string {
	var sets [][]string
	for _, m := range msgs {
		if m.RedactedAt == nil {
			sets = append(sets, m.SourceRefs)
		}
	}
	return unionRefs(sets...)
}

// givenSource is one thing a turn was given besides its own recalls, with the memory
// records behind it (ENG-2165, owner decision 2026-11-19, option B). always are
// inherited by the turn's reply whatever it says: the OWN refs of the immediately
// preceding agent reply (one hop). onOverlap are inherited only when the reply states a
// distinctive value text also holds (replycheck.SharesValue): an older reply's refs, the
// preceding reply's inherited refs, the rolling summary's and the turn index's. Before,
// every given ref travelled unconditionally, so one recall of a document reached every
// later reply of the conversation and forgetting it blanked them all.
type givenSource struct {
	text      string
	always    []string
	onOverlap []string
}

// historySources is the transcript as given sources: the preceding agent reply's own
// refs always and its inherited ones on overlap; every older reply's refs on overlap. A
// redacted reply holds only domain.RedactedReplyText, so its refs no longer travel
// (they would make every later reply rest on a record already gone).
func historySources(msgs []domain.Message) []givenSource {
	var out []givenSource
	preceding := true
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != domain.MessageRoleAgent {
			continue
		}
		if m.RedactedAt != nil || len(m.SourceRefs) == 0 {
			preceding = false
			continue
		}
		g := givenSource{text: m.Content}
		if preceding {
			g.always = unionRefs(m.OwnSourceRefs)
			g.onOverlap = minusRefs(m.SourceRefs, g.always)
		} else {
			g.onOverlap = unionRefs(m.SourceRefs)
		}
		preceding = false
		out = append(out, g)
	}
	return out
}

// inheritedRefs is what a reply text inherits from sources: every always ref, and the
// onOverlap refs of each source the reply shares a distinctive value with.
func inheritedRefs(reply string, sources []givenSource, opt replycheck.Options) []string {
	var sets [][]string
	for _, g := range sources {
		sets = append(sets, g.always)
		if len(g.onOverlap) > 0 && replycheck.SharesValue(reply, g.text, opt) {
			sets = append(sets, g.onOverlap)
		}
	}
	return unionRefs(sets...)
}

// broadRefs is every ref behind sources, gated by nothing: for a kernel record that
// quotes the turn's whole input (the reply-check report), which an erase of any of them
// must delete.
func broadRefs(sources []givenSource) []string {
	var sets [][]string
	for _, g := range sources {
		sets = append(sets, g.always, g.onOverlap)
	}
	return unionRefs(sets...)
}

// minusRefs is set a without the ids in b, sorted and unique; nil when empty.
func minusRefs(a, b []string) []string {
	drop := make(map[string]bool, len(b))
	for _, r := range b {
		drop[r] = true
	}
	var out []string
	for _, r := range a {
		if !drop[r] {
			out = append(out, r)
		}
	}
	return unionRefs(out)
}
