package app

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// closedCeilingDecisionPoint restates the premium write rule as amended by ENG-2161
// (ADR-0035 C2): the classification is the writer's ceiling narrowed by the hint, the
// whole ceiling for an empty hint — EXCEPT that an empty hint under a ceiling holding
// closed tags is refused with ReasonClassificationRequired, naming the ceiling and its
// closed tags. Core cannot import premium (AGENTS.md §1.2), so the rule is restated
// here; premium's own tests pin the rule on the real Authorizer. The property under
// test is the KERNEL's half: the operator plane refuses with InvalidArgument naming
// the tags, and nothing is stored.
type closedCeilingDecisionPoint struct {
	domain.AllowAllAuthorizer
	ceilings map[string][]string
	closed   []string
}

func (c closedCeilingDecisionPoint) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	if p.Kind == domain.PrincipalSystem {
		return hint, domain.AccessDecision{Allowed: true, Reason: domain.ReasonBypass}
	}
	ceiling := c.ceilings[p.WriterID()]
	var req []string
	for _, t := range hint {
		if !strings.HasPrefix(t, "provenance:") && !domain.IsOwnerTag(t) {
			req = append(req, t)
		}
	}
	if len(req) == 0 {
		var closed []string
		for _, t := range ceiling {
			if slices.Contains(c.closed, t) {
				closed = append(closed, t)
			}
		}
		if len(closed) > 0 {
			return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonClassificationRequired,
				ClassificationChoice: &domain.WriteClassificationChoice{Ceiling: ceiling, Closed: closed}}
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

// ENG-2161 end to end (owner decision 2026-11-19, B): through the operator plane's
// real IngestMemory RPC bound to the composition root's ingestor
// (operatorMemoryIngestor), over a migrated Postgres and the memory stack
// NewMemoryStack builds with the decision point, an UNTAGGED upload by an operator
// whose write ceiling holds closed tags is refused with InvalidArgument naming the
// closed tags and the ceiling, and nothing is stored. The same operator choosing a
// classification from the ceiling (an open tag alone) is stored with exactly it, and
// an operator whose ceiling is open-only keeps the ADR-0035 C2 default (the ceiling).
func TestPg_E2E_UntaggedUploadUnderClosedCeilingIsRefused(t *testing.T) {
	dsn, dim := pgscratch.Migrated(t)
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)
	cfg.Execution.Facts.ExtractEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	closedOp, openOp := "eng2161-op-"+stamp, "eng2161-open-"+stamp
	dp := closedCeilingDecisionPoint{
		ceilings: map[string][]string{
			closedOp: {"scope-public", "scope-finance", "scope-hr"},
			openOp:   {"scope-public", "scope-docs"},
		},
		closed: []string{"scope-finance", "scope-hr"},
	}
	mem := kernel.NewMemoryStack(kernel.NewMemoryBackend(vec), nil, e2eHashEmbedder{dim: cfg.Embedder.Dimensions},
		cfg.Execution, dp, cfg.Chunker, nil, kernel.FactSubstrate{}, kernel.RetrievalDeps{})
	if mem.ChunkTripletsBatcher != nil {
		mem.ChunkTripletsBatcher.UseExtractor(noTriplets{})
		mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mem.Start(runCtx) }()
	t.Cleanup(func() { cancel(); <-done; mem.Shutdown(context.Background()) })

	audit, err := postgres.NewPgAuditStore(ctx, vec.Pool())
	if err != nil {
		t.Fatal(err)
	}
	op := operatortest.New(t, nil, func(d *operator.Deps, _ *operator.Options) {
		d.Audit, d.Ingestor = audit, operatorMemoryIngestor(mem.IngestionManager)
	})

	token := func(name string) string { return "zqeng2161" + name + stamp }
	t.Cleanup(func() {
		for _, name := range []string{"untagged", "chosen", "openonly"} {
			tok := token(name)
			_, _ = vec.Pool().Exec(context.Background(), `DELETE FROM chunks WHERE text LIKE '%' || $1 || '%'`, tok)
			_, _ = vec.Pool().Exec(context.Background(), `DELETE FROM document_versions WHERE document_id IN (SELECT id FROM documents WHERE title = $1)`, tok)
			_, _ = vec.Pool().Exec(context.Background(), `DELETE FROM documents WHERE title = $1`, tok)
		}
	})
	upload := func(writer, name string, tags []string) (*pb.IngestMemoryOpResponse, error) {
		tok := token(name)
		return op.IngestMemory(domain.WithPrincipal(ctx, domain.UserPrincipal(writer)), &pb.IngestMemoryOpRequest{
			CommandId: "eng2161-" + name + "-" + stamp, Reason: "ENG-2161 e2e", Title: tok, Tags: tags,
			Text: "The quarterly review of " + tok + " moved to Thursday after the board asked for the revised figures.",
		})
	}
	rows := func(name string) (docs, chunks int) {
		t.Helper()
		tok := token(name)
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM documents WHERE title = $1`, tok).Scan(&docs); err != nil {
			t.Fatal(err)
		}
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE text LIKE '%' || $1 || '%'`, tok).Scan(&chunks); err != nil {
			t.Fatal(err)
		}
		return docs, chunks
	}
	docTags := func(name string) []string {
		t.Helper()
		var tags []string
		if err := vec.Pool().QueryRow(ctx, `SELECT tags FROM documents WHERE title = $1`, token(name)).Scan(&tags); err != nil {
			t.Fatalf("tags of %s: %v", name, err)
		}
		return tags
	}

	// The readiness repro: an untagged upload under a ceiling holding closed tags.
	_, err = upload(closedOp, "untagged", nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("untagged upload under a closed ceiling: want InvalidArgument, got %v", err)
	}
	msg := status.Convert(err).Message()
	for _, w := range []string{"scope-finance", "scope-hr", "scope-public", "nothing was stored"} {
		if !strings.Contains(msg, w) {
			t.Errorf("refusal must name %q: %q", w, msg)
		}
	}
	if d, c := rows("untagged"); d != 0 || c != 0 {
		t.Fatalf("a refused upload stored %d document(s) and %d chunk(s)", d, c)
	}

	// Choosing from the ceiling — the open tag alone — is stored with exactly it.
	if _, err := upload(closedOp, "chosen", []string{"scope-public"}); err != nil {
		t.Fatalf("an upload classified from the ceiling: %v", err)
	}
	if got := docTags("chosen"); !slices.Contains(got, "scope-public") || slices.Contains(got, "scope-hr") || slices.Contains(got, "scope-finance") {
		t.Fatalf("chosen: document tags %v, want scope-public and no closed tag", got)
	}

	// An open-only ceiling keeps the default: the whole ceiling.
	if _, err := upload(openOp, "openonly", nil); err != nil {
		t.Fatalf("untagged upload under an open-only ceiling must still be stored: %v", err)
	}
	if got := docTags("openonly"); !slices.Contains(got, "scope-public") || !slices.Contains(got, "scope-docs") {
		t.Fatalf("openonly: document tags %v, want the whole ceiling", got)
	}
}
