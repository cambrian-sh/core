package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/migrate"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

var dialect = goqu.Dialect("postgres")

// Pool exposes the underlying pgx pool so sibling stores (e.g. the ADR-0034
// agent_scopes store) can reuse the same connection pool instead of opening a
// second one. ADR-0034 (R1).
func (p *PgVectorAdapter) Pool() *pgxpool.Pool { return p.pool }

const (
	TableDocuments         = "documents"
	TableEdges             = "document_edges"
	TableChunkTriplets     = "chunk_triplets"
	TableChunkPagerank     = "chunk_pagerank"
	TableChunkPagerankMeta = "chunk_pagerank_meta"
)

func mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("substrate: postgres %s failure: %w", op, err)
}

type PgVectorAdapter struct {
	pool        *pgxpool.Pool
	dim         int  // Dynamic dimension support (ADR-0012)
	autoMigrate bool // PLAT-02 / ADR-0064: run the migration runner at boot (default true)
}

// scanDocument is the central data-integrity gate.
func scanDocument(row pgx.Row, includeDistance bool) (domain.Document, float64, error) {
	var doc domain.Document
	var metadataBytes []byte
	var distance float64
	var lastAccessedAt *time.Time
	var embeddingVec *pgvector.Vector

	dest := []any{
		&doc.ID, &doc.Text, &metadataBytes, &doc.AccessCount,
		&doc.ActivationStrength, &doc.ScoringPromptVersion, &lastAccessedAt, &doc.CreatedAt, &doc.DocumentType, &doc.Version,
		&embeddingVec, &doc.Summary, // ADR-0048 #1: summary is the 12th SELECT column (before the appended distance)
		&doc.SectionPath, // ADR-0060: 13th column; NOT NULL DEFAULT '' in the baseline, so no null guard
	}

	if includeDistance {
		dest = append(dest, &distance)
	}

	if err := row.Scan(dest...); err != nil {
		return domain.Document{}, 0, err
	}

	if embeddingVec != nil {
		doc.Embedding = domain.Embedding{Vector: embeddingVec.Slice()}
	}

	if err := json.Unmarshal(metadataBytes, &doc.Metadata); err != nil {
		return domain.Document{}, 0, fmt.Errorf("metadata integrity error: %w", err)
	}

	if lastAccessedAt != nil {
		doc.LastAccessedAt = *lastAccessedAt
	}

	return doc, distance, nil
}

// NewPgVectorAdapter establishes a connection pool to the PostgreSQL database.
func NewPgVectorAdapter(ctx context.Context, cfg *config.Config) (*PgVectorAdapter, error) {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.Database.Host, cfg.Database.Port, cfg.Database.User, cfg.Database.Password, cfg.Database.DBName,
	)

	pgxCfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}

	pgxCfg.MaxConns = 50
	// Keep a warm floor of idle connections. With MinConns=0 the pool was fully
	// lazy, so the first concurrent burst of queries (e.g. the Gatekeeper scoring
	// every auction candidate at once) triggered a dozen+ simultaneous cold
	// handshakes — each also paying the AfterConnect RegisterTypes round-trip —
	// which blew the callers' step deadline together ("context deadline exceeded"
	// on profile fetch). A warm floor absorbs that first burst.
	pgxCfg.MinConns = 8
	// Bound connection establishment so a stalled handshake fails fast and clearly
	// instead of hanging until the caller's (possibly large) context deadline.
	pgxCfg.ConnConfig.ConnectTimeout = 10 * time.Second
	// ADR-0054 recall tuning: raise HNSW ef_search so the seed search can actually
	// return a large candidate pool. pgvector's default ef_search=40 caps the
	// number of candidates HNSW considers, so a bigger recall_over_fetch LIMIT is
	// useless unless ef_search >= it. Set per-connection (GUC). 0 ⇒ a safe 100.
	efSearch := cfg.Execution.HnswEfSearch
	if efSearch <= 0 {
		efSearch = 100
	}
	pgxCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvector.RegisterTypes(ctx, conn); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, fmt.Sprintf("SET hnsw.ef_search = %d", efSearch))
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		return nil, err
	}

	// Eagerly establish the warm floor at boot (pgxpool fills MinConns lazily via
	// its background health check, which would not help the very first auction).
	// Each Acquire runs AfterConnect/RegisterTypes once; releasing returns the live
	// connection to the idle pool. Best-effort: a warm-up failure is logged, not fatal.
	warm := make([]*pgxpool.Conn, 0, pgxCfg.MinConns)
	for i := int32(0); i < pgxCfg.MinConns; i++ {
		c, aerr := pool.Acquire(ctx)
		if aerr != nil {
			slog.Warn("Substrate: Postgres pool warm-up incomplete", "established", i, "err", aerr)
			break
		}
		warm = append(warm, c)
	}
	for _, c := range warm {
		c.Release()
	}

	// Dynamic Dimension (Audit 1): ADR-0042 — embedder owns its dimensions.
	// SAFETY (post-incident): a zero/missing embedder dimension used to silently
	// default to 1536. On a crash-boot where the embedder config fails to load,
	// that guessed dim can mismatch the real documents table and trigger a
	// DESTRUCTIVE recreate (the vector-store wipe we saw). Refuse to boot rather
	// than guess, so a config hiccup can never silently wipe the corpus.
	dims := cfg.Embedder.Dimensions
	if dims == 0 {
		return nil, fmt.Errorf(
			"embedder dimensions not configured (embedder.dimensions==0): refusing to boot with a " +
				"guessed default to avoid a destructive documents-table recreate on a dimension mismatch; " +
				"set embedder.dimensions explicitly (e.g. 1024 for bge-large)")
	}

	p := &PgVectorAdapter{pool: pool, dim: dims, autoMigrate: cfg.Storage.AutoMigrate}

	// REDEMPTION: migration is no longer a side effect but a controlled startup step.
	// In production, do this with an external 'migrate' tool.
	if err := p.ensureSchema(ctx); err != nil {
		return nil, err
	}

	return p, nil
}

