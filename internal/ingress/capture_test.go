package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Chat capture (FIVE-PLANES-BUILD Wave-3 C1, seam S7). The property under test
// throughout: a message that was answered left a record of itself, and a record
// that could not be written never cost anybody a reply.

type capturedRaw struct {
	raw domain.RawEvidence
}

type fakeIngest struct {
	mu   sync.Mutex
	got  []capturedRaw
	err  error
	next string
}

func (f *fakeIngest) ingest(_ context.Context, raw domain.RawEvidence) (domain.EvidenceID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	f.got = append(f.got, capturedRaw{raw: raw})
	id := f.next
	if id == "" {
		id = "ev_test"
	}
	return domain.EvidenceID(id), true, nil
}

func (f *fakeIngest) rows() []capturedRaw {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRaw(nil), f.got...)
}

// fakeEvents is the narrowest EventStore that satisfies the port: the capture
// lane only ever writes, and the read half exists so the interface is met.
type fakeEvents struct {
	mu   sync.Mutex
	got  []domain.Event
	err  error
	seen map[string]struct{}
}

func (f *fakeEvents) RecordEvent(_ context.Context, ev domain.Event) (domain.EventID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	if f.seen == nil {
		f.seen = map[string]struct{}{}
	}
	if _, dup := f.seen[ev.NamespaceID+"|"+ev.SourceRef]; dup && ev.SourceRef != "" {
		return "evt_existing", false, nil
	}
	f.seen[ev.NamespaceID+"|"+ev.SourceRef] = struct{}{}
	f.got = append(f.got, ev)
	return "evt_test", true, nil
}

func (f *fakeEvents) RecordObservation(context.Context, domain.Observation) (bool, error) {
	return false, nil
}

func (f *fakeEvents) PointLookup(context.Context, string, string, string) (*domain.Observation, error) {
	return nil, nil
}

func (f *fakeEvents) History(context.Context, string, string, string, time.Time, time.Time) ([]domain.Observation, error) {
	return nil, nil
}

func (f *fakeEvents) EventsForEntity(context.Context, string, string, time.Time, time.Time) ([]domain.Event, error) {
	return nil, nil
}

func (f *fakeEvents) events() []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Event(nil), f.got...)
}

type fakeEntities struct {
	mu     sync.Mutex
	minted []domain.Entity
}

func (f *fakeEntities) EnsureEntity(_ context.Context, e domain.Entity) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted = append(f.minted, e)
	return true, nil
}

func (f *fakeEntities) GetEntity(context.Context, string, string) (domain.Entity, bool, error) {
	return domain.Entity{}, false, nil
}

func (f *fakeEntities) ListEntitiesByKind(context.Context, string, string, int) ([]domain.Entity, error) {
	return nil, nil
}

func (f *fakeEntities) ResolveEntityByLocal(context.Context, string, string, int) ([]domain.Entity, error) {
	return nil, nil
}

func (f *fakeEntities) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.minted))
	for _, e := range f.minted {
		out = append(out, e.ID)
	}
	return out
}

// boundIdentities resolves every sender to one bound principal, which is what
// the deployment looks like once an operator has bound somebody.
type boundIdentities struct {
	binding domain.IdentityBinding
	bound   bool
	mode    string
}

func (b boundIdentities) ResolveIdentity(context.Context, string, domain.SenderProfile) (domain.IdentityBinding, bool) {
	return b.binding, b.bound
}

func (b boundIdentities) StrangerPolicyFor(context.Context, string) domain.StrangerPolicy {
	return domain.StrangerPolicy{Mode: b.mode}
}

func captureFixture(t *testing.T) (*InboundService, *fakeIngest, *fakeEvents, *fakeEntities) {
	t.Helper()
	s, _, _ := inboundFixture(t)
	ing, evs, ents := &fakeIngest{}, &fakeEvents{}, &fakeEntities{}
	c := NewChatCapture(ing.ingest, evs)
	c.SetEntityStore(ents)
	s.SetCapture(c)
	return s, ing, evs, ents
}

