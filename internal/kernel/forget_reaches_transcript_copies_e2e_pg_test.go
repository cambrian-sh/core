package kernel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memory/vault"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-2155 end to end (owner decision 2026-11-19, the broad rule): forgetting a
// document reaches every transcript copy of its value, through the real chat path and
// the one erasure path:
//
//   - the production composition of the ENG-2109 test (Postgres adapter, the raw
//     PgErasureStore as MemoryBackend.Eraser, NewMemoryStack with erasure on, the run
//     ledger over Postgres and the vault, the Postgres conversation store, the chat
//     TurnService with its LLM-gateway lease, the agent-plane QueryMemory), plus the
//     ADR-0145 D2 token-budgeted transcript with the rolling summary and the ingress
//     consent bridge posting through TurnService.Emit;
//   - a turn recalls a contract note and answers quoting the agreement number; while it
//     runs, the consent bridge posts a machine notice naming the note's file (an Emit
//     outside the reply, built from the turn's work);
//   - the next turn (a bridging reply that says nothing of the note) inherits the
//     quoting reply's own records (one hop, ENG-2165 option B);
//   - the next turn recalls nothing and repeats the number from the transcript (it
//     shares the value with the quoting reply, so it inherits the note);
//   - unrelated replies follow until the quoting reply is folded into the rolling
//     summary, which the next turn is given — the summary holds the number, and that
//     turn copies it;
//   - the operator forgets the note: the quoting reply, the notice, the bridging reply,
//     the repeat and the copy read "[removed: forgotten source]" — the unrelated
//     replies do NOT (ENG-2165; before, every later reply was blanked) —
//     every summary that condensed it is deleted (counted on the receipt), and the
//     next turn is given neither the value in its transcript nor in its summary;
//   - after a restart (fresh adapter, stores, ledger and TurnService over the same
//     database) no agent reply and no summary holds the value, and a new turn is not
//     given it.
//
// The summarizer is the organ call's stand-in (it concatenates, so it keeps every
// value — the worst case for a summary). Requires PG_TEST_DSN.
func TestPg_ForgetReachesEveryTranscriptCopyOfTheValue(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Ledger.Enabled = true
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
	vaultDir := t.TempDir()
	newLedger := func(p LedgerStackDeps) *LedgerStack {
		t.Helper()
		led := NewLedgerStack(ctx, cfg.Execution.Ledger, cfg.Execution.Content, p)
		if led == nil {
			t.Fatal("ledger enabled but NewLedgerStack returned nil")
		}
		return led
	}
	led := newLedger(LedgerStackDeps{Pool: pool, Bodies: vault.NewArtifactVault(vaultDir), Authorizer: domain.AllowAllAuthorizer{}})
	convs, err := postgres.NewPgConversationStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	person := "eng2155-emp-" + stamp
	agentID := "eng2155-chat-" + stamp
	convID := "eng2155-conv-" + stamp
	noteDoc := "eng2155-note-" + stamp
	secret := "HERON-b5r2-" + stamp
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM conversations WHERE id = $1`, convID)
		_, _ = pool.Exec(bg, `DELETE FROM memory_erasures WHERE principal = $1`, "eng2155-op-"+stamp)
		_, _ = pool.Exec(bg, `DELETE FROM ledger_entries WHERE conversation_id = $1`, convID)
		_, _ = pool.Exec(bg, `DELETE FROM chunks WHERE document_id = $1`, noteDoc)
		_, _ = pool.Exec(bg, `DELETE FROM documents WHERE id = $1`, noteDoc)
	})

	text := "Contract note: agreement number " + secret + ", file /contracts/" + secret + ".pdf."
	if _, err := vec.SaveDocument(ctx, domain.SourceDocument{ID: noteDoc, Title: noteDoc, Text: text, WrittenBy: person, ContentDigest: "dg-" + noteDoc}); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	v := make([]float32, cfg.Embedder.Dimensions)
	v[0] = 1
	noteChunk := noteDoc + "-chunk-1"
	if err := vec.SaveBatch(ctx, []*domain.Document{{ID: noteChunk, Text: text, DocumentType: domain.DocTypeMnemonicFact,
		Embedding: domain.Embedding{Vector: v, Size: len(v), Model: "eng2155"},
		Metadata:  map[string]any{"document_id": noteDoc, "source_agent_id": person}, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}

	gw := subnetwork.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, llm.NewSpendMeter(0, nil, nil))
	ownerOf := func(_ context.Context, b domain.LeaseBinding) string {
		if b.ConversationID == convID {
			return person
		}
		return ""
	}
	svcForget := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "e2e"})
	srv := &subnetwork.Server{
		LLMGateway: gw, RunPrincipal: ownerOf, ConversationOwner: ownerOf, Forgetter: svcForget,
		MemorySearcher: chunkSearcher{reader: mem.Reader(), byQuery: map[string]string{"agreement number": noteChunk}},
		Ledger:         &subnetwork.LedgerAccess{Writer: led.Writer, Reader: led.Reader},
	}
	agentCtx := func(lease string) context.Context {
		c := metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", agentID, "x-lease-id", lease))
		c = domain.WithSurface(c, domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
		return domain.WithPrincipal(c, domain.AgentPrincipal(agentID))
	}

	// The consent bridge over a hub the worker drives (the real bridge, the real Emit).
	hub := &noticeHub{ch: make(chan domain.ConsentPrompt, 4)}
	var turns *corechat.TurnService
	posted := func(store *postgres.PgConversationStore, substr string) bool {
		msgs, _ := store.ListMessages(ctx, convID, 0, 0)
		for _, m := range msgs {
			if strings.Contains(m.Content, substr) {
				return true
			}
		}
		return false
	}

	long := strings.Repeat("Some general remarks about the supplier relationship. ", 6)
	var mu sync.Mutex
	var handoffs []*domain.Handoff
	worker := dispatcherFunc(func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		mu.Lock()
		handoffs = append(handoffs, h)
		mu.Unlock()
		lease := h.Context["_session_token_id"]
		q := strings.ToLower(string(h.Payload.Data))
		given := h.Payload.Metadata["transcript"] + "\n" + h.Payload.Metadata["conversation_summary"]
		switch {
		case strings.Contains(q, "agreement number"):
			resp, err := srv.QueryMemory(agentCtx(lease), &pb.MemoryRequest{Query: "agreement number"})
			if err != nil || len(resp.GetResults()) == 0 {
				return nil, err
			}
			// The machine notice for the file the note names, through the bridge.
			hub.ch <- domain.ConsentPrompt{ID: "p-" + stamp, Kind: domain.ConsentPromptNotice, ConversationID: convID,
				Notice: "Parked: writing /contracts/" + secret + ".pdf waits for a machine."}
			deadline := time.Now().Add(5 * time.Second)
			for !posted(convs, "Parked: writing") && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("From your records: " + resp.GetResults()[0].GetText())}}, nil
		case strings.Contains(q, "repeat"):
			// Recalls nothing: repeats the value from what it was given.
			if i := strings.Index(given, secret); i >= 0 {
				return &domain.Handoff{Payload: &domain.Payload{Data: []byte("As I said: " + secret + ".")}}, nil
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("I do not have it any more.")}}, nil
		case strings.Contains(q, "hello"):
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("Hello! How can I help?")}}, nil
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(long)}}, nil
	})
	summarize := func(_ context.Context, prev string, msgs []domain.Message) (string, error) {
		var b strings.Builder
		b.WriteString(prev)
		for _, m := range msgs {
			b.WriteString(" | " + m.Content)
		}
		return b.String(), nil
	}
	newTurns := func(store *postgres.PgConversationStore, l *LedgerStack) *corechat.TurnService {
		ts := corechat.NewTurnService(store, worker, func(ctx context.Context, limit int, ttl time.Duration) (string, func(), error) {
			lease, err := gw.Acquire(ctx, domain.StepAllocation{}, limit, ttl)
			if err != nil {
				return "", nil, err
			}
			return string(lease), func() { _, _ = gw.Complete(context.Background(), lease) }, nil
		})
		ts.SetLeaseBinder(gw, agentID)
		ts.SetLedger(l.Writer, l.Reader)
		ts.SetSummarizer(summarize)
		ts.Grounding = corechat.Grounding{TranscriptBudgetTokens: 160}
		return ts
	}
	turns = newTurns(convs, led)
	bridge := ingress.NewConsentBridge(hub, turns, time.Minute)
	bridge.Start(ctx)
	t.Cleanup(bridge.Stop)

	if err := convs.CreateConversation(ctx, domain.Conversation{ID: convID, OwnerID: person,
		Status: domain.ConversationOpen, Profile: domain.ProfileEmployee}); err != nil {
		t.Fatal(err)
	}
	say := func(ts *corechat.TurnService, text string) (domain.Message, *domain.Handoff) {
		t.Helper()
		m, err := ts.Turn(ctx, corechat.TurnRequest{ConversationID: convID, Text: text})
		if err != nil {
			t.Fatalf("turn %q: %v", text, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return m, handoffs[len(handoffs)-1]
	}

	hello, _ := say(turns, "hello")
	quoting, _ := say(turns, "What is the agreement number?")
	if !strings.Contains(quoting.Content, secret) {
		t.Fatalf("precondition: the reply quotes the note: %q", quoting.Content)
	}
	bridging, _ := say(turns, "ok thanks")
	if strings.Contains(bridging.Content, secret) {
		t.Fatalf("precondition: the bridging reply says nothing of the note: %q", bridging.Content)
	}
	repeat, _ := say(turns, "Can you repeat it?")
	if !strings.Contains(repeat.Content, secret) {
		t.Fatalf("precondition: the repeat reply carries the value: %q", repeat.Content)
	}
	var unrelated []domain.Message
	for i := 0; i < 4; i++ {
		m, _ := say(turns, "tell me more")
		unrelated = append(unrelated, m)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM ledger_entries WHERE conversation_id = $1 AND tool_name = $2`,
		convID, domain.LedgerToolConversationSummary); n == 0 {
		t.Fatal("precondition: no rolling summary was written")
	}
	copying, h := say(turns, "repeat the number please")
	if !strings.Contains(copying.Content, secret) {
		t.Fatalf("precondition: the copying reply carries the value: %q", copying.Content)
	}
	if !strings.Contains(h.Payload.Metadata["conversation_summary"], secret) {
		t.Fatalf("precondition: the turn was given a summary holding the value: %q", h.Payload.Metadata["conversation_summary"])
	}
	if strings.Contains(h.Payload.Metadata["transcript"], quoting.Content) {
		t.Fatal("precondition: the quoting reply should have left the verbatim transcript for the summary")
	}
	var notice domain.Message
	all, _ := convs.ListMessages(ctx, convID, 0, 0)
	for _, m := range all {
		if strings.HasPrefix(m.Content, "Parked: writing") {
			notice = m
		}
	}
	if notice.ID == "" || len(notice.SourceRefs) == 0 {
		t.Fatalf("the bridge's notice = %+v, want it stored resting on the turn's sources", notice)
	}

	// The operator forgets the note.
	opCtx := domain.WithScope(domain.WithPrincipalRoles(domain.WithPrincipal(ctx, domain.UserPrincipal("eng2155-op-"+stamp)),
		[]string{"operator"}), domain.ScopeMaintenance)
	rec, err := svcForget.Forget(opCtx, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{noteDoc}},
		Reason: domain.ErasureReasonCompliance})
	if err != nil {
		t.Fatalf("operator forget: %v", err)
	}
	redacted := map[string]bool{}
	for _, id := range rec.RedactedMessages {
		redacted[id] = true
	}
	for name, id := range map[string]string{"quoting reply": quoting.ID, "consent notice": notice.ID, "bridging reply": bridging.ID,
		"repeat reply": repeat.ID, "copying reply": copying.ID} {
		if !redacted[id] {
			t.Errorf("the %s %s is not on the receipt's redacted messages %v", name, id, rec.RedactedMessages)
		}
	}
	if redacted[hello.ID] {
		t.Error("the greeting before the recall was redacted")
	}
	// ENG-2165: the unrelated replies after the bridge rest on nothing they could have
	// repeated from the note, so forgetting it leaves them.
	for i, m := range unrelated {
		if redacted[m.ID] {
			t.Errorf("unrelated reply %d (%s) was redacted; only replies that can repeat the note may be", i, m.ID)
		}
	}
	if got := len(rec.RedactedMessages); got != 5 {
		t.Errorf("receipt redacted %d messages %v, want exactly the 5 that carry or may repeat the note", got, rec.RedactedMessages)
	}
	if rec.Counts["conversation_summaries"] == 0 {
		t.Errorf("receipt counts %v: no summary erased", rec.Counts)
	}

	check := func(store *postgres.PgConversationStore, q func(string, ...any) int) {
		t.Helper()
		if n := q(`SELECT count(*) FROM conversation_messages WHERE conversation_id = $1 AND content LIKE $2`, convID, "%"+secret+"%"); n != 0 {
			t.Errorf("%d transcript rows still hold the forgotten value", n)
		}
		if n := q(`SELECT count(*) FROM ledger_entries WHERE conversation_id = $1 AND tool_name = $2 AND source_refs ? $3`,
			convID, domain.LedgerToolConversationSummary, noteDoc); n != 0 {
			t.Errorf("%d rolling summaries resting on the forgotten note survived", n)
		}
		msgs, err := store.ListMessages(ctx, convID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.ID == hello.ID && m.Content != hello.Content {
				t.Errorf("the greeting changed: %q", m.Content)
			}
			if m.Role == domain.MessageRoleUser && m.RedactedAt != nil {
				t.Errorf("a person's message was redacted: %s", m.ID)
			}
		}
	}
	check(convs, func(sql string, args ...any) int { return countRows(t, pool, sql, args...) })

	// The next turn is given the value neither verbatim nor in a summary.
	after, h := say(turns, "repeat the number once more")
	if strings.Contains(h.Payload.Metadata["transcript"], secret) || strings.Contains(h.Payload.Metadata["conversation_summary"], secret) {
		t.Fatalf("the turn after the forget was given the value: transcript %q summary %q",
			h.Payload.Metadata["transcript"], h.Payload.Metadata["conversation_summary"])
	}
	if strings.Contains(after.Content, secret) {
		t.Fatalf("the reply after the forget holds the value: %q", after.Content)
	}

	// Restart: fresh adapter, conversation store, ledger and TurnService over the same
	// database.
	bridge.Stop()
	vec2, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vec2.Close)
	pool2 := vec2.Pool()
	convs2, err := postgres.NewPgConversationStore(ctx, pool2)
	if err != nil {
		t.Fatal(err)
	}
	check(convs2, func(sql string, args ...any) int { return countRows(t, pool2, sql, args...) })
	led2 := newLedger(LedgerStackDeps{Pool: pool2, Bodies: vault.NewArtifactVault(vaultDir), Authorizer: domain.AllowAllAuthorizer{}})
	turns2 := newTurns(convs2, led2)
	_, h = say(turns2, "repeat it after the restart")
	if strings.Contains(h.Payload.Metadata["transcript"], secret) || strings.Contains(h.Payload.Metadata["conversation_summary"], secret) {
		t.Fatal("after a restart a turn was given the forgotten value")
	}
	check(convs2, func(sql string, args ...any) int { return countRows(t, pool2, sql, args...) })
}

// countRows runs a count query.
func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// noticeHub is a WorkerConsentHub whose prompts the test pushes (the real hub's
// Watch side); the bridge renders them into the conversation.
type noticeHub struct{ ch chan domain.ConsentPrompt }

func (h *noticeHub) Request(context.Context, domain.ConsentPrompt) (domain.ConsentAnswer, domain.ConsentOutcome, error) {
	return domain.ConsentAnswer{}, "", nil
}
func (h *noticeHub) Notify(context.Context, domain.ConsentPrompt) {}
func (h *noticeHub) Watch() (<-chan domain.ConsentPrompt, func()) {
	return h.ch, func() {}
}
func (h *noticeHub) Submit(context.Context, string, domain.ConsentAnswer) bool { return false }
