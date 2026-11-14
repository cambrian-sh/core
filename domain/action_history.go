package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ─────────────────────────────────────────────────────────────────────────────
// Conversation action history and the approver's duplicate notice
// (T-20261107-approver-is-not-told-that-an-identical-side).
//
// An approval for a side-effecting tool call used to reach the approver with the
// call alone: tool, a 200-char argument preview, the rule. The only record of what
// had already been done was the per-RUN plan ledger (network/plan_actions.go),
// which is rendered into the plan's result for its caller and deleted when the
// plan returns. A chat agent that re-delegates opens a NEW run, so its second
// write was approved blind — twice in the usecases runs: issues re-created with a
// slightly different summary ('INC-1040 (Ashvale) - …' vs 'INC-1040 - …') and
// mails re-sent with near-identical bodies.
//
// ActionHistory is the conversation-scoped record the approval path consults. The
// ToolExecutor — the one reference monitor, which sees the final arguments and the
// approval id — records every side-effecting call of a conversation (pending while
// it waits for approval, approved while it runs, then done; a refused call is
// dropped: it never ran, and so is a call that FAILED — owner decision 2026-11-10,
// T-20261109-duplicate-notice-fires-on-the-retry-of-a-write: the notice counts
// only successful earlier writes, so the corrected retry of a write the system
// rejected is not flagged). Before it raises an approval it matches the call
// against that record DETERMINISTICALLY (no model) and attaches what it found to
// the approval item as a notice for the approver. It annotates, never refuses: a
// legitimate repeat (the same reminder sent again) must stay approvable.
//
// In-memory and bounded, like the plan ledger: a restart forgets it, and the
// notice is a hint to a person, not a control.
// ─────────────────────────────────────────────────────────────────────────────

// Action history entry states.
const (
	ActionAwaitingApproval = "awaiting approval"
	ActionApproved         = "approved, running"
	ActionRunning          = "running"
	ActionDone             = "done"
)

// Bounds that keep the history's memory finite. They are capacity, not policy:
// the policy knob is the window (execution.tools.duplicate_notice_window_minutes).
const (
	actionHistoryPerScope  = 200
	actionHistoryMaxScopes = 4096
	duplicateNoticeMax     = 3
)

// ActionRecord is one side-effecting call in the history.
type ActionRecord struct {
	Tool string
	// Args is the call's argument JSON as dispatched (after any hook rewrite).
	// Held in memory only, for matching; never rendered whole.
	Args []byte
	// ApprovalID is the approval that admitted the call ("" when none was needed).
	ApprovalID string
	// IDs are the identifiers the call returned (PlanActionIDs).
	IDs   []string
	State string
	// At is when the entry last changed state (raise, approval, completion).
	At time.Time

	seq  uint64
	norm actionArgs
}

// ActionRef names one entry so its state can be advanced; the zero value is a
// no-op handle (no history, no scope, not side-effecting).
type ActionRef struct {
	scope string
	seq   uint64
}

// IsZero reports whether r names no entry.
func (r ActionRef) IsZero() bool { return r.seq == 0 }

// ActionHistory is the conversation-scoped record of side-effecting tool calls.
// Safe for concurrent use; a nil *ActionHistory records and matches nothing.
type ActionHistory struct {
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	seq     uint64
	scopes  map[string][]ActionRecord
	touched map[string]time.Time
}

// NewActionHistory builds a history whose matches look back window. window <= 0
// returns nil: the notice is disabled and every method is a no-op.
func NewActionHistory(window time.Duration, now func() time.Time) *ActionHistory {
	if window <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &ActionHistory{window: window, now: now, scopes: map[string][]ActionRecord{}, touched: map[string]time.Time{}}
}

