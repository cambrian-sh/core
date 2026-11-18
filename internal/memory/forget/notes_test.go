package forget_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// notesFake is the memory-notes service as the forget service sees it.
type notesFake struct {
	byOwner map[string][]memorynotes.EntryMatch
	owners  []string // MatchForErase calls
	dropped []string
}

func (n *notesFake) MatchForErase(_ context.Context, owner, query string) ([]memorynotes.EntryMatch, error) {
	n.owners = append(n.owners, owner)
	var out []memorynotes.EntryMatch
	for _, m := range n.byOwner[owner] {
		if strings.Contains(strings.ToLower(m.Text), strings.ToLower(query)) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (n *notesFake) DropSnapshots(owner string) { n.dropped = append(n.dropped, owner) }

// noteRaw resolves note ids and chunk ids (chunkEraser) and erases with real counts, so
// the tool's receipt shape can be checked.
type noteRaw struct {
	chunkEraser
	notes    map[string][]string // live note entry id → tags
	gone     map[string]bool
	receipts map[string]domain.ErasureReceipt
}

// Receipt answers stored receipts, as the Postgres store does: the chokepoint's D6
// replay check reads it before re-resolving.
func (f *noteRaw) Receipt(_ context.Context, id string) (domain.ErasureReceipt, bool, error) {
	r, ok := f.receipts[id]
	return r, ok, nil
}

func (f *noteRaw) Resolve(ctx context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	out, _ := f.chunkEraser.Resolve(ctx, sel)
	for _, id := range sel.MemoryNoteIDs {
		if tags, ok := f.notes[id]; ok && !f.gone[id] {
			out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetMemoryNote, ID: id, Via: id, Tags: tags})
		}
	}
	return out, nil
}

func (f *noteRaw) Erase(ctx context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	rec, err := f.chunkEraser.Erase(ctx, req)
	if err != nil || rec.Replayed {
		return rec, err
	}
	rec.Targets = req.Selector.Count()
	for _, id := range req.Selector.MemoryNoteIDs {
		f.gone[id] = true
	}
	f.receipts[rec.RequestID] = rec
	return rec, nil
}

func lockerFixture() (*noteRaw, *notesFake, string) {
	id := domain.MemoryNoteEntryID("personal", "emp:EMP-001", "Locker code: LOCKER-b681ce")
	raw := &noteRaw{chunkEraser: *queryFixture(), gone: map[string]bool{}, receipts: map[string]domain.ErasureReceipt{},
		notes: map[string][]string{id: {"memory-notes", "memory-notes:personal", domain.OwnerTag("emp:EMP-001")}}}
	notes := &notesFake{byOwner: map[string][]memorynotes.EntryMatch{
		"emp:EMP-001": {{ID: id, Text: "Locker code: LOCKER-b681ce"}},
	}}
	return raw, notes, id
}

func newNotesService(raw *noteRaw, notes forget.Notes, srch forget.Searcher) *forget.Service {
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	return forget.New(e, forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "fp", Searcher: srch, Notes: notes})
}

// chatFor is the chat agent's tool context in the person's conversation: the read
// predicate holds the person's owner tag, and the run principal is the person.
func chatFor(person string) context.Context {
	return domain.WithRunPrincipal(domain.WithScope(context.Background(),
		&domain.TagPredicate{Bypass: true, Owners: []string{person}}), person)
}

