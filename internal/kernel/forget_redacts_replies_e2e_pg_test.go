package kernel

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memory/vault"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-2109 end to end, the readiness repro (ENG-1995 phase B block 5) through the real
// chat path and the one erasure path (owner decision 2026-11-18, option B):
//
//   - the production composition: a real Postgres adapter, the raw PgErasureStore as
//     MemoryBackend.Eraser, NewMemoryStack with erasure on, the run ledger
//     (NewLedgerStack over Postgres and the artifact vault), the Postgres conversation
//     store, the chat TurnService with its lease bound by the LLM gateway, and the
//     agent-plane QueryMemory the chat worker calls on the turn's lease (which records
//     the recall in the ledger with its source refs);
//   - a contract note is ingested; a chat turn recalls it and answers quoting the
//     agreement number — the reply records the document and chunk it rested on; a second
//     turn answers from another document, a third recalls nothing;
//   - the operator forgets the note (compliance erase, MemoryStack.Eraser): the quoting
//     reply now reads "[removed: forgotten source]" with its row, role, seq and time
//     kept, every other message is untouched, and the receipt lists the message;
//   - the chat agent forgets the second document (forget_memory through ExecuteTool):
//     its reply is redacted the same way;
//   - after a "restart" (a fresh adapter, store and eraser over the same database) the
//     replies are still redacted, the receipt still lists them, and no agent reply holds
//     the erased value.
//
// The recall's ranking is not under test: the worker's searcher returns the note's real
// chunk, read back through MemoryStack.Reader(). Requires PG_TEST_DSN.
func TestPg_ForgetRedactsChatRepliesThatQuotedTheDocument(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
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
	led := NewLedgerStack(ctx, cfg.Execution.Ledger, cfg.Execution.Content, LedgerStackDeps{
		Pool: pool, Bodies: vault.NewArtifactVault(t.TempDir()), Authorizer: domain.AllowAllAuthorizer{},
	})
	if led == nil {
		t.Fatal("ledger enabled but NewLedgerStack returned nil")
	}
	convs, err := postgres.NewPgConversationStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	person := "eng2109-emp-" + stamp
	agentID := "eng2109-chat-" + stamp
	convID := "eng2109-conv-" + stamp
	noteDoc, otherDoc := "eng2109-note-"+stamp, "eng2109-other-"+stamp
	secret := "HERON-b5r1-" + stamp
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM conversations WHERE id = $1`, convID)
		_, _ = pool.Exec(bg, `DELETE FROM memory_erasures WHERE principal IN ($1, $2)`, "eng2109-op-"+stamp, agentID)
		_, _ = pool.Exec(bg, `DELETE FROM ledger_entries WHERE conversation_id = $1`, convID)
		for _, d := range []string{noteDoc, otherDoc} {
			_, _ = pool.Exec(bg, `DELETE FROM chunks WHERE document_id = $1`, d)
			_, _ = pool.Exec(bg, `DELETE FROM documents WHERE id = $1`, d)
		}
	})

	// Ingest: the contract note (written by the person's upload) and a second document
	// the chat agent wrote, each a document row with one embedded chunk.
	ingest := func(doc, writer, text string, axis int) string {
		t.Helper()
		if _, err := vec.SaveDocument(ctx, domain.SourceDocument{ID: doc, Title: doc, Text: text, WrittenBy: writer, ContentDigest: "dg-" + doc}); err != nil {
			t.Fatalf("SaveDocument: %v", err)
		}
		v := make([]float32, cfg.Embedder.Dimensions)
		v[axis] = 1
		chunk := doc + "-chunk-1"
		if err := vec.SaveBatch(ctx, []*domain.Document{{ID: chunk, Text: text, DocumentType: domain.DocTypeMnemonicFact,
			Embedding: domain.Embedding{Vector: v, Size: len(v), Model: "eng2109"},
			Metadata:  map[string]any{"document_id": doc, "source_agent_id": writer}, CreatedAt: time.Now().UTC()}}); err != nil {
			t.Fatalf("SaveBatch: %v", err)
		}
		return chunk
	}
	noteChunk := ingest(noteDoc, person, "Contract note: agreement number "+secret+", account manager Irmak Sahin.", 0)
	otherChunk := ingest(otherDoc, agentID, "Supplier visit is on Thursday.", 1)

	// The kernel side of the chat lane.
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
	te, err := domain.NewToolExecutor(domain.ToolExecutorControls{
		Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore(), Authz: domain.AllowAllAuthorizer{},
	}, domain.ToolExecutor{KernelTools: []domain.KernelTool{forget.NewTool(svcForget, true)}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &subnetwork.Server{
		LLMGateway: gw, RunPrincipal: ownerOf, ConversationOwner: ownerOf, Forgetter: svcForget, ToolExecutor: te,
		MemorySearcher: chunkSearcher{reader: mem.Reader(), byQuery: map[string]string{
			"agreement number": noteChunk, "supplier visit": otherChunk,
		}},
		Ledger: &subnetwork.LedgerAccess{Writer: led.Writer, Reader: led.Reader},
	}
	agentCtx := func(lease string) context.Context {
		c := metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", agentID, "x-lease-id", lease))
		c = domain.WithSurface(c, domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
		return domain.WithPrincipal(c, domain.AgentPrincipal(agentID))
	}

	// The chat worker: recalls on the turn's lease when the question names something,
	// and answers with what the recall returned (quoting it), else greets.
	worker := dispatcherFunc(func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		lease := h.Context["_session_token_id"]
		q := strings.ToLower(string(h.Payload.Data))
		answer := "Hello! How can I help?"
		for _, key := range []string{"agreement number", "supplier visit"} {
			if !strings.Contains(q, key) {
				continue
			}
			resp, err := srv.QueryMemory(agentCtx(lease), &pb.MemoryRequest{Query: key})
			if err != nil {
				return nil, err
			}
			if len(resp.GetResults()) == 0 {
				answer = "I found nothing about that."
				break
			}
			answer = "From your records: " + resp.GetResults()[0].GetText()
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(answer)}}, nil
	})
	turns := corechat.NewTurnService(convs, worker, func(ctx context.Context, limit int, ttl time.Duration) (string, func(), error) {
		l, err := gw.Acquire(ctx, domain.StepAllocation{}, limit, ttl)
		if err != nil {
			return "", nil, err
		}
		return string(l), func() { _, _ = gw.Complete(context.Background(), l) }, nil
	})
	turns.SetLeaseBinder(gw, agentID)
	turns.SetLedger(led.Writer, led.Reader)

	if err := convs.CreateConversation(ctx, domain.Conversation{ID: convID, OwnerID: person,
		Status: domain.ConversationOpen, Profile: domain.ProfileEmployee}); err != nil {
		t.Fatal(err)
	}
	say := func(text string) domain.Message {
		t.Helper()
		m, err := turns.Turn(ctx, corechat.TurnRequest{ConversationID: convID, Text: text})
		if err != nil {
			t.Fatalf("turn %q: %v", text, err)
		}
		return m
	}
	quoting := say("What is the agreement number with the supplier?")
	if !strings.Contains(quoting.Content, secret) {
		t.Fatalf("precondition: the reply quotes the note: %q", quoting.Content)
	}
	if !reflect.DeepEqual(quoting.SourceRefs, domain.SortedUniqueIDs([]string{noteChunk, noteDoc})) {
		t.Fatalf("the reply's citation = %v, want the note and its chunk", quoting.SourceRefs)
	}
	visit := say("When is the supplier visit?")
	hello := say("thanks, hi")
	if len(hello.SourceRefs) != 0 {
		t.Fatalf("a reply that recalled nothing cites %v", hello.SourceRefs)
	}

	// The operator forgets the note (the operator plane's compliance erase context).
	opCtx := domain.WithScope(domain.WithPrincipalRoles(domain.WithPrincipal(ctx, domain.UserPrincipal("eng2109-op-"+stamp)),
		[]string{"operator"}), domain.ScopeMaintenance)
	rec, err := svcForget.Forget(opCtx, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{noteDoc}},
		Reason: domain.ErasureReasonCompliance})
	if err != nil {
		t.Fatalf("operator forget: %v", err)
	}
	if rec.PrincipalKind != domain.ErasureByOperator {
		t.Fatalf("receipt principal kind = %q, want operator", rec.PrincipalKind)
	}
	if !reflect.DeepEqual(rec.RedactedMessages, []string{quoting.ID}) || rec.Counts["conversation_messages"] != 1 {
		t.Fatalf("receipt redacted %v (counts %v), want exactly the quoting reply %s", rec.RedactedMessages, rec.Counts, quoting.ID)
	}

	check := func(store *postgres.PgConversationStore, redacted ...string) {
		t.Helper()
		msgs, err := store.ListMessages(ctx, convID, 0, 0)
		if err != nil || len(msgs) != 6 {
			t.Fatalf("transcript = %d rows, %v; want all 6 kept", len(msgs), err)
		}
		isRedacted := map[string]bool{}
		for _, id := range redacted {
			isRedacted[id] = true
		}
		for _, m := range msgs {
			switch {
			case isRedacted[m.ID]:
				if m.Content != domain.RedactedReplyText || m.RedactedAt == nil || m.Role != domain.MessageRoleAgent {
					t.Errorf("%s = %q (%s, redacted_at %v), want the redaction marker", m.ID, m.Content, m.Role, m.RedactedAt)
				}
			case m.Content == domain.RedactedReplyText || m.RedactedAt != nil:
				t.Errorf("%s (%s) was redacted but cited nothing erased", m.ID, m.Role)
			}
		}
		if msgs[0].Role != domain.MessageRoleUser || !strings.Contains(msgs[0].Content, "agreement number") {
			t.Errorf("the person's own question changed: %+v", msgs[0])
		}
	}
	check(convs, quoting.ID)
	if got, _ := convs.ListMessages(ctx, convID, 0, 0); got[3].ID != visit.ID || got[3].Content != visit.Content {
		t.Fatalf("the reply from the other document must be untouched: %+v", got[3])
	}

	// The chat agent forgets the document it wrote (forget_memory: preview, then token).
	toolCall := func(lease, args string) map[string]any {
		t.Helper()
		resp, err := srv.ExecuteTool(agentCtx(lease), &pb.ExecuteToolRequest{ToolName: forget.ToolName, ArgsJson: args, LeaseId: lease})
		if err != nil || resp.GetDenied() || resp.GetError() != "" {
			t.Fatalf("forget_memory %s: %+v %v", args, resp, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(resp.GetResultJson()), &m); err != nil {
			t.Fatalf("result %s: %v", resp.GetResultJson(), err)
		}
		return m
	}
	l, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(l, domain.LeaseBinding{AgentID: agentID, ConversationID: convID})
	prev := toolCall(string(l), `{"document_ids":["`+otherDoc+`"]}`)
	tok, _ := prev["preview_token"].(string)
	if tok == "" {
		t.Fatalf("preview = %v", prev)
	}
	done := toolCall(string(l), `{"preview_token":"`+tok+`"}`)
	ids, _ := done["redacted_messages"].([]any)
	if done["erased"] != true || len(ids) != 1 || ids[0] != visit.ID ||
		!strings.Contains(done["tell_the_person"].(string), domain.RedactedReplyText) {
		t.Fatalf("agent forget = %v, want the visit reply redacted and said so", done)
	}
	check(convs, quoting.ID, visit.ID)

	// Restart: a fresh adapter, conversation store and eraser over the same database.
	vec2, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vec2.Close)
	convs2, err := postgres.NewPgConversationStore(ctx, vec2.Pool())
	if err != nil {
		t.Fatal(err)
	}
	check(convs2, quoting.ID, visit.ID)
	er2 := postgres.NewPgErasureStore(postgres.NewPgDocumentLifecycle(postgres.NewPgKnowledgeStore(vec2.Pool(), nil)))
	back, found, err := er2.Receipt(ctx, rec.RequestID)
	if err != nil || !found || !reflect.DeepEqual(back.RedactedMessages, []string{quoting.ID}) {
		t.Fatalf("receipt after restart = %+v %v %v", back.RedactedMessages, found, err)
	}
	var n int
	if err := vec2.Pool().QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE role = 'agent' AND content LIKE $1`,
		"%"+secret+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d agent replies still hold the erased value (%v)", n, err)
	}
}

// dispatcherFunc adapts a function to corechat.Dispatcher.
type dispatcherFunc func(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error)

func (f dispatcherFunc) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	return f(ctx, h)
}

// chunkSearcher answers a known query with one stored chunk read back through the
// enforced reader (the kernel's own bypass predicate; the recall's ranking is not what
// this test is about). A chunk that is gone is no hit.
type chunkSearcher struct {
	reader  domain.VectorReader
	byQuery map[string]string
}

func (s chunkSearcher) Search(ctx context.Context, query, _ string) ([]domain.SearchResult, error) {
	id, ok := s.byQuery[query]
	if !ok {
		return nil, nil
	}
	d, err := s.reader.GetByID(domain.WithScope(ctx, &domain.TagPredicate{Bypass: true}), id)
	if err != nil || d == nil {
		return nil, err
	}
	return []domain.SearchResult{{Document: *d, Score: 0.9}}, nil
}

func (chunkSearcher) SearchActions(context.Context, string, string) ([]domain.SearchResult, error) {
	return nil, nil
}

func (chunkSearcher) SearchProcedures(context.Context, string, string) ([]domain.SearchResult, error) {
	return nil, nil
}
