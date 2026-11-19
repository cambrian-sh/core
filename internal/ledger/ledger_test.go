package ledger

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// AGENTS.md §3.3: the sweeper is a background worker.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// memStore is an in-memory domain.LedgerStore (the Postgres adapter has its own
// TestPg_ suite against a migrated database).
type memStore struct {
	mu   sync.Mutex
	rows map[string]domain.LedgerEntry
	puts int
}

func newMem() *memStore { return &memStore{rows: map[string]domain.LedgerEntry{}} }

func (m *memStore) Put(_ context.Context, e domain.LedgerEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if _, ok := m.rows[e.ID]; !ok {
		m.rows[e.ID] = e
	}
	return nil
}

func (m *memStore) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (m *memStore) Find(_ context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.LedgerEntry
	for _, e := range m.rows {
		switch {
		case q.RunID != "" && e.RunID != q.RunID,
			q.TurnID != "" && e.TurnID != q.TurnID,
			q.ConversationID != "" && e.ConversationID != q.ConversationID,
			q.HasStep && e.StepIndex != q.StepIndex,
			q.ToolName != "" && e.ToolName != q.ToolName,
			q.Kind != "" && e.Kind != q.Kind,
			q.Text != "" && !strings.Contains(strings.ToLower(string(e.Body)+e.Preview), strings.ToLower(q.Text)):
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (m *memStore) DeleteScratch(_ context.Context, turn string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, e := range m.rows {
		if e.TurnID == turn && e.Kind == domain.LedgerScratch {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

func (m *memStore) PurgeExpired(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, e := range m.rows {
		if !e.ExpiresAt.After(now) {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

var prov = domain.LedgerProvenance{ConversationID: "conv", TurnID: "msg-1", RunID: "run-1", StepIndex: 0, Owner: "alice"}

func TestRecorder_ClassifiesCapsAndIsIdempotent(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, domain.AllowAllAuthorizer{}, RecorderConfig{MaxEntryBytes: 16, PrivateByDefault: true, Retention: time.Hour})
	rec := domain.LedgerRecord{
		Kind: domain.LedgerToolResult, Provenance: prov, ToolName: "crm_get",
		ArgsJSON: []byte(`{"id": "C-4"}`), Writer: domain.AgentPrincipal("worker"),
		ClassificationHint: []string{"crm"}, ContentType: "application/json",
		Body: []byte(`{"id":"C-4","amount":1250.5,"due":"2026-10-01"}`),
	}
	e1, err := r.Record(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	// A retry (same call, same content) lands on the same entry.
	rec.ArgsJSON = []byte(`{"id":"C-4"}`)
	e2, _ := r.Record(context.Background(), rec)
	if e1.ID != e2.ID || len(st.rows) != 1 {
		t.Fatalf("retry was not idempotent: %s vs %s, %d rows", e1.ID, e2.ID, len(st.rows))
	}
	got := st.rows[e1.ID]
	// Kernel classification: the tool's domain tag (the source's classification)
	// plus the ADR-0142 D12 owner stamp from the provenance.
	if strings.Join(got.Tags, ",") != "crm,"+domain.OwnerTag("alice") {
		t.Fatalf("tags = %v", got.Tags)
	}
	if !got.Truncated || len(got.Body) != 16 || got.SizeBytes != int64(len(rec.Body)) {
		t.Fatalf("cap: truncated=%v body=%d size=%d", got.Truncated, len(got.Body), got.SizeBytes)
	}
	if !strings.Contains(got.Preview, "1250.5") || got.Owner != "alice" || got.StepIndex != 0 {
		t.Fatalf("entry: %+v", got)
	}
	if !got.ExpiresAt.After(got.CreatedAt) {
		t.Fatal("no expiry")
	}
}

type denyAll struct{ domain.AllowAllAuthorizer }

func (denyAll) ClassifyWrite(_ context.Context, p domain.PrincipalRef, _ []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{Allowed: false, Principal: p}
}

// Fail-closed: an entry the decision point will not classify is not written.
func TestRecorder_UnclassifiableIsNotWritten(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, denyAll{}, RecorderConfig{})
	_, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: prov,
		Writer: domain.AgentPrincipal("w"), Body: []byte("x")})
	if err != ErrUnclassifiable || len(st.rows) != 0 {
		t.Fatalf("err %v rows %d", err, len(st.rows))
	}
}

// Retention: the sweeper purges entries whose retention ended; scratch goes when
// its turn ends.
func TestSweeper_RetentionAndTurnScratch(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, nil, RecorderConfig{Retention: time.Hour})
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	keep, _ := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: prov, Writer: domain.AgentPrincipal("w"), Body: []byte("a")})
	scratch, _ := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerScratch, Provenance: prov, Writer: domain.AgentPrincipal("w"), Body: []byte("s")})

	sw := NewSweeper(st, time.Hour)
	sw.EndTurn(context.Background(), "msg-1")
	if _, ok := st.rows[scratch.ID]; ok {
		t.Fatal("scratch survived its turn")
	}
	if _, ok := st.rows[keep.ID]; !ok {
		t.Fatal("a step output was deleted at turn end")
	}
	sw.now = func() time.Time { return base.Add(30 * time.Minute) }
	if n := sw.Purge(context.Background()); n != 0 {
		t.Fatalf("purged %d entries inside their retention", n)
	}
	sw.now = func() time.Time { return base.Add(2 * time.Hour) }
	if n := sw.Purge(context.Background()); n != 1 || len(st.rows) != 0 {
		t.Fatalf("purge after retention: n=%d rows=%d", n, len(st.rows))
	}
	// Start/Stop drain (goleak checks nothing is left running).
	sw.Start(context.Background())
	sw.Start(context.Background())
	sw.Stop()
	sw.Stop()
}

func toolsOver(st *memStore) (get, find domain.KernelTool) {
	ts := Tools(authz.NewEnforcingLedger(st, nil), 40, nil)
	return ts[0], ts[1]
}

func call(t *testing.T, kt domain.KernelTool, ctx context.Context, args string) map[string]any {
	t.Helper()
	out, err := kt.Call(ctx, domain.KernelToolCall{AgentID: "worker", ArgsJSON: []byte(args)})
	if err != nil {
		t.Fatalf("%s %s: %v", kt.Spec().Name, args, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLedgerGet_PathRowsPageAndScope(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, nil, RecorderConfig{PrivateByDefault: true})
	body := `{"data":{"customers":[{"id":"C-1","amount":10},{"id":"C-4","amount":1250.5},{"id":"C-7","amount":80}]}}`
	e, _ := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: prov,
		ToolName: "crm_list", Writer: domain.AgentPrincipal("worker"), ContentType: "application/json", Body: []byte(body)})
	text, _ := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: prov,
		Writer: domain.AgentPrincipal("worker"), Body: []byte(strings.Repeat("abcdefghij", 10))})
	get, _ := toolsOver(st)
	alice := domain.WithScope(context.Background(), &domain.TagPredicate{Bypass: true, Owners: []string{"alice"}})

	m := call(t, get, alice, `{"id":"`+e.ID+`","path":"data.customers[1].amount"}`)
	if m["value"] != 1250.5 {
		t.Fatalf("path value = %v", m["value"])
	}
	m = call(t, get, alice, `{"id":"`+e.ID+`","path":"/data/customers","rows":"1:2"}`)
	rows, _ := m["value"].([]any)
	if len(rows) != 1 || m["total_rows"] != float64(3) {
		t.Fatalf("rows = %v total=%v", m["value"], m["total_rows"])
	}
	m = call(t, get, alice, `{"id":"`+text.ID+`","page":2}`)
	if m["pages"] != float64(3) || m["text"] != strings.Repeat("abcdefghij", 4) {
		t.Fatalf("page 2: %v", m)
	}
	// Another person's reader: absent, not denied.
	bob := domain.WithScope(context.Background(), &domain.TagPredicate{Bypass: true, Owners: []string{"bob"}})
	if m = call(t, get, bob, `{"id":"`+e.ID+`"}`); m["found"] != false {
		t.Fatalf("bob read alice's entry: %v", m)
	}
	if _, err := get.Call(alice, domain.KernelToolCall{ArgsJSON: []byte(`{"id":"` + e.ID + `","path":"data.nope"}`)}); err == nil {
		t.Fatal("bad path must be an error the model can correct")
	}
}

