package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/calc"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ledger"
	"github.com/cambrian-sh/core/internal/memory/vault"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// eng2173DecisionPoint restates the premium write rule as amended by ENG-2161
// (ADR-0035 C2): ceiling ∩ hint, the whole ceiling for an empty hint — except that an
// empty hint under a ceiling holding closed tags is refused with
// ReasonClassificationRequired. Core cannot import premium (AGENTS.md §1.2); premium's
// tests pin the rule on the real Authorizer. The property under test is the KERNEL's
// half: what the ledger asks the decision point for, and what it does with a refusal.
type eng2173DecisionPoint struct {
	domain.AllowAllAuthorizer
	ceilings map[string][]string
	closed   []string
}

func (c eng2173DecisionPoint) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	if p.Kind == domain.PrincipalSystem {
		return hint, domain.AccessDecision{Allowed: true, Reason: domain.ReasonBypass}
	}
	ceiling := c.ceilings[p.WriterID()]
	req := authz.RequestedClassification(hint)
	if len(req) == 0 {
		for _, t := range ceiling {
			if slices.Contains(c.closed, t) {
				return nil, domain.AccessDecision{Reason: domain.ReasonClassificationRequired,
					ClassificationChoice: &domain.WriteClassificationChoice{Ceiling: ceiling, Closed: c.closed}}
			}
		}
	}
	out := []string{}
	for _, t := range ceiling {
		if len(req) == 0 || slices.Contains(req, t) {
			out = append(out, t)
		}
	}
	return domain.StampProvenanceSource(out, p.WriterID()), domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed}
}

type eng2173Handler struct{}

func (eng2173Handler) Execute(_ context.Context, call domain.ToolCall) ([]byte, error) {
	return []byte(`{"tool":"` + call.ToolName + `","ok":true}`), nil
}

