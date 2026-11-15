package domain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// The one addressed approval hub (ADR-0142 D11; owner acceptance 2026-11-06).

var (
	hubAlice = UserPrincipal("alice")
	hubBob   = UserPrincipal("bob")
)

func opWatch(id string) ApprovalWatch {
	return ApprovalWatch{For: OperatorApprover(id), Resolutions: true}
}

// raiseAsync runs Request in the background and returns its decision channel.
func raiseAsync(ctx context.Context, h *AddressedHub, req ApprovalRequest) <-chan ApprovalDecision {
	out := make(chan ApprovalDecision, 1)
	go func() {
		dec, _ := h.Request(ctx, req)
		out <- dec
	}()
	return out
}

func nextRaised(t *testing.T, ch <-chan ApprovalEvent) ApprovalItem {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if ev.Type == ApprovalEventRaised {
				return ev.Item
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no approval was raised")
		}
	}
}

func nextResolved(t *testing.T, ch <-chan ApprovalEvent) ApprovalItem {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if ev.Type == ApprovalEventResolved {
				return ev.Item
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no resolution was published")
		}
	}
}

type busLog struct {
	mu       sync.Mutex
	raised   []HITLRaisedEvent
	resolved []HITLResolvedEvent
}

func newBusLog() (*busLog, EventBus) {
	l := &busLog{}
	bus := NewInMemoryEventBus()
	bus.Subscribe(EventTypeHITLRaised, func(e DomainEvent) {
		l.mu.Lock()
		l.raised = append(l.raised, e.(HITLRaisedEvent))
		l.mu.Unlock()
	})
	bus.Subscribe(EventTypeHITLResolved, func(e DomainEvent) {
		l.mu.Lock()
		l.resolved = append(l.resolved, e.(HITLResolvedEvent))
		l.mu.Unlock()
	})
	return l, bus
}

func (l *busLog) outcomes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.resolved {
		out = append(out, r.Outcome)
	}
	return out
}

func TestDefaultApprovalPolicyIsValid(t *testing.T) {
	if _, err := NewConfigApprovalPolicy(DefaultApprovalPolicyConfig()); err != nil {
		t.Fatalf("shipped approval policy is invalid: %v", err)
	}
}

func TestApprovalPolicy_RejectsSelfOnlyNeverSelfClass(t *testing.T) {
	cfg := DefaultApprovalPolicyConfig()
	cfg.Rules = append(cfg.Rules, ApprovalClassRule{Class: "wire_transfer", Approvers: []string{"self"}})
	cfg.NeverSelfApprovable = append(cfg.NeverSelfApprovable, "wire_transfer")
	if _, err := NewConfigApprovalPolicy(cfg); err == nil {
		t.Fatal("a never-self class approvable only by self must be a config error")
	}
	if _, err := ParseApprovalRule("boss"); err == nil {
		t.Fatal("unknown rule kind must not parse")
	}
	if r, err := ParseApprovalRule("role:compliance"); err != nil || r.Kind != ApprovalRuleRole || r.Value != "compliance" {
		t.Fatalf("role rule: %+v %v", r, err)
	}
}

// Operator answers a tool approval; raise and resolve are published on the feed
// under the same id, and the id is not appr-<n>.
func TestHub_OperatorApprovesToolAndBothEventsPublish(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	log, bus := newBusLog()
	h := NewAddressedHub(AddressedHubConfig{Bus: bus, DefaultTimeout: 2 * time.Second})
	ch, cancel := h.Watch(opWatch("op-1"))
	defer cancel()

	dec := raiseAsync(context.Background(), h, ApprovalRequest{AgentID: "a1", ToolName: "execute_command", ArgsPreview: "rm -rf x"})
	it := nextRaised(t, ch)
	if !strings.HasPrefix(it.ID, "apr-") || it.RuleSummary() != "operator" {
		t.Fatalf("item = %+v", it)
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op-1")}); err != nil {
		t.Fatalf("operator answer refused: %v", err)
	}
	d := <-dec
	if !d.Approved || !d.Decided || d.ApproverID != "op-1" {
		t.Fatalf("decision = %+v", d)
	}
	if res := nextResolved(t, ch); res.ID != it.ID || res.State != ApprovalApproved {
		t.Fatalf("resolution = %+v", res)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.raised) != 1 || log.raised[0].InterventionID != it.ID || len(log.resolved) != 1 || log.resolved[0].Outcome != "approved" {
		t.Fatalf("feed events: raised=%+v resolved=%+v", log.raised, log.resolved)
	}
}

