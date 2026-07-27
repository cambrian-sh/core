package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ADR-0089: plugin identity rides the snapshot handshake, so a console can detect
// plugin-level skew the way it already detects contract skew.
func TestSnapshot_ReportsDeclaredPlugins(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetPlugins([]operator.PluginInfo{{
		ID:           "authz",
		DisplayName:  "Access Policy",
		Version:      "1.0.0",
		State:        "active",
		Capabilities: []string{"access-policy"},
		Panels:       []operator.PluginPanel{{ID: "scope", Title: "Access Policy", Capability: "access-policy"}},
	}})

	resp, err := svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(resp.GetPlugins()) != 1 {
		t.Fatalf("expected 1 plugin on the handshake, got %d", len(resp.GetPlugins()))
	}
	p := resp.GetPlugins()[0]
	if p.GetId() != "authz" || p.GetVersion() != "1.0.0" || p.GetState() != "active" {
		t.Errorf("plugin identity drifted: %+v", p)
	}
	if len(p.GetCapabilities()) != 1 || p.GetCapabilities()[0] != "access-policy" {
		t.Errorf("capabilities drifted: %v", p.GetCapabilities())
	}
	if len(p.GetPanels()) != 1 || p.GetPanels()[0].GetCapability() != "access-policy" {
		t.Errorf("panels drifted: %+v", p.GetPanels())
	}
}

// A plugin that did NOT register is still reported, with its reason. Otherwise a
// console cannot tell "this deployment has no reactive engine" from "the reactive
// engine declined to register", and the operator who paid for it gets silence.
func TestSnapshot_ReportsUnregisteredPluginsWithReason(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetPlugins([]operator.PluginInfo{
		{ID: "reactive", DisplayName: "Reactive Engine", Version: "2.1.0", State: "not_entitled", Reason: "licence expired"},
		{ID: "chatops", Version: "0.3.0", State: "deps_unmet", Missing: []string{"reactive"}},
	})

	resp, err := svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(resp.GetPlugins()) != 2 {
		t.Fatalf("an unregistered plugin must still appear, got %d", len(resp.GetPlugins()))
	}
	if got := resp.GetPlugins()[0]; got.GetState() != "not_entitled" || got.GetReason() != "licence expired" {
		t.Errorf("the reason a plugin is absent must reach the console: %+v", got)
	}
	if got := resp.GetPlugins()[1]; len(got.GetMissing()) != 1 || got.GetMissing()[0] != "reactive" {
		t.Errorf("unmet dependencies must reach the console: %+v", got)
	}
}

// An OSS kernel declares no plugins, so the list is empty rather than absent-but-
// implied. An empty list is what keeps the console from inventing a skew warning
// for a deployment that simply has no plugins.
func TestSnapshot_OSSReportsNoPlugins(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetHandshake("0.6.9-alpha", "0067", []string{"feed", "snapshot"})

	resp, err := svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(resp.GetPlugins()) != 0 {
		t.Errorf("an OSS kernel must report no plugins, got %+v", resp.GetPlugins())
	}
	if resp.GetContractVersion() != "0067" {
		t.Errorf("contract version drifted: %q", resp.GetContractVersion())
	}
}
