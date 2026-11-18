package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// memLedger is an in-memory ledger writer + reader. Like authz.EnforcingLedger it
// refuses a read with no scope predicate on ctx (fail-closed) and applies the
// owner fence of a bypass predicate.
type memLedger struct {
	mu      sync.Mutex
	entries []domain.LedgerEntry
	n       int
}

func (l *memLedger) Record(_ context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	e := domain.LedgerEntry{
		ID: fmt.Sprintf("led_%032x", l.n), Kind: rec.Kind, ConversationID: rec.Provenance.ConversationID,
		TurnID: rec.Provenance.TurnID, RunID: rec.Provenance.RunID, StepIndex: rec.Provenance.StepIndex,
		ToolName: rec.ToolName, Owner: rec.Provenance.Owner, Body: rec.Body, SourceRefs: rec.SourceRefs,
		Preview: domain.LedgerPreview(rec.Body, 300), CreatedAt: time.Now(),
	}
	l.entries = append(l.entries, e)
	out := e
	out.Body = nil
	return out, nil
}

func (l *memLedger) admit(ctx context.Context, e domain.LedgerEntry) (bool, error) {
	eff, ok := domain.ScopeFromContext(ctx)
	if !ok || eff == nil {
		return false, errors.New("scope missing")
	}
	if len(eff.Owners) > 0 && e.Owner != "" && e.Owner != eff.Owners[0] {
		return false, nil
	}
	return true, nil
}

func (l *memLedger) Get(ctx context.Context, id string) (*domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.ID == id {
			ok, err := l.admit(ctx, e)
			if err != nil || !ok {
				return nil, err
			}
			cp := e
			return &cp, nil
		}
	}
	return nil, nil
}

func (l *memLedger) Find(ctx context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.LedgerEntry
	for _, e := range l.entries {
		if (q.ConversationID != "" && e.ConversationID != q.ConversationID) || (q.TurnID != "" && e.TurnID != q.TurnID) ||
			(q.ToolName != "" && e.ToolName != q.ToolName) || (q.Kind != "" && e.Kind != q.Kind) {
			continue
		}
		ok, err := l.admit(ctx, e)
		if err != nil {
			return nil, err
		}
		if ok {
			cp := e
			cp.Body = nil
			out = append(out, cp)
		}
	}
	return out, nil
}

func (l *memLedger) tool(name string) []domain.LedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.LedgerEntry
	for _, e := range l.entries {
		if e.ToolName == name {
			out = append(out, e)
		}
	}
	return out
}

// scriptedPool replies from a script, one reply per dispatch, and records the
// handoffs; it also "runs" the delegations of a turn by putting results under
// the turn's origin message (what TurnDelegations captures from Execute).
type scriptedPool struct {
	mu       sync.Mutex
	replies  []string
	handoffs []*domain.Handoff
	// onDispatch runs before replying (to record ledger entries / results for the turn).
	onDispatch func(h *domain.Handoff, call int)
}

func (p *scriptedPool) Dispatch(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	p.mu.Lock()
	p.handoffs = append(p.handoffs, h)
	call := len(p.handoffs)
	reply := "ok"
	if len(p.replies) > 0 {
		reply, p.replies = p.replies[0], p.replies[1:]
	}
	f := p.onDispatch
	p.mu.Unlock()
	if f != nil {
		f(h, call)
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(reply)}}, nil
}

type groundingRig struct {
	svc     *TurnService
	store   *memStore
	pool    *scriptedPool
	ledger  *memLedger
	binder  *recordingBinder
	results map[string][]domain.ExecuteResult
	turns   []string // user message id per turn
}

func newGroundingRig(t *testing.T, g Grounding) *groundingRig {
	t.Helper()
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatal(err)
	}
	r := &groundingRig{store: store, pool: &scriptedPool{}, ledger: &memLedger{}, binder: &recordingBinder{},
		results: map[string][]domain.ExecuteResult{}}
	r.svc = NewTurnService(store, r.pool, nil)
	r.svc.Grounding = g
	r.svc.SetLedger(r.ledger, r.ledger)
	r.svc.SetTurnResults(func(origin string) []domain.ExecuteResult { return r.results[origin] })
	r.svc.SetLeaseBinder(r.binder, "chat_agent")
	lease := 0
	r.svc.acquireToken = func(context.Context, int, time.Duration) (string, func(), error) {
		lease++
		return fmt.Sprintf("lease-%d", lease), func() {}, nil
	}
	return r
}