// The headline: an admitted message becomes an evidence row, in the bound
// namespace, carrying the surface tag a policy can grant on.
func TestCapture_AdmittedMessageBecomesEvidence(t *testing.T) {
	s, ing, _, _ := captureFixture(t)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "my order is late",
		SpeakerID: "67890", Username: "@afsin", DisplayName: "Afşin",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	rows := ing.rows()
	if len(rows) != 1 {
		t.Fatalf("expected exactly one evidence row, got %d", len(rows))
	}
	raw := rows[0].raw
	if raw.NamespaceID != "default" {
		t.Errorf("captured chat must land in a namespace the read plane addresses, got %q", raw.NamespaceID)
	}
	if raw.SourceID != "chat:"+tgAgent {
		t.Errorf("source id must name the ingress it arrived through, got %q", raw.SourceID)
	}
	if !strings.HasPrefix(raw.SourceKey, "conv-fixed/") {
		t.Errorf("source key must be per-message inside its conversation, got %q", raw.SourceKey)
	}
	// The surface tag is the whole point of classifying at all: an untagged row
	// leaks to a broad policy and hides from a surface-scoped one at once.
	if len(raw.Classification) != 1 || raw.Classification[0] != "ingress:"+tgAgent {
		t.Errorf("expected the ingress surface tag, got %v", raw.Classification)
	}
	// The archived body is what arrived, and nothing derived.
	var body capturedMessage
	if err := json.Unmarshal(raw.Bytes, &body); err != nil {
		t.Fatalf("archived body is not the captured shape: %v", err)
	}
	if body.Text != "my order is late" || body.ExternalID != "tg:12345" || body.SpeakerID != "67890" {
		t.Errorf("the archive lost what arrived: %+v", body)
	}
	if body.Surface != "chat:telegram" {
		t.Errorf("the entry point must be preserved beside the message, got %q", body.Surface)
	}
}

// The identity binding resolved at admission is no longer discarded (S7): the
// bound principal is the party the captured row is about, which is what makes
// "this person's own conversations" expressible as a row-level scope.
func TestCapture_PartiesComeFromTheResolvedBinding(t *testing.T) {
	s, ing, _, _ := captureFixture(t)
	s.SetIdentityResolver(boundIdentities{
		bound: true,
		binding: domain.IdentityBinding{
			Surface: "chat:telegram", ExternalID: "tg:12345",
			BoundToKind: domain.BindPrincipal, BoundToID: "principal:afsin",
		},
	})

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	rows := ing.rows()
	if len(rows) != 1 {
		t.Fatalf("expected one evidence row, got %d", len(rows))
	}
	if got := rows[0].raw.Parties; len(got) != 1 || got[0] != "principal:afsin" {
		t.Errorf("the resolved binding must reach the row's parties, got %v", got)
	}
}

// An unbound stranger is a party to NOTHING. Fail-closed (ADR-0121 D6): "we
// could not tell who this is" must never read as "everyone".
func TestCapture_UnboundSenderIsAPartyToNothing(t *testing.T) {
	s, ing, _, _ := captureFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: false, mode: domain.StrangerSurfaceDefault})

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := ing.rows()[0].raw.Parties; len(got) != 0 {
		t.Errorf("an unbound sender must produce no parties, got %v", got)
	}
}

// One event per message, with the author and the thread as real participant
// edges — the difference between a sender id inside a payload and an entity the
// query plane can walk.
func TestCapture_WritesTheChatMessageEventWithRoles(t *testing.T) {
	s, _, evs, ents := captureFixture(t)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
		SpeakerID: "67890",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	got := evs.events()
	if len(got) != 1 {
		t.Fatalf("expected one event, got %d", len(got))
	}
	ev := got[0]
	if ev.Type != ChatMessageEventType {
		t.Errorf("event type: got %q", ev.Type)
	}
	if ev.EvidenceID != "ev_test" {
		t.Errorf("the event must cite the evidence row it came from, got %q", ev.EvidenceID)
	}
	if !strings.HasPrefix(ev.SourceRef, "conv-fixed/") {
		t.Errorf("source ref must be the per-message key, got %q", ev.SourceRef)
	}
	roles := map[string]string{}
	for _, r := range ev.Roles {
		roles[r.Role] = r.EntityID
	}
	// Author keys on the SPEAKER, not the external id: in a group the external
	// id names the room, and keying on it merges every member into one person.
	if roles[ChatRoleAuthor] != "chat_user/67890" {
		t.Errorf("author role: got %q", roles[ChatRoleAuthor])
	}
	if roles[ChatRoleThread] != "thread/conv-fixed" {
		t.Errorf("thread role: got %q", roles[ChatRoleThread])
	}
	// And both are minted, so `entity chat_user/67890` names something.
	minted := strings.Join(ents.ids(), ",")
	if !strings.Contains(minted, "chat_user/67890") || !strings.Contains(minted, "thread/conv-fixed") {
		t.Errorf("author and thread must be minted entities, got %v", ents.ids())
	}
}

// With no speaker reported, the author falls back to the external id — a
// one-to-one bridge reports nothing else, and a nameless author would be worse
// than a coarse one.
func TestCapture_AuthorFallsBackToTheExternalID(t *testing.T) {
	s, _, evs, _ := captureFixture(t)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	for _, r := range evs.events()[0].Roles {
		if r.Role == ChatRoleAuthor && r.EntityID != "chat_user/tg:12345" {
			t.Errorf("author fallback: got %q", r.EntityID)
		}
	}
}

