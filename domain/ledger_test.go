package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ADR-0145 D1: the ledger's domain half — deterministic ids, provenance on ctx,
// and the tool-executor choke point.

func TestLedgerEntryID_DeterministicAndContentSensitive(t *testing.T) {
	p := LedgerProvenance{ConversationID: "c1", TurnID: "m1", RunID: "r1", StepIndex: 2}
	key := LedgerCallKey(LedgerToolResult, "mcp:crm/get", CanonicalJSON([]byte(`{"b":2, "a":1}`)))
	id1 := LedgerEntryID(p, key, "h1")
	id2 := LedgerEntryID(p, LedgerCallKey(LedgerToolResult, "mcp:crm/get", CanonicalJSON([]byte(`{"a":1,"b":2}`))), "h1")
	if id1 != id2 {
		t.Fatalf("same call, same content, reordered args: %s != %s (retries must be idempotent)", id1, id2)
	}
	if !IsLedgerID(id1) || len(id1) != len(LedgerIDPrefix)+32 {
		t.Fatalf("id shape: %q", id1)
	}
	if LedgerEntryID(p, key, "h2") == id1 {
		t.Fatal("different content must be a different entry")
	}
	p2 := p
	p2.StepIndex = 3
	if LedgerEntryID(p2, key, "h1") == id1 {
		t.Fatal("a different step must be a different entry")
	}
}

func TestCanonicalJSON(t *testing.T) {
	if got := string(CanonicalJSON([]byte(" {\"z\": [1, 2], \"a\": {\"y\":1,\"x\":2}} "))); got != `{"a":{"x":2,"y":1},"z":[1,2]}` {
		t.Fatalf("got %s", got)
	}
	if got := string(CanonicalJSON([]byte("not json"))); got != "not json" {
		t.Fatalf("non-JSON passes through, got %s", got)
	}
}

func TestLedgerProvenanceFromContext(t *testing.T) {
	ctx := WithConversationOwner(WithConversationID(context.Background(), "conv-9"), "alice")
	p := LedgerProvenanceFromContext(ctx)
	if p.ConversationID != "conv-9" || p.Owner != "alice" || p.StepIndex != -1 || p.RunID != "" {
		t.Fatalf("fallback provenance: %+v", p)
	}
	ctx = WithLedgerProvenance(ctx, LedgerProvenance{RunID: "r", StepIndex: 4, TurnID: "m"})
	p = LedgerProvenanceFromContext(ctx)
	if p.RunID != "r" || p.StepIndex != 4 || p.TurnID != "m" || p.Owner != "alice" || p.ConversationID != "conv-9" {
		t.Fatalf("explicit provenance, filled from ctx: %+v", p)
	}
}

func TestLedgerPreview(t *testing.T) {
	if got := LedgerPreview([]byte("  héllo wörld  "), 5); got != "héllo…" {
		t.Fatalf("got %q", got)
	}
	if got := LedgerPreview([]byte{0xff, 0xfe}, 5); got != "" {
		t.Fatalf("binary preview %q", got)
	}
}

// --- tool executor choke point ---

type recordingLedger struct {
	mu   sync.Mutex
	recs []LedgerRecord
	err  error
}

func (l *recordingLedger) Record(_ context.Context, rec LedgerRecord) (LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return LedgerEntry{}, l.err
	}
	l.recs = append(l.recs, rec)
	sum := hashBytes(rec.Body)
	return LedgerEntry{ID: LedgerEntryID(rec.Provenance, LedgerCallKey(rec.Kind, rec.ToolName, rec.ArgsJSON), sum)}, nil
}

func ledgerExec(t *testing.T, h ToolHandler, tools ...SystemTool) (*ToolExecutor, *recordingLedger) {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	grants := make([]ToolGrant, 0, len(tools))
	for _, tl := range tools {
		reg.Register(tl)
		grants = append(grants, ToolGrant{Tool: tl.Name})
	}
	led := &recordingLedger{}
	e := &ToolExecutor{Registry: reg, Grants: grantStore("worker", grants...), Handler: h,
		InlineThreshold: 64, Ledger: led}
	return e, led
}

// Every result of a call that ran is recorded — a read, a write and a handler
// error alike — with the lease-resolved provenance and the tool's domain tags as
// the classification hint (a result inherits the classification of its source).
func TestToolExecutor_RecordsEveryToolResult(t *testing.T) {
	read := SystemTool{Name: "crm_get", ClassificationTags: []string{"crm"}, Effects: []ToolEffect{EffectRead}}
	write := SystemTool{Name: "crm_put", DataWriteKinds: []string{"crm"}}
	h := &fakeHandler{result: []byte(`{"id":"C-4","amount":1250.5}`)}
	e, led := ledgerExec(t, h, read, write)
	prov := LedgerProvenance{ConversationID: "c", TurnID: "m", RunID: "r", StepIndex: 1, Owner: "alice"}
	ctx := WithLedgerProvenance(context.Background(), prov)

	for _, name := range []string{"crm_get", "crm_put"} {
		if out := e.Execute(ctx, ToolCallRequest{AgentID: "worker", ToolName: name, ArgsJSON: []byte(`{"id":"C-4"}`)}); out.Denied || out.Error != "" {
			t.Fatalf("%s: %+v", name, out)
		}
	}
	h.err, h.result = errors.New("upstream 503"), nil
	if out := e.Execute(ctx, ToolCallRequest{AgentID: "worker", ToolName: "crm_get", ArgsJSON: []byte(`{"id":"C-5"}`)}); out.Error == "" {
		t.Fatalf("handler error expected, got %+v", out)
	}
	if len(led.recs) != 3 {
		t.Fatalf("recorded %d results, want 3 (read, write, error)", len(led.recs))
	}
	for _, r := range led.recs {
		if r.Kind != LedgerToolResult || r.Provenance != prov || r.Writer != AgentPrincipal("worker") {
			t.Fatalf("record: %+v", r)
		}
	}
	if got := led.recs[0].ClassificationHint; len(got) != 1 || got[0] != "crm" {
		t.Fatalf("read result hint = %v, want the tool's domain tags", got)
	}
	if !strings.Contains(string(led.recs[2].Body), "upstream 503") {
		t.Fatalf("error result body: %s", led.recs[2].Body)
	}
}