func TestHub_NoEligibleSubscriberFailsClosed(t *testing.T) {
	h := NewAddressedHub(AddressedHubConfig{DefaultTimeout: time.Second})
	// A subscriber exists, but it delivers to a non-operator, who cannot answer a
	// tool approval: still "no approver".
	_, cancel := h.Watch(ApprovalWatch{For: Approver{Principal: hubBob}})
	defer cancel()
	d, err := h.Request(context.Background(), ApprovalRequest{ToolName: "execute_command"})
	if err != nil || d.Approved || d.State != ApprovalUnroutable {
		t.Fatalf("no eligible approver must fail closed: %+v %v", d, err)
	}
}

// Timeout and cancellation both resolve the item AND publish it (the gap in
// T-20261106-withdrawn-or-timed-out-tool-approvals-publish).
func TestHub_ExpiredAndWithdrawnPublishResolution(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	log, bus := newBusLog()
	h := NewAddressedHub(AddressedHubConfig{Bus: bus, DefaultTimeout: 40 * time.Millisecond})
	ch, cancel := h.Watch(opWatch("op"))
	defer cancel()

	d, _ := h.Request(context.Background(), ApprovalRequest{ToolName: "t1"})
	if d.Approved || d.State != ApprovalExpired {
		t.Fatalf("timeout must expire closed: %+v", d)
	}
	if res := nextResolved(t, ch); res.State != ApprovalExpired {
		t.Fatalf("expired resolution: %+v", res)
	}

	ctx, stop := context.WithCancel(context.Background())
	done := raiseAsync(ctx, h, ApprovalRequest{ToolName: "t2", Timeout: 5 * time.Second})
	nextRaised(t, ch)
	stop()
	if d := <-done; d.Approved || d.State != ApprovalWithdrawn {
		t.Fatalf("withdrawn decision: %+v", d)
	}
	if res := nextResolved(t, ch); res.State != ApprovalWithdrawn {
		t.Fatalf("withdrawn resolution: %+v", res)
	}
	got := log.outcomes()
	if len(got) != 2 || got[0] != "expired" || got[1] != "withdrawn" {
		t.Fatalf("feed resolutions = %v", got)
	}
}

// answerOnPendingStore answers an item the moment raise persists it pending —
// i.e. inside the gap between raise (persist + publish) and await registering
// its waiter. It deterministically reproduces the lost-answer race of
// T-20261115-addressedhub-request-loses-an-answer-that-lands (an immediately
// answering surface beating the requester to await).
type answerOnPendingStore struct {
	ApprovalStore
	once   sync.Once
	answer func(id string)
}

func (s *answerOnPendingStore) PutApproval(ctx context.Context, it ApprovalItem) error {
	if err := s.ApprovalStore.PutApproval(ctx, it); err != nil {
		return err
	}
	if it.State == ApprovalPending {
		s.once.Do(func() { s.answer(it.ID) })
	}
	return nil
}

