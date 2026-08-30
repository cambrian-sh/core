package ingress

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── fakes ───────────────────────────────────────────────────────────────────

// fakePoster stands in for chat.TurnService.Emit and captures what the bridge
// says into which conversation.
type fakePoster struct {
	mu    sync.Mutex
	posts []struct{ conv, text string }
}

func (f *fakePoster) Emit(_ context.Context, conv, text string) (domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, struct{ conv, text string }{conv, text})
	return domain.Message{ID: "msg", ConversationID: conv, Content: text}, nil
}

func (f *fakePoster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakePoster) at(i int) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts[i].conv, f.posts[i].text
}

func (f *fakePoster) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.posts))
	for _, p := range f.posts {
		out = append(out, p.conv+": "+p.text)
	}
	return out
}

func waitForPosts(t *testing.T, fp *fakePoster, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fp.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d posts; have %d: %v", n, fp.count(), fp.all())
}

type reqResult struct {
	ans     domain.ConsentAnswer
	outcome domain.ConsentOutcome
	err     error
}

func requestAsync(hub domain.WorkerConsentHub, p domain.ConsentPrompt) <-chan reqResult {
	ch := make(chan reqResult, 1)
	go func() {
		ans, out, err := hub.Request(context.Background(), p)
		ch <- reqResult{ans, out, err}
	}()
	return ch
}

func awaitResult(t *testing.T, ch <-chan reqResult) reqResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("consent Request did not resolve")
		return reqResult{}
	}
}

func bridgeFixture(t *testing.T, grace, window time.Duration) (*ConsentBridge, *domain.InMemoryConsentController, *fakePoster) {
	t.Helper()
	hub := domain.NewInMemoryConsentController(grace)
	fp := &fakePoster{}
	b := NewConsentBridge(hub, fp, window)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.Start(ctx)
	t.Cleanup(b.Stop)
	return b, hub, fp
}

var fleetOwner = domain.AgentPrincipal("afsin")

func approvePrompt(conv string) domain.ConsentPrompt {
	return domain.ConsentPrompt{
		Kind:           domain.ConsentPromptApprove,
		Machine:        "laptop",
		Tool:           "local:laptop/write_file",
		Object:         "C:/notes/todo.md",
		ArgsJSON:       `{"path":"C:/notes/todo.md"}`,
		TaskID:         "task-1",
		Beneficiary:    fleetOwner,
		ConversationID: conv,
	}
}

// ── tests ───────────────────────────────────────────────────────────────────

// An effectful contributed step's prompt reaches the ORDERING conversation as
// plain language naming the tool, the machine and the exact object.
func TestConsentBridge_PromptPostsIntoTheOrderingConversation(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	res := requestAsync(hub, approvePrompt("conv-c"))
	waitForPosts(t, fp, 1)

	conv, text := fp.at(0)
	if conv != "conv-c" {
		t.Fatalf("prompt posted into the wrong conversation: %q", conv)
	}
	for _, want := range []string{"local:laptop/write_file", "laptop", "C:/notes/todo.md", "'approve'", "'deny'", "Expires in"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt text missing %q: %q", want, text)
		}
	}

	// Resolve so the goroutine does not outlive the test.
	if !b.HandleReply(context.Background(), "conv-c", "deny", fleetOwner) {
		t.Fatal("the beneficiary's 'deny' must be consumed")
	}
	awaitResult(t, res)
}

// The beneficiary's 'approve' resolves the step end-to-end: the controller
// returns an approved answer naming the approver, and the conversation gets a
// confirmation.
func TestConsentBridge_BeneficiaryApproveResolvesTheStep(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	res := requestAsync(hub, approvePrompt("conv-a"))
	waitForPosts(t, fp, 1)

	if !b.HandleReply(context.Background(), "conv-a", "Approve", fleetOwner) {
		t.Fatal("the beneficiary's 'approve' must be consumed as the answer")
	}
	r := awaitResult(t, res)
	if r.err != nil || r.outcome != domain.ConsentAnswered || !r.ans.Approved {
		t.Fatalf("expected an approved answer, got %+v", r)
	}
	if r.ans.AnsweredBy != fleetOwner.String() {
		t.Errorf("the decision record must name the approver: %q", r.ans.AnsweredBy)
	}
	waitForPosts(t, fp, 2)
	if _, text := fp.at(1); text != "Approved — running on laptop." {
		t.Errorf("confirmation not posted: %q", text)
	}
}

