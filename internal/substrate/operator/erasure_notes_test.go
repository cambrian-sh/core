package operator

import (
	"context"
	"slices"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// notesRawFake resolves memory-note entries (only those in the live document, as
// postgres.PgErasureStore does) and memory_notes_owner selectors over a notes store.
type notesRawFake struct {
	rawEraserFake
	store *memorynotes.MemStore
}

func (f *notesRawFake) Resolve(ctx context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, id := range sel.MemoryNoteIDs {
		scope, owner, _, ok := domain.ParseMemoryNoteEntryID(id)
		if !ok {
			continue
		}
		key := memorynotes.Key{Scope: memorynotes.Scope(scope), OwnerID: owner}
		d, _ := f.store.Get(ctx, key)
		for _, e := range memorynotes.Entries(d.Content) {
			if domain.MemoryNoteEntryID(scope, owner, e.Text) == id {
				out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetMemoryNote, ID: id, Via: id,
					DocumentID: "memory-notes:" + key.String(), Tags: memorynotes.ResourceTags(key)})
				break
			}
		}
	}
	for _, o := range sel.MemoryNoteOwners {
		key := memorynotes.Key{Scope: memorynotes.ScopePersonal, OwnerID: o}
		if d, _ := f.store.Get(ctx, key); d.Content != "" {
			out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetMemoryNotesOwner, ID: o, Via: o,
				DocumentID: "memory-notes:" + key.String(), Tags: memorynotes.ResourceTags(key)})
		}
	}
	return out, nil
}

func (f *notesRawFake) Erase(ctx context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	rec, err := f.rawEraserFake.Erase(ctx, req)
	rec.RedactedMessages = []string{"msg-1"}
	f.receipts[rec.RequestID] = rec
	return rec, err
}

// ENG-2147 (contract 0110): the operator names memory-note lines and whole personal
// notes on ErasureSelectorOp; the preview shows the line (note_text) read through the
// notes service, and the selector reaches the chokepoint unchanged; the receipt lists
// the redacted replies (ENG-2154).
func TestForgetMemory_OperatorNamesNoteLinesAndWholeNotes(t *testing.T) {
	ctx := operatorErasureCtx(RoleOperator, "operator")
	store := memorynotes.NewMemStore()
	notes, err := memorynotes.New(store, memorynotes.ScannerFunc(func(string) (bool, string) { return false, "" }),
		domain.AllowAllAuthorizer{}, memorynotes.Config{PersonalBudgetChars: 1000, OrgBudgetChars: 1000})
	if err != nil {
		t.Fatal(err)
	}
	key := memorynotes.Key{Scope: memorynotes.ScopePersonal, OwnerID: "alice"}
	if _, err := store.Put(ctx, key, "## Personal\n- Locker code: 4711\n- Gym on Tuesdays\n", 0, memorynotes.WriteMeta{Actor: "t", Op: "set"}); err != nil {
		t.Fatal(err)
	}
	raw := &notesRawFake{store: store}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{}, nil)
	s := NewService(NewSpool(SpoolConfig{}))
	s.SetCommandSources(NewInMemoryAuditStore(), nil)
	s.SetForgetService(forget.New(e, forget.Config{PolicyFingerprint: "t"}))
	s.SetMemoryNotesAdmin(notes)

	line := domain.MemoryNoteEntryID("personal", "alice", "- Locker code: 4711")
	p, err := s.PreviewForgetMemory(ctx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{MemoryNoteIds: []string{line}}})
	if err != nil {
		t.Fatalf("preview by note id: %v", err)
	}
	if len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetKind() != domain.ErasureTargetMemoryNote ||
		p.GetCandidates()[0].GetId() != line || p.GetCandidates()[0].GetNoteText() != "- Locker code: 4711" ||
		p.GetCountsByKind()[domain.ErasureTargetMemoryNote] != 1 {
		t.Fatalf("preview = %+v", p)
	}
	resp, err := s.ForgetMemory(ctx, &pb.ForgetMemoryOpRequest{CommandId: "n1", Reason: "KVKK #1", PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.erased[0].Selector; !slices.Equal(got.MemoryNoteIDs, []string{line}) || len(got.MemoryNoteOwners) != 0 {
		t.Fatalf("erased selector = %+v", got)
	}
	if !slices.Equal(resp.GetReceipt().GetRedactedMessages(), []string{"msg-1"}) {
		t.Fatalf("receipt redacted_messages = %v", resp.GetReceipt().GetRedactedMessages())
	}

	whole, err := s.PreviewForgetMemory(ctx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{MemoryNoteOwners: []string{"alice"}}})
	if err != nil {
		t.Fatalf("subject-wide preview: %v", err)
	}
	if len(whole.GetCandidates()) != 1 || whole.GetCandidates()[0].GetKind() != domain.ErasureTargetMemoryNotesOwner ||
		whole.GetCandidates()[0].GetId() != "alice" || whole.GetCandidates()[0].GetNoteText() == "" {
		t.Fatalf("subject-wide preview = %+v", whole)
	}
	if _, err := s.ForgetMemory(ctx, &pb.ForgetMemoryOpRequest{CommandId: "n2", Reason: "KVKK #2",
		Selector: &pb.ErasureSelectorOp{MemoryNoteOwners: []string{"alice"}}}); err != nil {
		t.Fatal(err)
	}
	if got := raw.erased[1].Selector; !slices.Equal(got.MemoryNoteOwners, []string{"alice"}) ||
		!slices.Equal(got.Kinds(), []string{domain.ErasureTargetMemoryNotesOwner}) {
		t.Fatalf("subject-wide selector = %+v", got)
	}

	// Without the notes surface the preview still lists the candidate, with no text.
	s.SetMemoryNotesAdmin(nil)
	bare, err := s.PreviewForgetMemory(ctx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{MemoryNoteIds: []string{line}}})
	if err != nil || len(bare.GetCandidates()) != 1 || bare.GetCandidates()[0].GetNoteText() != "" {
		t.Fatalf("preview without the notes surface = %+v, %v", bare, err)
	}
}

// ENG-2154 (contract 0110): the operator receipt lists the redacted replies.
func TestReceiptToOp_CarriesRedactedMessages(t *testing.T) {
	got := receiptToOp(domain.ErasureReceipt{RequestID: "erase:x", RedactedMessages: []string{"m1", "m2"}}, time.Now())
	if !slices.Equal(got.GetRedactedMessages(), []string{"m1", "m2"}) {
		t.Fatalf("redacted_messages = %v", got.GetRedactedMessages())
	}
}
