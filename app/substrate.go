package app

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/objects"
	"github.com/cambrian-sh/core/internal/ontology"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Substrate is the knowledge substrate's store set over one Postgres pool:
// evidence, typed events and observations, knowledge items, the identity plane
// and the ONE fenced query plane over all of them (ADR-0105/0106/0108/0111/0128).
//
// It is the single composition point for these stores. The kernel builds them
// through NewSubstrate in bootstrapKernel and nowhere else, and a test in another
// module (premium's end-to-end tests, ADR-0147 D15) builds them through the
// same function — so there is no second construction path to drift from the
// kernel's. Exported because premium cannot import internal/infrastructure/postgres.
//
// The stores here are the substrate's own: enforcement of who may read what is
// the query plane's (every read takes a TagPredicate) and the authorizer's on
// the plugin seams, exactly as when the kernel wires them.
type Substrate struct {
	Evidence  *postgres.PgEvidenceStore
	Events    *postgres.PgEventStore
	Knowledge *postgres.PgKnowledgeStore
	Entities  *postgres.PgEntityStore
	Links     *postgres.PgLinkStore
	Query     *postgres.PgQueryPlane
	// IdentityScans is the background identity producers' read port
	// (domain.IdentityScanSource, ENG-1393): namespace-wide paged scans with the
	// retirement rule and the access fence applied inside, so premium's producers
	// never read substrate tables with their own SQL.
	IdentityScans *postgres.PgIdentityScanSource
	// Objects reads the per-kind object tables (ADR-0149 D6 ListObjects, D7
	// GetObject); plugins reach it only through ObjectsSeam.
	Objects *objects.Reader
}

// ObjectsSeam is the plugin seam over s.Objects (KernelServices.Objects): it
// runs as a principal whose predicate authorizer resolves.
func (s *Substrate) ObjectsSeam(authorizer domain.Authorizer) domain.Objects {
	return authz.ScopedObjects(authorizer, s.Objects, slog.Default())
}

// NewSubstrate composes the substrate over pool. kinds yields the knowledge kind
// registry (ADR-0110) and relations the link-verb registry (five-planes step 2)
// for a namespace: the kernel passes its live ontology (ADR-0149 D2), a test or a
// tool may pass fixed registries. Either way they were validated by their own
// constructors before they get here.
func NewSubstrate(pool *pgxpool.Pool, kinds domain.KindSource, relations domain.RelationSource) *Substrate {
	s := &Substrate{
		Evidence:  postgres.NewPgEvidenceStore(pool),
		Events:    postgres.NewPgEventStore(pool, kinds),
		Knowledge: postgres.NewPgKnowledgeStore(pool, kinds),
		Entities:  postgres.NewPgEntityStore(pool, kinds),
		Links:     postgres.NewPgLinkStore(pool, relations),

		IdentityScans: postgres.NewPgIdentityScanSource(pool),
	}
	s.Query = postgres.NewPgQueryPlane(pool, s.Events, s.Knowledge, s.Evidence, s.Links, s.Entities, relations)
	s.Objects = objects.NewReader(pool, kinds, relations)
	return s
}

// LinkReview is the operator plane's review lane over this substrate (contract
// 0098 row-level, 0109 pair-level; ENG-2030): the review inbox and the pair
// inbox read through the query plane at the operator plane's scope, and the
// confirm/reject writes guarded by the same per-row judgement. The single
// construction of the lane — the kernel binds it to the operator console, and
// premium's end-to-end tests drive the same adapter (the NewSubstrate rule).
func (s *Substrate) LinkReview() operator.LinkWriter {
	return linkWriteSource{links: s.Links, review: s.Query, pairs: s.Links}
}

// Ontology is the live ontology declarations as the kernel composes them
// (ADR-0149 D2): Live is what the substrate stores take (NewSubstrate's kinds and
// relations), Seam is what plugins get (KernelServices.Ontology), with the
// violations report scoped by the authorizer.
type Ontology struct {
	Live *ontology.Ontology
	Seam domain.Ontology
}

// LoadOntology seeds and loads the live declarations over pool: seed is what code
// and config declare at boot, authorities the resolution policies kinds may name.
// The single construction path, used by bootstrapKernel and by premium's
// end-to-end tests alike (the NewSubstrate rule).
func LoadOntology(ctx context.Context, pool *pgxpool.Pool, seed domain.OntologyDoc,
	authorities []domain.ResolutionAuthority, authorizer domain.Authorizer) (Ontology, error) {
	live, err := ontology.Load(ctx, pool, seed, authorities)
	if err != nil {
		return Ontology{}, err
	}
	return Ontology{Live: live, Seam: authz.ScopedOntology(authorizer, live, slog.Default())}, nil
}

// Ingestor composes evidence capture over this substrate (ADR-0105): the bytes
// go to blobs, the row and its outbox item to the evidence store.
func (s *Substrate) Ingestor(blobs domain.ContentStore) (*evidence.Ingestor, error) {
	return evidence.NewIngestor(blobs, s.Evidence)
}

// Consumer composes the transformation stage (ADR-0108 D3): it drains the
// evidence outbox through the given transformers.
func (s *Substrate) Consumer(blobs domain.ContentStore, transformers []domain.EvidenceTransformer) (*evidence.Consumer, error) {
	return evidence.NewConsumer(s.Evidence, blobs, transformers, slog.Default())
}
