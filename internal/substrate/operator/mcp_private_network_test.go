package operator

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
)

// Contract 0114 (ENG-1545): SaveMCPServer carries the ADR-0153 private-network
// opt-in to the writer with its presence intact (absent = keep the stored value,
// present = the operator's decision), names the decision in the audit summary,
// and ListMCPServers reports the configured value so a console edit shows it.
func TestSaveMCPServer_CarriesThePrivateNetworkOptInWithPresence(t *testing.T) {
	s := newWriteService(nil, nil)
	w := &recordingMCPWriter{}
	s.SetMCPWriter(w)
	ctx := ContextWithPrincipal(context.Background(), "op", RoleOperator)
	on := true
	for i, opt := range []*bool{nil, &on} {
		if _, err := s.SaveMCPServer(ctx, &pb.SaveMCPServerOpRequest{CommandId: "c" + string(rune('0'+i)), Reason: "lan server",
			Server: &pb.MCPServerSpecOp{Id: "lan", Transport: "http", Endpoint: "http://10.1.2.3/mcp", AllowPrivateNetwork: opt}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.saved) != 2 || w.saved[0].AllowPrivateNetwork != nil || w.saved[1].AllowPrivateNetwork == nil || !*w.saved[1].AllowPrivateNetwork {
		t.Fatalf("writer saw %+v, want [absent, true]", w.saved)
	}
	if !strings.Contains(privateNetworkNote(&on), "allow_private_network: on") || privateNetworkNote(nil) != "" {
		t.Fatalf("audit note: %q / %q", privateNetworkNote(&on), privateNetworkNote(nil))
	}
}

func TestListMCPServers_ReportsThePrivateNetworkOptIn(t *testing.T) {
	t.Parallel()
	s := &Service{}
	s.SetWave1Reads(nil, stubMCP{configured: true, servers: []MCPServerInfo{
		{Name: "lan", Transport: "http", URL: "http://10.1.2.3/mcp", AllowPrivateNetwork: true},
		{Name: "pub", Transport: "http", URL: "https://mcp.example.com"},
	}}, nil, nil)
	resp, err := s.ListMCPServers(context.Background(), &pb.ListMCPServersOpRequest{})
	if err != nil || len(resp.GetServers()) != 2 {
		t.Fatalf("ListMCPServers: %+v %v", resp, err)
	}
	if !resp.GetServers()[0].GetAllowPrivateNetwork() || resp.GetServers()[1].GetAllowPrivateNetwork() {
		t.Fatalf("opt-in not reported: %+v", resp.GetServers())
	}
}
