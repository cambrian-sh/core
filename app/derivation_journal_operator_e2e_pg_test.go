package app

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ENG-2139 end to end through the production composition: a really bootstrapped
// kernel (scratch database, derivation journal on by default), the operator plane
// built by composeOperator, the "derivation-journal" capability advertised, a dead
// job in derivation_jobs listed by GetDerivationJournal with its last error, and
// RequeueDerivationJob making it live and due once (a second requeue is NotFound).
func TestPg_DerivationJournalOnTheOperatorPlane(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	k, err := bootstrapKernel(ctx, cfg, lis, Options{}, nil)
	if err != nil {
		t.Fatalf("bootstrapKernel: %v", err)
	}
	defer shutdownWithin(t, k, 30*time.Second)
	svc, err := composeOperator(ctx, k, operator.NewSpool(operator.SpoolConfig{}), operator.NewProjection())
	if err != nil {
		t.Fatal(err)
	}
	advertised := false
	for _, c := range fieldAny(svc, "capabilities").([]string) {
		advertised = advertised || c == "derivation-journal"
	}
	if !advertised {
		t.Fatal(`"derivation-journal" is not advertised although the journal runs by default`)
	}

	u := &url.URL{Scheme: "postgres", User: url.UserPassword(cfg.Database.User, cfg.Database.Password),
		Host: cfg.Database.Host + ":" + cfg.Database.Port, Path: "/" + cfg.Database.DBName, RawQuery: "sslmode=disable"}
	db, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	const doc = "e2e-eng2139-doc"
	if _, err := db.Exec(ctx, `INSERT INTO derivation_jobs (kind, document_id, content_digest, chunk_ids, attempts, last_error, dead_at)
		VALUES ('kg_triplets', $1, 'v1', ARRAY['c1','c2'], 5, 'provider 503', now())`, doc); err != nil {
		t.Fatalf("seed a dead job: %v", err)
	}

	opctx := ctx // composeOperator's handlers are called directly; the interceptor would add the principal
	resp, err := svc.GetDerivationJournal(opctx, &pb.GetDerivationJournalOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var found *pb.DeadDerivationOp
	for _, d := range resp.Dead {
		if d.DocumentId == doc {
			found = d
		}
	}
	if found == nil || found.Kind != "kg_triplets" || found.LastError != "provider 503" || found.Attempts != 5 || found.ChunkCount != 2 {
		t.Fatalf("dead job not listed: %+v", resp.Dead)
	}
	deadKG := int32(0)
	for _, kb := range resp.Kinds {
		if kb.Kind == "kg_triplets" {
			deadKG = kb.Dead
		}
	}
	if deadKG < 1 {
		t.Fatalf("backlog does not count the dead job: %+v", resp.Kinds)
	}

	req := &pb.RequeueDerivationJobOpRequest{CommandId: "c-e2e-1", Reason: "provider recovered", Kind: "kg_triplets", DocumentId: doc, ContentDigest: "v1"}
	if _, err := svc.RequeueDerivationJob(opctx, req); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	var attempts int
	var dead bool
	if err := db.QueryRow(ctx, `SELECT attempts, dead_at IS NOT NULL FROM derivation_jobs WHERE document_id = $1`, doc).Scan(&attempts, &dead); err != nil {
		t.Fatal(err)
	}
	if dead || attempts != 0 {
		t.Fatalf("requeued row: attempts=%d dead=%v", attempts, dead)
	}
	req.CommandId = "c-e2e-2"
	if _, err := svc.RequeueDerivationJob(opctx, req); status.Code(err) != codes.NotFound {
		t.Fatalf("second requeue: %v, want NotFound", err)
	}
}