// Begin opens an entry for a call about to be approved or dispatched. scope ""
// records nothing.
func (h *ActionHistory) Begin(scope, tool string, args []byte, state string) ActionRef {
	if h == nil || scope == "" {
		return ActionRef{}
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	rec := ActionRecord{Tool: tool, Args: append([]byte(nil), args...), State: state, At: now, seq: h.seq, norm: normaliseActionArgs(args)}
	list := append(h.prune(scope, now), rec)
	if len(list) > actionHistoryPerScope {
		list = append([]ActionRecord(nil), list[len(list)-actionHistoryPerScope:]...)
	}
	h.scopes[scope] = list
	h.touched[scope] = now
	h.evict()
	return ActionRef{scope: scope, seq: h.seq}
}

// Approved marks ref approved under approval id, running.
func (h *ActionHistory) Approved(ref ActionRef, approvalID string) {
	h.update(ref, func(r *ActionRecord) { r.State, r.ApprovalID = ActionApproved, approvalID })
}

// Finish records a dispatched call that succeeded: done, with the ids its result
// names. A call that failed is not finished but dropped (Drop): only a successful
// earlier write counts as one a later call may duplicate (owner decision
// 2026-11-10, T-20261109-duplicate-notice-fires-on-the-retry-of-a-write). The
// incident it fixes: a calendar_create_event refused with not_found ("No user
// Fenella Oakhurst in the directory") stayed in the history as "failed (it may or
// may not have taken effect)", so the corrected retry was flagged and denied.
func (h *ActionHistory) Finish(ref ActionRef, ids []string) {
	h.update(ref, func(r *ActionRecord) {
		r.State = ActionDone
		r.IDs = append([]string(nil), ids...)
	})
}

// Drop removes ref: the call was refused and never ran, or it ran and failed.
func (h *ActionHistory) Drop(ref ActionRef) {
	if h == nil || ref.IsZero() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.scopes[ref.scope]
	for i := range list {
		if list[i].seq == ref.seq {
			h.scopes[ref.scope] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

func (h *ActionHistory) update(ref ActionRef, fn func(*ActionRecord)) {
	if h == nil || ref.IsZero() {
		return
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.scopes[ref.scope]
	for i := range list {
		if list[i].seq == ref.seq {
			fn(&list[i])
			list[i].At = now
			return
		}
	}
}

// Matches returns the earlier entries of scope that the call (tool, args) may
// duplicate, most recent first, at most duplicateNoticeMax. except excludes the
// call's own entry.
func (h *ActionHistory) Matches(scope, tool string, args []byte, except ActionRef) []ActionMatch {
	if h == nil || scope == "" {
		return nil
	}
	cand := normaliseActionArgs(args)
	now := h.now()
	h.mu.Lock()
	list := append([]ActionRecord(nil), h.prune(scope, now)...)
	h.mu.Unlock()
	var out []ActionMatch
	for i := len(list) - 1; i >= 0 && len(out) < duplicateNoticeMax; i-- {
		r := list[i]
		if r.seq == except.seq || r.Tool != tool || h.expired(r, now) {
			continue
		}
		if how, ok := matchActionArgs(r.norm, cand); ok {
			out = append(out, ActionMatch{Prior: r, How: how})
		}
	}
	return out
}

// expired reports whether r is outside the window. A call still waiting for its
// approval or still running never expires: it is the most likely to be duplicated.
func (h *ActionHistory) expired(r ActionRecord, now time.Time) bool {
	return now.Sub(r.At) > h.window && r.State != ActionAwaitingApproval && r.State != ActionApproved &&
		r.State != ActionRunning
}

// prune drops the expired entries at the head of scope's list (entries are in
// Begin order; one whose state changed later may outlive an expired one behind
// it, which Matches skips). Caller holds mu.
func (h *ActionHistory) prune(scope string, now time.Time) []ActionRecord {
	list := h.scopes[scope]
	cut := 0
	for cut < len(list) && h.expired(list[cut], now) {
		cut++
	}
	if cut > 0 {
		list = append([]ActionRecord(nil), list[cut:]...)
		h.scopes[scope] = list
	}
	return list
}

// evict bounds the number of scopes, dropping the least recently touched. Caller
// holds mu.
func (h *ActionHistory) evict() {
	if len(h.scopes) <= actionHistoryMaxScopes {
		return
	}
	oldest, at := "", time.Time{}
	for s, t := range h.touched {
		if oldest == "" || t.Before(at) {
			oldest, at = s, t
		}
	}
	delete(h.scopes, oldest)
	delete(h.touched, oldest)
}

// SideEffecting reports whether a call of t can change the world: a dangerous
// tool, or one declaring write, spend, admin or egress_send. The one
// classification the plan ledger (network/plan_actions.go), the action history,
// the ADR-0145 D9 idempotency cache and the request guards use.
//
// egress_send (ADR-0150 D9) is by definition a send — a message, an email, a
// webhook, a state-changing remote call — so a repeat is exactly the duplicate
// these mechanisms exist to catch, dangerous or not.
//
// egress_read is a fetch and does not count. Plain (legacy) egress does not count
// either: the shipped catalog declares [read, egress] on ~27 non-dangerous pure
// reads (mail search, calendar listing, channel history), and counting it would
// put a duplicate notice and a ledger entry on every repeated read. Every shipped
// tool that sends over plain egress is dangerous or also declares write, so it is
// already covered above. Whether plain egress should count is open
// (T-20261113-systemtool-sideeffecting-ignores-egress-a-non).
func (t SystemTool) SideEffecting() bool {
	if t.Dangerous {
		return true
	}
	for _, e := range t.Effects {
		switch e {
		case EffectWrite, EffectSpend, EffectAdmin, EffectEgressSend:
			return true
		}
	}
	return false
}

// ActionHistoryScope is the history scope of a call: its conversation, else its
// task session; "" when it has neither (nothing to compare against).
func ActionHistoryScope(conversationID, sessionID string) string {
	switch {
	case conversationID != "":
		return "conversation:" + conversationID
	case sessionID != "":
		return "session:" + sessionID
	}
	return ""
}

// ── matching ────────────────────────────────────────────────────────────────

// ActionMatch is one earlier entry a call may duplicate, and why.
type ActionMatch struct {
	Prior ActionRecord
	// How is "identical arguments" or "same <keys>" (the target fields that agree).
	How string
}

// actionArgs is a call's arguments, normalised for comparison.
type actionArgs struct {
	// canonical is the whole argument set, normalised (case, whitespace, key and
	// list order, empty values dropped); "" when the args were not a JSON object.
	canonical string
	// leaves maps each leaf field's NAME (the last path segment, lower-case) to
	// its normalised value(s). A list of scalars is one sorted, comma-joined value.
	leaves map[string]string
	// lists names the leaves whose value was a list of scalars — the structured
	// part of a decision (release / withhold / refer lists, labels, attendees).
	lists map[string]bool
}

// Field roles. A TARGET says whom or what the write reaches (recipient, project,
// path); a RECORD KEY is the target that names one existing record the write acts
// on (a case, request or ticket reference); a SUBJECT names it (subject, title,
// summary); a BODY is its content. The names are the common argument names of
// mail, chat, ticketing, calendar, case-management and file tools; a tool whose
// arguments use none of them is compared on identical arguments only.
var (
	actionTargetFields = fieldSet("to", "cc", "bcc", "recipient", "recipients", "email", "emails", "address",
		"channel", "channel_id", "chat_id", "user", "user_id", "assignee", "phone", "phone_number",
		"project", "project_key", "projectkey", "project_id", "repo", "repository", "owner", "path", "file",
		"file_path", "filepath", "url", "issue", "issue_key", "issue_id", "issuekey", "parent", "parent_key",
		"calendar_id", "event_id", "thread_id", "page_id", "database_id", "folder", "folder_id", "list_id",
		"board", "board_id", "space", "space_key", "team", "team_id", "account", "account_id", "customer",
		"customer_id", "contact", "contact_id")
	// T-20261108-duplicate-notice-misses-a-repeated-record-write: the records
	// incident (cases_respond_request {ref: FOI-2026-0001, …} recorded twice, the
	// free-text 'bases' reworded) matched nothing, because 'ref' was no target and
	// the args carry no subject. A record key is also a target (a differing one
	// vetoes the match).
	actionRecordKeyFields = fieldSet("ref", "reference", "reference_id", "case", "case_id", "case_ref",
		"case_number", "request", "request_id", "request_ref", "record", "record_id", "record_ref",
		"ticket", "ticket_id", "order_id", "invoice_id", "document_id", "doc_id", "id")
	actionSubjectFields = fieldSet("subject", "title", "summary", "name", "topic", "headline")
	actionBodyFields    = fieldSet("body", "text", "message", "content", "description", "html", "markdown",
		"comment", "note", "notes", "letter", "reply")
)

func init() {
	for k := range actionRecordKeyFields {
		actionTargetFields[k] = true
	}
}

func fieldSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// Similarity thresholds over word-token sets (Jaccard). A subject that agrees on
// most words, or whose words are all inside the other's ('INC-1040 - Outage' in
// 'INC-1040 (Ashvale) - Outage'), names the same thing; a body must agree almost
// entirely, since bodies are long and share filler words.
const (
	subjectJaccard = 0.6
	bodyJaccard    = 0.8
)

// matchActionArgs reports whether cand may duplicate prior, and how.
//
//  1. identical: the normalised argument sets are equal;
//  2. same target: no target field present in both differs, AND the call names
//     the same thing — similar subjects when both have one, otherwise (neither
//     has one) near-identical bodies — AND at least one target or subject field
//     is shared, so two calls with nothing in common never match;
//  3. same record decision (neither has a subject, bodies absent or differing):
//     a record key agrees, and the structured lists outside the targets agree —
//     at least one shared, none differing, none present on one side only. A
//     reworded free-text field (a 'bases' or 'reason' note) does not make the
//     same decision on the same record a different write.
func matchActionArgs(prior, cand actionArgs) (string, bool) {
	if prior.canonical != "" && prior.canonical == cand.canonical {
		return "identical arguments", true
	}
	var agreed []string
	for k, pv := range prior.leaves {
		if !actionTargetFields[k] {
			continue
		}
		cv, ok := cand.leaves[k]
		if !ok {
			continue
		}
		if pv != cv {
			return "", false
		}
		agreed = append(agreed, k)
	}
	ps, pHas := firstField(prior.leaves, actionSubjectFields)
	cs, cHas := firstField(cand.leaves, actionSubjectFields)
	switch {
	case pHas && cHas:
		if !similarText(ps, cs, subjectJaccard, true) {
			return "", false
		}
		agreed = append(agreed, "subject")
	case pHas != cHas:
		return "", false
	default:
		if len(agreed) == 0 {
			return "", false
		}
		pb, pBody := firstField(prior.leaves, actionBodyFields)
		cb, cBody := firstField(cand.leaves, actionBodyFields)
		if pBody && cBody && similarText(pb, cb, bodyJaccard, false) {
			agreed = append(agreed, "content")
			break
		}
		lists, ok := sameRecordDecision(prior, cand, agreed)
		if !ok {
			return "", false
		}
		agreed = append(agreed, lists...)
	}
	sort.Strings(agreed)
	return "same " + strings.Join(agreed, ", "), true
}

// sameRecordDecision is rule 3 of matchActionArgs: given the target fields that
// agreed, it returns the structured list fields that agree when a record key is
// among them and every non-target list field matches on both sides.
func sameRecordDecision(prior, cand actionArgs, agreed []string) ([]string, bool) {
	keyed := false
	for _, k := range agreed {
		keyed = keyed || actionRecordKeyFields[k]
	}
	if !keyed {
		return nil, false
	}
	var same []string
	for k := range prior.lists {
		if actionTargetFields[k] {
			continue
		}
		if !cand.lists[k] || prior.leaves[k] != cand.leaves[k] {
			return nil, false
		}
		same = append(same, k)
	}
	for k := range cand.lists {
		if !actionTargetFields[k] && !prior.lists[k] {
			return nil, false
		}
	}
	if len(same) == 0 {
		return nil, false
	}
	return same, true
}

func firstField(leaves map[string]string, set map[string]bool) (string, bool) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := leaves[k]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// similarText compares two normalised texts by their word-token sets: equal,
// Jaccard >= min, or (subsetOK) every word of the shorter (two or more words) in
// the longer.
func similarText(a, b string, min float64, subsetOK bool) bool {
	if a == b {
		return true
	}
	ta, tb := tokenSet(a), tokenSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if float64(inter)/float64(union) >= min {
		return true
	}
	small := len(ta)
	if len(tb) < small {
		small = len(tb)
	}
	return subsetOK && small >= 2 && inter == small
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '@' || r == '.')
	}) {
		f = strings.Trim(f, "-_.")
		if f != "" {
			out[f] = true
		}
	}
	return out
}

// normaliseActionArgs parses and normalises a call's argument JSON.
func normaliseActionArgs(args []byte) actionArgs {
	var v map[string]any
	if json.Unmarshal(args, &v) != nil || v == nil {
		return actionArgs{}
	}
	n := normaliseValue(v)
	out := actionArgs{leaves: map[string]string{}, lists: map[string]bool{}}
	if b, err := json.Marshal(n); err == nil {
		out.canonical = string(b)
	}
	collectLeaves(n, "", out.leaves, out.lists)
	return out
}

// normaliseValue lower-cases and whitespace-folds strings, drops empty values,
// and sorts lists of scalars (recipient order carries no meaning).
func normaliseValue(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ToLower(strings.Join(strings.Fields(t), " "))
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			nx := normaliseValue(x)
			if isEmptyValue(nx) {
				continue
			}
			m[strings.ToLower(k)] = nx
		}
		return m
	case []any:
		out := make([]any, 0, len(t))
		scalars := true
		for _, x := range t {
			nx := normaliseValue(x)
			if isEmptyValue(nx) {
				continue
			}
			if _, isMap := nx.(map[string]any); isMap {
				scalars = false
			} else if _, isList := nx.([]any); isList {
				scalars = false
			}
			out = append(out, nx)
		}
		if scalars {
			sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
		}
		return out
	}
	return v
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// collectLeaves records each scalar (or list-of-scalars) field under its own
// name. A name seen twice keeps its first value in key order, deterministically.
func collectLeaves(v any, name string, out map[string]string, lists map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectLeaves(t[k], k, out, lists)
		}
	case []any:
		var parts []string
		for _, x := range t {
			switch x.(type) {
			case map[string]any, []any:
				collectLeaves(x, name, out, lists)
			default:
				parts = append(parts, fmt.Sprint(x))
			}
		}
		if name != "" && len(parts) > 0 {
			if _, seen := out[name]; !seen {
				out[name] = strings.Join(parts, ",")
				lists[name] = true
			}
		}
	default:
		if name == "" {
			return
		}
		if _, seen := out[name]; !seen {
			out[name] = fmt.Sprint(t)
		}
	}
}

