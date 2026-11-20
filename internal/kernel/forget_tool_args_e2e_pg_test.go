package kernel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-2254 end to end (owner decision 2026-11-20, option A), over the production
// composition: a real Postgres adapter, the raw PgErasureStore as the backend's
// eraser, NewMemoryStack with erasure on, and the erase called through
// MemoryStack.Eraser() (the enforcing chokepoint) as an operator. Alice — who has
// NO personal notes — is erased with the subject-wide memory_note_owners
// selector: her conversation's action_history rows are deleted and the intent of
// her pending approvals is cleared, counted in the receipt; Bob's rows and
// intent, and Alice's resolved approval, are untouched.
func TestPg_OperatorForget_ErasesAPersonsToolCallArguments(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
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

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	alice, bob := "eng2254-alice-"+stamp, "eng2254-bob-"+stamp
	convA, convB := "eng2254-conv-a-"+stamp, "eng2254-conv-b-"+stamp
	now := time.Now().UTC()
	for conv, owner := range map[string]string{convA: alice, convB: bob} {
		if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, title, status, profile, created_at, updated_at)
			VALUES ($1, $2, '', 'active', 'default', $3, $3)`, conv, owner, now); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM action_history WHERE scope IN ($1, $2)`, "conversation:"+convA, "conversation:"+convB)
		_, _ = pool.Exec(bg, `DELETE FROM approvals WHERE tool_name LIKE 'eng2254-%'`)
		_, _ = pool.Exec(bg, `DELETE FROM conversations WHERE id IN ($1, $2)`, convA, convB)
	})

	hist := postgres.NewPgActionHistoryStore(pool)
	for i, scope := range []string{"conversation:" + convA, "conversation:" + convA, "conversation:" + convB} {
		rec := domain.ActionRecord{Tool: "send_mail", Args: []byte(`{"to":"ayse@example.com","n":` + string(rune('0'+i)) + `}`), At: now}
		if err := hist.RecordDone(ctx, scope, rec, domain.ActionRecordKey(scope, rec), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	approvals := postgres.NewPgApprovalStore(pool)
	put := func(id string, state domain.ApprovalState, onBehalf, conv string) {
		t.Helper()
		if err := approvals.PutApproval(ctx, domain.ApprovalItem{ID: id, NaturalKey: id, ToolName: "eng2254-send_mail",
			State: state, RaisedAt: now, ExpiresAt: now.Add(time.Hour), ConversationID: conv,
			Intent: &domain.ApprovalIntent{Executor: domain.ApprovalIntentTool, Tool: "send_mail",
				Args: []byte(`{"to":"ayse@example.com"}`), OnBehalfOf: onBehalf, ConversationID: conv}}); err != nil {
			t.Fatal(err)
		}
	}
	put("apr-eng2254-a-"+stamp, domain.ApprovalPending, alice, convA)
	put("apr-eng2254-b-"+stamp, domain.ApprovalPending, bob, convB)
	put("apr-eng2254-a-done-"+stamp, domain.ApprovalApproved, alice, convA) // resolved: not "pending"

	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	intentHeld := func(id string) bool {
		return count(`SELECT count(*) FROM approvals WHERE id = $1 AND intent IS NOT NULL`, id) == 1
	}

	opCtx := domain.WithScope(domain.WithPrincipal(domain.WithPrincipalRoles(ctx, []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng2254-ops"}), domain.ScopeMaintenance) // the operator plane's erase scope
	rc, err := mem.Eraser().Erase(opCtx, domain.ErasureRequest{
		Reason: domain.ErasureReasonCompliance, Selector: domain.ErasureSelector{MemoryNoteOwners: []string{alice}},
	})
	if err != nil {
		t.Fatalf("erase alice: %v", err)
	}
	if rc.Counts["action_history"] != 2 || rc.Counts["approval_intents"] != 1 {
		t.Fatalf("receipt counts = %v, want action_history 2, approval_intents 1", rc.Counts)
	}
	if n := count(`SELECT count(*) FROM action_history WHERE scope = $1`, "conversation:"+convA); n != 0 {
		t.Errorf("alice's conversation still has %d action_history row(s)", n)
	}
	if n := count(`SELECT count(*) FROM action_history WHERE scope = $1`, "conversation:"+convB); n != 1 {
		t.Errorf("bob's action_history rows = %d, want 1 untouched", n)
	}
	if intentHeld("apr-eng2254-a-" + stamp) {
		t.Error("alice's pending approval still holds its intent")
	}
	if !intentHeld("apr-eng2254-b-" + stamp) {
		t.Error("bob's pending approval lost its intent")
	}
	if !intentHeld("apr-eng2254-a-done-" + stamp) {
		t.Error("a resolved approval's intent was touched (only pending ones are)")
	}
	// Nothing of hers left: a second subject-wide erase finds nothing.
	_, err = mem.Eraser().Erase(opCtx, domain.ErasureRequest{
		Reason: domain.ErasureReasonCorrection, Selector: domain.ErasureSelector{MemoryNoteOwners: []string{alice, "x"}},
	})
	if err == nil {
		t.Fatal("a second erase of alice (nothing left) must report nothing to erase")
	}
}