// ChunkTripletsStore returns the adapter itself as a memory.ChunkTripletsStore.
// The pgvector adapter already implements the full ChunkTripletsStore
// interface (ForChunk, ForChunks, ChunksMentioningEntity, SaveChunkTriplets);
// this helper exists so external tools can grab the interface handle
// without a type assertion. (ADR-0053 Phase 0.)
func (p *PgVectorAdapter) ChunkTripletsStore() memory.ChunkTripletsStore { return p }

func (p *PgVectorAdapter) Close() {
	if p.pool != nil {
		p.pool.Close()
		slog.Info("🔌 Substrate: Postgres pool drained.")
	}
}

func (p *PgVectorAdapter) ensureSchema(ctx context.Context) error {
	// REDEMPTION: uses p.dim instead of a hardcoded dimension.

	// ADR-0021: Detect dimension mismatch (e.g. switching from 1536 to 768).
	// pgvector does not support ALTER COLUMN on VECTOR types; the only migration
	// path is destructive recreation.
	//
	// SAFETY GUARD (post-incident): an embedder dim change — OR a misconfigured
	// dim (cfg.Embedder.Dimensions==0 silently defaults to 1536 above) — must
	// never SILENTLY destroy the corpus. A restart once wiped 5882 ingested
	// chunks this way. So a mismatch is only allowed to recreate when it cannot
	// lose real data:
	//   - No memory documents present (only system-seeded tool/skill/agent_profile,
	//     which the boot path recreates anyway) ⇒ recreate freely.
	//   - Memory documents present ⇒ REFUSE TO BOOT with a loud, actionable error,
	//     UNLESS the operator explicitly opts in via ALLOW_DESTRUCTIVE_DIM_MIGRATION=1.
	// This makes a destructive migration an explicit operator decision, never a
	// silent side effect of a normal restart.
	var existingDim int
	_ = p.pool.QueryRow(ctx, `
		SELECT atttypmod
		FROM pg_attribute
		WHERE attrelid = $1::regclass AND attname = 'embedding'
	`, TableDocuments).Scan(&existingDim)
	if existingDim != 0 && existingDim != p.dim {
		// Count documents that a drop would DESTROY — exclude system-seeded types
		// (tool/skill/agent_profile), which are recreated on every boot regardless.
		var memDocs int
		// SAFETY: if the count itself fails (e.g. a degraded DB right after a
		// disk-full crash), do NOT treat that as "empty" and wipe — refuse. A
		// swallowed error here is exactly how a crash could silently drop a
		// populated corpus.
		if err := p.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(*) FROM %s WHERE document_type NOT IN ('tool','skill','agent_profile')`,
			TableDocuments,
		)).Scan(&memDocs); err != nil {
			return fmt.Errorf(
				"REFUSING to recreate %s: embedding dimension mismatch (table=%d, configured=%d) and "+
					"could not verify the document count (%w) — refusing a destructive recreate that could "+
					"wipe the corpus; retry once the database is healthy",
				TableDocuments, existingDim, p.dim, err)
		}

		allowDestructive := os.Getenv("ALLOW_DESTRUCTIVE_DIM_MIGRATION") == "1"
		if memDocs > 0 && !allowDestructive {
			return fmt.Errorf(
				"REFUSING to recreate %s: embedding dimension mismatch (table=%d, configured=%d) "+
					"with %d memory document(s) present — dropping would DESTROY the corpus. "+
					"Make the embedder dimension in config match the table (e.g. the table is %d), "+
					"or re-embed the corpus at the new dimension. To intentionally wipe and recreate, "+
					"restart once with ALLOW_DESTRUCTIVE_DIM_MIGRATION=1.",
				TableDocuments, existingDim, p.dim, memDocs, existingDim,
			)
		}

		slog.Warn("🔧 Substrate: VECTOR dimension changed; recreating documents table",
			"old_dim", existingDim, "new_dim", p.dim, "mem_docs_dropped", memDocs, "explicit_opt_in", allowDestructive)
		if _, err := p.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE;", TableEdges)); err != nil {
			return fmt.Errorf("drop edges table for dimension migration: %w", err)
		}
		if _, err := p.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE;", TableDocuments)); err != nil {
			return fmt.Errorf("drop documents table for dimension migration: %w", err)
		}
		// PLAT-02 / ADR-0064: the corpus tables were just dropped for a dimension
		// change; forget the recorded migrations so the runner re-applies the baseline
		// (0001) — and any deltas — to recreate them at the new dimension. The
		// migrations are idempotent, so the re-apply is safe.
		if _, err := p.pool.Exec(ctx, "DROP TABLE IF EXISTS schema_migrations;"); err != nil {
			return fmt.Errorf("reset schema_migrations for dimension migration: %w", err)
		}
	}

	return p.runMigrations(ctx)
}

// runMigrations creates the schema by applying the migration runner (baseline 0001 +
// pending deltas) after the dimension guard has run (PLAT-02 / ADR-0064). Gated by
// storage.auto_migrate (default true); when off, boot skips schema creation entirely
// and the operator must run `migrate up` / manage migrations externally.
func (p *PgVectorAdapter) runMigrations(ctx context.Context) error {
	if !p.autoMigrate {
		return nil
	}
	return migrate.Migrate(ctx, p.pool, p.dim)
}

// migrationPool opens a plain pool for the CLI migration paths (no vector-type
// registration needed to run DDL).
func migrationPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.Database.Host, cfg.Database.Port, cfg.Database.User, cfg.Database.Password, cfg.Database.DBName)
	return pgxpool.New(ctx, connStr)
}

// RunMigrations applies every pending migration (baseline 0001 + forward deltas) via the
// runner, then closes the pool. It is the `migrate up` CLI path — the runner every
// downstream consumer (installer, K8s init, benchmark reset) can call. PLAT-02 / ADR-0064.
func RunMigrations(ctx context.Context, cfg *config.Config) error {
	dim := cfg.Embedder.Dimensions
	if dim == 0 {
		return fmt.Errorf("embedder.dimensions must be set (non-zero) to run migrations")
	}
	pool, err := migrationPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("migrate: connect: %w", err)
	}
	defer pool.Close()
	return migrate.Migrate(ctx, pool, dim)
}

// MigrationStatus returns the applied/pending migration list for the `migrate status` CLI.
func MigrationStatus(ctx context.Context, cfg *config.Config) ([]migrate.Record, error) {
	pool, err := migrationPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}
	defer pool.Close()
	return migrate.Status(ctx, pool)
}

// getUpsertBuilder: Hem Save hem SaveBatch için tek bir kaynak (Audit 3)
func (p *PgVectorAdapter) getUpsertBuilder(doc *domain.Document) *goqu.InsertDataset {
	metadataBytes, _ := json.Marshal(doc.Metadata)

	record := goqu.Record{
		"id":                     doc.ID,
		"text":                   doc.Text,
		"summary":                doc.Summary,
		"metadata":               metadataBytes,
		"access_count":           doc.AccessCount,
		"activation_strength":    doc.ActivationStrength,
		"scoring_prompt_version": doc.ScoringPromptVersion,
		"last_accessed_at":       doc.LastAccessedAt,
		"document_type":          doc.DocumentType,
		"version":                doc.Version,
	}
	// REQ-DATA-1: only include created_at when explicitly set so PostgreSQL's
	// DEFAULT NOW() fires for new inserts. Go zero-value would store 0001-01-01,
	// breaking Ebbinghaus decay GC age predicates.
	if !doc.CreatedAt.IsZero() {
		record["created_at"] = doc.CreatedAt
	}
	update := goqu.Record{
		"text":                   goqu.L("EXCLUDED.text"),
		"summary":                goqu.L("EXCLUDED.summary"),
		"metadata":               goqu.L("EXCLUDED.metadata"),
		"activation_strength":    goqu.L("EXCLUDED.activation_strength"),
		"scoring_prompt_version": goqu.L("EXCLUDED.scoring_prompt_version"),
		"last_accessed_at":       goqu.L("EXCLUDED.last_accessed_at"),
		"version":                goqu.L("documents.version + 1"), // Optimistic Concurrency
	}
	if len(doc.Embedding.Vector) > 0 {
		record["embedding"] = pgvector.NewVector(doc.Embedding.Vector)
		update["embedding"] = goqu.L("EXCLUDED.embedding")
	}

	return dialect.Insert(TableDocuments).Rows(record).OnConflict(
		goqu.DoUpdate("id", update),
	)
}

func (p *PgVectorAdapter) Save(ctx context.Context, doc *domain.Document) error {
	sql, args, _ := p.getUpsertBuilder(doc).ToSQL()
	_, err := p.pool.Exec(ctx, sql, args...)
	return mapError("Save", err)
}

func (p *PgVectorAdapter) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapError("BeginBatch", err)
	}
	defer tx.Rollback(ctx)

	for _, doc := range docs {
		sql, args, _ := p.getUpsertBuilder(doc).ToSQL()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return mapError("ExecBatch", err)
		}
	}
	return tx.Commit(ctx)
}

func (p *PgVectorAdapter) Search(ctx context.Context, vector []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	floor := opts.RetrievalFloor
	if floor <= 0 {
		floor = 0.2
	}
	exploreRate := opts.ExplorationRate
	if exploreRate <= 0 {
		exploreRate = 0.05
	}

	// Over-fetch: get enough candidates for exploration pool (capped at 3000).
	overFetch := opts.TopK * 20
	if overFetch < 10 {
		overFetch = 10
	}
	const maxOverFetch = 3000
	if overFetch > maxOverFetch {
		overFetch = maxOverFetch
	}
	if opts.TopK <= 0 {
		overFetch = 10
	}

	allCandidates, err := p.fetchCandidates(ctx, vector, opts, overFetch)
	if err != nil {
		return nil, err
	}

	// Capture RawScore (pre-multiplier cosine) before applying floor-multiplier.
	// WorkspaceStage.enrich() uses RawScore for MinFactCosine filtering (PLANNERREQ REQ1).
	for i := range allCandidates {
		allCandidates[i].RawScore = allCandidates[i].Score
	}

	// Floor-multiplier re-ranking: cosine × (α + (1-α) × effectiveActivation).
	// When DecayLambda > 0, activation_strength is decayed by query-time temporal
	// function (ADR-0030); otherwise raw activation_strength is used (ADR-0015).
	now := time.Now()
	for i := range allCandidates {
		as := allCandidates[i].Document.ActivationStrength
		if opts.DecayLambda > 0 {
			as = domain.TemporalDecay(as, allCandidates[i].Document.LastAccessedAt, opts.DecayLambda, now)
		}
		allCandidates[i].Score = allCandidates[i].Score * (floor + (1-floor)*as)
	}
	sort.Slice(allCandidates, func(i, j int) bool {
		return allCandidates[i].Score > allCandidates[j].Score
	})

	// Exploration sampling: replace lowest-scored slots with random tail picks.
	exploreSlots := int(math.Ceil(float64(opts.TopK) * exploreRate))
	if exploreSlots == 0 && opts.TopK > 0 {
		exploreSlots = 1
	}
	if exploreSlots > opts.TopK {
		exploreSlots = opts.TopK
	}
	if exploreSlots > len(allCandidates)-opts.TopK {
		exploreSlots = len(allCandidates) - opts.TopK
		if exploreSlots < 0 {
			exploreSlots = 0
		}
	}

	result := make([]domain.SearchResult, 0, opts.TopK)
	regular := opts.TopK - exploreSlots
	for i := 0; i < regular && i < len(allCandidates); i++ {
		result = append(result, allCandidates[i])
	}

	if exploreSlots > 0 && len(allCandidates) > regular {
		tail := allCandidates[regular:]
		rand.Shuffle(len(tail), func(i, j int) { tail[i], tail[j] = tail[j], tail[i] })
		for i := 0; i < exploreSlots && i < len(tail); i++ {
			doc := tail[i].Document
			if doc.Metadata == nil {
				doc.Metadata = make(map[string]interface{})
			}
			doc.Metadata["exploration_slot"] = true
			result = append(result, domain.SearchResult{Document: doc, Score: tail[i].Score})
		}
	}

	if len(result) > opts.TopK {
		result = result[:opts.TopK]
	}
	return result, nil
}

// scopeExpressions builds the parameterized jsonb-containment predicates for an
// effective access scope (ADR-0034 D12), served by the existing idx_doc_metadata
// GIN (jsonb_path_ops) index. Returns nil for a nil or System scope (no
// filtering). Tags live under metadata.tags as a JSON array. This is the SQL
// mirror of domain.EffectiveScope.Allows.
func scopeExpressions(eff *domain.EffectiveScope) []goqu.Expression {
	if eff == nil || eff.System {
		return nil
	}
	contains := func(tag string) string {
		b, _ := json.Marshal(map[string][]string{"tags": {tag}})
		return string(b)
	}
	var exprs []goqu.Expression
	// Required: a doc must carry ALL → one AND containment term per tag.
	for _, r := range eff.RequiredTags {
		exprs = append(exprs, goqu.L("metadata @> ?::jsonb", contains(r)))
	}
	// AnyOfClauses (CNF): each clause is an OR of containment; clauses are ANDed.
	for _, clause := range eff.AnyOfClauses {
		if len(clause) == 0 {
			continue
		}
		ors := make([]goqu.Expression, 0, len(clause))
		for _, a := range clause {
			ors = append(ors, goqu.L("metadata @> ?::jsonb", contains(a)))
		}
		exprs = append(exprs, goqu.Or(ors...))
	}
	// Forbidden: a doc must NOT carry ANY → NOT(a OR b) == (NOT a) AND (NOT b).
	for _, f := range eff.ForbiddenTags {
		exprs = append(exprs, goqu.L("NOT (metadata @> ?::jsonb)", contains(f)))
	}
	return exprs
}

func (p *PgVectorAdapter) fetchCandidates(ctx context.Context, vector []float32, opts domain.SearchOptions, limit int) ([]domain.SearchResult, error) {
	ds := dialect.From(TableDocuments).
		Select("id", "text", "metadata", "access_count", "activation_strength", "scoring_prompt_version", "last_accessed_at", "created_at", "document_type", "version", "embedding", "summary", "section_path")

	if vector != nil {
		vec := pgvector.NewVector(vector)
		ds = ds.SelectAppend(goqu.L("embedding <=> ?", vec).As("distance")).
			Order(goqu.L("embedding <=> ?", vec).Asc()).
			Where(goqu.L("embedding IS NOT NULL"))
	} else {
		ds = ds.SelectAppend(goqu.V(0.0).As("distance")).
			Order(goqu.I("last_accessed_at").Desc())
	}

	if opts.DocumentType != "" {
		ds = ds.Where(goqu.Ex{"document_type": opts.DocumentType})
	}
	// ADR-0034: apply the three-set/CNF access predicate. ScopeSystem (or nil,
	// for legacy/system call sites not yet routed through ScopedVectorStore)
	// adds no predicate. The ScopedVectorStore decorator is the fail-closed gate;
	// this is the SQL-building mirror of EffectiveScope.Allows.
	for _, expr := range scopeExpressions(opts.Scope) {
		ds = ds.Where(expr)
	}
	// Wire the previously-dead SearchOptions.Filter as a raw additional predicate.
	if opts.Filter != "" {
		ds = ds.Where(goqu.L(opts.Filter))
	}
	if limit > 0 {
		ds = ds.Limit(uint(limit))
	}

	sql, args, err := ds.ToSQL()
	if err != nil {
		return nil, mapError("BuildSearch", err)
	}

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("Search", err)
	}
	defer rows.Close()

	var results []domain.SearchResult
	for rows.Next() {
		doc, distance, err := scanDocument(rows, vector != nil)
		if err != nil {
			return nil, mapError("ScanSearch", err)
		}
		results = append(results, domain.SearchResult{Document: doc, Score: 1.0 - distance})
	}
	return results, rows.Err()
}

// LexicalSearch is the sparse/lexical half of hybrid retrieval (ADR-0054): a
// Postgres full-text match ranked by ts_rank_cd, returning the same Document
// shape (incl. embedding, so the downstream blend can score it). It catches
// exact-token chunks (names, titles, places) that the dense embedder ranks low.
//
// The query is OR-of-lexemes (plainto's AND is too strict for QA — a chunk that
// answers "what books has X read" may not contain the word "books"). plainto_tsquery
// keeps it injection-safe; the text is a bound parameter. Scope + doc-type are the
// SAME predicates as the vector search (scopeExpressions), so this is not a scope hole.
// Results are ordered best-lexical-first; the caller uses the ORDER (RRF), not the
// raw rank value.
func (p *PgVectorAdapter) LexicalSearch(ctx context.Context, queryText string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(queryText) == "" {
		return nil, nil
	}
	// OR-tsquery: plainto -> text (' & ' joined lexemes) -> swap & for | -> tsquery.
	orQuery := goqu.L("replace(plainto_tsquery('english', ?)::text, '&', '|')::tsquery", queryText)
	ds := dialect.From(TableDocuments).
		Select("id", "text", "metadata", "access_count", "activation_strength", "scoring_prompt_version", "last_accessed_at", "created_at", "document_type", "version", "embedding", "summary", "section_path").
		SelectAppend(goqu.L("ts_rank_cd(to_tsvector('english', text), replace(plainto_tsquery('english', ?)::text, '&', '|')::tsquery)", queryText).As("distance")).
		Where(goqu.L("to_tsvector('english', text) @@ ?", orQuery)).
		Order(goqu.C("distance").Desc())

	if opts.DocumentType != "" {
		ds = ds.Where(goqu.Ex{"document_type": opts.DocumentType})
	}
	for _, expr := range scopeExpressions(opts.Scope) {
		ds = ds.Where(expr)
	}
	if opts.Filter != "" {
		ds = ds.Where(goqu.L(opts.Filter))
	}
	if opts.TopK > 0 {
		ds = ds.Limit(uint(opts.TopK))
	}

	sql, args, err := ds.ToSQL()
	if err != nil {
		return nil, mapError("BuildLexicalSearch", err)
	}
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("LexicalSearch", err)
	}
	defer rows.Close()
	var results []domain.SearchResult
	for rows.Next() {
		// hasDistance=true scans the appended ts_rank column; we keep it as Score
		// only for monotonicity (RRF re-scores by rank position, ignores the value).
		doc, rank, scanErr := scanDocument(rows, true)
		if scanErr != nil {
			return nil, mapError("ScanLexicalSearch", scanErr)
		}
		results = append(results, domain.SearchResult{Document: doc, Score: rank})
	}
	return results, rows.Err()
}

func (p *PgVectorAdapter) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	// 1. Fetch the document
	sql, args, _ := dialect.From(TableDocuments).
		Select("id", "text", "metadata", "access_count", "activation_strength", "scoring_prompt_version", "last_accessed_at", "created_at", "document_type", "version", "embedding", "summary", "section_path").
		Where(goqu.Ex{"id": id}).ToSQL()

	doc, _, err := scanDocument(p.pool.QueryRow(ctx, sql, args...), false)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, mapError("GetByID", err)
	}

	return &doc, nil
}

func (p *PgVectorAdapter) Delete(ctx context.Context, id string) error {
	sql, args, _ := dialect.Delete(TableDocuments).Where(goqu.Ex{"id": id}).ToSQL()
	_, err := p.pool.Exec(ctx, sql, args...)
	return mapError("Delete", err)
}

func (p *PgVectorAdapter) DeleteBatch(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	sql, args, _ := dialect.Delete(TableDocuments).Where(goqu.Ex{"id": ids}).ToSQL()
	_, err := p.pool.Exec(ctx, sql, args...)
	return mapError("DeleteBatch", err)
}

// ListIDsByType returns the IDs of every document of the given document_type.
// It backs the startup index reconcile (ADR-0044/0046): tool and skill
// descriptors are persisted DocType{Tool,Skill} documents, but the in-memory
// registries are rebuilt from disk + connected MCP servers each boot — so a
// tool/skill whose source is gone (a removed MCP server, a deleted SKILL.md)
// leaves an orphaned index document that find_tools / find_skills could still
// surface. Diffing this list against the freshly-built registry yields the
// orphans to Delete. Deliberately NOT on the VectorStore port — it is a
// boot-only maintenance query on the concrete adapter, so no fake must grow it.
func (p *PgVectorAdapter) ListIDsByType(ctx context.Context, docType string) ([]string, error) {
	sql, args, _ := dialect.From(TableDocuments).
		Select("id").
		Where(goqu.Ex{"document_type": docType}).ToSQL()
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("ListIDsByType", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapError("ListIDsByType.Scan", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError("ListIDsByType.Rows", err)
	}
	return ids, nil
}

func (p *PgVectorAdapter) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// goqu.Ex{"id": ids} maps to Postgres 'id = ANY($1)' or 'IN ($1, $2...)'
	// logic automatically.
	sql, args, err := dialect.From(TableDocuments).
		Select("id", "text", "metadata", "access_count", "activation_strength", "scoring_prompt_version", "last_accessed_at", "created_at", "document_type", "version", "embedding", "summary", "section_path").
		Where(goqu.Ex{"id": ids}).ToSQL()

	if err != nil {
		return nil, mapError("BuildGetBatch", err)
	}

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("GetBatch", err)
	}
	defer rows.Close()

	var results []domain.Document
	for rows.Next() {
		// Call the central scanner (distance not requested)
		doc, _, err := scanDocument(rows, false)
		if err != nil {
			return nil, mapError("ScanGetBatch", err)
		}
		results = append(results, doc)
	}

	// Cursor error check (Audit 6: Fail-Fast)
	if err := rows.Err(); err != nil {
		return nil, mapError("RowsGetBatch", err)
	}

	return results, nil
}

func (p *PgVectorAdapter) GetStaleMemories(ctx context.Context, limit int) ([]domain.Document, error) {
	// Fetches the lowest-activation, least-accessed/oldest memories
	sql, args, _ := dialect.From(TableDocuments).
		Select("id", "text", "metadata", "access_count", "activation_strength", "scoring_prompt_version", "last_accessed_at", "created_at", "document_type", "version", "embedding", "summary", "section_path").
		Where(goqu.Ex{"activation_strength": goqu.Op{"lt": 0.5}}).
		Order(goqu.I("access_count").Asc(), goqu.I("last_accessed_at").Asc().NullsFirst()).
		Limit(uint(limit)).ToSQL()

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("GetStaleMemories", err)
	}
	defer rows.Close()

	var results []domain.Document
	for rows.Next() {
		doc, _, err := scanDocument(rows, false)
		if err != nil {
			return nil, mapError("ScanStaleMemories", err)
		}
		results = append(results, doc)
	}
	return results, rows.Err()
}

// UpdateActivationStrength increments activation_strength by delta, clamped to [0.0, 0.8] via stored procedure.
func (p *PgVectorAdapter) UpdateActivationStrength(ctx context.Context, docID string, delta float64) error {
	_, err := p.pool.Exec(ctx, "SELECT update_activation_strength($1, $2);", docID, delta)
	return mapError("UpdateActivationStrength", err)
}

// CountStaleDocuments returns the number of documents with activation_strength below the given threshold.
// Used by CircadianRhythm to detect pg_cron being disabled (ADR-0015).
func (p *PgVectorAdapter) CountStaleDocuments(ctx context.Context, threshold float64) (int, error) {
	var count int
	err := p.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM documents WHERE activation_strength < $1", threshold,
	).Scan(&count)
	return count, mapError("CountStaleDocuments", err)
}

func (p *PgVectorAdapter) IncrementAccess(ctx context.Context, id string) error {
	sql, args, _ := dialect.Update(TableDocuments).
		Set(goqu.Record{
			"access_count":     goqu.L("access_count + 1"),
			"last_accessed_at": goqu.L("CURRENT_TIMESTAMP"),
		}).
		Where(goqu.Ex{"id": id}).ToSQL()

	_, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return mapError("IncrementAccess", err)
	}

	return p.UpdateActivationStrength(ctx, id, 0.05)
}

// QueryByMetadata returns documents whose metadata JSONB contains all key-value
// pairs in filter (@> containment), ordered by created_at ASC. limit=0 returns all. ADR-0033.
func (p *PgVectorAdapter) QueryByMetadata(ctx context.Context, filter map[string]string, limit int) ([]domain.Document, error) {
	filterBytes, err := json.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("QueryByMetadata: marshal filter: %w", err)
	}

	q := dialect.From(TableDocuments).
		Select("id", "text", "metadata", "access_count", "activation_strength", "scoring_prompt_version", "last_accessed_at", "created_at", "document_type", "version", "embedding", "summary", "section_path").
		Where(goqu.L("metadata @> ?::jsonb", string(filterBytes))).
		Order(goqu.I("created_at").Asc())
	if limit > 0 {
		q = q.Limit(uint(limit))
	}
	sqlStr, args, _ := q.ToSQL()

	rows, err := p.pool.Query(ctx, sqlStr, args...)
	if err != nil {
		return nil, mapError("QueryByMetadata", err)
	}
	defer rows.Close()

	var docs []domain.Document
	for rows.Next() {
		doc, _, scanErr := scanDocument(rows, false)
		if scanErr != nil {
			return nil, mapError("QueryByMetadata scan", scanErr)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError("QueryByMetadata rows", err)
	}
	return docs, nil
}

// ── GraphStore interface (ADR-0017) ──────────────────────────────────────

// SaveEdge inserts or updates a document edge.
func (p *PgVectorAdapter) SaveEdge(ctx context.Context, edge domain.DocumentEdge) error {
	sql, args, _ := dialect.Insert(TableEdges).Rows(goqu.Record{
		"source_id": edge.SourceID,
		"target_id": edge.TargetID,
		"edge_type": string(edge.EdgeType),
		"weight":    edge.Weight,
	}).OnConflict(goqu.DoUpdate("source_id, target_id, edge_type", goqu.Record{
		"weight": goqu.L("EXCLUDED.weight"),
	})).ToSQL()

	_, err := p.pool.Exec(ctx, sql, args...)
	return mapError("SaveEdge", err)
}

// GetAdjacentEdges returns all edges where source_id is in docIDs.
// Slice-only API enforces batch-per-depth query pattern (ADR-0017 review).
func (p *PgVectorAdapter) GetAdjacentEdges(ctx context.Context, docIDs []string) ([]domain.DocumentEdge, error) {
	if len(docIDs) == 0 {
		return nil, nil
	}

	sql, args, _ := dialect.From(TableEdges).
		Select("source_id", "target_id", "edge_type", "weight", "created_at").
		Where(goqu.Ex{"source_id": docIDs}).ToSQL()

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("GetAdjacentEdges", err)
	}
	defer rows.Close()

	var edges []domain.DocumentEdge
	for rows.Next() {
		var e domain.DocumentEdge
		var edgeType string
		if err := rows.Scan(&e.SourceID, &e.TargetID, &edgeType, &e.Weight, &e.CreatedAt); err != nil {
			return nil, mapError("ScanEdges", err)
		}
		e.EdgeType = domain.EdgeType(edgeType)
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// UpdateEdgeWeight updates the weight of a specific edge.
func (p *PgVectorAdapter) UpdateEdgeWeight(ctx context.Context, sourceID, targetID string, edgeType domain.EdgeType, newWeight float32) error {
	sql, args, _ := dialect.Update(TableEdges).
		Set(goqu.Record{"weight": newWeight}).
		Where(goqu.Ex{"source_id": sourceID, "target_id": targetID, "edge_type": string(edgeType)}).ToSQL()

	_, err := p.pool.Exec(ctx, sql, args...)
	return mapError("UpdateEdgeWeight", err)
}

// SaveChunkTriplets persists a batch of (h, r, t) triplets for one chunk.
// Idempotent on (chunk_id, h, r, t) — repeated inserts are no-ops via
// ON CONFLICT DO NOTHING. Used by the LLM extraction at write time.
func (p *PgVectorAdapter) SaveChunkTriplets(ctx context.Context, chunkID string, triplets []memory.ChunkTriplet) error {
	if chunkID == "" || len(triplets) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapError("BeginTriplets", err)
	}
	defer tx.Rollback(ctx)

	// Build the prepared insert once (parameterized) and run per row. The
	// columns are TEXT / REAL — no explicit cast needed (the SQL gets
	// placeholder params $1..$5 that pgx infers from the column types).
	// We bypass goqu here because goqu's parameterized-Vals + ON CONFLICT
	// combo is awkward; a literal $1..$5 string is clearer and faster.
	// ADR-0053 D2 (revised, migration 009): also persist confidence + sources[]
	// from the tiered extractor. Both are nullable — a nil pointer / empty slice
	// inserts NULL (legacy rows, read as filler).
	insertSQL := fmt.Sprintf(
		`INSERT INTO %s (chunk_id, h, r, t, weight, confidence, sources) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		TableChunkTriplets,
	)

	for _, t := range triplets {
		if t.H == "" || t.R == "" || t.T == "" {
			continue
		}
		var conf any
		if t.Confidence != nil {
			conf = *t.Confidence
		}
		var srcs any
		if len(t.Sources) > 0 {
			srcs = t.Sources // pgx maps []string -> text[]
		}
		if _, err := tx.Exec(ctx, insertSQL, chunkID, t.H, t.R, t.T, t.Weight, conf, srcs); err != nil {
			return mapError("SaveChunkTriplets", err)
		}
	}
	return tx.Commit(ctx)
}

