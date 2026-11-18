package forget_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// rawEraser stands in for postgres.PgErasureStore under the REAL enforcing eraser, so
// these tests exercise the two-phase rule over the actual chokepoint.
type rawEraser struct {
	records []domain.ErasureCandidate
	erased  []domain.ErasureRequest
}

func (f *rawEraser) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, r := range f.records {
		switch {
		case slices.Contains(sel.DocumentIDs, r.ID): // named directly: Via is the id itself
			r.Via = r.ID
			out = append(out, r)
		case r.Via != r.ID && slices.Contains(sel.ThreadIDs, r.Via):
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *rawEraser) Erase(_ context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	for _, prev := range f.erased {
		if prev.ID() == req.ID() {
			return domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, Replayed: true}, nil
		}
	}
	f.erased = append(f.erased, req)
	// One tombstone per named id, as the Postgres store records (the tool reports
	// erased only from a receipt with targets, ENG-2106).
	return domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, PrincipalKind: req.PrincipalKind, Reason: req.Reason,
		Targets: req.Selector.Count()}, nil
}

func (f *rawEraser) Receipt(context.Context, string) (domain.ErasureReceipt, bool, error) {
	return domain.ErasureReceipt{}, false, nil
}

func (f *rawEraser) ListReceipts(context.Context, domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	return nil, nil
}

func doc(id, via, writer string) domain.ErasureCandidate {
	return domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: id, Via: via, DocumentID: id, WrittenBy: writer}
}

func agent(id string) context.Context {
	return domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal(id)), &domain.TagPredicate{})
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(raw *rawEraser, c *clock) *forget.Service {
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	return forget.New(e, forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "fp", Now: c.now})
}

// Preview returns the concrete records and a deterministic token; Forget with the token
// erases exactly them — a document that joins the thread after the preview is not erased.
func TestForget_PreviewThenForgetExecutesOnlyPreviewedIDs(t *testing.T) {
	raw := &rawEraser{records: []domain.ErasureCandidate{doc("d1", "th", "alice")}}
	c := &clock{t: time.Unix(1000, 0)}
	s := newService(raw, c)
	ctx := agent("alice")

	p, err := s.Preview(ctx, domain.ErasureSelector{ThreadIDs: []string{"th"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || !slices.Equal(p.Selector.DocumentIDs, []string{"d1"}) || p.Token == "" {
		t.Fatalf("preview = %+v", p)
	}
	again, err := s.Preview(ctx, domain.ErasureSelector{ThreadIDs: []string{"th"}})
	if err != nil || again.Token != p.Token {
		t.Fatalf("preview token not deterministic: %q vs %q (%v)", again.Token, p.Token, err)
	}

	raw.records = append(raw.records, doc("d2", "th", "alice")) // arrives after the preview
	rec, err := s.Forget(ctx, forget.Request{PreviewToken: p.Token})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.erased) != 1 || !slices.Equal(raw.erased[0].Selector.DocumentIDs, []string{"d1"}) || len(raw.erased[0].Selector.ThreadIDs) != 0 {
		t.Fatalf("erased = %+v, want exactly the previewed d1", raw.erased)
	}
	if rec.Principal != "alice" || rec.Reason != domain.ErasureReasonUserRequest {
		t.Fatalf("receipt = %+v", rec)
	}
	// A retry with the same token is the same act: the same request, replayed.
	rec2, err := s.Forget(ctx, forget.Request{PreviewToken: p.Token})
	if err != nil || !rec2.Replayed || rec2.RequestID != rec.RequestID {
		t.Fatalf("retry = %+v, %v; want the same request replayed", rec2, err)
	}
}

// A token is the previewing caller's only, and it expires.
func TestForget_TokenIsTheCallersAndExpires(t *testing.T) {
	raw := &rawEraser{records: []domain.ErasureCandidate{doc("d1", "d1", "alice")}}
	c := &clock{t: time.Unix(1000, 0)}
	s := newService(raw, c)
	p, err := s.Preview(agent("alice"), domain.ErasureSelector{DocumentIDs: []string{"d1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(agent("bob"), forget.Request{PreviewToken: p.Token}); !errors.Is(err, domain.ErrPreviewUnknown) {
		t.Fatalf("another caller's token: err = %v, want ErrPreviewUnknown", err)
	}
	c.t = c.t.Add(2 * time.Minute)
	if _, err := s.Forget(agent("alice"), forget.Request{PreviewToken: p.Token}); !errors.Is(err, domain.ErrPreviewUnknown) {
		t.Fatalf("expired token: err = %v, want ErrPreviewUnknown", err)
	}
	if len(raw.erased) != 0 {
		t.Fatalf("nothing may be erased: %+v", raw.erased)
	}
	if !forget.IsCallerError(domain.ErrPreviewUnknown) {
		t.Fatal("an unknown preview is the caller's error")
	}
}

// Explicit ids still go through the chokepoint: denied stays denied, nothing visible is
// NothingToErase, a token plus ids is malformed, and a nil eraser answers disabled.
func TestForget_ExplicitIDsDeniedNothingAndDisabled(t *testing.T) {
	raw := &rawEraser{records: []domain.ErasureCandidate{doc("d1", "d1", "bob")}}
	s := newService(raw, &clock{t: time.Unix(1000, 0)})
	ctx := agent("alice")

	if _, err := s.Forget(ctx, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{"d1"}}}); !errors.Is(err, domain.ErrEraseDenied) {
		t.Fatalf("not the writer: err = %v, want ErrEraseDenied", err)
	}
	if _, err := s.Preview(ctx, domain.ErasureSelector{DocumentIDs: []string{"nope"}}); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("nothing visible: err = %v, want ErrNothingToErase", err)
	}
	_, err := s.Forget(ctx, forget.Request{PreviewToken: "pv:x", Selector: domain.ErasureSelector{DocumentIDs: []string{"d1"}}})
	if !errors.Is(err, domain.ErrInvalidErasureRequest) {
		t.Fatalf("token + ids: err = %v, want ErrInvalidErasureRequest", err)
	}
	if _, err := s.Forget(ctx, forget.Request{}); !errors.Is(err, domain.ErrInvalidErasureRequest) {
		t.Fatalf("empty: err = %v", err)
	}
	if _, err := s.Preview(context.Background(), domain.ErasureSelector{DocumentIDs: []string{"d1"}}); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("no principal: err = %v, want ErrScopeMissing", err)
	}

	off := forget.New(nil, forget.Config{})
	if off.Enabled() {
		t.Fatal("nil eraser must be disabled")
	}
	if _, err := off.Preview(ctx, domain.ErasureSelector{DocumentIDs: []string{"d1"}}); !errors.Is(err, domain.ErrErasureDisabled) {
		t.Fatalf("disabled preview: %v", err)
	}
	if _, err := off.List(ctx, domain.ErasureReceiptFilter{}); !errors.Is(err, domain.ErrErasureDisabled) {
		t.Fatalf("disabled list: %v", err)
	}
}

