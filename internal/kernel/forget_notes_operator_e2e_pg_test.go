package kernel

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memorynotes"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
)

// ENG-2147 end to end (owner decision 2026-11-19: BOTH), through the operator RPCs and
// the agent-plane RPCs over the production composition — a real Postgres adapter, the
// raw PgErasureStore as MemoryBackend.Eraser, NewMemoryStack with erasure on, the
// Postgres memory-notes store and service, the Postgres operator audit store, the forget
// service over MemoryStack.Eraser(), the operator.Service the console calls and the
// agent-plane Server a chat agent calls with its lease. Asserted in Postgres:
//
//   - (a) an operator previews specific note lines by their content-derived id
//     (PreviewForgetMemory memory_note_ids) and sees the line (note_text); ForgetMemory
//     with the token erases exactly that line from the live notes and every history
//     row, tombstoned, audited, receipted; the rest stays;
//   - the agent plane names a line of its run principal's own notes the same way
//     (PreviewForget / Forget memory_note_ids); another person's agent cannot;
//   - DeleteMemoryNotes stays a soft delete: history and the operator journal keep the
//     text afterwards (it is not the compliance path);
//   - (b) the subject-wide erase (memory_note_owners) after that soft delete and a new
//     write: the live notes, every history revision's text and reason, and the soft
//     delete's journalled text are gone; one memory_notes_owner tombstone plus one
//     memory_note tombstone per distinct line ever held (history-only lines included),
//     content-free; the receipt names the kind and the stores; another person's notes
//     survive; the next run's notes are empty;
//   - after a "restart" (a fresh adapter, stack, services over the same database) it is
//     all still gone, the receipt still reads back, and a new subject-wide preview finds
//     nothing to erase.
//
// Requires PG_TEST_DSN pointing at a scratch database (migrated to HEAD here).
func TestPg_OperatorForget_NamesNoteLinesAndErasesAPersonsWholeNotes(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	ctx := context.Background()

	type stack struct {
		vec   *postgres.PgVectorAdapter
		notes *memorynotes.Service
		svc   *forget.Service
		op    *operator.Service
		audit *postgres.PgAuditStore
	}
	build := func() stack {
		t.Helper()
		vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
		if err != nil {
			t.Fatalf("NewPgVectorAdapter: %v", err)
		}
		t.Cleanup(vec.Close)
		pool := vec.Pool()
		backend := NewMemoryBackend(vec)
		backend.Eraser = postgres.NewPgErasureStore(postgres.NewPgDocumentLifecycle(postgres.NewPgKnowledgeStore(pool, nil)))
		mem := NewMemoryStack(backend, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
		if mem.Eraser() == nil {
			t.Fatal("erasure enabled but MemoryStack.Eraser() is nil")
		}
		ns, err := memorynotes.NewPgStore(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		notes, err := memorynotes.New(ns, memorynotes.ScannerFunc(func(string) (bool, string) { return false, "" }),
			domain.AllowAllAuthorizer{}, memorynotes.Config{PersonalBudgetChars: 4000, OrgBudgetChars: 4000})
		if err != nil {
			t.Fatal(err)
		}
		audit, err := postgres.NewPgAuditStore(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "e2e", Notes: notes})
		op := operatortest.New(t, nil, func(d *operator.Deps, o *operator.Options) {
			d.Audit = audit
			o.Forget = svc
			o.MemoryNotes = notes
		})
		return stack{vec: vec, notes: notes, svc: svc, op: op, audit: audit}
	}
	st := build()
	pool := st.vec.Pool()

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	alice, bob := "eng2147-alice-"+stamp, "eng2147-bob-"+stamp
	agentID := "eng2147-chat-" + stamp
	locker, passport, gym := "- Locker code: L"+stamp, "- Passport: P"+stamp, "- Gym on Tuesdays "+stamp
	oldAddr, newFact, bobLine := "- Old address: A"+stamp, "- New fact: N"+stamp, "- Bob secret: B"+stamp
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM memory_notes WHERE owner_id IN ($1, $2)`,
			`DELETE FROM memory_notes_history WHERE owner_id IN ($1, $2)`,
		} {
			_, _ = pool.Exec(bg, q, alice, bob)
		}
		_, _ = pool.Exec(bg, `DELETE FROM operator_audit WHERE command_id LIKE $1`, "eng2147-"+stamp+"%")
	})
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	// held counts the copies of text in every notes store: live, history (text or
	// reason) and the operator journal.
	held := func(text string) [3]int {
		t.Helper()
		like := "%" + text + "%"
		return [3]int{
			count(`SELECT count(*) FROM memory_notes WHERE content LIKE $1`, like),
			count(`SELECT count(*) FROM memory_notes_history WHERE content LIKE $1 OR reason LIKE $1`, like),
			count(`SELECT count(*) FROM operator_audit WHERE before_json LIKE $1 OR after_json LIKE $1 OR reason LIKE $1`, like),
		}
	}
	opCtx := domain.WithPrincipal(domain.WithPrincipalRoles(ctx, []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng2147-ops"})
	n := 0
	cmd := func() string { n++; return "eng2147-" + stamp + "-" + string(rune('a'+n)) }
	set := func(owner, content, reason string) {
		t.Helper()
		r, err := st.op.SetMemoryNotes(opCtx, &pb.SetMemoryNotesOpRequest{CommandId: cmd(), Reason: reason,
			Scope: "personal", OwnerId: owner, Content: content})
		if err != nil || !r.GetAccepted() {
			t.Fatalf("SetMemoryNotes %s: %+v %v", owner, r, err)
		}
	}

	// Seed: three revisions of alice's notes (the old address lives only in history
	// after the third, and the second's reason quotes it), and bob's notes.
	set(alice, "## Personal\n"+locker+"\n"+passport+"\n"+gym+"\n", "seed")
	set(alice, "## Personal\n"+locker+"\n"+passport+"\n"+gym+"\n"+oldAddr+"\n", "moved from "+oldAddr)
	set(alice, "## Personal\n"+locker+"\n"+passport+"\n"+gym+"\n", "address no longer current")
	set(bob, "## Personal\n"+bobLine+"\n", "seed")
	if h := held(oldAddr); h[0] != 0 || h[1] != 1 || h[2] != 1 {
		t.Fatalf("precondition: the old address is history-only, got %v", h)
	}

	// ── (a) the operator names a line ──
	lockerID := domain.MemoryNoteEntryID("personal", alice, locker)
	p, err := st.op.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{MemoryNoteIds: []string{lockerID}}})
	if err != nil {
		t.Fatalf("PreviewForgetMemory by note id: %v", err)
	}
	if len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetKind() != domain.ErasureTargetMemoryNote ||
		p.GetCandidates()[0].GetId() != lockerID || p.GetCandidates()[0].GetNoteText() != locker {
		t.Fatalf("preview = %+v", p.GetCandidates())
	}
	if held(locker)[0] != 1 {
		t.Fatal("a preview must erase nothing")
	}
	fr, err := st.op.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: cmd(), Reason: "KVKK request: locker line",
		PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatalf("ForgetMemory by note id: %v", err)
	}
	lineRec := fr.GetReceipt()
	if lineRec.GetPrincipalKind() != "operator" || !slices.Equal(lineRec.GetSelectorKinds(), []string{domain.ErasureTargetMemoryNote}) ||
		lineRec.GetCounts()["memory_notes"] != 1 || lineRec.GetTargets() != 1 {
		t.Fatalf("line receipt = %+v", lineRec)
	}
	if h := held(locker); h != [3]int{} {
		t.Fatalf("the locker line survives the line erase in (live, history, journal) = %v", h)
	}
	if held(passport)[0] != 1 || held(gym)[0] != 1 {
		t.Fatal("the rest of alice's notes must survive a line erase")
	}
	if count(`SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND target_kind = 'memory_note' AND target_id = $2
			AND content_digest = $3`, lineRec.GetRequestId(), lockerID, domain.MemoryNoteEntryDigest(locker)) != 1 {
		t.Fatal("the line erase must leave its tombstone")
	}

	// ── the agent plane names a line of its run principal's own notes ──
	gw := subnetwork.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, llm.NewSpendMeter(0, nil, nil))
	owners := map[string]string{"conv-a-" + stamp: alice, "conv-b-" + stamp: bob}
	ownerOf := func(_ context.Context, b domain.LeaseBinding) string { return owners[b.ConversationID] }
	srv := &subnetwork.Server{LLMGateway: gw, MemoryNotes: st.notes, MemoryNotesPrincipal: ownerOf,
		RunPrincipal: ownerOf, ConversationOwner: ownerOf, Forgetter: st.svc}
	agentFor := func(conv string) (context.Context, string) {
		t.Helper()
		l, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gw.BindLease(l, domain.LeaseBinding{RunID: domain.RunID("run-" + conv), AgentID: agentID, ConversationID: conv})
		c := metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", agentID, "x-lease-id", string(l)))
		c = domain.WithSurface(c, domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
		return domain.WithPrincipal(c, domain.AgentPrincipal(agentID)), string(l)
	}
	passportID := domain.MemoryNoteEntryID("personal", alice, passport)
	bobAgent, _ := agentFor("conv-b-" + stamp)
	if _, err := srv.PreviewForget(bobAgent, &pb.PreviewForgetRequest{
		Selector: &pb.ErasureSelector{MemoryNoteIds: []string{passportID}}}); err == nil {
		t.Fatal("bob's agent must not reach alice's note line")
	}
	aliceAgent, _ := agentFor("conv-a-" + stamp)
	ap, err := srv.PreviewForget(aliceAgent, &pb.PreviewForgetRequest{Selector: &pb.ErasureSelector{MemoryNoteIds: []string{passportID}}})
	if err != nil || len(ap.GetCandidates()) != 1 || ap.GetCandidates()[0].GetKind() != domain.ErasureTargetMemoryNote ||
		ap.GetCandidates()[0].GetId() != passportID {
		t.Fatalf("agent-plane preview by note id = %+v, %v", ap, err)
	}
	af, err := srv.Forget(aliceAgent, &pb.ForgetRequest{PreviewToken: ap.GetPreviewToken(), ReasonCode: "user_request"})
	if err != nil || af.GetReceipt().GetPrincipalKind() != "agent" || af.GetReceipt().GetCounts()["memory_notes"] != 1 {
		t.Fatalf("agent-plane forget = %+v, %v", af, err)
	}
	if h := held(passport); h != [3]int{} {
		t.Fatalf("the passport line survives the agent's erase in %v", h)
	}

	// ── DeleteMemoryNotes is a soft delete: the text stays recoverable ──
	if _, err := st.op.DeleteMemoryNotes(opCtx, &pb.DeleteMemoryNotesOpRequest{CommandId: cmd(), Reason: "clear",
		Scope: "personal", OwnerId: alice}); err != nil {
		t.Fatalf("DeleteMemoryNotes: %v", err)
	}
	if h := held(gym); h[0] != 0 || h[1] == 0 || h[2] != 1 {
		t.Fatalf("after a soft delete the gym line is in (live, history, journal) = %v; want history and journal to keep it", h)
	}
	set(alice, "## Personal\n"+newFact+"\n", "written after the clear")

	// ── (b) the subject-wide erase ──
	sp, err := st.op.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{MemoryNoteOwners: []string{alice}}})
	if err != nil {
		t.Fatalf("subject-wide preview: %v", err)
	}
	if len(sp.GetCandidates()) != 1 || sp.GetCandidates()[0].GetKind() != domain.ErasureTargetMemoryNotesOwner ||
		sp.GetCandidates()[0].GetId() != alice || !strings.Contains(sp.GetCandidates()[0].GetNoteText(), newFact) ||
		sp.GetCountsByKind()[domain.ErasureTargetMemoryNotesOwner] != 1 {
		t.Fatalf("subject-wide preview = %+v", sp.GetCandidates())
	}
	sf, err := st.op.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: cmd(), Reason: "KVKK erasure request #2147",
		PreviewToken: sp.GetPreviewToken()})
	if err != nil {
		t.Fatalf("subject-wide ForgetMemory: %v", err)
	}
	rec := sf.GetReceipt()
	if rec.GetPrincipalKind() != "operator" || rec.GetReason() != "compliance" ||
		!slices.Equal(rec.GetSelectorKinds(), []string{domain.ErasureTargetMemoryNotesOwner}) ||
		rec.GetCounts()["memory_notes"] != 1 || rec.GetCounts()["memory_notes_history"] == 0 || rec.GetCounts()["operator_audit"] != 2 {
		t.Fatalf("subject-wide receipt = %+v", rec)
	}
	for _, line := range []string{gym, oldAddr, newFact, locker, passport} {
		if h := held(line); h != [3]int{} {
			t.Errorf("%q survives the subject-wide erase in (live, history, journal) = %v", line, h)
		}
	}
	if c := count(`SELECT count(*) FROM memory_notes_history WHERE owner_id = $1 AND (content <> '' OR reason <> '')`, alice); c != 0 {
		t.Fatalf("%d history rows of alice still hold text", c)
	}
	if count(`SELECT count(*) FROM memory_notes WHERE owner_id = $1 AND deleted AND content = ''`, alice) != 1 {
		t.Fatal("the live document must be empty and deleted")
	}
	// Tombstones: the person, and every line any copy held (history-only included).
	if count(`SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND target_kind = 'memory_notes_owner' AND target_id = $2`,
		rec.GetRequestId(), alice) != 1 {
		t.Fatal("no memory_notes_owner tombstone")
	}
	for _, line := range []string{gym, oldAddr, newFact} {
		if count(`SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND target_kind = 'memory_note' AND target_id = $2
				AND content_digest = $3`, rec.GetRequestId(), domain.MemoryNoteEntryID("personal", alice, line), domain.MemoryNoteEntryDigest(line)) != 1 {
			t.Errorf("no tombstone for %q", line)
		}
	}
	if count(`SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND (target_id LIKE $2 OR content_digest LIKE $2)`,
		rec.GetRequestId(), "%"+stamp+"%Gym%") != 0 {
		t.Fatal("tombstones must be content-free")
	}
	if entries, _ := st.audit.Query(ctx, domain.AuditFilter{CommandID: "eng2147-" + stamp + "-" + string(rune('a'+n))}); len(entries) != 1 ||
		entries[0].ActionType != "forget_memory" || entries[0].TargetID != rec.GetRequestId() {
		t.Fatalf("the subject-wide erase must be journalled: %+v", entries)
	}
	if held(bobLine)[0] != 1 {
		t.Fatal("bob's notes must survive alice's erase")
	}
	// The next run of alice's agent reads no notes.
	nextRun, nextLease := agentFor("conv-a-" + stamp)
	g, err := srv.GetMemoryNotes(nextRun, &pb.GetMemoryNotesRequest{LeaseId: nextLease})
	if err != nil {
		t.Fatalf("next run's GetMemoryNotes: %v", err)
	}
	for _, d := range g.GetDocuments() {
		if strings.Contains(d.GetContent(), stamp) {
			t.Fatalf("the next run still reads alice's notes: %q", d.GetContent())
		}
	}
	// A retry of the command is deduped to the same receipt.
	dup, err := st.op.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: "eng2147-" + stamp + "-" + string(rune('a'+n)),
		Reason: "KVKK erasure request #2147", PreviewToken: sp.GetPreviewToken()})
	if err != nil || !dup.GetDeduped() || dup.GetReceipt().GetRequestId() != rec.GetRequestId() {
		t.Fatalf("retried command = %+v, %v", dup, err)
	}

	// ── restart: a fresh adapter, stack and services over the same database ──
	st2 := build()
	for _, line := range []string{gym, oldAddr, newFact, locker, passport} {
		if h := held(line); h != [3]int{} {
			t.Errorf("after restart %q is back in %v", line, h)
		}
	}
	got, err := st2.op.GetErasure(opCtx, &pb.GetErasureOpRequest{RequestId: rec.GetRequestId()})
	if err != nil || !slices.Equal(got.GetReceipt().GetSelectorKinds(), []string{domain.ErasureTargetMemoryNotesOwner}) ||
		got.GetReceipt().GetCounts()["memory_notes_history"] != rec.GetCounts()["memory_notes_history"] {
		t.Fatalf("receipt after restart = %+v, %v", got, err)
	}
	if _, err := st2.op.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{
		Selector: &pb.ErasureSelectorOp{MemoryNoteOwners: []string{alice}}}); status.Code(err) != codes.NotFound {
		t.Fatalf("a second subject-wide preview after restart = %v, want NotFound (nothing left)", err)
	}
}