// turnID is the origin message id of the turn being dispatched (the user message
// just appended).
func (r *groundingRig) lastUserID() string {
	msgs, _ := r.store.ListMessages(context.Background(), "c1", 0, 0)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == domain.MessageRoleUser {
			return msgs[i].ID
		}
	}
	return ""
}

func (r *groundingRig) turn(t *testing.T, text string) (domain.Message, *domain.Handoff) {
	t.Helper()
	before := len(r.pool.handoffs)
	msg, err := r.svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: text})
	if err != nil {
		t.Fatalf("turn %q: %v", text, err)
	}
	return msg, r.pool.handoffs[before]
}

// o3xResult is a delegated run that drafted three reminders, with the checklist.
func o3xResult() domain.ExecuteResult {
	cov := `{"checklist":{"version":1},"items":[` +
		`{"item":{"id":"R1","type":"answer","text":"List the customers more than 60 days overdue.","quote":"Which customers are more than 60 days overdue?"},"status":"satisfied",` +
		`"values":[{"name":"customer","type":"id","value":"CUS-002","evidence_ref":"led_00000000000000000000000000000001"},{"name":"customer","type":"id","value":"CUS-004","evidence_ref":"led_00000000000000000000000000000001"}],"evidence":["led_00000000000000000000000000000001"]},` +
		`{"item":{"id":"R2","type":"figure","text":"State the total overdue.","quote":"more than 60 days overdue"},"status":"satisfied",` +
		`"values":[{"name":"over_60_total","type":"money","value":66483.75,"evidence_ref":"led_00000000000000000000000000000001"}],"evidence":["led_00000000000000000000000000000001"]},` +
		`{"item":{"id":"R3","type":"answer","text":"Name who approved the write-off.","quote":"who approved it"},"status":"not_found","reason":"no record names an approver"}],"stages":1,"settled":true}`
	return domain.ExecuteResult{Version: 1, RunID: "run-1", Requirements: json.RawMessage(cov), FailedStep: -1, Ledger: true,
		Actions: []domain.ExecuteAction{
			{Step: 2, Tool: "mail_draft", Status: "done", IDs: []string{"MSG-000173"}},
			{Step: 2, Tool: "mail_draft", Status: "done", IDs: []string{"MSG-000174"}},
			{Step: 2, Tool: "mail_draft", Status: "done", IDs: []string{"MSG-000175"}},
		},
		Steps: []domain.ExecuteStepRef{{Step: 0, Status: "completed", Ref: "led_00000000000000000000000000000002", Preview: "aging report read"}},
	}
}

// ── D2: transcript budget and the rolling summary ──────────────────────────

func TestBudgetWindowKeepsTheNewestMessagesThatFit(t *testing.T) {
	msgs := []domain.Message{
		{Seq: 1, Content: strings.Repeat("a", 400)}, // 100 tokens + 4
		{Seq: 2, Content: strings.Repeat("b", 400)},
		{Seq: 3, Content: strings.Repeat("c", 40)}, // 10 + 4
	}
	over, win := budgetWindow(msgs, 100)
	if len(over) != 2 || len(win) != 1 || win[0].Seq != 3 {
		t.Fatalf("overflow %d window %d", len(over), len(win))
	}
	// The newest message is kept even when it alone exceeds the budget.
	over, win = budgetWindow(msgs[:1], 10)
	if len(over) != 0 || len(win) != 1 {
		t.Fatalf("newest dropped: overflow %d window %d", len(over), len(win))
	}
}

