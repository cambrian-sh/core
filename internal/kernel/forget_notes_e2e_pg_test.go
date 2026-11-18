package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memorynotes"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-2106 end to end, the readiness repro (ENG-1995 phase B) through the real chat
// path: the production composition — a real Postgres adapter, the raw PgErasureStore as
// MemoryBackend.Eraser, NewMemoryStack with erasure on, the Postgres memory-notes store,
// the forget service over MemoryStack.Eraser() reaching the notes service — driven
// through the agent-plane handlers a chat agent calls: ProposeMemoryNote (its
// propose_memory_note) and ExecuteTool (its forget_memory), each with the lease the
// kernel bound to the person's conversation. Asserted in Postgres:
//
//   - "remember my locker code" lands in memory_notes; "forget it" previews the note
//     line (kind memory_note) and erases it with the token: the line is gone from the
//     live document AND from every memory_notes_history row, the rest of the notes
//     stays, a content-free tombstone and a receipt remain, and the next run's notes no
//     longer carry it;
//   - the tool's answer comes from the receipt: erased true with erased_count 1 and the
//     request id; asking again (the repro's "Yes, I confirm. Erase it.") and asking to
//     forget something that was never saved both answer erased false / nothing_found;
//   - another person's agent cannot reach the note.
//
// Requires PG_TEST_DSN pointing at a scratch database (migrated to HEAD here).
func TestPg_ForgetViaChat_ErasesTheMemoryNoteAndReportsFromTheReceipt(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	ctx := context.Background()
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
	notesStore, err := memorynotes.NewPgStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	notes, err := memorynotes.New(notesStore, memorynotes.ScannerFunc(func(string) (bool, string) { return false, "" }),
		domain.AllowAllAuthorizer{}, memorynotes.Config{PersonalBudgetChars: 4000, OrgBudgetChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "e2e", Notes: notes})

	stamp := time.Now().UTC().Format("150405.000000")
	person, other := "eng2106-emp-"+stamp, "eng2106-bob-"+stamp
	agentID := "eng2106-chat-" + stamp
	code := "LOCKER-" + strings.ReplaceAll(stamp, ".", "")
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM memory_notes WHERE owner_id IN ($1, $2)`,
			`DELETE FROM memory_notes_history WHERE owner_id IN ($1, $2)`,
		} {
			_, _ = pool.Exec(bg, q, person, other)
		}
		_, _ = pool.Exec(bg, `DELETE FROM memory_erasures WHERE principal = $1`, agentID)
	})

	// The kernel side of the chat lane: the lease the kernel bound to the person's
	// conversation, the run principal resolved from it (conversation owner), the tool
	// executor with forget_memory on it.
	gw := subnetwork.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, llm.NewSpendMeter(0, nil, nil))
	owners := map[string]string{"conv-" + stamp: person, "conv-bob-" + stamp: other}
	ownerOf := func(_ context.Context, b domain.LeaseBinding) string { return owners[b.ConversationID] }
	te, err := domain.NewToolExecutor(domain.ToolExecutorControls{
		Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore(), Authz: domain.AllowAllAuthorizer{},
	}, domain.ToolExecutor{KernelTools: []domain.KernelTool{forget.NewTool(svc, true)}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &subnetwork.Server{LLMGateway: gw, MemoryNotes: notes, MemoryNotesPrincipal: ownerOf,
		RunPrincipal: ownerOf, ConversationOwner: ownerOf, Forgetter: svc, ToolExecutor: te}
	lease := func(run, conv string) string {
		t.Helper()
		l, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gw.BindLease(l, domain.LeaseBinding{RunID: domain.RunID(run), AgentID: agentID, ConversationID: conv})
		return string(l)
	}
	agentCtx := domain.WithSurface(metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", agentID)),
		domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
	agentCtx = domain.WithPrincipal(agentCtx, domain.AgentPrincipal(agentID))

	// Turn 1: "Please remember this for me: my locker code is …" → propose_memory_note.
	turn1 := lease("run-1-"+stamp, "conv-"+stamp)
	for _, line := range []string{"Locker code: " + code, "Gym on Tuesdays"} {
		w, err := srv.ProposeMemoryNote(agentCtx, &pb.ProposeMemoryNoteRequest{LeaseId: turn1,
			Op: pb.MemoryNoteOp_MEMORY_NOTE_OP_APPEND, Section: "Personal", Content: line, Reason: "the person asked to remember " + line})
		if err != nil || !w.GetAccepted() {
			t.Fatalf("propose %q: %+v %v", line, w, err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	like := "%" + code + "%"
	if count(`SELECT count(*) FROM memory_notes WHERE owner_id = $1 AND content LIKE $2`, person, like) != 1 ||
		count(`SELECT count(*) FROM memory_notes_history WHERE owner_id = $1 AND (content LIKE $2 OR reason LIKE $2)`, person, like) < 2 {
		t.Fatal("precondition: the note and its history carry the code")
	}

	tool := func(lease, args string) map[string]any {
		t.Helper()
		resp, err := srv.ExecuteTool(agentCtx, &pb.ExecuteToolRequest{ToolName: forget.ToolName, ArgsJson: args, LeaseId: lease})
		if err != nil || resp.GetDenied() || resp.GetError() != "" {
			t.Fatalf("forget_memory %s: %+v %v", args, resp, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(resp.GetResultJson()), &m); err != nil {
			t.Fatalf("result %s: %v", resp.GetResultJson(), err)
		}
		return m
	}

	// Another person's chat cannot reach the note: nothing found for bob.
	bobTurn := lease("run-bob-"+stamp, "conv-bob-"+stamp)
	if got := tool(bobTurn, `{"query":"locker code"}`); got["erased"] != false || got["outcome"] != "nothing_found" {
		t.Fatalf("bob's agent previewing alice's note = %v", got)
	}

	// Turn 3: "Please forget my locker code permanently" → preview, then the token.
	turn3 := lease("run-3-"+stamp, "conv-"+stamp)
	prev := tool(turn3, `{"query":"locker code"}`)
	would, _ := prev["would_erase"].([]any)
	wantID := domain.MemoryNoteEntryID("personal", person, "Locker code: "+code)
	if prev["erased"] != false || len(would) != 1 || would[0].(map[string]any)["kind"] != domain.ErasureTargetMemoryNote ||
		would[0].(map[string]any)["id"] != wantID || would[0].(map[string]any)["snippet"] != "Locker code: "+code {
		t.Fatalf("preview = %v", prev)
	}
	if count(`SELECT count(*) FROM memory_notes WHERE owner_id = $1 AND content LIKE $2`, person, like) != 1 {
		t.Fatal("a preview must erase nothing")
	}
	done := tool(turn3, `{"preview_token":"`+prev["preview_token"].(string)+`"}`)
	reqID, _ := done["request_id"].(string)
	if done["erased"] != true || done["erased_count"] != float64(1) || reqID == "" ||
		!strings.Contains(done["tell_the_person"].(string), reqID) {
		t.Fatalf("erase = %v", done)
	}

	// Postgres: gone from the live note and every history row; the rest stays.
	if n := count(`SELECT count(*) FROM memory_notes WHERE owner_id = $1 AND content LIKE $2`, person, like); n != 0 {
		t.Fatalf("memory_notes still carries the code (%d rows)", n)
	}
	if n := count(`SELECT count(*) FROM memory_notes_history WHERE owner_id = $1 AND (content LIKE $2 OR reason LIKE $2)`, person, like); n != 0 {
		t.Fatalf("memory_notes_history still carries the code (%d rows)", n)
	}
	if count(`SELECT count(*) FROM memory_notes WHERE owner_id = $1 AND content LIKE '%Gym on Tuesdays%' AND NOT deleted`, person) != 1 {
		t.Fatal("the rest of the person's notes must survive")
	}
	// Notes are never in the vector or lexical store; nothing there carries the code.
	if n := count(`SELECT count(*) FROM chunks WHERE text LIKE $1`, like); n != 0 {
		t.Fatalf("chunks carry the code (%d)", n)
	}
	if count(`SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND target_kind = 'memory_note' AND target_id = $2
			AND content_digest = $3`, reqID, wantID, domain.MemoryNoteEntryDigest("Locker code: "+code)) != 1 ||
		count(`SELECT count(*) FROM memory_erasures WHERE request_id = $1 AND principal = $2 AND counts->>'memory_notes' = '1'`, reqID, agentID) != 1 {
		t.Fatal("the tombstone and the receipt must record the erase")
	}
	if count(`SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND content_digest IS NOT NULL
			AND (target_id LIKE $2 OR content_digest LIKE $2)`, reqID, like) != 0 {
		t.Fatal("the tombstone must be content-free")
	}
	// The next run's notes no longer carry it.
	next := lease("run-4-"+stamp, "conv-"+stamp)
	g, err := srv.GetMemoryNotes(agentCtx, &pb.GetMemoryNotesRequest{LeaseId: next})
	if err != nil || len(g.GetDocuments()) != 1 || strings.Contains(g.GetDocuments()[0].GetContent(), code) ||
		!strings.Contains(g.GetDocuments()[0].GetContent(), "Gym on Tuesdays") {
		t.Fatalf("next run's notes = %+v %v", g, err)
	}

	// Turn 4: "Yes, I confirm. Erase it." → another query: nothing left, said so.
	if again := tool(turn3, `{"query":"locker code"}`); again["erased"] != false || again["outcome"] != "nothing_found" {
		t.Fatalf("after the erase = %v", again)
	}
	// Forgetting something never saved: nothing found, nothing claimed.
	if none := tool(turn3, `{"query":"passport number"}`); none["erased"] != false || none["found"] != false ||
		none["outcome"] != "nothing_found" {
		t.Fatalf("forget of a fact never saved = %v", none)
	}
}