func TestLedgerFind_ScopeComesFromTheCallersProvenance(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, nil, RecorderConfig{})
	other := prov
	other.RunID, other.TurnID = "run-2", "msg-2"
	for _, p := range []domain.LedgerProvenance{prov, other} {
		_, _ = r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: p,
			ToolName: "crm_get", Writer: domain.AgentPrincipal("w"), Body: []byte(`{"run":"` + p.RunID + `"}`)})
	}
	_, find := toolsOver(st)
	ctx := domain.WithScope(domain.WithLedgerProvenance(context.Background(), prov), &domain.TagPredicate{Bypass: true})

	m := call(t, find, ctx, `{}`) // default scope: the caller's run
	if m["scope"] != "run" || m["count"] != float64(1) {
		t.Fatalf("default find: %v", m)
	}
	m = call(t, find, ctx, `{"scope":"conversation","tool":"crm_get"}`)
	if m["count"] != float64(2) {
		t.Fatalf("conversation find: %v", m)
	}
	m = call(t, find, ctx, `{"scope":"turn","text":"run-2"}`)
	if m["count"] != float64(0) {
		t.Fatalf("turn find leaked another turn: %v", m)
	}
	// No provenance (an agent outside any conversation or run): nothing to search.
	bare := domain.WithScope(context.Background(), &domain.TagPredicate{Bypass: true})
	if _, err := find.Call(bare, domain.KernelToolCall{ArgsJSON: []byte(`{"scope":"run"}`)}); err == nil {
		t.Fatal("a call with no run must not search the whole ledger")
	}
}

// AuthorizeErase completes domain.Authorizer (ADR-0137 D3); this fake is not about erasure.
func (denyAll) AuthorizeErase(ctx context.Context, p domain.PrincipalRef, s domain.SurfaceRef, t domain.Taggable) domain.AccessDecision {
	return domain.AllowAllAuthorizer{}.AuthorizeErase(ctx, p, s, t)
}