// An answer that lands before the requester's waiter is registered is not
// lost: Request returns it at once (not after its window) and takes it, so an
// identical later request does not reuse the decision.
func TestHub_AnswerBeforeAwaitRegistersIsNotLost(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	store := &answerOnPendingStore{ApprovalStore: NewMemoryApprovalStore()}
	h := NewAddressedHub(AddressedHubConfig{Store: store, DefaultTimeout: 10 * time.Second})
	store.answer = func(id string) {
		if _, err := h.Answer(context.Background(), id, ApprovalAnswer{Approved: true, By: OperatorApprover("op-1")}); err != nil {
			t.Errorf("answer in the gap refused: %v", err)
		}
	}
	_, cancel := h.Watch(opWatch("op-1"))
	defer cancel()

	start := time.Now()
	d, err := h.Request(context.Background(), ApprovalRequest{AgentID: "a1", ToolName: "execute_command", ArgsPreview: "ls"})
	if err != nil || !d.Approved || !d.Decided || d.ApproverID != "op-1" {
		t.Fatalf("decision = %+v err=%v, want the operator's approval", d, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Request took %v: the early answer was only found after the window", took)
	}
	stored, err := store.GetApproval(context.Background(), d.ApprovalID)
	if err != nil || !stored.Consumed {
		t.Fatalf("stored item = %+v err=%v, want the decision marked consumed", stored, err)
	}
}

// Only addressees answer: a self-ruled consent prompt refuses an operator and a
// stranger, and accepts the beneficiary.
func TestHub_OnlyAddresseesCanAnswer(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	h := NewAddressedHub(AddressedHubConfig{ConsentTimeout: 2 * time.Second})
	audit, cancelAudit := h.Watch(ApprovalWatch{For: OperatorApprover("root"), All: true})
	defer cancelAudit()
	_, cancelAlice := h.Watch(ApprovalWatch{For: Approver{Principal: hubAlice}})
	defer cancelAlice()

	done := raiseAsync(context.Background(), h, ApprovalRequest{
		Kind: ApprovalKindMachineConsent, ToolName: "local:laptop/write_file", Machine: "laptop", Beneficiary: hubAlice,
	})
	var ev ApprovalEvent
	select {
	case ev = <-audit:
	case <-time.After(3 * time.Second):
		t.Fatal("the operator audit view saw nothing")
	}
	if ev.Answerable {
		t.Fatal("the audit view must mark a self-ruled item not answerable by an operator")
	}
	if _, err := h.Answer(context.Background(), ev.Item.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("root")}); !errors.Is(err, ErrApprovalNotAddressee) {
		t.Fatalf("an operator must be refused on a self-ruled item: %v", err)
	}
	if _, err := h.Answer(context.Background(), ev.Item.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: hubBob}}); !errors.Is(err, ErrApprovalNotAddressee) {
		t.Fatalf("another person must be refused: %v", err)
	}
	// Alice arriving on the agent kind during the stage-1 migration is still Alice.
	if _, err := h.Answer(context.Background(), ev.Item.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: AgentPrincipal("alice")}}); err != nil {
		t.Fatalf("the beneficiary must be able to answer: %v", err)
	}
	if d := <-done; !d.Approved {
		t.Fatalf("decision = %+v", d)
	}
}

func TestHub_NamedPersonAndRoleRules(t *testing.T) {
	cfg := DefaultApprovalPolicyConfig()
	cfg.Rules = append(cfg.Rules,
		ApprovalClassRule{Class: "records_release", Approvers: []string{"person:carol"}},
		ApprovalClassRule{Class: "export", Approvers: []string{"role:compliance"}})
	p, err := NewConfigApprovalPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAddressedHub(AddressedHubConfig{Policy: p})
	carol := Approver{Principal: UserPrincipal("carol")}
	comp := Approver{Principal: UserPrincipal("dave"), Roles: []string{"compliance"}}
	for _, tc := range []struct {
		class string
		by    Approver
		want  bool
	}{
		{"records_release", carol, true},
		{"records_release", OperatorApprover("root"), false},
		{"export", comp, true},
		{"export", carol, false},
	} {
		it, err := h.Raise(context.Background(), ApprovalRequest{Kind: "x", ActionClass: tc.class, ToolName: tc.class + tc.by.Principal.ID})
		if err != nil {
			t.Fatal(err)
		}
		ok, _ := h.Answerable(context.Background(), it, tc.by)
		if ok != tc.want {
			t.Errorf("%s by %s: answerable=%v want %v", tc.class, tc.by.Principal, ok, tc.want)
		}
	}
}

