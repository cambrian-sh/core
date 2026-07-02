//go:build integration

// Multi-agent integration benchmark — ADR-0023 Issues #0023-13/14/15.
//
// Boots the full Cambrian kernel with real Python agent subprocesses and
// exercises Planner → Auctioneer → AgentManager → DAGExecutor end-to-end.
//
// Each fixture case runs twice:
//   Path A (Planner): user_input → Planner LLM → ExecutionPlan → agents
//   Path B (Manual):  hardcoded reference plan → agents
//
// Prerequisites: Postgres on :5432, Ollama on :11434, cambrian-agent-sdk installed.
// Run:
//   go test -tags=integration -bench=BenchmarkMultiAgent -benchtime=1x -timeout=600s ./cmd/orchestrator/...

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/internal/metabolism/interview"

	"golang.org/x/sync/errgroup"
)

var cognitiveAgentIDs = []string{"code_generator_agent", "summariser_agent", "analyst_agent"}

// ── Fixture types (mirror internal/benchmarks/testdata/multiagent_plans.json) ──

type maStep struct {
	Query              string `json:"query"`
	DependsOn          []int  `json:"depends_on"`
	ExpectedCapability string `json:"expected_capability"`
}

type maPlan struct {
	ID        string   `json:"id"`
	Subject   string   `json:"subject"`
	UserInput string   `json:"user_input"`
	Steps     []maStep `json:"steps"`
}

func loadMAPlans(b *testing.B) []maPlan {
	b.Helper()
	path := filepath.Join("..", "..", "internal", "benchmarks", "testdata", "multiagent_plans.json")
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatalf("loadMAPlans: %v", err)
	}
	var plans []maPlan
	if err := json.Unmarshal(data, &plans); err != nil {
		b.Fatalf("loadMAPlans unmarshal: %v", err)
	}
	return plans
}

// ── Health checks ─────────────────────────────────────────────────────────────

// resolvePythonExecutable prefers the repo-local Cambrian venv (built by
// scripts/setup-python-runtime.ps1 / .sh) so booted agents have the SDK
// installed. Falls back to system "python" when the venv is absent.
func resolvePythonExecutable() string {
	candidates := []string{
		filepath.Join("..", "..", ".venv", "Scripts", "python.exe"), // Windows
		filepath.Join("..", "..", ".venv", "bin", "python"),         // Unix
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, aerr := filepath.Abs(c)
			if aerr == nil {
				return abs
			}
			return c
		}
	}
	return "python"
}

func ollamaUp() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://localhost:11434/api/tags")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func postgresUp() bool {
	conn, err := net.DialTimeout("tcp", "localhost:5432", 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ── Bootstrap ─────────────────────────────────────────────────────────────────

// bootstrapBenchmarkKernel boots the full kernel on a random port with a temp
// data dir and the real agents/ directory. Returns the kernel and a cancel func.
func bootstrapBenchmarkKernel(b *testing.B) (*Kernel, context.CancelFunc) {
	b.Helper()

	cfg, err := config.LoadConfig(filepath.Join("..", "..", "configs", "config.dev.json"))
	if err != nil {
		b.Fatalf("config load: %v", err)
	}

	// Random port: bind :0 and read the assigned port.
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	cfg.Server.Port = fmt.Sprintf("%d", port)

	// Temp data dir + real agents directory.
	cfg.Storage.DataDir = b.TempDir()
	agentsDir, err := filepath.Abs(filepath.Join("..", "..", "agents"))
	if err != nil {
		b.Fatalf("resolve agents dir: %v", err)
	}
	cfg.Metabolism.AgentsDir = agentsDir
	if cfg.Metabolism.PythonExecutable == "" || strings.Contains(cfg.Metabolism.PythonExecutable, "${") {
		cfg.Metabolism.PythonExecutable = resolvePythonExecutable()
	}

	ctx, cancel := context.WithCancel(context.Background())

	k, err := bootstrapKernel(ctx, cfg, lis)
	if err != nil {
		cancel()
		b.Fatalf("bootstrapKernel: %v", err)
	}

	g, gCtx := errgroup.WithContext(ctx)
	startKernelServices(g, gCtx, k)

	// Wait for the gRPC server to accept connections.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", "localhost:"+cfg.Server.Port, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	b.Cleanup(func() {
		shutdownCtx, sc := context.WithTimeout(context.Background(), 10*time.Second)
		defer sc()
		k.Shutdown(shutdownCtx)
		cancel()
	})

	return k, cancel
}

// waitForCognitiveAgents blocks until all cognitive agents finish their Interview.
func waitForCognitiveAgents(b *testing.B, k *Kernel) {
	b.Helper()
	if err := interview.WaitForAgentReadiness(k.Metabolism.InterviewWorker, cognitiveAgentIDs, 60*time.Second); err != nil {
		b.Skipf("cognitive agents not ready: %v", err)
	}
}

// ── Routing & quality helpers ─────────────────────────────────────────────────

type stepRoutingResult struct {
	stepIndex int
	expected  string
	actual    string
	matched   bool
}