// The outstanding-preview table is bounded.
func TestForget_PreviewTableIsBounded(t *testing.T) {
	raw := &rawEraser{}
	for _, id := range []string{"a", "b", "c"} {
		raw.records = append(raw.records, doc(id, id, "alice"))
	}
	c := &clock{t: time.Unix(1000, 0)}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	s := forget.New(e, forget.Config{PreviewTTL: time.Minute, MaxPreviews: 2, Now: c.now})
	var tokens []string
	for _, id := range []string{"a", "b", "c"} {
		c.t = c.t.Add(time.Second)
		p, err := s.Preview(agent("alice"), domain.ErasureSelector{DocumentIDs: []string{id}})
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, p.Token)
	}
	if _, err := s.Forget(agent("alice"), forget.Request{PreviewToken: tokens[0]}); !errors.Is(err, domain.ErrPreviewUnknown) {
		t.Fatalf("the oldest preview must have been evicted: %v", err)
	}
	if _, err := s.Forget(agent("alice"), forget.Request{PreviewToken: tokens[2]}); err != nil {
		t.Fatalf("the newest preview must survive: %v", err)
	}
}

// forget_memory: a call without a token only previews; a second call with the token
// erases exactly the previewed records; offered only with erasure + agent erase on.
func TestForgetTool_TwoCallsAndOffer(t *testing.T) {
	raw := &rawEraser{records: []domain.ErasureCandidate{doc("d1", "d1", "alice")}}
	c := &clock{t: time.Unix(1000, 0)}
	svc := newService(raw, c)
	tool := forget.NewTool(svc, true)
	if !tool.OfferedTo("lease") || forget.NewTool(svc, false).OfferedTo("lease") || forget.NewTool(forget.New(nil, forget.Config{}), true).OfferedTo("lease") {
		t.Fatal("offer must require erasure enabled and agent erase on")
	}
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{})
	out, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "alice", ArgsJSON: []byte(`{"document_ids":["d1"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var prev struct {
		Erased       bool   `json:"erased"`
		PreviewToken string `json:"preview_token"`
		WouldErase   []struct {
			ID string `json:"id"`
		} `json:"would_erase"`
	}
	if err := json.Unmarshal(out, &prev); err != nil || prev.Erased || prev.PreviewToken == "" || len(prev.WouldErase) != 1 || len(raw.erased) != 0 {
		t.Fatalf("preview call = %s (%v), erased %d", out, err, len(raw.erased))
	}
	if _, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "alice", ArgsJSON: []byte(`{"document_ids":["d1"],"preview_token":"` + prev.PreviewToken + `"}`)}); err == nil {
		t.Fatal("ids + token must be refused")
	}
	if _, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "bob", ArgsJSON: []byte(`{"preview_token":"` + prev.PreviewToken + `"}`)}); err == nil {
		t.Fatal("another agent cannot use alice's preview")
	}
	out, err = tool.Call(ctx, domain.KernelToolCall{AgentID: "alice", ArgsJSON: []byte(`{"preview_token":"` + prev.PreviewToken + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.erased) != 1 || !strings.Contains(string(out), `"erased":true`) {
		t.Fatalf("erase call = %s, erased %d", out, len(raw.erased))
	}
}