// ENG-2106: the query form reaches the run principal's memory notes — listed first, with
// the line as the snippet — and only the run principal's; without a run principal no
// notes are searched.
func TestPreviewQuery_ReachesTheRunPrincipalsMemoryNotes(t *testing.T) {
	raw, notes, id := lockerFixture()
	srch := &searcher{hits: []domain.SearchResult{hit("a1", "locker code doc")}}
	svc := newNotesService(raw, notes, srch)
	ctx := domain.WithPrincipal(chatFor("emp:EMP-001"), domain.AgentPrincipal("alice"))

	p, err := svc.PreviewQuery(ctx, "locker code", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 2 || p.Candidates[0].ID != id || p.Candidates[0].Kind != domain.ErasureTargetMemoryNote ||
		p.Candidates[1].ID != "docA" {
		t.Fatalf("candidates = %+v", p.Candidates)
	}
	if got := p.Snippets[forget.SnippetKey(p.Candidates[0])]; got != "Locker code: LOCKER-b681ce" {
		t.Fatalf("note snippet = %q", got)
	}
	if !slices.Equal(p.Selector.MemoryNoteIDs, []string{id}) || !slices.Equal(notes.owners, []string{"emp:EMP-001"}) {
		t.Fatalf("selector %+v, notes searched for %v", p.Selector, notes.owners)
	}

	// No run principal: notes are not searched at all.
	notes.owners = nil
	noRun := domain.WithPrincipal(domain.WithScope(context.Background(), &domain.TagPredicate{}), domain.AgentPrincipal("alice"))
	if _, err := svc.PreviewQuery(noRun, "locker code", "alice"); err != nil {
		t.Fatal(err)
	}
	if len(notes.owners) != 0 {
		t.Fatalf("notes searched without a run principal: %v", notes.owners)
	}
	// Notes alone answer a query (no memory searcher composed).
	onlyNotes := newNotesService(raw, notes, nil)
	if p, err := onlyNotes.PreviewQuery(ctx, "locker code", "alice"); err != nil || len(p.Candidates) != 1 {
		t.Fatalf("notes-only preview = %+v, %v", p, err)
	}
}

// The tool's answer is computed from the receipt (owner decision 2026-11-18): erased is
// true only when this call erased something, nothing-found is a result saying so, and a
// replay is not a fresh erasure. The erase drops the owner's snapshot cache.
func TestForgetTool_ReportsOnlyFromTheReceipt(t *testing.T) {
	raw, notes, id := lockerFixture()
	tool := forget.NewTool(newNotesService(raw, notes, nil), true)
	ctx := chatFor("emp:EMP-001")
	call := func(args string) map[string]any {
		t.Helper()
		out, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "chat_agent", ArgsJSON: []byte(args)})
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if !strings.Contains(tool.Spec().Description, `"erased": true`) || !strings.Contains(tool.Spec().Description, "memory notes") {
		t.Fatal("the spec must tell the model to report erasure only from erased:true, and cover memory notes")
	}

	none := call(`{"query":"salary"}`)
	if none["erased"] != false || none["found"] != false || none["outcome"] != "nothing_found" || none["tell_the_person"] == "" {
		t.Fatalf("nothing found = %v", none)
	}
	prev := call(`{"query":"locker code"}`)
	if prev["erased"] != false || prev["outcome"] != "preview" {
		t.Fatalf("preview = %v", prev)
	}
	would := prev["would_erase"].([]any)
	if len(would) != 1 || would[0].(map[string]any)["id"] != id || would[0].(map[string]any)["kind"] != "memory_note" {
		t.Fatalf("would_erase = %v", would)
	}
	token := prev["preview_token"].(string)
	done := call(`{"preview_token":"` + token + `"}`)
	if done["erased"] != true || done["erased_count"] != float64(1) || done["outcome"] != "erased" ||
		!strings.Contains(done["tell_the_person"].(string), done["request_id"].(string)) {
		t.Fatalf("erase = %v", done)
	}
	if !slices.Equal(notes.dropped, []string{"emp:EMP-001"}) {
		t.Fatalf("snapshot cache not dropped for the owner: %v", notes.dropped)
	}
	again := call(`{"preview_token":"` + token + `"}`)
	if again["erased"] != false || again["outcome"] != "already_erased" || again["erased_count"] != float64(0) {
		t.Fatalf("replay = %v", again)
	}
	if len(notes.dropped) != 1 {
		t.Fatal("a replay must not count as an erasure")
	}
	// The repro's second turn: "Yes, I confirm. Erase it." → another query preview,
	// which now finds nothing and must say so.
	if after := call(`{"query":"locker code"}`); after["erased"] != false || after["outcome"] != "nothing_found" {
		t.Fatalf("after the erase = %v", after)
	}
	// An unknown token is still an error the model can correct.
	if _, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "chat_agent", ArgsJSON: []byte(`{"preview_token":"pv:nope"}`)}); err == nil ||
		!errors.Is(err, domain.ErrPreviewUnknown) && !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown token = %v", err)
	}
}

// ENG-2147: an executed subject-wide erase (MemoryNoteOwners) drops the owner's cached
// run snapshots too, so no run keeps serving the erased notes from its frozen copy.
func TestForget_MemoryNoteOwnersDropsTheOwnersSnapshots(t *testing.T) {
	raw := &ownerRaw{}
	notes := &notesFake{}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{}, nil)
	svc := forget.New(e, forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "fp", Notes: notes})
	op := domain.WithScope(domain.WithPrincipal(domain.WithPrincipalRoles(context.Background(), []string{"operator"}),
		domain.UserPrincipal("ops")), domain.ScopeMaintenance)
	p, err := svc.Preview(op, domain.ErasureSelector{MemoryNoteOwners: []string{"alice"}})
	if err != nil || len(p.Candidates) != 1 || p.Candidates[0].Kind != domain.ErasureTargetMemoryNotesOwner {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	if _, err := svc.Forget(op, forget.Request{PreviewToken: p.Token, Reason: domain.ErasureReasonCompliance}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(notes.dropped, []string{"alice"}) {
		t.Fatalf("dropped snapshots = %v, want [alice]", notes.dropped)
	}
}

// ownerRaw resolves every named owner to a memory_notes_owner candidate.
type ownerRaw struct{ erased []domain.ErasureRequest }

func (f *ownerRaw) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, o := range sel.MemoryNoteOwners {
		out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetMemoryNotesOwner, ID: o, Via: o,
			Tags: []string{"memory-notes", domain.OwnerTag(o)}})
	}
	return out, nil
}

func (f *ownerRaw) Erase(_ context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	f.erased = append(f.erased, req)
	return domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, PrincipalKind: req.PrincipalKind,
		Status: domain.ErasureStatusComplete, SelectorKinds: req.Selector.Kinds()}, nil
}

func (f *ownerRaw) Receipt(context.Context, string) (domain.ErasureReceipt, bool, error) {
	return domain.ErasureReceipt{}, false, nil
}

func (f *ownerRaw) ListReceipts(context.Context, domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	return nil, nil
}