// ── the notice ──────────────────────────────────────────────────────────────

// DuplicateNoticePrefix opens every duplicate notice, so a surface (or a test)
// can recognise one without parsing it.
const DuplicateNoticePrefix = "Possible duplicate"

// RenderDuplicateNotice words the matches for the approver, or "" when there are
// none: "Possible duplicate of mcp:jira/create_issue (approval apr-…) — done at
// 2026-11-07 17:02:11 UTC, ids: key=OPS-11; same project_key, subject." One
// sentence per match, most recent first; times in UTC.
func RenderDuplicateNotice(matches []ActionMatch) string {
	if len(matches) == 0 {
		return ""
	}
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		var b strings.Builder
		b.WriteString(DuplicateNoticePrefix + " of " + m.Prior.Tool)
		if m.Prior.ApprovalID != "" {
			b.WriteString(" (approval " + m.Prior.ApprovalID + ")")
		}
		b.WriteString(" — " + m.Prior.State + " at " + m.Prior.At.UTC().Format("2006-01-02 15:04:05 UTC"))
		if len(m.Prior.IDs) > 0 {
			b.WriteString(", ids: " + strings.Join(m.Prior.IDs, ", "))
		}
		b.WriteString("; " + m.How + ".")
		parts = append(parts, b.String())
	}
	return strings.Join(parts, " ")
}