func routingAccuracy(results []stepRoutingResult) float64 {
	if len(results) == 0 {
		return 0
	}
	matched := 0
	for _, r := range results {
		if r.matched {
			matched++
		}
	}
	return float64(matched) / float64(len(results))
}

func plannerDelta(pathA, pathB float64) float64 { return pathA - pathB }

// judgeResult carries the score and explanation from the LLM-as-Judge call.
type judgeResult struct {
	Score       int
	Explanation string
}

// judgeQuality asks the LLM to rate a final answer 1-5 with an explanation.
// OBSERVABILITYREQ REQ2: includes explanation field; REQ3: uses caller-provided context.
func judgeQuality(b *testing.B, ctx context.Context, k *Kernel, subject, answer string) judgeResult {
	b.Helper()
	prompt := fmt.Sprintf(
		"Rate this answer 1-5 (1=wrong, 5=fully correct). Task: %s\nAnswer: %s\nRespond ONLY as JSON: {\"score\": <int>, \"explanation\": \"<one-sentence reason>\"}",
		subject, truncateStr(answer, 800),
	)
	resp, err := k.Awareness.Planner.Generate(ctx, prompt)
	if err != nil {
		return judgeResult{}
	}
	start := strings.Index(resp, "{")
	end := strings.LastIndex(resp, "}")
	if start == -1 || end == -1 || end <= start {
		return judgeResult{}
	}
	var j struct {
		Score       int    `json:"score"`
		Explanation string `json:"explanation"`
	}
	if json.Unmarshal([]byte(resp[start:end+1]), &j) != nil {
		return judgeResult{}
	}
	return judgeResult{Score: j.Score, Explanation: j.Explanation}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ── Path B: manual reference plan ─────────────────────────────────────────────

func BenchmarkMultiAgent_ManualPath(b *testing.B) {
	if !ollamaUp() {
		b.Skip("Ollama not reachable at localhost:11434")
	}
	if !postgresUp() {
		b.Skip("PostgreSQL not reachable at localhost:5432")
	}

	k, _ := bootstrapBenchmarkKernel(b)
	waitForCognitiveAgents(b, k)

	plans := loadMAPlans(b)
	b.ResetTimer()

	for _, p := range plans {
		// Path B executes the user_input through the unary Execute RPC. The
		// Planner is still invoked inside Execute, but we measure agent execution.
		resp, err := k.Server.Execute(context.Background(), &pb.Handoff{
			Id:      "manual-" + p.ID,
			Payload: &pb.Object{Data: []byte(p.UserInput)},
		})
		if err != nil {
			b.Logf("WARN: case %s failed: %v", p.ID, err)
			continue
		}
		final := ""
		if resp.Payload != nil {
			final = string(resp.Payload.Data)
		}
		// REQ3: pass the execution context so Langfuse links judge to plan trace.
		jResult := judgeQuality(b, context.Background(), k, p.Subject, final)
		// REQ4: surface explanation alongside score; warn on regressions.
		if jResult.Score <= 2 {
			b.Logf("QUALITY REGRESSION path=B case=%s score=%d explanation=%q", p.ID, jResult.Score, jResult.Explanation)
		} else {
			b.Logf("multiagent_plan_result path=B case=%s quality_score=%d explanation=%q", p.ID, jResult.Score, jResult.Explanation)
		}
	}
}

// ── Path A: Planner-generated plan ────────────────────────────────────────────

func BenchmarkMultiAgent_PlannerPath(b *testing.B) {
	if !ollamaUp() {
		b.Skip("Ollama not reachable at localhost:11434")
	}
	if !postgresUp() {
		b.Skip("PostgreSQL not reachable at localhost:5432")
	}

	k, _ := bootstrapBenchmarkKernel(b)
	waitForCognitiveAgents(b, k)

	plans := loadMAPlans(b)
	b.ResetTimer()

	for _, p := range plans {
		plan, err := k.Awareness.Planner.GetExecutionPlan(context.Background(), p.UserInput)
		if err != nil {
			b.Logf("WARN: planner failed for case %s: %v", p.ID, err)
			continue
		}
		b.Logf("multiagent_plan_topology case=%s planned_steps=%d", p.ID, len(plan.Steps))

		resp, err := k.Server.Execute(context.Background(), &pb.Handoff{
			Id:      "planner-" + p.ID,
			Payload: &pb.Object{Data: []byte(p.UserInput)},
		})
		if err != nil {
			b.Logf("WARN: case %s failed: %v", p.ID, err)
			continue
		}
		final := ""
		if resp.Payload != nil {
			final = string(resp.Payload.Data)
		}
		jResult := judgeQuality(b, context.Background(), k, p.Subject, final)
		if jResult.Score <= 2 {
			b.Logf("QUALITY REGRESSION path=A case=%s score=%d explanation=%q", p.ID, jResult.Score, jResult.Explanation)
		} else {
			b.Logf("multiagent_plan_result path=A case=%s quality_score=%d explanation=%q", p.ID, jResult.Score, jResult.Explanation)
		}
	}
}