// ForChunk returns the (h, r, t) triplets extracted from one chunk.
func (p *PgVectorAdapter) ForChunk(ctx context.Context, chunkID string) ([]memory.ChunkTriplet, error) {
	if chunkID == "" {
		return nil, nil
	}
	sql, args, _ := dialect.From(TableChunkTriplets).
		Select("h", "r", "t", "weight").
		Where(goqu.Ex{"chunk_id": chunkID}).ToSQL()
	return p.queryChunkTriplets(ctx, sql, args...)
}

// ForChunks returns the triplets for many chunks, keyed by chunk ID.
// One query; cheaper than N round-trips.
func (p *PgVectorAdapter) ForChunks(ctx context.Context, chunkIDs []string) (map[string][]memory.ChunkTriplet, error) {
	if len(chunkIDs) == 0 {
		return map[string][]memory.ChunkTriplet{}, nil
	}
	sql, args, _ := dialect.From(TableChunkTriplets).
		Select("chunk_id", "h", "r", "t", "weight").
		Where(goqu.Ex{"chunk_id": chunkIDs}).ToSQL()
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("GetChunkTripletsBatch", err)
	}
	defer rows.Close()
	out := make(map[string][]memory.ChunkTriplet, len(chunkIDs))
	for rows.Next() {
		var (
			cid, h, r, t string
			w            float64
		)
		if err := rows.Scan(&cid, &h, &r, &t, &w); err != nil {
			return nil, mapError("ScanChunkTriplets", err)
		}
		out[cid] = append(out[cid], memory.ChunkTriplet{H: h, R: r, T: t, Weight: w})
	}
	return out, rows.Err()
}

