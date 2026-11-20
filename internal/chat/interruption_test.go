package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
)

// ENG-2124 (owner decision 2026-11-18): an interrupted or failed chat turn shows the person
// a plain, short sentence — no provider JSON, URLs, socket paths or RPC status text — and
// leaves an interruption marker in the conversation; the raw cause goes to the logs only.

// The readiness run's raw errors (ENG-1995 phase B block 7), as the kernel received them.
var (
	rawProvider401 = status.Error(codes.Internal, `INTERNAL: openai: HTTP 401: {"type": "authentication_error", "message": "OAuth token revoked", "code": "authentication_error"}`)
	rawProvider429 = status.Error(codes.Internal, `INTERNAL: openai: HTTP 429: {"type": "rate_limit_error", "message": "Rate limited", "code": "rate_limit_error"}`)
	rawProviderRst = status.Error(codes.Internal, `INTERNAL: openai: Post "http://127.0.0.1:3132/v1/chat/completions": read tcp 127.0.0.1:65370->127.0.0.1:3132: wsarecv: An existing connection was forcibly closed by the remote host.`)
	rawWorkerKill  = status.Error(codes.Unavailable, `error reading from server: read unix @->C:\Users\someone\AppData\Local\cambrian\agents\sock\1195e0a5\cambrian_chat_agent_1195e0a5.sock: wsarecv: An existing connection was forcibly closed by the remote host.`)
)