// A denied call never ran: nothing is recorded.
func TestToolExecutor_DeniedCallIsNotRecorded(t *testing.T) {
	h := &fakeHandler{result: []byte(`{}`)}
	e, led := ledgerExec(t, h, SystemTool{Name: "ok"})
	if out := e.Execute(context.Background(), ToolCallRequest{AgentID: "intruder", ToolName: "ok"}); !out.Denied {
		t.Fatalf("expected denial, got %+v", out)
	}
	if len(led.recs) != 0 || h.called {
		t.Fatalf("denied call recorded or ran: %d %v", len(led.recs), h.called)
	}
}

// With the ledger on, a result over the inline threshold travels as its ledger id,
// never as a content-store cid (one pathway), and a small one stays inline.
func TestToolExecutor_LargeResultIsALedgerReference(t *testing.T) {
	big := []byte(`{"rows":"` + strings.Repeat("x", 200) + `"}`)
	h := &fakeHandler{result: big}
	e, _ := ledgerExec(t, h, SystemTool{Name: "dump"})
	cs := &countingCS{}
	e.ContentStore = cs
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "worker", ToolName: "dump"})
	if !IsLedgerID(out.ResultCID) || out.ResultJSON != nil {
		t.Fatalf("large result: cid=%q inline=%d bytes", out.ResultCID, len(out.ResultJSON))
	}
	if cs.puts != 0 {
		t.Fatalf("content store written %d times with the ledger on", cs.puts)
	}
	h.result = []byte(`{"a":1}`)
	out = e.Execute(context.Background(), ToolCallRequest{AgentID: "worker", ToolName: "dump"})
	if out.ResultCID != "" || string(out.ResultJSON) != `{"a":1}` {
		t.Fatalf("small result: %+v", out)
	}
}

// A ledger write that fails never loses the result: it is returned inline.
func TestToolExecutor_LedgerFailureKeepsResultInline(t *testing.T) {
	big := []byte(strings.Repeat("y", 200))
	e, led := ledgerExec(t, &fakeHandler{result: big}, SystemTool{Name: "dump"})
	led.err = errors.New("db down")
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "worker", ToolName: "dump"})
	if out.ResultCID != "" || len(out.ResultJSON) != len(big) {
		t.Fatalf("got cid=%q inline=%d", out.ResultCID, len(out.ResultJSON))
	}
}

type countingCS struct{ puts int }

func (c *countingCS) Put(context.Context, []byte, string, []string, string) (CID, error) {
	c.puts++
	return "cid", nil
}
func (c *countingCS) Get(context.Context, CID) (*ContextNode, error) { return nil, errors.New("none") }
func (c *countingCS) Has(context.Context, CID) (bool, error)         { return false, nil }
func (c *countingCS) GC(context.Context, []CID) error                { return nil }

type echoScopeTool struct{ gotScope *TagPredicate }

func (echoScopeTool) Spec() SystemTool { return SystemTool{Name: "ledger_echo"} }
func (t *echoScopeTool) Call(ctx context.Context, call KernelToolCall) ([]byte, error) {
	t.gotScope, _ = ScopeFromContext(ctx)
	return json.Marshal(map[string]string{"agent": call.AgentID})
}

// A kernel tool resolves before the registry, needs no grant, runs under the
// caller's own read predicate (ReadFilter, owner fence included) and is not itself
// recorded to the ledger.
func TestToolExecutor_KernelToolRunsUnderCallerScope(t *testing.T) {
	kt := &echoScopeTool{}
	e, led := ledgerExec(t, &fakeHandler{}, SystemTool{Name: "other"})
	e.KernelTools = []KernelTool{kt}
	ctx := WithConversationOwner(context.Background(), "alice")
	out := e.Execute(ctx, ToolCallRequest{AgentID: "anyone-ungranted", ToolName: "ledger_echo"})
	if out.Denied || out.Error != "" || !strings.Contains(string(out.ResultJSON), "anyone-ungranted") {
		t.Fatalf("kernel tool: %+v", out)
	}
	if kt.gotScope == nil || len(kt.gotScope.Owners) != 1 || kt.gotScope.Owners[0] != "alice" {
		t.Fatalf("scope on ctx = %+v, want the reader's predicate acting for alice", kt.gotScope)
	}
	if len(led.recs) != 0 {
		t.Fatalf("a kernel tool's result was recorded: %d", len(led.recs))
	}
	if out := e.Execute(ctx, ToolCallRequest{ToolName: "ledger_echo"}); !out.Denied {
		t.Fatalf("anonymous caller must be denied, got %+v", out)
	}
	if specs := e.KernelToolSpecs(); len(specs) != 1 || specs[0].Name != "ledger_echo" {
		t.Fatalf("specs: %+v", specs)
	}
}