// ChunksMentioningEntity returns the chunk IDs that have a triplet
// referencing the given entity (head OR tail). Match is case-insensitive.
// Returns at most `limit` chunk IDs, ordered by recency (extracted_at DESC).
func (p *PgVectorAdapter) ChunksMentioningEntity(ctx context.Context, entity string, limit int) ([]string, error) {
	if entity == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	e := strings.ToLower(strings.TrimSpace(entity))
	sql, args, _ := dialect.From(TableChunkTriplets).
		Select("chunk_id").
		Where(goqu.ExOr{
			"h": e,
			"t": e,
		}).
		GroupBy("chunk_id").
		Order(goqu.L("MAX(extracted_at)").Desc()).
		Limit(uint(limit)).ToSQL()
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("ChunksMentioningEntity", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			return nil, mapError("ScanChunksMentioningEntity", err)
		}
		out = append(out, cid)
	}
	return out, rows.Err()
}

func (p *PgVectorAdapter) queryChunkTriplets(ctx context.Context, sql string, args ...any) ([]memory.ChunkTriplet, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError("queryChunkTriplets", err)
	}
	defer rows.Close()
	var out []memory.ChunkTriplet
	for rows.Next() {
		var h, r, t string
		var w float64
		if err := rows.Scan(&h, &r, &t, &w); err != nil {
			return nil, mapError("ScanChunkTriplet", err)
		}
		out = append(out, memory.ChunkTriplet{H: h, R: r, T: t, Weight: w})
	}
	return out, rows.Err()
}