// leakMarkers are fragments of internal detail that must never reach the person.
var leakMarkers = []string{"http", "{", "}", ".sock", "unix", "rpc error", "code =", "internal:", "openai", "127.0.0.1", `\`, "wsarecv"}

func assertPlain(t *testing.T, what, text string) {
	t.Helper()
	if strings.TrimSpace(text) == "" {
		t.Fatalf("%s: empty — a failed turn must say something", what)
	}
	low := strings.ToLower(text)
	for _, bad := range leakMarkers {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Fatalf("%s: %q leaks %q", what, text, bad)
		}
	}
}

var allInterruptReasons = []domain.TurnInterruptReason{
	domain.TurnInterruptProviderAuth, domain.TurnInterruptProviderBusy, domain.TurnInterruptProviderUnavailable,
	domain.TurnInterruptNotConfigured,
	domain.TurnInterruptSpendLimit, domain.TurnInterruptContextTooLarge, domain.TurnInterruptAssistantRestarted,
	domain.TurnInterruptStalled, domain.TurnInterruptTimeout, domain.TurnInterruptCancelled,
	domain.TurnInterruptAtCapacity, domain.TurnInterruptSessionLost, domain.TurnInterruptEmptyReply,
	domain.TurnInterruptFailed,
	// ENG-1634: the loud ends of a killed turn the lane did not re-drive.
	domain.TurnInterruptAssistantRestartedAfterAction, domain.TurnInterruptAssistantRestartedTwice,
	// ENG-2269: a quarantined (crash-looping) worker stream.
	domain.TurnInterruptAssistantUnavailable,
}

func TestInterruptMessages_EveryReasonHasASentence(t *testing.T) {
	seen := map[MessageID]bool{}
	for _, r := range allInterruptReasons {
		id, ok := interruptMessages[r]
		if !ok {
			t.Errorf("reason %q has no sentence in interruptMessages", r)
			continue
		}
		seen[id] = true
		for lang := range messageCatalogue {
			assertPlain(t, fmt.Sprintf("%s/%s", r, lang), Message(id, lang))
		}
	}
	if len(interruptMessages) != len(allInterruptReasons) {
		t.Errorf("interruptMessages has %d entries, the reason list %d", len(interruptMessages), len(allInterruptReasons))
	}
}

func TestFailureReason_Table(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		fault domain.ProviderFault
		want  domain.TurnInterruptReason
	}{
		// What the kernel sees today: the worker reports Internal, the lease knows why.
		{"401 via lease", rawProvider401, domain.ProviderFaultAuth, domain.TurnInterruptProviderAuth},
		{"429 via lease", rawProvider429, domain.ProviderFaultRateLimited, domain.TurnInterruptProviderBusy},
		{"reset via lease", rawProviderRst, domain.ProviderFaultUnavailable, domain.TurnInterruptProviderUnavailable},
		{"spend via lease", rawProvider429, domain.ProviderFaultSpendLimit, domain.TurnInterruptSpendLimit},
		{"overflow via lease", rawProvider429, domain.ProviderFaultContextOverflow, domain.TurnInterruptContextTooLarge},
		// Once the SDK relays the gateway's code (ENG-2125), the code alone suffices.
		{"Unauthenticated relayed", status.Error(codes.Unauthenticated, "x"), "", domain.TurnInterruptProviderAuth},
		{"ResourceExhausted relayed", status.Error(codes.ResourceExhausted, "x"), "", domain.TurnInterruptProviderBusy},
		// A provider outage relayed as Unavailable is told apart from a dead worker by the lease.
		{"Unavailable relayed with fault", status.Error(codes.Unavailable, "x"), domain.ProviderFaultUnavailable, domain.TurnInterruptProviderUnavailable},
		// ENG-2156: no model allocated is answered Unavailable too; the lease fault says
		// it is a missing setting, not a dead worker or an outage.
		{"no model allocated relayed", status.Error(codes.Unavailable, "no model allocated for session s1 and no default model configured"), domain.ProviderFaultNotConfigured, domain.TurnInterruptNotConfigured},
		{"no model allocated via lease", rawProvider401, domain.ProviderFaultNotConfigured, domain.TurnInterruptNotConfigured},
		// ENG-2156: the gateway's other refusals, relayed by code.
		{"OutOfRange relayed", status.Error(codes.OutOfRange, "prompt too large"), "", domain.TurnInterruptContextTooLarge},
		{"PermissionDenied relayed", status.Error(codes.PermissionDenied, "lease is not valid for this caller"), "", domain.TurnInterruptSessionLost},
		{"no lease relayed (ENG-2171)", status.Error(codes.PermissionDenied, "a lease is required"), "", domain.TurnInterruptSessionLost},
		{"PermissionDenied without the word lease", status.Error(codes.PermissionDenied, "no agent identity is bound"), "", domain.TurnInterruptSessionLost},
		{"NotFound relayed", status.Error(codes.NotFound, "session not found: x"), "", domain.TurnInterruptSessionLost},
		// Before ENG-2125 the refusal arrives as Internal text; "lease" must be the word.
		{"lease refusal as Internal", status.Error(codes.Internal, "INTERNAL: lease is not valid for this caller"), "", domain.TurnInterruptSessionLost},
		{"please is not lease", status.Error(codes.Internal, "INTERNAL: please retry"), "", domain.TurnInterruptFailed},
		// The worker died mid-turn / was gone at dispatch.
		{"worker killed", rawWorkerKill, "", domain.TurnInterruptAssistantRestarted},
		{"worker lost", fmt.Errorf("%w: %v", agentpool.ErrWorkerLost, rawWorkerKill), "", domain.TurnInterruptAssistantRestarted},
		{"pool busy", agentpool.ErrPoolBusy, "", domain.TurnInterruptAtCapacity},
		// The kernel's own verdicts win over a stale lease fault.
		{"stalled", ErrTurnStalled, domain.ProviderFaultRateLimited, domain.TurnInterruptStalled},
		{"deadline", fmt.Errorf("chat: turn deadline: %w", context.DeadlineExceeded), domain.ProviderFaultAuth, domain.TurnInterruptTimeout},
		{"empty", ErrEmptyReply, "", domain.TurnInterruptEmptyReply},
		{"cancelled", context.Canceled, "", domain.TurnInterruptCancelled},
		{"unknown internal", rawProvider401, "", domain.TurnInterruptFailed},
	}
	for _, c := range cases {
		if got := failureReason(c.err, c.fault); got != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got, c.want)
		}
		ti := interruption(c.err, c.fault, "en")
		assertPlain(t, c.name, ti.Message)
		if !errors.Is(ti, c.err) {
			t.Errorf("%s: the raw error must stay reachable as the cause (for logs)", c.name)
		}
	}
}

// faultPool fails every turn with err after recording the lease's fault on the probe,
// the way the LLM gateway records it while the worker's model call fails.
type faultPool struct {
	fakePool
	fail  bool
	err   error
	fault domain.ProviderFault
	seen  map[domain.LeaseID]domain.ProviderFault
}

func (p *faultPool) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	if !p.fail {
		return p.fakePool.Dispatch(ctx, h)
	}
	p.fakePool.mu.Lock()
	p.fakePool.handoffs = append(p.fakePool.handoffs, h)
	p.fakePool.mu.Unlock()
	if tok := h.Context["_session_token_id"]; tok != "" && p.fault != "" {
		p.seen[domain.LeaseID(tok)] = p.fault
	}
	return nil, p.err
}

func setupFault(t *testing.T, err error, fault domain.ProviderFault) (*TurnService, *memStore, *faultPool) {
	t.Helper()
	store := newMemStore()
	if cerr := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); cerr != nil {
		t.Fatal(cerr)
	}
	pool := &faultPool{fakePool: fakePool{reply: "the answer"}, fail: true, err: err, fault: fault, seen: map[domain.LeaseID]domain.ProviderFault{}}
	n := 0
	svc := NewTurnService(store, pool, func(context.Context, int, time.Duration) (string, func(), error) {
		n++
		return fmt.Sprintf("lease-%d", n), func() {}, nil
	})
	svc.SetLeaseFaultProbe(func(l domain.LeaseID) domain.ProviderFault { return pool.seen[l] })
	return svc, store, pool
}

func TestTurn_FailureIsPlainAndLeavesAMarker(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		fault domain.ProviderFault
		want  domain.TurnInterruptReason
	}{
		{"provider auth", rawProvider401, domain.ProviderFaultAuth, domain.TurnInterruptProviderAuth},
		{"provider 429", rawProvider429, domain.ProviderFaultRateLimited, domain.TurnInterruptProviderBusy},
		{"worker killed", rawWorkerKill, "", domain.TurnInterruptAssistantRestarted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store, pool := setupFault(t, c.err, c.fault)
			_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "who owns SOP-007?", ClientID: "t1"})
			var ti *domain.TurnInterruption
			if !errors.As(err, &ti) || ti.Reason != c.want {
				t.Fatalf("Turn error = %v, want a TurnInterruption %q", err, c.want)
			}
			assertPlain(t, "returned message", ti.Message)

			msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
			if len(msgs) != 2 || msgs[0].Role != domain.MessageRoleUser || msgs[1].Role != domain.MessageRoleInterruption {
				t.Fatalf("transcript = %+v, want the user message then an interruption marker", msgs)
			}
			if msgs[1].Content != ti.Message {
				t.Fatalf("marker %q must say what the person was told (%q)", msgs[1].Content, ti.Message)
			}
			assertPlain(t, "marker", msgs[1].Content)

			// The retry (same command id) runs the turn again rather than returning the
			// marker as if it were a reply, and stores no second marker.
			pool.fail = false
			got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "who owns SOP-007?", ClientID: "t1"})
			if err != nil || got.Content != "the answer" {
				t.Fatalf("retry = %+v, %v; want the reply", got, err)
			}
			// The next turn's model context does not carry the marker.
			if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "and SOP-008?"}); err != nil {
				t.Fatal(err)
			}
			hs := pool.all()
			if tr := hs[len(hs)-1].Payload.Metadata["transcript"]; strings.Contains(tr, ti.Message) || !strings.Contains(tr, "the answer") {
				t.Fatalf("transcript sent to the model = %q; the marker must be absent, the reply present", tr)
			}
			msgs, _ = store.ListMessages(context.Background(), "c1", 0, 0)
			markers := 0
			for _, m := range msgs {
				if m.Role == domain.MessageRoleInterruption {
					markers++
				}
			}
			if markers != 1 {
				t.Fatalf("%d interruption markers, want 1: %+v", markers, msgs)
			}
		})
	}
}

// A Turkish conversation is told in Turkish, by the catalogue.
func TestTurn_FailureSpeaksTheConversationLanguage(t *testing.T) {
	svc, store, _ := setupFault(t, rawProvider429, domain.ProviderFaultRateLimited)
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "SOP-007 belgesinin sahibi kim, lütfen söyler misiniz?"})
	if got := humanFailure(err, "en"); got != messageCatalogue["tr"][MsgProviderBusy] {
		t.Fatalf("message = %q, want the Turkish busy sentence", got)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if msgs[len(msgs)-1].Content != messageCatalogue["tr"][MsgProviderBusy] {
		t.Fatalf("marker = %q", msgs[len(msgs)-1].Content)
	}
}

// A kernel stop leaves the user's message with no reply; the boot sweep marks it once.
func TestMarkInterruptedTurns_MarksTurnsTheLastStopCut(t *testing.T) {
	svc, store, _ := setup(t, domain.ProfileEmployee)
	ctx := context.Background()
	if err := store.CreateConversation(ctx, domain.Conversation{ID: "c2", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee}); err != nil {
		t.Fatal(err)
	}
	// c1: answered. c2: cut off mid-turn.
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessage(ctx, domain.Message{ID: "u-cut", ConversationID: "c2", Role: domain.MessageRoleUser, Content: "who owns SOP-007?"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // twice: the second boot finds nothing new to mark
		n, err := svc.MarkInterruptedTurns(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if want := 1 - i; n != want {
			t.Fatalf("sweep %d marked %d, want %d", i, n, want)
		}
	}
	msgs, _ := store.ListMessages(ctx, "c2", 0, 0)
	if len(msgs) != 2 || msgs[1].Role != domain.MessageRoleInterruption || msgs[1].Content != messageCatalogue["en"][MsgAssistantRestarted] {
		t.Fatalf("c2 = %+v, want the user message then the restarted marker", msgs)
	}
	if c1, _ := store.ListMessages(ctx, "c1", 0, 0); len(c1) != 2 || c1[1].Role != domain.MessageRoleAgent {
		t.Fatalf("an answered turn must not be marked: %+v", c1)
	}
}

// ENG-2146 (owner decision 2026-11-20, option B): at boot, a turn the last stop cut off is
// also told to the person through the conversation's ingress when its message arrived
// within RestartNoticeWindow before the boot; older ones are marked in the transcript only;
// a console conversation (no ingress) is not a failure; off (window 0) delivers nothing.
func TestMarkInterruptedTurns_DeliversOnlyRecentCutOffsOverTheIngress(t *testing.T) {
	ctx := context.Background()
	bootAt := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	seed := func(t *testing.T) *memStore {
		t.Helper()
		store := newMemStore()
		for id, age := range map[string]time.Duration{"recent": 2 * time.Minute, "stale": 3 * time.Hour} {
			if err := store.CreateConversation(ctx, domain.Conversation{ID: "conv-" + id, OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileCustomer}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AppendMessage(ctx, domain.Message{ID: "u-" + id, ConversationID: "conv-" + id,
				Role: domain.MessageRoleUser, Content: "where is my order?", CreatedAt: bootAt.Add(-age)}); err != nil {
				t.Fatal(err)
			}
		}
		return store
	}

	t.Run("window", func(t *testing.T) {
		store := seed(t)
		svc := NewTurnService(store, &fakePool{}, nil)
		d := &recordingDeliverer{}
		svc.SetDeliverer(d)
		svc.RestartNoticeWindow = 10 * time.Minute
		n, err := svc.MarkInterruptedTurns(ctx, bootAt)
		if err != nil || n != 2 {
			t.Fatalf("marked %d (%v), want both cut-off turns marked", n, err)
		}
		if d.count() != 1 {
			t.Fatalf("delivered %d notices, want only the recent one: %+v", d.count(), d.sent)
		}
		got := d.sent[0]
		if got.conv != "conv-recent" || got.txn != interruptionMarkerKey("u-recent") || got.text != messageCatalogue["en"][MsgAssistantRestarted] {
			t.Fatalf("delivery = %+v", got)
		}
		// The stale turn is still marked in its transcript.
		if msgs, _ := store.ListMessages(ctx, "conv-stale", 0, 0); len(msgs) != 2 || msgs[1].Role != domain.MessageRoleInterruption {
			t.Fatalf("stale conversation = %+v, want the transcript marker", msgs)
		}
		// A second boot finds nothing unanswered and sends nothing again.
		if n, _ := svc.MarkInterruptedTurns(ctx, bootAt); n != 0 || d.count() != 1 {
			t.Fatalf("second sweep: marked %d, delivered %d", n, d.count())
		}
	})
	t.Run("off", func(t *testing.T) {
		svc := NewTurnService(seed(t), &fakePool{}, nil)
		d := &recordingDeliverer{}
		svc.SetDeliverer(d)
		if n, _ := svc.MarkInterruptedTurns(ctx, bootAt); n != 2 || d.count() != 0 {
			t.Fatalf("window off: marked %d, delivered %d; want 2 marked, none delivered", n, d.count())
		}
	})
	t.Run("console conversation", func(t *testing.T) {
		svc := NewTurnService(seed(t), &fakePool{}, nil)
		svc.SetDeliverer(&recordingDeliverer{err: domain.ErrNoDeliveryAddress})
		svc.RestartNoticeWindow = 10 * time.Minute
		if n, err := svc.MarkInterruptedTurns(ctx, bootAt); err != nil || n != 2 {
			t.Fatalf("no ingress: marked %d (%v)", n, err)
		}
	})
}
