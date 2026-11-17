package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

func newOutputsServer(t *testing.T) (*Server, *SubstrateLLMGateway, *memLedgerStore) {
	t.Helper()
	s, gw := newLeaseServer(t)
	led, raw := newArtifactLedger(domain.AllowAllAuthorizer{})
	s.Ledger = led
	s.ExecCfg.Ledger.Artifacts = config.LedgerArtifactsConfig{MaxFileBytes: 32, MaxCollectFiles: 2}
	return s, gw, raw
}

// An upload carries the run, session and step of the lease it presented, and the
// lease is then remembered for the calling instance: a later upload by the same
// instance without the header lands in the same step.
func TestUploadArtifact_ProvenanceFromLeaseThenInstance(t *testing.T) {
	t.Parallel()
	s, gw, raw := newOutputsServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "analyst", RunID: "run-u", StepIndex: 2, SessionID: "sess-u"})
	ctx := domain.WithAgentInstance(agentPlaneCtx("analyst", leaseHeader, string(lease), "x-agent-id", "analyst"), "inst-u")
	ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal("analyst"))
	r1, err := s.UploadArtifact(ctx, &pb.UploadArtifactRequest{Content: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	e := raw.rows[r1.Hash]
	if e.RunID != "run-u" || e.StepIndex != 2 || e.SessionID != "sess-u" || e.InstanceID != "inst-u" {
		t.Fatalf("lease provenance: %+v", e)
	}
	ctx2 := domain.WithPrincipal(domain.WithAgentInstance(agentPlaneCtx("analyst", "x-agent-id", "analyst"), "inst-u"),
		domain.AgentPrincipal("analyst"))
	r2, err := s.UploadArtifact(ctx2, &pb.UploadArtifactRequest{Content: []byte("two")})
	if err != nil {
		t.Fatal(err)
	}
	if e2 := raw.rows[r2.Hash]; e2.RunID != "run-u" || e2.SessionID != "sess-u" {
		t.Fatalf("instance-remembered provenance: %+v", e2)
	}
}

// out/ collection: regular files become artifact entries attributed to the
// instance in the step of its last lease; a file over max_file_bytes and files
// past max_collect_files are named, not recorded; the instance's record is
// forgotten afterwards.
func TestCollectAgentOutputs_RecordsCapsAndAttributes(t *testing.T) {
	t.Parallel()
	s, gw, raw := newOutputsServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "analyst", RunID: "run-c", StepIndex: 1, SessionID: "sess-c"})
	ctx := domain.WithPrincipal(domain.WithAgentInstance(agentPlaneCtx("analyst", "x-agent-id", "analyst"), "inst-c"),
		domain.AgentPrincipal("analyst"))
	if _, known := s.resolveLeaseFor(ctx, lease); !known {
		t.Fatal("lease not resolved")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.csv", "x,y\n")
	write("big.bin", strings.Repeat("z", 33))
	write("sub/b.txt", "b")
	write("sub/c.txt", "c") // the third small file: over max_collect_files (2)
	n, err := s.CollectAgentOutputs(context.Background(), domain.AgentOutputs{AgentID: "analyst", InstanceID: "inst-c", Dir: dir})
	if n != 2 || err == nil || !strings.Contains(err.Error(), "big.bin") || !strings.Contains(err.Error(), "sub/c.txt") {
		t.Fatalf("collect: n=%d err=%v", n, err)
	}
	var got []string
	for _, e := range raw.rows {
		if e.ToolName != agentOutToolName {
			continue
		}
		if e.Kind != domain.LedgerArtifact || e.Writer != "agent:analyst" || e.InstanceID != "inst-c" ||
			e.RunID != "run-c" || e.StepIndex != 1 || e.SessionID != "sess-c" {
			t.Fatalf("out/ entry: %+v", e)
		}
		got = append(got, string(e.Args))
	}
	if len(got) != 2 {
		t.Fatalf("out/ entries = %v", got)
	}
	if _, ok := s.instanceWork().lastFor("inst-c"); ok {
		t.Fatal("the exited instance's lease record was kept")
	}
	// A missing out/ is nothing to collect.
	if n, err := s.CollectAgentOutputs(context.Background(), domain.AgentOutputs{AgentID: "analyst", InstanceID: "x", Dir: filepath.Join(dir, "nope")}); n != 0 || err != nil {
		t.Fatalf("missing dir: %d %v", n, err)
	}
}