func TestTranscriptOverBudgetTriggersOneSummaryAndIsNotResummarised(t *testing.T) {
	r := newGroundingRig(t, Grounding{TranscriptBudgetTokens: 60})
	var calls [][]int64
	r.svc.SetSummarizer(func(_ context.Context, prev string, msgs []domain.Message) (string, error) {
		var seqs []int64
		for _, m := range msgs {
			seqs = append(seqs, m.Seq)
		}
		calls = append(calls, seqs)
		return fmt.Sprintf("summary through %d (prev: %q)", msgs[len(msgs)-1].Seq, prev), nil
	})
	long := strings.Repeat("word ", 40) // ~50 tokens
	r.pool.replies = []string{long, long, long}
	r.turn(t, "first "+long)
	if len(calls) != 0 {
		t.Fatalf("no history yet, no summary: %v", calls)
	}
	_, h := r.turn(t, "second")
	// History is user1(~52) + agent1(~54): over 60, so the older one is folded.
	if len(calls) != 1 || calls[0][0] != 1 {
		t.Fatalf("summary calls = %v, want one over message 1", calls)
	}
	if got := h.Payload.Metadata[turnKeySummary]; !strings.Contains(got, "summary through 1") {
		t.Errorf("summary key = %q", got)
	}
	if tr := h.Payload.Metadata["transcript"]; strings.Contains(tr, "first word") {
		t.Errorf("the summarised message is still in the transcript: %q", tr)
	}
	// Third turn: message 1 is never summarised again; the next overflow folds
	// into the previous summary.
	_, h = r.turn(t, "third")
	if len(calls) != 2 || calls[1][0] <= 1 {
		t.Fatalf("summary calls = %v", calls)
	}
	if got := h.Payload.Metadata[turnKeySummary]; !strings.Contains(got, `prev: "summary through 1`) {
		t.Errorf("the previous summary was not folded in: %q", got)
	}
	if n := len(r.ledger.tool(toolSummary)); n != 2 {
		t.Errorf("summaries recorded = %d, want 2", n)
	}
}

func TestNoBudgetKeepsTheLegacyWindowAndNoSummary(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.svc.SetSummarizer(func(context.Context, string, []domain.Message) (string, error) {
		t.Fatal("no summary without a budget")
		return "", nil
	})
	r.turn(t, "one")
	_, h := r.turn(t, "two")
	if _, ok := h.Payload.Metadata[turnKeySummary]; ok {
		t.Error("summary key without a budget")
	}
}

// ── D2: the turn index ─────────────────────────────────────────────────────

func TestTurnIndexNumbersDraftsSoTheSecondDraftResolves(t *testing.T) {
	e := buildTurnIndex("m1", 1, "Draft reminders", []domain.ExecuteResult{o3xResult()})
	out := renderTurnIndex([]TurnIndexEntry{e})
	for _, want := range []string{
		"action mail_draft #1 done ids=MSG-000173",
		"action mail_draft #2 done ids=MSG-000174",
		"action mail_draft #3 done ids=MSG-000175",
		"R2 figure [satisfied] State the total overdue.: over_60_total=66483.75 led_00000000000000000000000000000001",
		"R3 answer [not_found] Name who approved the write-off. (no record names an approver)",
		"step 0 led_00000000000000000000000000000002: aging report read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("index lacks %q:\n%s", want, out)
		}
	}
}

func TestAFollowUpTurnIsShownTheIndexOfTheDelegatedTurn(t *testing.T) {
	r := newGroundingRig(t, Grounding{TurnIndex: true})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.results[r.lastUserID()] = []domain.ExecuteResult{o3xResult()}
		}
	}
	r.pool.replies = []string{"Three drafts saved.", "Sent the second one."}
	first, _ := r.turn(t, "Draft reminders to the overdue customers.")
	_ = first
	if n := len(r.ledger.tool(toolTurnIndex)); n != 1 {
		t.Fatalf("index entries = %d, want 1", n)
	}
	_, h := r.turn(t, "Send the second draft.")
	idx := h.Payload.Metadata[turnKeyTurnIndex]
	if !strings.Contains(idx, "action mail_draft #2 done ids=MSG-000174") || !strings.Contains(idx, "T1 (latest)") {
		t.Errorf("the follow-up was not shown the index:\n%s", idx)
	}
	// A turn that delegated nothing leaves no index entry.
	if n := len(r.ledger.tool(toolTurnIndex)); n != 1 {
		t.Errorf("index entries = %d, want 1", n)
	}
}

// ── D5a: ask-back round trip ───────────────────────────────────────────────