// ── PageRank (ADR-0054 D2): source reads + score store ──────────────────────

// LoadChunkEntities returns every chunk with its triplet entities (h+t), the
// input to ComputePageRank. One scan of chunk_triplets, folded in Go.
func (p *PgVectorAdapter) LoadChunkEntities(ctx context.Context) ([]memory.ChunkEntities, error) {
	rows, err := p.pool.Query(ctx, fmt.Sprintf(`SELECT chunk_id, h, t FROM %s`, TableChunkTriplets))
	if err != nil {
		return nil, mapError("LoadChunkEntities", err)
	}
	defer rows.Close()
	order := make([]string, 0)
	byChunk := make(map[string]map[string]struct{})
	for rows.Next() {
		var cid, h, t string
		if err := rows.Scan(&cid, &h, &t); err != nil {
			return nil, mapError("ScanChunkEntities", err)
		}
		if cid == "" {
			continue
		}
		set := byChunk[cid]
		if set == nil {
			set = make(map[string]struct{})
			byChunk[cid] = set
			order = append(order, cid)
		}
		if h != "" {
			set[h] = struct{}{}
		}
		if t != "" {
			set[t] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapError("LoadChunkEntities", err)
	}
	out := make([]memory.ChunkEntities, 0, len(order))
	for _, id := range order {
		ents := make([]string, 0, len(byChunk[id]))
		for e := range byChunk[id] {
			ents = append(ents, e)
		}
		out = append(out, memory.ChunkEntities{ChunkID: id, Entities: ents})
	}
	return out, nil
}

// CorpusStats returns (distinct chunk count, total triplet rows).
func (p *PgVectorAdapter) CorpusStats(ctx context.Context) (int, int, error) {
	var chunks, triplets int
	err := p.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(DISTINCT chunk_id), COUNT(*) FROM %s`, TableChunkTriplets),
	).Scan(&chunks, &triplets)
	if err != nil {
		return 0, 0, mapError("CorpusStats", err)
	}
	return chunks, triplets, nil
}

// SaveChunkPageRanks replaces the whole chunk_pagerank table (the score is a
// derived snapshot) in one transaction via COPY, then upserts the meta row.
func (p *PgVectorAdapter) SaveChunkPageRanks(ctx context.Context, scores map[string]float64, chunkCount, tripletCount int) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapError("BeginPageRank", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf(`TRUNCATE %s`, TableChunkPagerank)); err != nil {
		return mapError("TruncatePageRank", err)
	}
	if len(scores) > 0 {
		rows := make([][]any, 0, len(scores))
		now := time.Now()
		for id, s := range scores {
			rows = append(rows, []any{id, float32(s), now})
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{TableChunkPagerank},
			[]string{"chunk_id", "score", "computed_at"}, pgx.CopyFromRows(rows))
		if err != nil {
			return mapError("CopyPageRank", err)
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, computed_at, chunk_count, triplet_count) VALUES (1, NOW(), $1, $2)
		 ON CONFLICT (id) DO UPDATE SET computed_at = NOW(), chunk_count = $1, triplet_count = $2`,
		TableChunkPagerankMeta), chunkCount, tripletCount); err != nil {
		return mapError("UpsertPageRankMeta", err)
	}
	return tx.Commit(ctx)
}