// 'deny' resolves the prompt as a refusal — answered, not approved — and the
// conversation is told.
func TestConsentBridge_BeneficiaryDenyRefuses(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	res := requestAsync(hub, approvePrompt("conv-d"))
	waitForPosts(t, fp, 1)

	if !b.HandleReply(context.Background(), "conv-d", "no", fleetOwner) {
		t.Fatal("the beneficiary's 'no' must be consumed as the answer")
	}
	r := awaitResult(t, res)
	if r.outcome != domain.ConsentAnswered || r.ans.Approved {
		t.Fatalf("expected an answered denial, got %+v", r)
	}
	waitForPosts(t, fp, 2)
	if _, text := fp.at(1); text != "Denied." {
		t.Errorf("denial confirmation not posted: %q", text)
	}
}

// THE security invariant: nobody but the beneficiary can answer. A different
// bound principal and an unbound sender are both ignored — not consumed, prompt
// still pending — and the beneficiary can still answer afterwards.
func TestConsentBridge_OnlyTheBeneficiaryCanAnswer(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	res := requestAsync(hub, approvePrompt("conv-s"))
	waitForPosts(t, fp, 1)

	if b.HandleReply(context.Background(), "conv-s", "approve", domain.AgentPrincipal("mallory")) {
		t.Fatal("a different principal must never answer another owner's prompt")
	}
	if b.HandleReply(context.Background(), "conv-s", "approve", domain.PrincipalRef{}) {
		t.Fatal("an unbound sender must never answer a prompt")
	}
	// Still pending: the beneficiary's answer lands.
	if !b.HandleReply(context.Background(), "conv-s", "approve", fleetOwner) {
		t.Fatal("the prompt must still be pending for the beneficiary")
	}
	r := awaitResult(t, res)
	if r.outcome != domain.ConsentAnswered || !r.ans.Approved {
		t.Fatalf("expected the beneficiary's approval, got %+v", r)
	}
}

// A zero beneficiary can never be matched by a zero sender: two nothings must
// not add up to an approval.
func TestConsentBridge_ZeroBeneficiaryNeverMatches(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 200*time.Millisecond, 3*time.Second)

	p := approvePrompt("conv-z")
	p.Beneficiary = domain.PrincipalRef{}
	res := requestAsync(hub, p)
	waitForPosts(t, fp, 1)

	if b.HandleReply(context.Background(), "conv-z", "approve", domain.PrincipalRef{}) {
		t.Fatal("a zero sender must not match a zero beneficiary")
	}
	if r := awaitResult(t, res); r.outcome != domain.ConsentTimedOut {
		t.Fatalf("expected the fail-closed timeout, got %+v", r)
	}
}

// choose_machine: a reply naming a live candidate (any casing) is honored; a
// non-candidate reply is not consumed and the prompt stays pending.
func TestConsentBridge_ChooseMachineHonorsCandidatesOnly(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	p := domain.ConsentPrompt{
		Kind:           domain.ConsentPromptChooseMachine,
		Candidates:     []string{"alpha", "beta"},
		Tool:           "write_file",
		Beneficiary:    fleetOwner,
		ConversationID: "conv-m",
	}
	res := requestAsync(hub, p)
	waitForPosts(t, fp, 1)

	if _, text := fp.at(0); !strings.Contains(text, "Which machine should run write_file?") ||
		!strings.Contains(text, "alpha, beta") {
		t.Errorf("choose_machine prompt malformed: %q", text)
	}
	if b.HandleReply(context.Background(), "conv-m", "gamma", fleetOwner) {
		t.Fatal("a non-candidate machine name must not be consumed")
	}
	if !b.HandleReply(context.Background(), "conv-m", "BETA", fleetOwner) {
		t.Fatal("a candidate reply must be consumed, case-insensitively")
	}
	r := awaitResult(t, res)
	if r.outcome != domain.ConsentAnswered || !r.ans.Approved || r.ans.Machine != "beta" {
		t.Fatalf("expected the canonical candidate name, got %+v", r)
	}
	waitForPosts(t, fp, 2)
	if _, text := fp.at(1); text != "Approved — running on beta." {
		t.Errorf("confirmation not posted: %q", text)
	}
}

// A prompt with NO ordering conversation is never guessed into a transcript:
// the bridge posts nothing and the controller's fail-closed timeout refuses.
func TestConsentBridge_NoConversationIsLeftToTimeOut(t *testing.T) {
	_, hub, fp := bridgeFixture(t, 150*time.Millisecond, 3*time.Second)

	r := awaitResult(t, requestAsync(hub, approvePrompt("")))
	if r.outcome != domain.ConsentTimedOut || r.ans.Approved {
		t.Fatalf("an unattributed prompt must time out refused, got %+v", r)
	}
	if fp.count() != 0 {
		t.Errorf("nothing may be posted for an unattributed prompt: %v", fp.all())
	}
}