func TestAskBackIsSavedWithTheTurnAndTheNextMessageResumesIt(t *testing.T) {
	r := newGroundingRig(t, Grounding{AskBack: true})
	ir := &domain.InputRequired{Version: 1, RunID: "run-9", Stage: 1, Round: 1, LedgerRef: "led_0000000000000000000000000000ab12",
		Questions: []domain.InputQuestion{{ID: "Q1", Question: "Which month should the report cover?", Kind: "missing"}}}
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.results[r.lastUserID()] = []domain.ExecuteResult{{Version: 1, RunID: "run-9", InputRequired: ir, FailedStep: -1}}
		}
	}
	r.pool.replies = []string{"Hangi ay için hazırlayayım?", "Ekim raporu hazır."}
	r.turn(t, "Raporu hazırla.")
	// The turn ended with the question; the pending state is saved with it.
	entries := r.ledger.tool(toolTurnIndex)
	if len(entries) != 1 || !strings.Contains(string(entries[0].Body), `"pending"`) {
		t.Fatalf("pending state not saved: %v", entries)
	}
	_, h := r.turn(t, "Ekim.")
	pending := h.Payload.Metadata[turnKeyPendingInput]
	if !strings.Contains(pending, "Q1 Which month should the report cover?") || !strings.Contains(pending, "Raporu hazırla.") {
		t.Errorf("pending_input = %q", pending)
	}
	b := r.binder.bound["lease-2"]
	if b.AskBackRound != 1 || b.AskBackPendingRef != "led_0000000000000000000000000000ab12" {
		t.Errorf("the resume is not on the turn lease: %+v", b)
	}
	// Answered: the turn after carries no pending state.
	_, h = r.turn(t, "Teşekkürler.")
	if _, ok := h.Payload.Metadata[turnKeyPendingInput]; ok {
		t.Error("an answered ask-back is resumed again")
	}
	if b := r.binder.bound["lease-3"]; b.AskBackRound != 0 {
		t.Errorf("round on an unrelated turn: %+v", b)
	}
}

func TestAskBackOffSavesNoPendingState(t *testing.T) {
	r := newGroundingRig(t, Grounding{TurnIndex: true})
	ir := &domain.InputRequired{Version: 1, Round: 1, Questions: []domain.InputQuestion{{ID: "Q1", Question: "Which month?"}}}
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.results[r.lastUserID()] = []domain.ExecuteResult{{Version: 1, InputRequired: ir, FailedStep: -1}}
		}
	}
	r.turn(t, "Prepare the report.")
	_, h := r.turn(t, "October.")
	if _, ok := h.Payload.Metadata[turnKeyPendingInput]; ok {
		t.Error("ask_back off: no resume")
	}
}

// ── D8: the reply check ────────────────────────────────────────────────────

func (r *groundingRig) seedTurnEvidence(body string) {
	turn := r.lastUserID()
	r.ledger.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, ToolName: "finance_aging_report",
		Provenance: domain.LedgerProvenance{ConversationID: "c1", TurnID: turn, RunID: "run-1", StepIndex: 0, Owner: "u1"},
		Body:       []byte(body)})
	r.results[turn] = []domain.ExecuteResult{o3xResult()}
}

const agingBody = `{"over_60":[{"customer_id":"CUS-002","overdue":20092.11},{"customer_id":"CUS-004","overdue":25602.43}],"over_60_total":66483.75}`

func TestObservedReplyCheckCountsAndRecordsButChangesNothingExceptMarkers(t *testing.T) {
	r := newGroundingRig(t, Grounding{ReplyCheck: ReplyCheckObserve})
	r.pool.onDispatch = func(h *domain.Handoff, call int) { r.seedTurnEvidence(agingBody) }
	r.pool.replies = []string{"CUS-002 owes £20,092.11 [led_00000000000000000000000000000001] and the total is £70,000 [R2]. No approver was found [R3]."}
	msg, h := r.turn(t, "Which customers are more than 60 days overdue?")
	if h.Payload.Metadata[turnKeyReplyCheck] != "observe" {
		t.Errorf("the agent is not told the mode: %v", h.Payload.Metadata)
	}
	if msg.Content != "CUS-002 owes £20,092.11 and the total is £70,000. No approver was found." {
		t.Errorf("reply = %q", msg.Content)
	}
	if len(r.pool.handoffs) != 1 {
		t.Errorf("observe must not retry: %d dispatches", len(r.pool.handoffs))
	}
	checked, violations, retries := r.svc.ReplyCheckStats()
	if checked != 1 || violations != 1 || retries != 0 {
		t.Errorf("stats = %d/%d/%d, want 1 checked, 1 violation (£70,000), 0 retries", checked, violations, retries)
	}
	rec := r.ledger.tool(toolReplyCheck)
	if len(rec) != 1 || !strings.Contains(string(rec[0].Body), "£70,000") {
		t.Errorf("check report not recorded: %v", rec)
	}
}