// A relationship rule (supervisor) is declared but needs the stage-2 hierarchy:
// it fails closed, visibly, and never falls back to self.
func TestHub_RelationshipRuleFailsClosedNeedsHierarchy(t *testing.T) {
	cfg := DefaultApprovalPolicyConfig()
	cfg.Rules = append(cfg.Rules, ApprovalClassRule{Class: "expense", Approvers: []string{"relationship:supervisor"}})
	p, _ := NewConfigApprovalPolicy(cfg)
	log, bus := newBusLog()
	h := NewAddressedHub(AddressedHubConfig{Policy: p, Bus: bus})
	_, cancel := h.Watch(ApprovalWatch{For: Approver{Principal: hubAlice}})
	defer cancel()
	d, err := h.Request(context.Background(), ApprovalRequest{ActionClass: "expense", ToolName: "file_expense", Beneficiary: hubAlice})
	if err != nil || d.Approved || d.State != ApprovalNeedsHierarchy {
		t.Fatalf("relationship rule without hierarchy must fail closed: %+v %v", d, err)
	}
	if got := log.outcomes(); len(got) != 1 || got[0] != "needs_hierarchy" {
		t.Fatalf("needs_hierarchy must be published: %v", got)
	}
	// With a hierarchy installed, the supervisor — and only the supervisor — may answer.
	h.SetRelationships(staticRelations{"alice": {"supervisor": {"bob"}}})
	it, err := h.Raise(context.Background(), ApprovalRequest{ActionClass: "expense", ToolName: "file_expense2", Beneficiary: hubAlice})
	if err != nil || it.State != ApprovalPending {
		t.Fatalf("raise with hierarchy: %+v %v", it, err)
	}
	if ok, _ := h.Answerable(context.Background(), it, Approver{Principal: hubAlice}); ok {
		t.Fatal("a relationship rule must never fall back to self")
	}
	if ok, _ := h.Answerable(context.Background(), it, Approver{Principal: hubBob}); !ok {
		t.Fatal("the supervisor must be able to answer")
	}
}

type staticRelations map[string]map[string][]string

func (s staticRelations) Approvers(_ context.Context, b PrincipalRef, rel string) ([]string, error) {
	return s[b.ID][rel], nil
}

// One-time vs standing: a one-time approval covers that action only; a standing
// approval pre-approves the same class/resource/scope until revoked or expired.
func TestHub_OneTimeVsStandingRevokeAndExpiry(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := NewAddressedHub(AddressedHubConfig{ConsentTimeout: 2 * time.Second, Now: clock})
	ch, cancel := h.Watch(ApprovalWatch{For: Approver{Principal: hubAlice}})
	defer cancel()
	req := func(args string) ApprovalRequest {
		return ApprovalRequest{Kind: ApprovalKindMachineConsent, ToolName: "local:laptop/write_file", Machine: "laptop",
			ArgsPreview: args, Beneficiary: hubAlice}
	}
	// One-time.
	done := raiseAsync(context.Background(), h, req(`{"path":"a"}`))
	it := nextRaised(t, ch)
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: hubAlice}}); err != nil {
		t.Fatal(err)
	}
	if d := <-done; !d.Approved {
		t.Fatalf("one-time: %+v", d)
	}
	// The next call is asked again.
	done = raiseAsync(context.Background(), h, req(`{"path":"b"}`))
	it = nextRaised(t, ch)
	// This time: standing, for a week.
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, StandingTTL: 7 * 24 * time.Hour, By: Approver{Principal: hubAlice}}); err != nil {
		t.Fatal(err)
	}
	<-done
	// Pre-approved without asking.
	d, _ := h.Request(context.Background(), req(`{"path":"c"}`))
	if !d.Approved || !strings.HasPrefix(d.ApproverID, "standing:") {
		t.Fatalf("standing approval must pre-approve: %+v", d)
	}
	list, _ := h.ListStanding(context.Background(), StandingQuery{})
	if len(list) != 1 || list[0].UseCount != 1 || list[0].Scope != "person:alice" {
		t.Fatalf("standing list = %+v", list)
	}
	// Bob may not revoke Alice's grant; Alice may.
	if _, err := h.RevokeStanding(context.Background(), list[0].ID, Approver{Principal: hubBob}, "x"); err == nil {
		t.Fatal("a stranger revoked a standing approval")
	}
	if _, err := h.RevokeStanding(context.Background(), list[0].ID, Approver{Principal: hubAlice}, "done"); err != nil {
		t.Fatal(err)
	}
	if _, cancelled := context.WithTimeout(context.Background(), time.Second); cancelled != nil {
		cancelled()
	}
	done = raiseAsync(context.Background(), h, req(`{"path":"d"}`))
	it = nextRaised(t, ch) // asked again after revoke
	_, _ = h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, StandingTTL: time.Hour, By: Approver{Principal: hubAlice}})
	<-done
	// Expiry: two hours later the one-hour grant no longer applies.
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	done = raiseAsync(context.Background(), h, req(`{"path":"e"}`))
	it = nextRaised(t, ch)
	_, _ = h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: false, By: Approver{Principal: hubAlice}})
	if d := <-done; d.Approved {
		t.Fatal("an expired standing approval pre-approved a call")
	}
}