// THE availability rule: chat is the one lane with a person waiting, so an
// archive that refuses costs a log line and not a reply.
func TestCapture_TurnProceedsWhenTheArchiveFails(t *testing.T) {
	b, turns := &fakeBinder{}, &fakeTurns{}
	s := NewInboundService(b, registered(), turns)
	s.newID = func() string { return "conv-fixed" }
	ing := &fakeIngest{err: errors.New("archive down")}
	evs := &fakeEvents{}
	s.SetCapture(NewChatCapture(ing.ingest, evs))

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
	}); err != nil {
		t.Fatalf("a capture failure must not fail the turn: %v", err)
	}
	if len(turns.ran) != 1 {
		t.Fatalf("the turn must still run, got %+v", turns.ran)
	}
	// And no event is written against evidence that does not exist.
	if len(evs.events()) != 0 {
		t.Errorf("an event must not cite an evidence row that was never written: %+v", evs.events())
	}
}

// The event store failing is a degradation, not a hole: the message is archived
// either way, only its identity edges are lost.
func TestCapture_EventFailureLeavesTheEvidence(t *testing.T) {
	b, turns := &fakeBinder{}, &fakeTurns{}
	s := NewInboundService(b, registered(), turns)
	s.newID = func() string { return "conv-fixed" }
	ing := &fakeIngest{}
	s.SetCapture(NewChatCapture(ing.ingest, &fakeEvents{err: errors.New("events down")}))

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(ing.rows()) != 1 || len(turns.ran) != 1 {
		t.Errorf("the evidence and the turn must both survive an event-store failure: %d rows, %d turns",
			len(ing.rows()), len(turns.ran))
	}
}

// No capture wired at all — evidence capture disabled — is the pre-Wave-3
// deployment, and it must behave exactly as it did.
func TestCapture_AbsentCaptureIsInert(t *testing.T) {
	s, _, turns := inboundFixture(t)
	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 {
		t.Errorf("a deployment with no archive must still answer: %+v", turns.ran)
	}
}

// The per-message key is DERIVED, so the same message redelivered dedups on the
// evidence triple and its event dedups on the same ref — the two planes cannot
// disagree about how many times a person said something.
func TestCapture_TheSameMessageProducesTheSameKey(t *testing.T) {
	s, ing, evs, _ := captureFixture(t)
	ctx := context.Background()
	msg := InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "same words"}

	_ = s.Accept(ctx, msg)
	_ = s.Accept(ctx, msg)

	rows := ing.rows()
	if len(rows) != 2 {
		t.Fatalf("both deliveries reach the chokepoint; the STORE dedups, not this lane: %d", len(rows))
	}
	if rows[0].raw.SourceKey != rows[1].raw.SourceKey || rows[0].raw.SourceRevision != rows[1].raw.SourceRevision {
		t.Errorf("a redelivered message must present the same idempotency triple: %q/%q vs %q/%q",
			rows[0].raw.SourceKey, rows[0].raw.SourceRevision,
			rows[1].raw.SourceKey, rows[1].raw.SourceRevision)
	}
	if len(evs.events()) != 1 {
		t.Errorf("the second delivery must not mint a second occurrence: %d", len(evs.events()))
	}
}

// A different message in the same conversation is a different row. Stated as a
// test because the key is derived from content: if it collapsed here, a
// conversation would archive as one message.
func TestCapture_DifferentMessagesGetDifferentKeys(t *testing.T) {
	s, ing, _, _ := captureFixture(t)
	ctx := context.Background()

	_ = s.Accept(ctx, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "first"})
	_ = s.Accept(ctx, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "second"})

	rows := ing.rows()
	if len(rows) != 2 || rows[0].raw.SourceKey == rows[1].raw.SourceKey {
		t.Fatalf("two messages must be two rows: %+v", rows)
	}
}

// The deployment's floor rides beneath the surface tag, and an ingress with no
// id produces no bare "ingress:" tag — one shared tag standing for nothing looks
// like a grantable surface and is not one.
func TestChatCapture_Classification(t *testing.T) {
	c := NewChatCapture(nil, nil)
	c.Floor = []string{"chat", "chat", " "}
	got := c.classification("telegram_ingress")
	if len(got) != 2 || got[0] != "ingress:telegram_ingress" || got[1] != "chat" {
		t.Errorf("surface tag first, then a deduplicated floor: %v", got)
	}
	if got := c.classification(""); len(got) != 1 || got[0] != "chat" {
		t.Errorf("an ingress with no id must contribute no surface tag: %v", got)
	}
}
