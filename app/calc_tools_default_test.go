package app

import (
	"context"
	"slices"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/calc"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"google.golang.org/grpc/metadata"
)

// ENG-897 (owner decision 2026-11-15): with the shipped defaults (the single-agent
// lane, calc_tools on) the calculation tools are wired and appear on an agent's
// ListTools menu with no grant — the menu the lane's agent is served.
func TestCalcTools_DefaultConfigPutsThemOnTheAgentsMenu(t *testing.T) {
	cfg := config.DefaultConfig()
	if !cfg.Execution.Lane.IsSingleAgent() {
		t.Fatal("the default lane must be single_agent")
	}
	srv := &subnetwork.Server{ToolExecutor: &domain.ToolExecutor{
		Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore()}}
	wireCalcTools(cfg, srv, domain.NewCalendar(time.UTC, time.Now))

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "uc-personal-assistant"))
	resp, err := srv.ListTools(ctx, &pb.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, d := range resp.GetTools() {
		names = append(names, d.GetName())
	}
	for _, want := range calc.Names {
		if !slices.Contains(names, want) {
			t.Errorf("%s missing from the default agent menu %v", want, names)
		}
	}

	cfg.Execution.Tools.CalcTools = false
	off := &subnetwork.Server{ToolExecutor: &domain.ToolExecutor{
		Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore()}}
	wireCalcTools(cfg, off, domain.NewCalendar(time.UTC, time.Now))
	if len(off.ToolExecutor.KernelTools) != 0 {
		t.Error("calc_tools=false must serve no calculation tools")
	}
}