// ChunkPageRanks returns the PageRank score for the given chunk ids (missing
// ids are simply absent from the map — callers treat absent as 0).
func (p *PgVectorAdapter) ChunkPageRanks(ctx context.Context, ids []string) (map[string]float64, error) {
	out := make(map[string]float64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx,
		fmt.Sprintf(`SELECT chunk_id, score FROM %s WHERE chunk_id = ANY($1)`, TableChunkPagerank), ids)
	if err != nil {
		return nil, mapError("ChunkPageRanks", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, mapError("ScanChunkPageRank", err)
		}
		out[id] = score
	}
	return out, rows.Err()
}

// MeanChunkConfidence returns the average triplet confidence per chunk (NULL
// confidences count as 0 — legacy/filler). Used as the Stage-A blend signal.
func (p *PgVectorAdapter) MeanChunkConfidence(ctx context.Context, ids []string) (map[string]float64, error) {
	out := make(map[string]float64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, fmt.Sprintf(
		`SELECT chunk_id, AVG(COALESCE(confidence, 0))::float8 FROM %s WHERE chunk_id = ANY($1) GROUP BY chunk_id`,
		TableChunkTriplets), ids)
	if err != nil {
		return nil, mapError("MeanChunkConfidence", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var mean float64
		if err := rows.Scan(&id, &mean); err != nil {
			return nil, mapError("ScanMeanConfidence", err)
		}
		out[id] = mean
	}
	return out, rows.Err()
}

// PageRankMeta returns the last recompute's freshness + corpus counts. A missing
// meta row (never computed) yields the zero time.
func (p *PgVectorAdapter) PageRankMeta(ctx context.Context) (time.Time, int, int, error) {
	var computedAt time.Time
	var chunkCount, tripletCount int
	err := p.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT computed_at, chunk_count, triplet_count FROM %s WHERE id = 1`, TableChunkPagerankMeta),
	).Scan(&computedAt, &chunkCount, &tripletCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, 0, 0, nil
		}
		return time.Time{}, 0, 0, mapError("PageRankMeta", err)
	}
	return computedAt, chunkCount, tripletCount, nil
}