// A never-self-approvable class: the beneficiary cannot answer it, and a
// standing approval for it needs a non-self granter.
func TestHub_NeverSelfApprovable(t *testing.T) {
	cfg := DefaultApprovalPolicyConfig()
	for i := range cfg.Rules {
		if cfg.Rules[i].Class == "payment" {
			cfg.Rules[i] = ApprovalClassRule{Class: "payment", Approvers: []string{"self", "role:finance"}, AllowStanding: true}
		}
	}
	p, err := NewConfigApprovalPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAddressedHub(AddressedHubConfig{Policy: p})
	it, _ := h.Raise(context.Background(), ApprovalRequest{ActionClass: "payment", ToolName: "pay", Beneficiary: hubAlice})
	if it.RuleSummary() != "role:finance" {
		t.Fatalf("self must be dropped for a never-self class: %q", it.RuleSummary())
	}
	if ok, _ := h.Answerable(context.Background(), it, Approver{Principal: hubAlice}); ok {
		t.Fatal("the beneficiary may never approve a payment for themself")
	}
	// Alice also holds finance: still not for her own payment's STANDING grant.
	aliceFin := Approver{Principal: hubAlice, Roles: []string{"finance"}}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, By: aliceFin}); !errors.Is(err, ErrStandingNotAllowed) {
		t.Fatalf("a self-granted standing approval for a never-self class must be refused: %v", err)
	}
	fin := Approver{Principal: UserPrincipal("fiona"), Roles: []string{"finance"}}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, By: fin}); err != nil {
		t.Fatalf("a non-self standing grant must be accepted: %v", err)
	}
	d, _ := h.Request(context.Background(), ApprovalRequest{ActionClass: "payment", ToolName: "pay", Beneficiary: hubAlice, ArgsPreview: "2"})
	if !d.Approved {
		t.Fatalf("a non-self standing grant must pre-approve: %+v", d)
	}
}

