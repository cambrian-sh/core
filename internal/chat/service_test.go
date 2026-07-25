package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"time"
)

// memStore is an in-memory domain.ConversationStore with the same Seq/idempotency semantics
// as the Postgres one, so the service can be tested without a database.
type memStore struct {
	mu    sync.Mutex
	convs map[string]domain.Conversation
	msgs  map[string][]domain.Message
	next  map[string]int64
}

func newMemStore() *memStore {
	return &memStore{convs: map[string]domain.Conversation{}, msgs: map[string][]domain.Message{}, next: map[string]int64{}}
}

func (m *memStore) CreateConversation(_ context.Context, c domain.Conversation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.convs[c.ID] = c
	m.next[c.ID] = 1
	return nil
}

func (m *memStore) GetConversation(_ context.Context, id string) (*domain.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return nil, domain.ErrConversationNotFound
	}
	return &c, nil
}

func (m *memStore) ListConversations(context.Context, string, int) ([]domain.Conversation, error) {
	return nil, nil
}

func (m *memStore) SetConversationStatus(_ context.Context, id string, s domain.ConversationStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return domain.ErrConversationNotFound
	}
	c.Status = s
	m.convs[id] = c
	return nil
}

func (m *memStore) AppendMessage(_ context.Context, msg domain.Message) (domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[msg.ConversationID]
	if !ok {
		return domain.Message{}, domain.ErrConversationNotFound
	}
	if c.Status != domain.ConversationOpen {
		return domain.Message{}, domain.ErrConversationClosed
	}
	if msg.ClientID != "" {
		for _, existing := range m.msgs[msg.ConversationID] {
			if existing.ClientID == msg.ClientID {
				return existing, nil
			}
		}
	}
	msg.Seq = m.next[msg.ConversationID]
	m.next[msg.ConversationID]++
	m.msgs[msg.ConversationID] = append(m.msgs[msg.ConversationID], msg)
	return msg, nil
}

func (m *memStore) ListMessages(_ context.Context, id string, afterSeq int64, limit int) ([]domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Message
	for _, msg := range m.msgs[id] {
		if msg.Seq > afterSeq {
			out = append(out, msg)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// fakePool records what it was asked to dispatch and returns a scripted reply.
type fakePool struct {
	mu       sync.Mutex
	handoffs []*domain.Handoff
	reply    string
	err      error
}

func (f *fakePool) Dispatch(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	f.mu.Lock()
	f.handoffs = append(f.handoffs, h)
	reply, err := f.reply, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(reply)}}, nil
}

func (f *fakePool) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handoffs)
}

func setup(t *testing.T, profile domain.ConversationProfile) (*TurnService, *memStore, *fakePool) {
	t.Helper()
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: profile,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pool := &fakePool{reply: "hello there"}
	return NewTurnService(store, pool, nil), store, pool
}

func TestTurn_AppendsUserThenReply(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)

	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got.Role != domain.MessageRoleAgent || got.Content != "hello there" {
		t.Fatalf("unexpected reply message: %+v", got)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 2 || msgs[0].Role != domain.MessageRoleUser || msgs[1].Role != domain.MessageRoleAgent {
		t.Fatalf("expected user then agent message, got %+v", msgs)
	}
	if pool.calls() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", pool.calls())
	}
}

// A retried turn must return the original reply WITHOUT dispatching again — otherwise a
// client retry double-charges an LLM call and may re-run side-effecting tools.
func TestTurn_RetryIsIdempotent(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	req := TurnRequest{ConversationID: "c1", Text: "hi", ClientID: "turn-1"}

	first, err := svc.Turn(context.Background(), req)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	second, err := svc.Turn(context.Background(), req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.ID != first.ID || second.Seq != first.Seq {
		t.Fatalf("retry produced a different reply: first=%+v second=%+v", first, second)
	}
	if pool.calls() != 1 {
		t.Fatalf("retry must not dispatch again; dispatches = %d", pool.calls())
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 2 {
		t.Fatalf("retry must not add messages; got %d", len(msgs))
	}
}

func TestTurn_ClosedAndMissingConversation(t *testing.T) {
	svc, store, _ := setup(t, domain.ProfileEmployee)
	if err := store.SetConversationStatus(context.Background(), "c1", domain.ConversationClosed); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); !errors.Is(err, domain.ErrConversationClosed) {
		t.Fatalf("want ErrConversationClosed, got %v", err)
	}
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "ghost", Text: "hi"}); !errors.Is(err, domain.ErrConversationNotFound) {
		t.Fatalf("want ErrConversationNotFound, got %v", err)
	}
}

// An empty reply must surface as an error, never be stored — a blank agent message would
// corrupt the transcript threaded into every later turn.
func TestTurn_EmptyReplyIsNotStored(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	pool.reply = "   "

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("want ErrEmptyReply, got %v", err)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	for _, m := range msgs {
		if m.Role == domain.MessageRoleAgent {
			t.Fatal("an empty reply must not be persisted")
		}
	}
}