func TestEnforcedReplyCheckRetriesOnceThenStatesTheGapsInTheTurnLanguage(t *testing.T) {
	r := newGroundingRig(t, Grounding{ReplyCheck: ReplyCheckEnforce})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.seedTurnEvidence(agingBody)
		}
	}
	r.pool.replies = []string{
		"CUS-002 ve CUS-004 gecikmede; toplam £70.000.",
		"CUS-002 ve CUS-004 gecikmede; toplam £71.000 [R2].",
	}
	msg, _ := r.turn(t, "60 günden fazla gecikmiş müşteriler hangileri?")
	if len(r.pool.handoffs) != 2 {
		t.Fatalf("dispatches = %d, want the turn + one retry", len(r.pool.handoffs))
	}
	retry := r.pool.handoffs[1].Payload.Metadata
	var rev replyRevision
	if err := json.Unmarshal([]byte(retry[turnKeyReplyRevision]), &rev); err != nil {
		t.Fatalf("reply_revision: %v", err)
	}
	if len(rev.Uncited) != 1 || rev.Uncited[0] != "£70.000" {
		t.Errorf("the retry must name the uncited value: %+v", rev)
	}
	if !strings.Contains(retry[turnKeyReplyEvidence], "over_60_total = 66483.75 [led_00000000000000000000000000000001]") {
		t.Errorf("the retry carries no evidence:\n%s", retry[turnKeyReplyEvidence])
	}
	// The retry still invents a figure: it is stated plainly, in Turkish.
	if !strings.Contains(msg.Content, "Not: Şu değerleri bu istek için okunan kayıtlarla doğrulayamadım: £71.000.") {
		t.Errorf("reply = %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "“who approved it”") {
		t.Errorf("the unstated gap R3 is not named: %q", msg.Content)
	}
	if _, _, retries := r.svc.ReplyCheckStats(); retries != 1 {
		t.Errorf("retries = %d", retries)
	}
}

func TestEnforcedReplyCheckAcceptsAGoodRetry(t *testing.T) {
	r := newGroundingRig(t, Grounding{ReplyCheck: ReplyCheckEnforce})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.seedTurnEvidence(agingBody)
		}
	}
	r.pool.replies = []string{
		"CUS-002 and CUS-004 are overdue; the total is £70,000.",
		"CUS-002 and CUS-004 are overdue [R1]; the total is £66,483.75 [R2]. No approver was found [R3].",
	}
	msg, _ := r.turn(t, "Which customers are more than 60 days overdue?")
	if msg.Content != "CUS-002 and CUS-004 are overdue; the total is £66,483.75. No approver was found." {
		t.Errorf("reply = %q", msg.Content)
	}
}

func TestReplyCheckOffOnlyStripsMarkers(t *testing.T) {
	r := newGroundingRig(t, Grounding{GroundedReply: true})
	r.pool.replies = []string{"The total is £70,000 [led_00000000000000000000000000000001]."}
	msg, h := r.turn(t, "total?")
	if msg.Content != "The total is £70,000." || h.Payload.Metadata[turnKeyGroundedReply] != "true" {
		t.Errorf("reply = %q, md = %v", msg.Content, h.Payload.Metadata)
	}
	if checked, _, _ := r.svc.ReplyCheckStats(); checked != 0 {
		t.Error("checked with the check off")
	}
}

func TestReplyCheckWithoutTheLedgerDoesNotRun(t *testing.T) {
	r := newGroundingRig(t, Grounding{ReplyCheck: ReplyCheckEnforce})
	r.svc.SetLedger(nil, nil)
	r.pool.replies = []string{"The total is £70,000."}
	msg, _ := r.turn(t, "total?")
	if msg.Content != "The total is £70,000." || len(r.pool.handoffs) != 1 {
		t.Errorf("reply = %q after %d dispatches", msg.Content, len(r.pool.handoffs))
	}
}