// Durability: a pending approval survives a restart (a new hub over the same
// store); the new process expires it when its window passes and publishes it,
// and an answer recorded while nobody waited is handed to the retried request.
func TestHub_DurableAcrossRestart(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	store := NewMemoryApprovalStore()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	// Process 1 raises and dies before an answer.
	h1 := NewAddressedHub(AddressedHubConfig{Store: store, DefaultTimeout: 10 * time.Minute, Now: clock})
	ch1, cancel1 := h1.Watch(opWatch("op"))
	ctx1, die := context.WithCancel(context.Background())
	done := raiseAsync(ctx1, h1, ApprovalRequest{AgentID: "a", ToolName: "deploy", Timeout: 10 * time.Minute})
	first := nextRaised(t, ch1)
	// Simulate the crash: the waiter vanishes without resolving (a crash runs no
	// deferred code), so put the row back to pending after the cancel's withdraw.
	die()
	<-done
	cancel1()
	first.State = ApprovalPending
	_ = store.PutApproval(context.Background(), first)

	// Process 2: the item is listed as pending, answerable by an operator.
	h2 := NewAddressedHub(AddressedHubConfig{Store: store, DefaultTimeout: 10 * time.Minute, Now: clock})
	pending, _ := h2.List(context.Background(), ApprovalQuery{States: []ApprovalState{ApprovalPending}})
	if len(pending) != 1 || pending[0].ID != first.ID {
		t.Fatalf("pending after restart = %+v", pending)
	}
	if _, err := h2.Answer(context.Background(), first.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")}); err != nil {
		t.Fatalf("answer after restart: %v", err)
	}
	// The retried identical request takes the recorded decision instead of asking again.
	d, err := h2.Request(context.Background(), ApprovalRequest{AgentID: "a", ToolName: "deploy", Timeout: 10 * time.Minute})
	if err != nil || !d.Approved || d.ApprovalID != first.ID {
		t.Fatalf("retried request after restart: %+v %v", d, err)
	}
	// And it is consumed: a third identical request is a NEW round with a new id.
	_, cancel2 := h2.Watch(opWatch("op"))
	defer cancel2()
	done3 := raiseAsync(context.Background(), h2, ApprovalRequest{AgentID: "a", ToolName: "deploy", Timeout: 10 * time.Minute})
	var third ApprovalItem
	for i := 0; i < 100; i++ {
		items, _ := h2.List(context.Background(), ApprovalQuery{States: []ApprovalState{ApprovalPending}})
		if len(items) == 1 {
			third = items[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if third.ID == "" || third.ID == first.ID {
		t.Fatalf("third request reused the consumed id: %+v", third)
	}
	_, _ = h2.Answer(context.Background(), third.ID, ApprovalAnswer{Approved: false, By: OperatorApprover("op")})
	<-done3

	// A pending item orphaned by a crash expires on the next process's sweep.
	orphan := ApprovalItem{ID: "apr-orphan", Kind: "", ToolName: "x", State: ApprovalPending, NaturalKey: "k",
		RaisedAt: now, ExpiresAt: now.Add(time.Minute), Rules: []ApprovalRule{{Kind: ApprovalRuleOperator}}}
	_ = store.PutApproval(context.Background(), orphan)
	log, bus := newBusLog()
	h3 := NewAddressedHub(AddressedHubConfig{Store: store, Now: clock, Bus: bus, SweepInterval: time.Hour})
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	h3.Start(context.Background())
	h3.Stop()
	got, _ := store.GetApproval(context.Background(), "apr-orphan")
	if got.State != ApprovalExpired {
		t.Fatalf("orphan after recovery sweep = %s", got.State)
	}
	if outs := log.outcomes(); len(outs) != 1 || outs[0] != "expired" {
		t.Fatalf("recovery expiry must publish: %v", outs)
	}
}

// Async items (account confirmation, publish) wait durably with no connected
// approver, and their kind handler applies the effect when approved.
func TestHub_AsyncItemRunsHandlerOnApproval(t *testing.T) {
	h := NewAddressedHub(AddressedHubConfig{})
	var applied []string
	h.HandleKind(ApprovalKindAccountConfirm, func(_ context.Context, it ApprovalItem) error {
		applied = append(applied, it.Object)
		return nil
	})
	it, err := h.Raise(context.Background(), ApprovalRequest{Kind: ApprovalKindAccountConfirm, ToolName: "gmail", Object: "alice@example.com", Beneficiary: hubAlice})
	if err != nil || it.State != ApprovalPending {
		t.Fatalf("async raise with nobody connected must wait: %+v %v", it, err)
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: hubBob}}); err == nil {
		t.Fatal("bob confirmed alice's account")
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: hubAlice}}); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != "alice@example.com" {
		t.Fatalf("handler applied = %v", applied)
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: hubAlice}}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("a second answer must be refused as resolved: %v", err)
	}
}

func TestHub_ChooseMachineValidatesChoice(t *testing.T) {
	h := NewAddressedHub(AddressedHubConfig{})
	it, _ := h.Raise(context.Background(), ApprovalRequest{Kind: ApprovalKindChooseMachine, ToolName: "read_file",
		Candidates: []string{"a", "b"}, Beneficiary: hubAlice})
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Choice: "c", By: Approver{Principal: hubAlice}}); !errors.Is(err, ErrApprovalBadChoice) {
		t.Fatalf("a non-candidate choice must be refused: %v", err)
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, Choice: "a", By: Approver{Principal: hubAlice}}); !errors.Is(err, ErrStandingNotAllowed) {
		t.Fatalf("a machine choice cannot be standing: %v", err)
	}
}
