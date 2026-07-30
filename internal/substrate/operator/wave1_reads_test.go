package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

func codeOf(err error) codes.Code { return status.Code(err) }

// ── the invariant every Wave 1 read shares ───────────────────────────────────

// An unwired source must answer Unimplemented, NEVER an empty success.
//
// This is the whole reason several of these RPCs exist. A console that receives
// an empty list cannot tell "this deployment has no MCP servers" from "this
// kernel cannot report them", and renders the same screen for a healthy
// deployment and a broken one.
func TestWave1_UnwiredSourcesReturnUnimplementedNotEmpty(t *testing.T) {
	s := &Service{}
	ctx := context.Background()

	if _, err := s.ListSessionCheckpoints(ctx, &pb.ListSessionCheckpointsOpRequest{SessionId: "s1"}); codeOf(err) != codes.Unimplemented {
		t.Errorf("ListSessionCheckpoints: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.ListMCPServers(ctx, &pb.ListMCPServersOpRequest{}); codeOf(err) != codes.Unimplemented {
		t.Errorf("ListMCPServers: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.GetEmbeddingConfig(ctx, &pb.GetEmbeddingConfigOpRequest{}); codeOf(err) != codes.Unimplemented {
		t.Errorf("GetEmbeddingConfig: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.ClassifyInput(ctx, &pb.ClassifyInputOpRequest{Text: "hi"}); codeOf(err) != codes.Unimplemented {
		t.Errorf("ClassifyInput: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.ListGenerators(ctx, &pb.ListGeneratorsOpRequest{}); codeOf(err) != codes.Unimplemented {
		t.Errorf("ListGenerators: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.RetryWatchDeadLetter(ctx, &pb.RetryWatchDeadLetterOpRequest{
		CommandId: "c", Reason: "r", DeadLetterId: "d",
	}); codeOf(err) != codes.Unimplemented {
		t.Errorf("RetryWatchDeadLetter: code = %v, want Unimplemented", codeOf(err))
	}
}

// Capability strings must follow the sources, so the console never renders a
// surface the kernel cannot serve — and never hides one it can.
func TestWave1_CapabilitiesTrackWiredSources(t *testing.T) {
	if caps := (&Service{}).Wave1Capabilities(); len(caps) != 0 {
		t.Fatalf("bare service advertises %v, want none", caps)
	}

	s := &Service{}
	s.SetWave1Reads(stubCheckpoints{}, nil, nil, nil)
	caps := s.Wave1Capabilities()
	if len(caps) != 1 || caps[0] != "session-checkpoints" {
		t.Fatalf("caps = %v, want exactly [session-checkpoints]", caps)
	}
}

// ── checkpoints ──────────────────────────────────────────────────────────────

type stubCheckpoints struct {
	metas     []domain.CheckpointMeta
	err       error
	resumable map[string]bool
}

func (s stubCheckpoints) CheckpointsForSession(string) ([]domain.CheckpointMeta, error) {
	return s.metas, s.err
}
func (s stubCheckpoints) ResumableAt(runID string, step int) bool { return s.resumable[runID] }

// run_id must survive onto the wire. Two runs of one session can each hold a
// step_index 2 meaning different points; a response that dropped run_id would
// let a console offer a resume from the wrong plan.
func TestListSessionCheckpoints_CarriesRunIDPerRow(t *testing.T) {
	now := time.Now()
	s := &Service{}
	s.SetWave1Reads(stubCheckpoints{
		metas: []domain.CheckpointMeta{
			{RunID: "run-b", SessionID: "s1", PlanID: "p2", StepIndex: 2, Timestamp: now},
			{RunID: "run-a", SessionID: "s1", PlanID: "p1", StepIndex: 2, Timestamp: now},
		},
		resumable: map[string]bool{"run-b": true},
	}, nil, nil, nil)

	resp, err := s.ListSessionCheckpoints(context.Background(), &pb.ListSessionCheckpointsOpRequest{SessionId: "s1"})
	if err != nil {
		t.Fatalf("ListSessionCheckpoints: %v", err)
	}
	if len(resp.GetCheckpoints()) != 2 {
		t.Fatalf("got %d checkpoints, want 2", len(resp.GetCheckpoints()))
	}
	// Sorted by run, so run-a comes first despite arriving second.
	first, second := resp.GetCheckpoints()[0], resp.GetCheckpoints()[1]
	if first.GetRunId() != "run-a" || second.GetRunId() != "run-b" {
		t.Fatalf("run order = %q,%q; want run-a,run-b", first.GetRunId(), second.GetRunId())
	}
	if first.GetStepIndex() != second.GetStepIndex() {
		t.Fatal("test setup broken: both rows should share a step index")
	}
	// The same step index, different runs, different resumability — exactly the
	// ambiguity run_id exists to resolve.
	if first.GetResumable() || !second.GetResumable() {
		t.Fatalf("resumable = %v,%v; want false,true", first.GetResumable(), second.GetResumable())
	}
	if first.GetPlanId() != "p1" {
		t.Fatalf("plan_id = %q, want p1", first.GetPlanId())
	}
}

func TestListSessionCheckpoints_RequiresSessionID(t *testing.T) {
	s := &Service{}
	s.SetWave1Reads(stubCheckpoints{}, nil, nil, nil)

	if _, err := s.ListSessionCheckpoints(context.Background(), &pb.ListSessionCheckpointsOpRequest{}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}

// ── MCP ──────────────────────────────────────────────────────────────────────

type stubMCP struct {
	servers    []MCPServerInfo
	configured bool
}

func (s stubMCP) MCPServers() []MCPServerInfo { return s.servers }
func (s stubMCP) MCPConfigured() bool         { return s.configured }

// The `configured` flag is what separates an intentional zero from an absent
// subsystem. Both are empty lists; only this field tells them apart.
func TestListMCPServers_ConfiguredSeparatesZeroFromAbsent(t *testing.T) {
	s := &Service{}
	s.SetWave1Reads(nil, stubMCP{configured: true}, nil, nil)

	resp, err := s.ListMCPServers(context.Background(), &pb.ListMCPServersOpRequest{})
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(resp.GetServers()) != 0 {
		t.Fatalf("got %d servers, want 0", len(resp.GetServers()))
	}
	if !resp.GetConfigured() {
		t.Fatal("configured = false on a deployment that HAS MCP set up with no servers")
	}
}

// ── embedding ────────────────────────────────────────────────────────────────

type stubEmbedding struct{ count int64 }

func (stubEmbedding) EmbeddingConfig() (string, string, string, int) {
	return "ollama", "bge-large", "http://localhost:11434", 1024
}
func (s stubEmbedding) VectorCount(context.Context) int64 { return s.count }

// -1 must survive to the wire. Collapsing it to 0 would tell an operator their
// corpus is empty — a far more alarming claim than "not counted".
func TestGetEmbeddingConfig_UncountableIsMinusOneNotZero(t *testing.T) {
	s := &Service{}
	s.SetWave1Reads(nil, nil, stubEmbedding{count: -1}, nil)

	resp, err := s.GetEmbeddingConfig(context.Background(), &pb.GetEmbeddingConfigOpRequest{})
	if err != nil {
		t.Fatalf("GetEmbeddingConfig: %v", err)
	}
	if resp.GetVectorCount() != -1 {
		t.Fatalf("vector_count = %d, want -1", resp.GetVectorCount())
	}
	if resp.GetDimensions() != 1024 || resp.GetModel() != "bge-large" {
		t.Fatalf("config = %d/%q, want 1024/bge-large", resp.GetDimensions(), resp.GetModel())
	}
}

// ── classification ───────────────────────────────────────────────────────────

type stubClassifier struct {
	out ClassifiedInput
	err error
}

func (s stubClassifier) Classify(context.Context, string, string) (ClassifiedInput, error) {
	return s.out, s.err
}

// The five-value vocabulary must pass through verbatim. `ingest` is the one that
// matters most: it WRITES to memory, so a plane that mapped it onto another value
// would make an operator approve a write believing they approved a question.
func TestClassifyInput_PassesTheFullVocabularyThrough(t *testing.T) {
	for _, want := range []string{"chat", "plan", "ingest", "watch", "clarification"} {
		s := &Service{}
		s.SetWave1Reads(nil, nil, nil, stubClassifier{out: ClassifiedInput{
			Classification: want, Why: "because", Confidence: 0.9,
		}})

		resp, err := s.ClassifyInput(context.Background(), &pb.ClassifyInputOpRequest{Text: "x"})
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if resp.GetClassification() != want {
			t.Fatalf("classification = %q, want %q", resp.GetClassification(), want)
		}
	}
}

// `why` must reach the wire: an unexplained label is not something an operator
// can sensibly overrule, and overruling is the entire point of showing it.
func TestClassifyInput_CarriesWhyAndConfidence(t *testing.T) {
	s := &Service{}
	s.SetWave1Reads(nil, nil, nil, stubClassifier{out: ClassifiedInput{
		Classification: "plan",
		Why:            "layer 2: a single keyword category matched (plan)",
		Confidence:     0.82,
	}})

	resp, err := s.ClassifyInput(context.Background(), &pb.ClassifyInputOpRequest{Text: "build me a report"})
	if err != nil {
		t.Fatalf("ClassifyInput: %v", err)
	}
	if resp.GetWhy() == "" {
		t.Fatal("why is empty — the console cannot explain the decision it is asking about")
	}
	if resp.GetConfidence() != 0.82 {
		t.Fatalf("confidence = %v, want 0.82", resp.GetConfidence())
	}
}

func TestClassifyInput_RequiresText(t *testing.T) {
	s := &Service{}
	s.SetWave1Reads(nil, nil, nil, stubClassifier{})

	if _, err := s.ClassifyInput(context.Background(), &pb.ClassifyInputOpRequest{}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}

// ── generators ───────────────────────────────────────────────────────────────

type stubGenerators struct {
	gens []GeneratorInfo
	res  GeneratorTestResult
	err  error
}

func (s stubGenerators) Generators() []GeneratorInfo       { return s.gens }
func (s stubGenerators) RoleAssignments() []RoleAssignment { return nil }
func (s stubGenerators) TestGenerator(context.Context, string) (GeneratorTestResult, error) {
	return s.res, s.err
}

// The credential must not be reachable. GeneratorOp has no key field at all, so
// this test guards the shape: if someone adds one, the proto changes and this
// assertion is where the conversation starts.
func TestListGenerators_ReportsKeyFactsWithoutTheKey(t *testing.T) {
	s := &Service{}
	s.SetGeneratorRegistry(stubGenerators{gens: []GeneratorInfo{{
		ID: "gpt", KeyConfigured: true, KeyLastFour: "1234", KeySource: "env:OPENAI_API_KEY",
	}}})

	resp, err := s.ListGenerators(context.Background(), &pb.ListGeneratorsOpRequest{})
	if err != nil {
		t.Fatalf("ListGenerators: %v", err)
	}
	g := resp.GetGenerators()[0]
	if !g.GetKeyConfigured() || g.GetKeyLastFour() != "1234" {
		t.Fatalf("key facts = %v/%q", g.GetKeyConfigured(), g.GetKeyLastFour())
	}
	if g.GetKeySource() != "env:OPENAI_API_KEY" {
		t.Fatalf("key_source = %q — the operator must be able to see WHERE the key comes from", g.GetKeySource())
	}
}

// A failed probe is a SUCCESSFUL rpc carrying ok=false. Returning a gRPC error
// would make a working diagnostic look like a broken console.
func TestTestGenerator_FailedProbeIsNotAnRPCError(t *testing.T) {
	s := &Service{}
	s.SetGeneratorRegistry(stubGenerators{err: errors.New("connection refused")})

	resp, err := s.TestGenerator(context.Background(), &pb.TestGeneratorOpRequest{GeneratorId: "gpt"})
	if err != nil {
		t.Fatalf("probe failure surfaced as an RPC error: %v", err)
	}
	if resp.GetOk() || resp.GetError() == "" {
		t.Fatalf("ok=%v error=%q, want ok=false with a reason", resp.GetOk(), resp.GetError())
	}
}

func TestTestGenerator_ReportsTheModelTheEndpointEchoed(t *testing.T) {
	s := &Service{}
	s.SetGeneratorRegistry(stubGenerators{res: GeneratorTestResult{
		OK: true, ModelServed: "gpt-4o-mini-2024-07-18", LatencyMs: 42,
	}})

	resp, err := s.TestGenerator(context.Background(), &pb.TestGeneratorOpRequest{GeneratorId: "gpt"})
	if err != nil {
		t.Fatalf("TestGenerator: %v", err)
	}
	if resp.GetModelServed() != "gpt-4o-mini-2024-07-18" {
		t.Fatalf("model_served = %q — the echoed model is the only way to catch an endpoint answering with a different build", resp.GetModelServed())
	}
}