// Malformed and unrelated replies never approve: a sentence containing the
// word is not the word.
func TestConsentBridge_MalformedRepliesNeverApprove(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	res := requestAsync(hub, approvePrompt("conv-f"))
	waitForPosts(t, fp, 1)

	for _, reply := range []string{"approve it", "sure", "ok", "APPROVE!!", "", "  ", "yes please", "which machine?"} {
		if b.HandleReply(context.Background(), "conv-f", reply, fleetOwner) {
			t.Fatalf("reply %q must not be consumed as an answer", reply)
		}
	}
	// The prompt survived all of it.
	if !b.HandleReply(context.Background(), "conv-f", "deny", fleetOwner) {
		t.Fatal("the prompt must still be pending after non-answers")
	}
	if r := awaitResult(t, res); r.ans.Approved {
		t.Fatalf("nothing here may approve: %+v", r)
	}
}

// One prompt per conversation at a time: a second prompt queues and posts only
// after the first resolves, so two questions never interleave their answers.
func TestConsentBridge_SecondPromptQueuesUntilTheFirstResolves(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 5*time.Second, 5*time.Second)

	resA := requestAsync(hub, approvePrompt("conv-q"))
	waitForPosts(t, fp, 1)

	pB := approvePrompt("conv-q")
	pB.Tool = "local:laptop/delete_file"
	resB := requestAsync(hub, pB)
	time.Sleep(100 * time.Millisecond)
	if fp.count() != 1 {
		t.Fatalf("the second prompt must wait its turn: %v", fp.all())
	}

	if !b.HandleReply(context.Background(), "conv-q", "approve", fleetOwner) {
		t.Fatal("answering the first prompt failed")
	}
	awaitResult(t, resA)
	waitForPosts(t, fp, 3) // confirmation, then prompt B
	if _, text := fp.at(2); !strings.Contains(text, "local:laptop/delete_file") {
		t.Fatalf("the queued prompt must post after the first resolves: %q", text)
	}
	if !b.HandleReply(context.Background(), "conv-q", "deny", fleetOwner) {
		t.Fatal("answering the second prompt failed")
	}
	if r := awaitResult(t, resB); r.ans.Approved {
		t.Fatalf("the second prompt was denied, got %+v", r)
	}
}

// A locally expired prompt stops blocking its conversation: the next queued
// prompt is promoted and posted.
func TestConsentBridge_ExpiryUnblocksTheQueue(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 5*time.Second, 5*time.Second)

	resA := requestAsync(hub, approvePrompt("conv-e"))
	waitForPosts(t, fp, 1)
	pB := approvePrompt("conv-e")
	pB.Tool = "local:laptop/delete_file"
	resB := requestAsync(hub, pB)

	// Deterministic promotion: expire A through the timer's own path.
	b.expire("conv-e", "consent-1")
	waitForPosts(t, fp, 2)
	if _, text := fp.at(1); !strings.Contains(text, "local:laptop/delete_file") {
		t.Fatalf("the queued prompt must be promoted on expiry: %q", text)
	}
	// A's reply is now nothing: the slot belongs to B.
	if !b.HandleReply(context.Background(), "conv-e", "deny", fleetOwner) {
		t.Fatal("prompt B must be answerable after A expired")
	}
	if r := awaitResult(t, resB); r.ans.Approved {
		t.Fatalf("B was denied, got %+v", r)
	}
	// A itself resolves through the controller (still pending there until its
	// own window ends; submit its id to release the goroutine promptly).
	hub.Submit("consent-1", domain.ConsentAnswer{})
	awaitResult(t, resA)
}

// After the bridge's local window passes, a late 'approve' is NOT consumed —
// it flows to chat, and nothing is approved (the controller already refused).
func TestConsentBridge_LateReplyIsNotConsumed(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 100*time.Millisecond, 100*time.Millisecond)

	res := requestAsync(hub, approvePrompt("conv-l"))
	waitForPosts(t, fp, 1)
	if r := awaitResult(t, res); r.outcome != domain.ConsentTimedOut {
		t.Fatalf("expected the timeout, got %+v", r)
	}
	time.Sleep(150 * time.Millisecond) // let the bridge's own timer clear the slot
	if b.HandleReply(context.Background(), "conv-l", "approve", fleetOwner) {
		t.Fatal("a reply after expiry must flow to chat, not be consumed")
	}
}

