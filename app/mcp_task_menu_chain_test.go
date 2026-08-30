package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	network "github.com/cambrian-sh/core/internal/substrate/network"
	session "github.com/cambrian-sh/core/internal/substrate/session"
)

// THE phase-4 gate (ADR-0126 D10 / ADR-0127 D4): the whole beneficiary chain,
// end to end, through the REAL pieces —
//
//	submit_task (mcpserve handler, principal off the context)
//	  → mcpTaskLane (session opened, beneficiary recorded in the task index)
//	  → an agent attends the task: its step lease is bound to the task session
//	  → network.ListTools resolves lease → session → TaskOwners → beneficiary
//	  → ToolExecutor.AvailableTools attaches the beneficiary's LIVE fleet
//
// The assertion pair is the ADR-0127 invariant: the submitter who owns a live
// worker sees local:<machine>/<tool> in the menu for THEIR task; a different
// principal's task never lists it.
func TestMCPTaskChain_ContributedMenuFollowsTaskBeneficiary(t *testing.T) {
	// Alice owns one live worker. Bob owns nothing.
	fleet := domain.NewInMemoryFleet()
	if err := fleet.RegisterWorker(domain.WorkerRegistration{
		Machine: "laptop",
		Owner:   domain.AgentPrincipal("mcp:alice"),
		Tools: []domain.SystemTool{{
			Name: "read_file", Description: "Read a file from this machine.",
			Effects: []domain.ToolEffect{domain.EffectRead},
		}},
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	fleet.SetLive("laptop", true)

	executor := &domain.ToolExecutor{
		Registry:     domain.NewInMemoryToolRegistry(),
		Unrestricted: true, // grants are not this gate's subject; CL-0 covers them
		Fleet:        fleet,
	}

	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(context.Context, string, string) (string, map[string]string, error) {
			return "done", nil, nil
		})

	// Submit through the REAL published tool handler, identity on the context
	// exactly as the endpoint middleware establishes it.
	var submit domain.PublishedToolHandler
	for _, e := range mcpserve.TaskTools(lane) {
		if e.Tool.Name == "submit_task" {
			submit = e.Handler
		}
	}
	submitAs := func(principal domain.PrincipalRef) string {
		t.Helper()
		ctx := domain.WithPrincipal(context.Background(), principal)
		res, err := submit.Invoke(ctx, json.RawMessage(`{"task":"work on my machine"}`))
		if err != nil {
			t.Fatalf("submit as %s: %v", principal, err)
		}
		return res.Structured.(map[string]any)["task_id"].(string)
	}
	taskAlice := submitAs(domain.AgentPrincipal("mcp:alice"))
	taskBob := submitAs(domain.AgentPrincipal("mcp:bob"))

	// The kernel's own lease machinery, exactly as Execute's stepFn binds it at
	// dispatch: a per-step lease bound to the task session.
	gw := network.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	})
	leaseFor := func(task string) string {
		t.Helper()
		lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			t.Fatalf("acquire lease: %v", err)
		}
		gw.BindLease(lease, domain.LeaseBinding{SessionID: domain.SessionID(task)})
		return string(lease)
	}

	srv := &network.Server{LLMGateway: gw, ToolExecutor: executor, TaskOwners: lane}

	menuFor := func(task string) []string {
		t.Helper()
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-lease-id", leaseFor(task), "x-agent-id", "attending-agent"))
		resp, err := srv.ListTools(ctx, &pb.ListToolsRequest{})
		if err != nil {
			t.Fatalf("ListTools for task %s: %v", task, err)
		}
		names := make([]string, 0, len(resp.Tools))
		for _, tool := range resp.Tools {
			names = append(names, tool.Name)
		}
		return names
	}

	aliceMenu := menuFor(taskAlice)
	found := false
	for _, n := range aliceMenu {
		if n == "local:laptop/read_file" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the beneficiary's task menu %v does not carry local:laptop/read_file — the chain is broken", aliceMenu)
	}

	bobMenu := menuFor(taskBob)
	for _, n := range bobMenu {
		if strings.HasPrefix(n, domain.LocalToolPrefix) {
			t.Fatalf("a DIFFERENT principal's task menu lists %q — one owner's fleet leaked into another owner's task", n)
		}
	}
}

// The machine spelling of the same chain: a worker machine submits FOR ITS
// OWNER, so the task it submits still resolves the owner's fleet.
func TestMCPTaskChain_MachineSubmissionCarriesItsOwner(t *testing.T) {
	fleet := domain.NewInMemoryFleet()
	if err := fleet.RegisterWorker(domain.WorkerRegistration{
		Machine: "laptop", Owner: domain.AgentPrincipal("mcp:alice"),
		Tools: []domain.SystemTool{{Name: "read_file", Effects: []domain.ToolEffect{domain.EffectRead}}},
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	fleet.SetLive("laptop", true)
	executor := &domain.ToolExecutor{
		Registry: domain.NewInMemoryToolRegistry(), Unrestricted: true, Fleet: fleet,
	}
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(context.Context, string, string) (string, map[string]string, error) { return "", nil, nil })

	var submit domain.PublishedToolHandler
	for _, e := range mcpserve.TaskTools(lane) {
		if e.Tool.Name == "submit_task" {
			submit = e.Handler
		}
	}
	machineCtx := domain.WithWorkerOwner(
		domain.WithPrincipal(context.Background(), domain.MachinePrincipal("laptop")),
		domain.AgentPrincipal("mcp:alice"))
	res, err := submit.Invoke(machineCtx, json.RawMessage(`{"task":"sync my notes"}`))
	if err != nil {
		t.Fatalf("machine submit: %v", err)
	}
	task := res.Structured.(map[string]any)["task_id"].(string)

	gw := network.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	})
	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: domain.SessionID(task)})

	srv := &network.Server{LLMGateway: gw, ToolExecutor: executor, TaskOwners: lane}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-lease-id", string(lease), "x-agent-id", "attending-agent"))
	resp, err := srv.ListTools(ctx, &pb.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range resp.Tools {
		if tool.Name == "local:laptop/read_file" {
			return // the owner's fleet reached the machine-submitted task
		}
	}
	t.Fatal("a machine-submitted task did not resolve its OWNER's fleet into the menu")
}
