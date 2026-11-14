package domain

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type hubCtxKey struct{}

// ctxRecordingPolicy records the hubCtxKey value of every Route call.
type ctxRecordingPolicy struct {
	base ApprovalPolicy
	mu   sync.Mutex
	seen []any
}

func (p *ctxRecordingPolicy) Route(ctx context.Context, it ApprovalItem) (ApprovalRoute, error) {
	p.mu.Lock()
	p.seen = append(p.seen, ctx.Value(hubCtxKey{}))
	p.mu.Unlock()
	return p.base.Route(ctx, it)
}

// ENG-1467: the standing-approval route is computed under the answerer's ctx,
// not context.Background(), so a decorating policy sees the request's values.
func TestHub_StandingRouteUsesAnswerContext(t *testing.T) {
	base, err := NewConfigApprovalPolicy(DefaultApprovalPolicyConfig())
	if err != nil {
		t.Fatal(err)
	}
	pol := &ctxRecordingPolicy{base: base}
	h := NewAddressedHub(AddressedHubConfig{Policy: pol, ConsentTimeout: 2 * time.Second})
	ch, cancel := h.Watch(ApprovalWatch{For: Approver{Principal: hubAlice}})
	defer cancel()
	done := raiseAsync(context.Background(), h, ApprovalRequest{Kind: ApprovalKindMachineConsent,
		ToolName: "local:laptop/write_file", Machine: "laptop", ArgsPreview: `{"path":"a"}`, Beneficiary: hubAlice})
	it := nextRaised(t, ch)
	ctx := context.WithValue(context.Background(), hubCtxKey{}, "answer")
	if _, err := h.Answer(ctx, it.ID, ApprovalAnswer{Approved: true, Standing: true, StandingTTL: time.Hour, By: Approver{Principal: hubAlice}}); err != nil {
		t.Fatal(err)
	}
	<-done
	pol.mu.Lock()
	defer pol.mu.Unlock()
	if len(pol.seen) == 0 || pol.seen[len(pol.seen)-1] != "answer" {
		t.Fatalf("standing route ctx values = %v, want the answer ctx last", pol.seen)
	}
}

// failingPutStore fails every PutApproval after the first n.
type failingPutStore struct {
	*MemoryApprovalStore
	mu   sync.Mutex
	left int
}

func (s *failingPutStore) PutApproval(ctx context.Context, it ApprovalItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.left <= 0 {
		return errors.New("disk full")
	}
	s.left--
	return s.MemoryApprovalStore.PutApproval(ctx, it)
}

// ENG-1472: when an approved async item's effect fails and its explanatory
// Detail cannot be persisted, the loss is logged, not discarded.
func TestHub_FailedEffectDetailWriteIsLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	store := &failingPutStore{MemoryApprovalStore: NewMemoryApprovalStore(), left: 1}
	h := NewAddressedHub(AddressedHubConfig{Store: store, Logger: logger})
	h.HandleKind(ApprovalKindAccountConfirm, func(context.Context, ApprovalItem) error {
		return errors.New("provider refused")
	})
	it, err := h.Raise(context.Background(), ApprovalRequest{Kind: ApprovalKindAccountConfirm, ToolName: "gmail",
		Object: "alice@example.com", Beneficiary: hubAlice})
	if err != nil || it.State != ApprovalPending {
		t.Fatalf("raise: %+v %v", it, err)
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: Approver{Principal: hubAlice}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "approval_detail_lost") {
		t.Fatalf("lost failed-effect detail was not logged:\n%s", buf.String())
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ctxRecordingStore records the hubCtxKey value of every GetApproval (the read
// Answer starts with).
type ctxRecordingStore struct {
	*MemoryApprovalStore
	mu   sync.Mutex
	seen []any
}

func (s *ctxRecordingStore) GetApproval(ctx context.Context, id string) (ApprovalItem, error) {
	s.mu.Lock()
	s.seen = append(s.seen, ctx.Value(hubCtxKey{}))
	s.mu.Unlock()
	return s.MemoryApprovalStore.GetApproval(ctx, id)
}

// ENG-1467 item 3: a consent answer submitted through the WorkerConsentHub view
// is recorded under the answerer's ctx (it used to answer on context.Background()).
func TestHub_ConsentSubmitUsesTheAnswerersContext(t *testing.T) {
	store := &ctxRecordingStore{MemoryApprovalStore: NewMemoryApprovalStore()}
	h := NewAddressedHub(AddressedHubConfig{Store: store, ConsentTimeout: 2 * time.Second})
	ch, cancel := h.Watch(ApprovalWatch{For: Approver{Principal: hubAlice}})
	defer cancel()
	done := raiseAsync(context.Background(), h, ApprovalRequest{Kind: ApprovalKindMachineConsent,
		ToolName: "local:laptop/write_file", Machine: "laptop", ArgsPreview: `{"path":"a"}`, Beneficiary: hubAlice})
	it := nextRaised(t, ch)
	ctx := context.WithValue(context.Background(), hubCtxKey{}, "answerer")
	if !h.Consent().Submit(ctx, it.ID, ConsentAnswer{Approved: true, By: Approver{Principal: hubAlice}}) {
		t.Fatal("consent answer refused")
	}
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, v := range store.seen {
		if v == "answerer" {
			return
		}
	}
	t.Fatalf("no approval read ran under the answerer's ctx: %v", store.seen)
}