// The transcript carries prior turns only; the message being answered travels as the payload.
func TestTurn_TranscriptExcludesCurrentMessage(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	ctx := context.Background()

	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "first"}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "second"}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	pool.mu.Lock()
	h := pool.handoffs[1]
	pool.mu.Unlock()

	if string(h.Payload.Data) != "second" {
		t.Fatalf("payload should be the current message, got %q", h.Payload.Data)
	}
	transcript := h.Payload.Metadata["transcript"]
	if !strings.Contains(transcript, "first") || !strings.Contains(transcript, "hello there") {
		t.Fatalf("transcript should carry prior turns, got %q", transcript)
	}
	if strings.Contains(transcript, "second") {
		t.Fatalf("transcript must not contain the message being answered, got %q", transcript)
	}
}

// ADR-0084 D7: the conversation's posture travels with the turn, so a worker cannot be
// pointed at shared memory just because an agent default says so.
func TestTurn_ProfileTravelsWithTurn(t *testing.T) {
	for _, tc := range []struct {
		profile    domain.ConversationProfile
		wantRecall string
	}{
		{domain.ProfileCustomer, "false"},
		{domain.ProfileEmployee, "true"},
		{domain.ProfileOperator, "true"},
	} {
		svc, _, pool := setup(t, tc.profile)
		if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
			t.Fatalf("%s: Turn: %v", tc.profile, err)
		}
		pool.mu.Lock()
		md := pool.handoffs[0].Payload.Metadata
		pool.mu.Unlock()

		if md["profile"] != string(tc.profile) {
			t.Errorf("%s: profile metadata = %q", tc.profile, md["profile"])
		}
		if md["recall_lookup"] != tc.wantRecall {
			t.Errorf("%s: recall_lookup = %q, want %q", tc.profile, md["recall_lookup"], tc.wantRecall)
		}
	}
}

// A shed or lost worker must propagate, and must not leave a half-written exchange.
func TestTurn_DispatchErrorPropagates(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	sentinel := errors.New("pool saturated")
	pool.err = sentinel

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); !errors.Is(err, sentinel) {
		t.Fatalf("want the dispatch error, got %v", err)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	for _, m := range msgs {
		if m.Role == domain.MessageRoleAgent {
			t.Fatal("no agent message should be stored when dispatch fails")
		}
	}
}

func TestTurn_RejectsEmptyText(t *testing.T) {
	svc, _, _ := setup(t, domain.ProfileEmployee)
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "  "}); err == nil {
		t.Fatal("expected an error for empty turn text")
	}
}

// ---------------------------------------------------------------------------
// ADR-0084 D2 — a turn's conversation travels on its LEASE.
//
// Work the turn delegates to the planner must be attributable back to the exchange that
// ordered it. Putting the conversation on the lease (rather than in agent-supplied metadata)
// means the kernel resolves it from something it issued, so an agent cannot claim a
// conversation it was not dispatched under.
// ---------------------------------------------------------------------------

type recordingBinder struct {
	bound map[domain.LeaseID]domain.LeaseBinding
}

func (r *recordingBinder) BindLease(id domain.LeaseID, b domain.LeaseBinding) {
	if r.bound == nil {
		r.bound = map[domain.LeaseID]domain.LeaseBinding{}
	}
	r.bound[id] = b
}

func (r *recordingBinder) ResolveLease(id domain.LeaseID) (domain.LeaseBinding, bool) {
	b, ok := r.bound[id]
	return b, ok
}

func TestTurn_BindsConversationOntoTheLease(t *testing.T) {
	svc, _, _ := setup(t, domain.ProfileOperator)
	binder := &recordingBinder{}
	svc.SetLeaseBinder(binder)
	svc.acquireToken = func(context.Context, int, time.Duration) (string, func(), error) {
		return "lease-1", func() {}, nil
	}

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "book a flight"}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	b, ok := binder.bound["lease-1"]
	if !ok {
		t.Fatal("the turn's lease was never bound — delegated work could not be attributed")
	}
	if b.ConversationID != "c1" {
		t.Errorf("ConversationID = %q, want c1", b.ConversationID)
	}
	if b.OriginMessageID == "" {
		t.Error("OriginMessageID must name the ordering turn — that is the causation half")
	}
}

// No binder wired ⇒ the turn still works; attribution is optional, not load-bearing.
func TestTurn_WithoutBinderStillSucceeds(t *testing.T) {
	svc, _, _ := setup(t, domain.ProfileOperator)
	svc.acquireToken = func(context.Context, int, time.Duration) (string, func(), error) {
		return "lease-1", func() {}, nil
	}
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("turn without binder: %v", err)
	}
}
