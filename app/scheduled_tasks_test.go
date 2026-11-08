package app

import (
	"context"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
)

// convMem is a minimal in-memory domain.ConversationStore for the owned-turn tests.
type convMem struct {
	mu    sync.Mutex
	convs map[string]domain.Conversation
	msgs  map[string][]domain.Message
}

func newConvMem() *convMem {
	return &convMem{convs: map[string]domain.Conversation{}, msgs: map[string][]domain.Message{}}
}

func (m *convMem) CreateConversation(_ context.Context, c domain.Conversation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.convs[c.ID] = c
	return nil
}

func (m *convMem) GetConversation(_ context.Context, id string) (*domain.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return nil, domain.ErrConversationNotFound
	}
	return &c, nil
}

func (m *convMem) ListConversations(context.Context, string, int) ([]domain.Conversation, error) {
	return nil, nil
}

func (m *convMem) SetConversationStatus(_ context.Context, id string, s domain.ConversationStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.convs[id]
	c.Status = s
	m.convs[id] = c
	return nil
}

func (m *convMem) AppendMessage(_ context.Context, msg domain.Message) (domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg.Seq = int64(len(m.msgs[msg.ConversationID]) + 1)
	m.msgs[msg.ConversationID] = append(m.msgs[msg.ConversationID], msg)
	return msg, nil
}

func (m *convMem) ListMessages(_ context.Context, id string, afterSeq int64, _ int) ([]domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Message
	for _, x := range m.msgs[id] {
		if x.Seq > afterSeq {
			out = append(out, x)
		}
	}
	return out, nil
}

func (m *convMem) BindDelivery(context.Context, string, domain.DeliveryAddress) error { return nil }
func (m *convMem) FindByDelivery(context.Context, domain.DeliveryAddress) (*domain.Conversation, error) {
	return nil, domain.ErrConversationNotFound
}

// echoPool answers every turn with the text it was given.
type echoPool struct{}

func (echoPool) Dispatch(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: append([]byte("done: "), h.Payload.Data...)}}, nil
}

// A scheduled task's run happens in a conversation its OWNER holds, so the run
// principal (the conversation-owner rule, ADR-0139 D14) is the owner.
func TestOwnedTurn_RunsInTheOwnersConversation(t *testing.T) {
	store := newConvMem()
	r := &ownedTurnRunner{}
	if _, err := r.run(context.Background(), domain.OwnedTurn{ConversationID: "c", OwnerID: "lena", Text: "x"}); err == nil {
		t.Fatal("no chat lane: want an error, not a silent no-op")
	}
	r.bind(store, corechat.NewTurnService(store, echoPool{}, nil))

	msg, err := r.run(context.Background(), domain.OwnedTurn{
		ConversationID: "sched:sched-1", OwnerID: "lena", Title: "weekly", Text: "summarise", ClientID: "fire-1",
	})
	if err != nil || msg.Content != "done: summarise" {
		t.Fatalf("run: %+v %v", msg, err)
	}
	if c := store.convs["sched:sched-1"]; c.OwnerID != "lena" {
		t.Fatalf("conversation owner = %q, want lena", c.OwnerID)
	}
	// The run principal resolved for a lease bound to that conversation is the owner.
	owner := conversationOwnerOf(store, nil)(context.Background(), domain.LeaseBinding{ConversationID: "sched:sched-1"})
	if owner != "lena" {
		t.Fatalf("conversation owner resolution = %q", owner)
	}
	notes := memoryNotesPrincipal("default-person", store, nil, nil)(context.Background(), domain.LeaseBinding{ConversationID: "sched:sched-1"})
	if notes != "lena" {
		t.Fatalf("run principal = %q, want the owner (never the default)", notes)
	}

	// Someone else's conversation is refused, never written into.
	if _, err := r.run(context.Background(), domain.OwnedTurn{ConversationID: "sched:sched-1", OwnerID: "dana", Text: "x"}); err == nil {
		t.Fatal("a turn for dana in lena's conversation must be refused")
	}
}

func TestConversationOwnerOf_NoFallback(t *testing.T) {
	store := newConvMem()
	if got := conversationOwnerOf(store, nil)(context.Background(), domain.LeaseBinding{}); got != "" {
		t.Fatalf("a lease with no conversation must resolve to nobody, got %q", got)
	}
	if got := conversationOwnerOf(nil, nil)(context.Background(), domain.LeaseBinding{ConversationID: "c"}); got != "" {
		t.Fatalf("no store: %q", got)
	}
}

func TestBuildScheduledTasks_Gates(t *testing.T) {
	approvals := domain.AlwaysApproveController{}
	if buildScheduledTasks(config.SchedulesConfig{}, nil, approvals) != nil {
		t.Fatal("no premium store ⇒ no service")
	}
	if buildScheduledTasks(config.SchedulesConfig{Disabled: true}, nopSchedules{}, approvals) != nil {
		t.Fatal("disabled ⇒ no service")
	}
	if buildScheduledTasks(config.SchedulesConfig{}, nopSchedules{}, nil) != nil {
		t.Fatal("no approval channel ⇒ no service (never create silently)")
	}
	svc := buildScheduledTasks(config.SchedulesConfig{MaxPerPrincipal: 3, MinIntervalSeconds: 60}, nopSchedules{}, approvals)
	if svc == nil || svc.Limits().MaxPerPrincipal != 3 || svc.Limits().MinInterval.Seconds() != 60 {
		t.Fatalf("config not threaded: %+v", svc)
	}
}

type nopSchedules struct{}

func (nopSchedules) Check(context.Context, domain.ScheduledTaskSpec, domain.ScheduleLimits) error {
	return nil
}
func (nopSchedules) Create(context.Context, domain.ScheduledTaskSpec, domain.ScheduleLimits) (domain.ScheduledTask, error) {
	return domain.ScheduledTask{}, nil
}
func (nopSchedules) List(context.Context, string) ([]domain.ScheduledTask, error) { return nil, nil }
func (nopSchedules) SetActive(context.Context, string, string, bool) error        { return nil }
func (nopSchedules) Delete(context.Context, string, string) error                 { return nil }