// ENG-2173 end to end (owner decision 2026-11-19, inherit from sources): an agent
// whose write ceiling holds a closed tag, over the production ledger composition
// (NewLedgerStack: the Postgres ledger store + the artifact vault, the Recorder with
// the decision point, the enforcing reader), through the two production choke points
// that record tool results:
//
//   - the agent plane's QueryMemory under a lease the gateway issued and bound to the
//     agent: the recall of a closed-tag (scope-finance) chunk is recorded with the
//     chunk's classification — readable by a reader holding scope-finance, not by one
//     forbidden it (before ENG-2173 the recall had no hint, the decision point refused
//     the untagged default, and the entry was dropped);
//   - the ToolExecutor: a tool that reads nothing, with domain tags, records its result
//     with them; a tool with neither sources nor tags is refused — not written, the
//     recorder's Unrecorded count rises, and the refusal is logged at WARN;
//   - ENG-2183 (owner decision 2026-11-19, inherit for step outputs and computed
//     entries): a step output inherits its step's scope-finance tool result, a
//     source-less step output is refused loudly, and calc_aggregate's computed entry
//     inherits the scope-finance recall it counted (its operand).
//
// Requires PG_TEST_DSN.
func TestPg_E2E_LedgerToolResultsInheritTheirSourcesUnderAClosedCeiling(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Ledger.Enabled = true
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	pool := vec.Pool()

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	agentID := "eng2173-analyst-" + stamp
	convID := "eng2173-conv-" + stamp
	chunkID := "eng2173-chunk-" + stamp
	dp := eng2173DecisionPoint{
		ceilings: map[string][]string{agentID: {"scope-public", "scope-finance"}},
		closed:   []string{"scope-finance"},
	}
	mem := NewMemoryStack(NewMemoryBackend(vec), nil, nil, cfg.Execution, dp, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	led := NewLedgerStack(ctx, cfg.Execution.Ledger, cfg.Execution.Content,
		LedgerStackDeps{Pool: pool, Bodies: vault.NewArtifactVault(t.TempDir()), Authorizer: dp})
	if led == nil {
		t.Fatal("ledger enabled but NewLedgerStack returned nil")
	}
	rec, ok := led.Writer.(*ledger.Recorder)
	if !ok {
		t.Fatalf("LedgerStack.Writer is %T, want *ledger.Recorder", led.Writer)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM ledger_entries WHERE conversation_id = $1`, convID)
		_, _ = pool.Exec(bg, `DELETE FROM chunks WHERE id = $1`, chunkID)
	})

	// A closed-tag record the agent's tool will read.
	v := make([]float32, cfg.Embedder.Dimensions)
	v[0] = 1
	if err := vec.SaveBatch(ctx, []*domain.Document{{ID: chunkID, Text: "Q3 gross margin fell to 12% on freight costs.",
		DocumentType: domain.DocTypeMnemonicFact, Embedding: domain.Embedding{Vector: v, Size: len(v), Model: "eng2173"},
		Metadata: map[string]any{"tags": []string{"scope-finance"}, "source_agent_id": "eng2173-ingest"}, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}

	var warn bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&warn, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	financeReader := domain.WithScope(ctx, &domain.TagPredicate{RequiredTags: []string{"scope-finance"}})
	outsider := domain.WithScope(ctx, &domain.TagPredicate{ForbiddenTags: []string{"scope-finance"}})

	// 1. Built from classified sources: the agent-plane recall.
	gw := subnetwork.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, llm.NewSpendMeter(0, nil, nil))
	lease, err := gw.Acquire(ctx, domain.StepAllocation{}, 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gw.Complete(context.Background(), lease) })
	gw.BindLease(lease, domain.LeaseBinding{AgentID: agentID, ConversationID: convID, OriginMessageID: "turn-" + stamp})
	srv := &subnetwork.Server{
		LLMGateway:     gw,
		MemorySearcher: chunkSearcher{reader: mem.Reader(), byQuery: map[string]string{"q3 margin": chunkID}},
		Ledger:         &subnetwork.LedgerAccess{Writer: led.Writer, Reader: led.Reader},
	}
	agentCtx := metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", agentID, "x-lease-id", string(lease)))
	agentCtx = domain.WithPrincipal(domain.WithSurface(agentCtx, domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"}),
		domain.AgentPrincipal(agentID))
	resp, err := srv.QueryMemory(agentCtx, &pb.MemoryRequest{Query: "q3 margin"})
	if err != nil || len(resp.GetResults()) == 0 {
		t.Fatalf("QueryMemory: %v (%d results)", err, len(resp.GetResults()))
	}
	recall, err := led.Reader.Find(financeReader, domain.LedgerQuery{ConversationID: convID, ToolName: domain.RecallToolName})
	if err != nil {
		t.Fatal(err)
	}
	if len(recall) != 1 {
		t.Fatalf("the recall of a closed-tag record must be recorded and readable by a scope-finance reader: %d entries (log: %s)",
			len(recall), warn.String())
	}
	if !slices.Contains(recall[0].Tags, "scope-finance") || slices.Contains(recall[0].Tags, "scope-public") {
		t.Fatalf("recall entry tags = %v, want the source's scope-finance, not the whole ceiling", recall[0].Tags)
	}
	if got, err := led.Reader.Get(outsider, recall[0].ID); err != nil || got != nil {
		t.Fatalf("a reader forbidden scope-finance read the recall entry: %v %v", got, err)
	}

	// 2 and 3. Through the ToolExecutor: domain tags, then neither.
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "weather", ClassificationTags: []string{"scope-public"}, Effects: []domain.ToolEffect{domain.EffectRead}})
	reg.Register(domain.SystemTool{Name: "echo", Effects: []domain.ToolEffect{domain.EffectRead}})
	reg.Register(domain.SystemTool{Name: "ledger_export", ClassificationTags: []string{"scope-finance"}, Effects: []domain.ToolEffect{domain.EffectRead}})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set(agentID, []domain.ToolGrant{{Tool: "weather"}, {Tool: "echo"}, {Tool: "ledger_export"}})
	exec, err := domain.NewToolExecutor(domain.ToolExecutorControls{Registry: reg, Grants: grants, Authz: dp},
		domain.ToolExecutor{Handler: eng2173Handler{}, Ledger: led.Writer})
	if err != nil {
		t.Fatal(err)
	}
	toolCtx := domain.WithLedgerProvenance(ctx, domain.LedgerProvenance{ConversationID: convID, TurnID: "turn-" + stamp, StepIndex: -1})
	if out := exec.Execute(toolCtx, domain.ToolCallRequest{AgentID: agentID, ToolName: "weather", ArgsJSON: []byte(`{"city":"Izmir"}`)}); out.Denied || out.Error != "" {
		t.Fatalf("weather: %+v", out)
	}
	unrecordedBefore := rec.Unrecorded()
	if out := exec.Execute(toolCtx, domain.ToolCallRequest{AgentID: agentID, ToolName: "echo", ArgsJSON: []byte(`{"say":"hi"}`)}); out.Denied || out.Error != "" {
		t.Fatalf("echo: the call itself must succeed (recording is best-effort): %+v", out)
	}

	bypass := domain.WithScope(ctx, &domain.TagPredicate{Bypass: true})
	weather, err := led.Reader.Find(bypass, domain.LedgerQuery{ConversationID: convID, ToolName: "weather"})
	if err != nil || len(weather) != 1 {
		t.Fatalf("weather entries: %d (%v)", len(weather), err)
	}
	if !slices.Contains(weather[0].Tags, "scope-public") || slices.Contains(weather[0].Tags, "scope-finance") {
		t.Fatalf("weather entry tags = %v, want the tool's scope-public", weather[0].Tags)
	}
	echo, err := led.Reader.Find(bypass, domain.LedgerQuery{ConversationID: convID, ToolName: "echo"})
	if err != nil || len(echo) != 0 {
		t.Fatalf("an unclassifiable result was written: %d entries (%v)", len(echo), err)
	}
	if got := rec.Unrecorded() - unrecordedBefore; got != 1 {
		t.Fatalf("Unrecorded rose by %d, want 1", got)
	}
	log := warn.String()
	for _, w := range []string{"level=WARN", "ledger_entry_unclassifiable", "tool=echo", "scope-finance"} {
		if !strings.Contains(log, w) {
			t.Errorf("the refusal must be logged at WARN with %q: %s", w, log)
		}
	}

	// 4. ENG-2183: a STEP OUTPUT under the same closed-tag ceiling. The step's tool
	// result (a scope-finance tool) is recorded through the ToolExecutor under the
	// run/step provenance; the step output is recorded exactly as the DAG executor
	// records it (executer.recordStepOutput: the step's provenance, the step's agent,
	// no hint) and inherits the step's classification.
	stepProv := domain.LedgerProvenance{ConversationID: convID, TurnID: "turn-" + stamp, RunID: "eng2183-run-" + stamp, StepIndex: 0}
	if out := exec.Execute(domain.WithLedgerProvenance(ctx, stepProv), domain.ToolCallRequest{AgentID: agentID,
		ToolName: "ledger_export", ArgsJSON: []byte(`{"period":"Q3"}`)}); out.Denied || out.Error != "" {
		t.Fatalf("ledger_export: %+v", out)
	}
	so, err := led.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: stepProv,
		Writer: domain.AgentPrincipal(agentID), ContentType: "text/plain", Body: []byte("Q3 margin fell to 12%.")})
	if err != nil {
		t.Fatalf("a step output built from its step's classified tool result must be recorded: %v", err)
	}
	if !slices.Contains(so.Tags, "scope-finance") || slices.Contains(so.Tags, "scope-public") {
		t.Fatalf("step output tags = %v, want the step's scope-finance", so.Tags)
	}
	if got, err := led.Reader.Get(financeReader, so.ID); err != nil || got == nil {
		t.Fatalf("a scope-finance reader could not read the step output: %v", err)
	}
	if got, _ := led.Reader.Get(outsider, so.ID); got != nil {
		t.Fatal("a reader forbidden scope-finance read the step output")
	}
	// A step that recorded nothing, with no tags of its own: refused, loudly.
	emptyStep := stepProv
	emptyStep.StepIndex = 1
	before := rec.Unrecorded()
	if _, err := led.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: emptyStep,
		Writer: domain.AgentPrincipal(agentID), ContentType: "text/plain", Body: []byte("nothing to go on")}); !errors.Is(err, ledger.ErrUnclassifiable) {
		t.Fatalf("a source-less step output under a closed ceiling: err = %v, want ErrUnclassifiable", err)
	}
	if rec.Unrecorded()-before != 1 || !strings.Contains(warn.String(), "kind=step_output") {
		t.Fatalf("the step output refusal must be counted and logged at WARN (unrecorded +%d): %s", rec.Unrecorded()-before, warn.String())
	}

	// 5. ENG-2183: a COMPUTED entry through the calc kernel tool (calc.Tools over the
	// enforced reader and the recorder): counting the recall's hits by reference
	// records a computed entry derived from its operand, which inherits the
	// operand's scope-finance.
	var aggregate domain.KernelTool
	for _, kt := range calc.Tools(calc.Deps{Reader: led.Reader, Writer: led.Writer}) {
		if kt.Spec().Name == calc.ToolAggregate {
			aggregate = kt
		}
	}
	calcCtx := domain.WithLedgerProvenance(financeReader, stepProv)
	raw, err := aggregate.Call(calcCtx, domain.KernelToolCall{AgentID: agentID,
		ArgsJSON: []byte(`{"ref":"` + recall[0].ID + `","path":"hits","op":"count"}`)})
	if err != nil {
		t.Fatalf("calc_aggregate: %v", err)
	}
	var calcOut struct {
		Result    float64 `json:"result"`
		LedgerRef string  `json:"ledger_ref"`
		Note      string  `json:"note"`
	}
	if err := json.Unmarshal(raw, &calcOut); err != nil {
		t.Fatal(err)
	}
	if calcOut.LedgerRef == "" || calcOut.Result != 1 {
		t.Fatalf("calc_aggregate under a closed ceiling must record its computed entry: %s", raw)
	}
	computed, err := led.Reader.Get(financeReader, calcOut.LedgerRef)
	if err != nil || computed == nil {
		t.Fatalf("a scope-finance reader could not read the computed entry: %v", err)
	}
	if computed.Kind != domain.LedgerComputed || !slices.Contains(computed.Tags, "scope-finance") {
		t.Fatalf("computed entry kind %s tags %v, want computed with the operand's scope-finance", computed.Kind, computed.Tags)
	}
	if got, _ := led.Reader.Get(outsider, calcOut.LedgerRef); got != nil {
		t.Fatal("a reader forbidden scope-finance read the computed entry")
	}
}