// A notice (parking etc.) posts as a plain message and expects no reply.
func TestConsentBridge_NoticePostsPlainMessage(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)

	hub.Notify(context.Background(), domain.ConsentPrompt{
		ConversationID: "conv-n",
		Notice:         "machine \"laptop\" is offline — queued until 18:00.",
	})
	waitForPosts(t, fp, 1)
	if conv, text := fp.at(0); conv != "conv-n" || !strings.Contains(text, "queued until") {
		t.Errorf("notice not posted verbatim: %q %q", conv, text)
	}
	// No reply is expected: nothing is pending.
	if b.HandleReply(context.Background(), "conv-n", "approve", fleetOwner) {
		t.Fatal("a notice must not leave an answerable prompt behind")
	}
}

// ── the inbound interception, end to end ────────────────────────────────────

// The beneficiary's 'approve' arriving THROUGH the chat inbound path is
// consumed as the answer: no chat turn runs, the step dispatches, and the
// confirmation lands in the conversation.
func TestAccept_BeneficiaryReplyIsConsumedAsTheAnswer(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
		Surface: "chat:telegram", ExternalID: "tg:12345",
		BoundToKind: domain.BindPrincipal, BoundToID: "afsin",
	}})
	s.SetConsentGate(b)

	res := requestAsync(hub, approvePrompt("conv-fixed"))
	waitForPosts(t, fp, 1)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "approve",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 0 {
		t.Fatalf("a consumed answer must not start a chat turn: %+v", turns.ran)
	}
	r := awaitResult(t, res)
	if r.outcome != domain.ConsentAnswered || !r.ans.Approved {
		t.Fatalf("expected the approval to reach the controller, got %+v", r)
	}
	waitForPosts(t, fp, 2)
	if _, text := fp.at(1); text != "Approved — running on laptop." {
		t.Errorf("confirmation not posted: %q", text)
	}
}

// A DIFFERENT bound principal's reply in the same conversation is an ordinary
// chat message: the turn runs, the prompt stays pending, nothing is approved.
func TestAccept_ForeignPrincipalReplyStillReachesChat(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
		Surface: "chat:telegram", ExternalID: "tg:12345",
		BoundToKind: domain.BindPrincipal, BoundToID: "mallory",
	}})
	s.SetConsentGate(b)

	res := requestAsync(hub, approvePrompt("conv-fixed"))
	waitForPosts(t, fp, 1)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "approve",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 || turns.ran[0].text != "approve" {
		t.Fatalf("a foreign principal's message must flow to chat: %+v", turns.ran)
	}
	// Still pending: only the beneficiary resolves it.
	if !b.HandleReply(context.Background(), "conv-fixed", "deny", fleetOwner) {
		t.Fatal("the prompt must still be pending after the foreign reply")
	}
	if r := awaitResult(t, res); r.ans.Approved {
		t.Fatalf("nothing here may approve: %+v", r)
	}
	_ = fp
}

// An UNBOUND sender's reply is likewise an ordinary chat message.
func TestAccept_UnboundSenderReplyStillReachesChat(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: false, mode: domain.StrangerSurfaceDefault})
	s.SetConsentGate(b)

	res := requestAsync(hub, approvePrompt("conv-fixed"))
	waitForPosts(t, fp, 1)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "approve",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 {
		t.Fatalf("an unbound sender's message must flow to chat: %+v", turns.ran)
	}
	if !b.HandleReply(context.Background(), "conv-fixed", "deny", fleetOwner) {
		t.Fatal("the prompt must still be pending after the unbound reply")
	}
	awaitResult(t, res)
}

// A non-matching message from the BENEFICIARY is a chat message too — asking a
// question about the prompt must not be eaten by it.
func TestAccept_BeneficiaryNonAnswerStillReachesChat(t *testing.T) {
	b, hub, fp := bridgeFixture(t, 3*time.Second, 3*time.Second)
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
		Surface: "chat:telegram", ExternalID: "tg:12345",
		BoundToKind: domain.BindPrincipal, BoundToID: "afsin",
	}})
	s.SetConsentGate(b)

	res := requestAsync(hub, approvePrompt("conv-fixed"))
	waitForPosts(t, fp, 1)

	if err := s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "what file is that exactly?",
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 {
		t.Fatalf("a non-answer must run as a turn: %+v", turns.ran)
	}
	if !b.HandleReply(context.Background(), "conv-fixed", "approve", fleetOwner) {
		t.Fatal("the prompt must still be pending")
	}
	if r := awaitResult(t, res); !r.ans.Approved {
		t.Fatalf("the later real answer must land, got %+v", r)
	}
}
